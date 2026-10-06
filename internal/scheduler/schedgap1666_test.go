package scheduler

// SCHED-GAP-1666 — the slot-pool admission gate: a nudge-sourced spawn
// (startup resume / board wake / wave or queue replay) must pass the SAME
// per-lane effective-cooldown decision the packer applies.
//
// This file is the load-bearing guard against a future entry point being
// added without the gate (deliverable (d)), plus the 7-day/3-restart
// simulation the acceptance names.
//
// TestSCHEDGAP1666_GateConsultsAllEntryPoints — the caller enumeration:
//   * a static table naming every non-test source that hands a tick to the
//     slot pool, with the gate call each entry point must reach. A NEW file
//     with a pool call, or an entry point whose gate call disappears, fails
//     the test;
//   * runtime probes asserting the behaviour of each entry point: the
//     operator bypass admits (and logs), the resume entry admits ONLY an
//     in-flight-interrupted row, the board-wake entry admits ONLY a
//     tasks-admission lane the shared gate sanctions, and the packer's
//     mirror refuses a lane inside its pin.
//
// TestSCHEDGAP1666_GateDecisionTable — the predicate itself, table-driven:
// every bypass is refused unless its entitlement is present.
//
// TestSCHEDGAP1666_SevenDaySimulation — 7 simulated days, continuous board
// writes, 3 restarts: the 72h cooldown lane's median inter-tick gap stays
// >= 48h, while the same restarts DO resume a lane whose tick was left
// running in flight, and the tasks lane parks, flips on the first
// non-perpetual row, and does not flip on a perpetual-only write.

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// ─────────────────────────────────────────────────────────────────────────
// 1. The declared admission surface (deliverable (d)).
// ─────────────────────────────────────────────────────────────────────────

// schedGap1666PoolCallFiles is the CLOSED set of non-test sources allowed to
// hand a tick to the slot pool. A new file here is a new admission surface:
// it must be added to this list AND to schedGap1666EntryPoints with the gate
// call it reaches, or this test fails.
var schedGap1666PoolCallFiles = []string{"loop.go", "session_resume.go", "tick_process.go"}

// schedGap1666EntryPoint is one way work can enter the slot pool.
type schedGap1666EntryPoint struct {
	// Name is the human label used in failure messages.
	Name string
	// EnqueueFile / EnqueueSnippet: the literal text that hands a tick to
	// the pool. Empty snippet = this entry point admits through the pass the
	// packer drives (the packer entry points).
	EnqueueFile    string
	EnqueueSnippet string
	// GateFile / GateSnippet: where and how this entry point reaches the ONE
	// shared admission predicate. The snippet must be present VERBATIM in
	// GateFile.
	GateFile    string
	GateSnippet string
	// Deliverable is the letter of the brief this entry point implements.
	Deliverable string
}

// schedGap1666EntryPoints enumerates every caller of
// SlotPool.Spawn/SpawnEnqueued (and the packer paths whose selection is what
// actually enters the pool) together with the shared-predicate call each one
// must reach. Table-driven on purpose: adding a caller without adding its row
// makes the enumeration fail.
var schedGap1666EntryPoints = []schedGap1666EntryPoint{
	{
		Name:        "packer-namespace selection gate",
		GateFile:    "packer_select.go",
		GateSnippet: "gate := effectiveCooldownGate(CooldownGateRequest{",
		Deliverable: "(a) the packer keeps gating, now through the shared predicate",
	},
	{
		Name:        "packer-flat fallback gate",
		GateFile:    "multipool_packer.go",
		GateSnippet: "gate := effectiveCooldownGate(CooldownGateRequest{",
		Deliverable: "(a) the flat fallback reaches the same predicate",
	},
	{
		Name:        "packer-legacy (greedy pack + isOverdue)",
		GateFile:    "packer.go",
		GateSnippet: "dec := p.effectiveCooldownGate(s, now)",
		Deliverable: "(a) the legacy path's effective cooldown IS the gate",
	},
	{
		Name:        "loop eligibility mirror (GAP-043)",
		GateFile:    "loop.go",
		GateSnippet: "gate := effectiveCooldownGate(CooldownGateRequest{",
		Deliverable: "(a) the zero-select mirror cannot disagree with the gate",
	},
	{
		Name:           "manual / operator spawn (Loop.SpawnNow)",
		EnqueueFile:    "loop.go",
		EnqueueSnippet: "l.slotPool.SpawnEnqueued(proj, tickID, l.clock().Now(), noDeliver, l.db)",
		GateFile:       "loop.go",
		GateSnippet:    "l.effectiveCooldownGate(cooldownGateCall{Project: proj.Name, Bypass: CooldownBypassManual})",
		Deliverable:    "(e) the one explicit, logged bypass",
	},
	{
		Name:           "startup / reconnect resume (resumeOrphans)",
		EnqueueFile:    "session_resume.go",
		EnqueueSnippet: "l.slotPool.SpawnEnqueued(packed, tickID, l.clock().Now(), noDeliver, l.db)",
		GateFile:       "session_resume.go",
		GateSnippet:    "if dec, ok := l.effectiveCooldownGate(cooldownGateCall{",
		Deliverable:    "(c) continuity only for an interrupted in-flight tick",
	},
	{
		Name:           "packer-driven evaluate spawn (tick_process)",
		EnqueueFile:    "tick_process.go",
		EnqueueSnippet: "spawnedTickID := l.slotPool.Spawn(proj, now, noDeliver, l.db)",
		GateFile:       "packer.go",
		GateSnippet:    "dec := p.effectiveCooldownGate(s, now)",
		Deliverable:    "(a) the set this pass spawns IS the packer's gated selection",
	},
	{
		Name:        "board wake (deliverable (b))",
		GateFile:    "board_wake.go",
		GateSnippet: "return l.effectiveCooldownGate(cooldownGateCall{",
		Deliverable: "(b) the only sanctioned board-write bypass",
	},
}

// schedGap1666NonTestSources reads every non-test .go source of this package,
// keyed by base name.
func schedGap1666NonTestSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(b)
	}
	return out
}

// TestSCHEDGAP1666_GateConsultsAllEntryPoints is the table-driven caller
// enumeration (deliverable (d)). It fails when a new file hands a tick to the
// slot pool without declaring its gate, when a declared entry point loses its
// gate call, or when a runtime entry point stops consulting the gate.
func TestSCHEDGAP1666_GateConsultsAllEntryPoints(t *testing.T) {
	src := schedGap1666NonTestSources(t)

	// (1) The declared surface is CLOSED: any non-test file calling into the
	// pool must be listed, so a new entry point cannot be added silently.
	declared := map[string]bool{}
	for _, f := range schedGap1666PoolCallFiles {
		declared[f] = true
	}
	var found []string
	for name, body := range src {
		if strings.Contains(body, "slotPool.Spawn") || strings.Contains(body, "slotPool.SpawnEnqueued") {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	for _, name := range found {
		if !declared[name] {
			t.Errorf("%s hands a tick to the slot pool but is not in schedGap1666PoolCallFiles — declare it AND its shared-gate call in schedGap1666EntryPoints, or route it through an existing entry point", name)
		}
	}
	for name := range declared {
		if _, ok := src[name]; !ok {
			t.Errorf("declared pool-call file %s does not exist in the package", name)
		}
	}

	// (2) Every declared entry point must still reach the shared predicate,
	// and every enqueue snippet must still be present.
	gateCallers := 0
	for _, e := range schedGap1666EntryPoints {
		body, ok := src[e.GateFile]
		if !ok {
			t.Errorf("entry point %q names gate file %s, which is not a source of this package", e.Name, e.GateFile)
			continue
		}
		if e.GateSnippet != "" && !strings.Contains(body, e.GateSnippet) {
			t.Errorf("entry point %q (%s) no longer reaches the shared admission gate: %q not found in %s",
				e.Name, e.Deliverable, e.GateSnippet, e.GateFile)
		}
		if e.EnqueueFile != "" && e.EnqueueSnippet != "" {
			eb, ok := src[e.EnqueueFile]
			if !ok || !strings.Contains(eb, e.EnqueueSnippet) {
				t.Errorf("entry point %q: enqueue snippet %q not found in %s", e.Name, e.EnqueueSnippet, e.EnqueueFile)
			}
		}
	}
	for _, body := range src {
		gateCallers += strings.Count(body, "effectiveCooldownGate")
	}
	// One definition (+ its doc/comment mentions) and 5+ call sites: the
	// acceptance threshold, asserted here so a refactor cannot quietly drop
	// an entry point's gate.
	if gateCallers < 6 {
		t.Errorf("effectiveCooldownGate appears %d time(s) across the package's non-test sources, want >= 6 (one definition + 5+ call sites)", gateCallers)
	}

	// (3) Runtime probes: each entry point's BEHAVIOUR, not just its text.
	for _, p := range schedGap1666RuntimeProbes {
		p := p
		t.Run("runtime/"+p.Name, func(t *testing.T) { p.Run(t) })
	}
}

// schedGap1666RuntimeProbe is one behavioural assertion about an entry point.
type schedGap1666RuntimeProbe struct {
	Name string
	Run  func(t *testing.T)
}

// schedGap1666RuntimeProbes asserts the observable effect of the gate on
// every entry point that is not the packer (whose refusal is asserted by the
// packer's own batteries plus the simulation below).
var schedGap1666RuntimeProbes = []schedGap1666RuntimeProbe{
	{Name: "resume-admits-only-in-flight", Run: schedGap1666ProbeResumeInFlight},
	{Name: "board-wake-refuses-cooldown-lane", Run: schedGap1666ProbeBoardWake},
	{Name: "manual-is-the-explicit-bypass", Run: schedGap1666ProbeManual},
	{Name: "packer-mirror-refuses-inside-pin", Run: schedGap1666ProbePackerRefuses},
}

// schedGap1666ProbeLane inserts a lane parked INSIDE its cooldown with a
// single pending board row (so the SCHED-GAP-1655 no-work gate is inert) and
// returns its workdir.
func schedGap1666ProbeLane(t *testing.T, db *sql.DB, name string, cooldownS int, mode string, age time.Duration) {
	t.Helper()
	wd, _ := gap1660BoardDir(t, `{"id":"P1","status":"pending","title":"probe work"}`)
	insertGap1660Project(t, db, name, wd, cooldownS, mode)
	gap1660SetLastCompleted(t, db, name, age)
}

// schedGap1666ProbeResumeInFlight is (c): the resume entry admits a lane
// inside its cooldown ONLY when the orphan row was left running in flight,
// and the scan never even offers a row without that mark.
func schedGap1666ProbeResumeInFlight(t *testing.T) {
	db := newTestDB(t)
	const inFlight, noMark = "gap1666-probe-inflight", "gap1666-probe-nomark"
	schedGap1666ProbeLane(t, db, inFlight, 72*3600, database.AdmissionModeCooldown, time.Hour)
	schedGap1666ProbeLane(t, db, noMark, 72*3600, database.AdmissionModeCooldown, time.Hour)

	// Arm 1: an in-flight drop mark — continuity, admitted ahead of the pin.
	orphanTickRow(t, db, "gap1666-orphan-inflight", inFlight, "failed", OrphanReasonStartupReap, 0)
	// Arm 2: an orphan row with NO in-flight mark — not evidence of an
	// interrupted tick, so the scan must not even offer it.
	orphanTickRow(t, db, "gap1666-orphan-nomark", noMark, "failed", "", 0)

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)
	defer l.Stop()

	offered := map[string]bool{}
	for _, o := range l.orphansToResume(t.Context()) {
		offered[o.id] = true
	}
	if !offered["gap1666-orphan-inflight"] {
		t.Fatalf("the in-flight-marked orphan was not offered for resume: %v", offered)
	}
	if offered["gap1666-orphan-nomark"] {
		t.Fatalf("an orphan row with no in-flight mark was offered for resume — a restart is not a cadence reset (c); offered=%v", offered)
	}

	l.resumeOrphansAtStartup()
	waitFor1660(t, 10*time.Second, func() bool {
		return tickRowExists(t, db, "gap1666-orphan-inflight-nudge1")
	})
	if tickRowExists(t, db, "gap1666-orphan-nomark-nudge1") {
		t.Fatal("an orphan without an in-flight mark was re-nudged — (c) refused it in the scan and the gate must refuse it too")
	}
}

// schedGap1666ProbeBoardWake is (b): the board-wake gate admits only what the
// shared predicate sanctions — a cooldown lane inside its pin is refused, a
// tasks lane with work is admitted, and the park mark scopes the flip.
func schedGap1666ProbeBoardWake(t *testing.T) {
	db := newTestDB(t)
	const cool, task, parked = "gap1666-probe-cool", "gap1666-probe-task", "gap1666-probe-parked"
	schedGap1666ProbeLane(t, db, cool, 72*3600, database.AdmissionModeCooldown, time.Hour)
	schedGap1666ProbeLane(t, db, task, 72*3600, database.AdmissionModeTasks, time.Hour)
	schedGap1666ProbeLane(t, db, parked, 72*3600, database.AdmissionModeTasks, time.Hour)
	noteParkedEmpty(parked, true)
	t.Cleanup(clearParkedEmpty)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	defer l.Stop()

	if dec, ok := boardWakeCooldownGate(l, cool); !ok || !dec.Defer {
		t.Errorf("board-wake gate on a cooldown lane inside its 72h pin = %+v (ok=%v), want Defer=true (a board write is structurally unable to act on a cooldown)", dec, ok)
	}
	if dec, ok := boardWakeCooldownGate(l, task); !ok || dec.Defer {
		t.Errorf("board-wake gate on a tasks lane with admissible board work = %+v (ok=%v), want Defer=false (the SCHED-GAP-124 waiver)", dec, ok)
	}
	if dec, ok := boardWakeCooldownGate(l, parked); !ok || dec.Defer {
		t.Errorf("board-wake gate on a parked-empty tasks lane whose board holds work = %+v (ok=%v), want Defer=false (the flipped park, (b))", dec, ok)
	}
	if dec, ok := boardWakeCooldownGate(l, "gap1666-probe-nonexistent"); ok {
		t.Errorf("board-wake gate on an unknown lane reported ok=true (%+v); an unreadable lane must fail open with ok=false", dec)
	}
}

// schedGap1666ProbeManual is (e): the operator entry point consults the same
// predicate, and the manual bypass is granted — the spawn is admitted and its
// row records the manual entry point.
func schedGap1666ProbeManual(t *testing.T) {
	db := newTestDB(t)
	const proj = "gap1666-probe-manual"
	schedGap1666ProbeLane(t, db, proj, 72*3600, database.AdmissionModeCooldown, time.Hour)

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)
	defer l.Stop()

	p, err := database.GetProject(t.Context(), db, proj)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	tickID, err := l.SpawnNow(*p)
	if err != nil {
		t.Fatalf("manual spawn inside the pin was refused: %v", err)
	}
	if tickID == "" {
		t.Fatal("manual spawn returned an empty tick id")
	}
	waitFor1660(t, 10*time.Second, func() bool {
		var nudge string
		return db.QueryRow(`SELECT COALESCE(nudge_source, '') FROM ticks WHERE id = ?`, tickID).Scan(&nudge) == nil &&
			nudge == NudgeSourceManual
	})
}

// schedGap1666ProbePackerRefuses is the packer arm: the eligibility mirror
// (same predicate) refuses a lane inside its pin, so a pass selects nothing.
func schedGap1666ProbePackerRefuses(t *testing.T) {
	db := newTestDB(t)
	const proj = "gap1666-probe-packer"
	schedGap1666ProbeLane(t, db, proj, 72*3600, database.AdmissionModeCooldown, time.Hour)

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	l.noDeliver = true
	gw.wire(l)
	defer l.Stop()

	if n := l.countEligibleProjects(time.Now(), map[string]bool{}); n != 0 {
		t.Errorf("countEligibleProjects = %d for a lane 1h into a 72h cooldown, want 0 — the mirror must agree with the gate", n)
	}
	l.evaluate()
	if n := schedGap1666TickCount(t, db, proj); n != 0 {
		t.Errorf("evaluate admitted %d tick(s) for a lane inside its 72h cooldown, want 0", n)
	}
}

// tickRowExists reports whether a tick row with the given id exists.
func tickRowExists(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count tick %s: %v", id, err)
	}
	return n > 0
}

// schedGap1666TickCount counts a lane's tick rows.
func schedGap1666TickCount(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, project).Scan(&n); err != nil {
		t.Fatalf("count ticks for %s: %v", project, err)
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────
// 2. The predicate's own decision table.
// ─────────────────────────────────────────────────────────────────────────

// TestSCHEDGAP1666_GateDecisionTable pins the shared predicate: every bypass
// is refused unless its entitlement is present, and the plain (no-bypass)
// decision is exactly "has the cooldown elapsed".
func TestSCHEDGAP1666_GateDecisionTable(t *testing.T) {
	now := time.Now()
	hourAgo := now.Add(-time.Hour)
	threeDaysAgo := now.Add(-72 * time.Hour)

	cases := []struct {
		name   string
		req    CooldownGateRequest
		defer_ bool
		bypass CooldownGateBypass
		reason string
	}{
		{
			name:   "never completed",
			req:    CooldownGateRequest{CooldownS: 72 * 3600},
			reason: AdmissionReasonOK,
		},
		{
			name:   "inside the pin",
			req:    CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo},
			defer_: true, reason: AdmissionReasonCooldown,
		},
		{
			name:   "pin elapsed",
			req:    CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &threeDaysAgo},
			reason: AdmissionReasonOK,
		},
		{
			name: "manual bypass is always granted",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				Bypass: CooldownBypassManual},
			bypass: CooldownBypassManual, reason: "resume:" + NudgeSourceManual,
		},
		{
			name: "continuity bypass needs the in-flight mark",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				Bypass: CooldownBypassInFlightContinuity, InFlightContinuity: true},
			bypass: CooldownBypassInFlightContinuity, reason: "resume:" + NudgeSourceStartup,
		},
		{
			name: "continuity claim without the mark is refused",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				Bypass: CooldownBypassInFlightContinuity, InFlightContinuity: false},
			defer_: true, reason: AdmissionReasonCooldown,
		},
		{
			name: "park-flip bypass needs a tasks lane with admissible work",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: true,
				Bypass: CooldownBypassTasksParkedFlip, ParkedEmpty: true},
			bypass: CooldownBypassTasksParkedFlip, reason: AdmissionReasonFlipBoardEmpty,
		},
		{
			name: "park-flip claim without admissible work is refused",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: false,
				Bypass: CooldownBypassTasksParkedFlip, ParkedEmpty: true},
			defer_: true, reason: AdmissionReasonCooldown,
		},
		{
			name: "board-wake claim on a non-parked tasks lane is the ordinary waiver",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: true,
				Bypass: CooldownBypassTasksParkedFlip, ParkedEmpty: false},
			bypass: CooldownBypassTasksParkedFlip, reason: AdmissionReasonOK,
		},
		{
			name: "park-flip claim on a cooldown lane is refused",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeCooldown,
				Bypass:        CooldownBypassTasksParkedFlip, ParkedEmpty: true, TasksWork: true},
			defer_: true, reason: AdmissionReasonCooldown,
		},
		{
			name: "tasks waiver admits a lane with work",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: true},
			reason: AdmissionReasonOK,
		},
		{
			name: "tasks waiver stands down after a failed tick",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: true,
				LastTickStatus: database.LastStatusFailed},
			defer_: true, reason: AdmissionReasonFailedCooldown,
		},
		{
			name: "tasks waiver stands down after repeated failures",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				AdmissionMode: database.AdmissionModeTasks, TasksWork: true,
				ConsecutiveFailures: 3},
			defer_: true, reason: AdmissionReasonFailedCooldown,
		},
		{
			name: "no bypass admits a cooldown lane with work on its board",
			req: CooldownGateRequest{CooldownS: 72 * 3600, LastCompleted: &hourAgo,
				Workdir: "/tmp/whatever", TasksWork: true},
			defer_: true, reason: AdmissionReasonCooldown,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dec := effectiveCooldownGate(c.req, now)
			if dec.Defer != c.defer_ {
				t.Errorf("Defer = %v, want %v (reason=%s remaining=%.0fs)", dec.Defer, c.defer_, dec.Reason, dec.RemainingS)
			}
			if dec.Bypass != c.bypass {
				t.Errorf("Bypass = %q, want %q", dec.Bypass, c.bypass)
			}
			if dec.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", dec.Reason, c.reason)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 3. The 7-day / 3-restart simulation.
// ─────────────────────────────────────────────────────────────────────────

// schedGap1666Rows renders a board body (one JSON object per line).
func schedGap1666Rows(lines ...string) []string { return lines }

// schedGap1666WaitSettled blocks until no tick row is queued/running, so a
// simulated step never runs into the slot pool's dedup on a live tick.
func schedGap1666WaitSettled(t *testing.T, db *sql.DB, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status IN ('queued','running')`).Scan(&n); err == nil && n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ticks did not settle within the wait budget")
}

// schedGap1666CompletionTimes returns a lane's terminal tick instants in id
// order (the sim clock's stamps, which is what the cadence is measured on).
func schedGap1666CompletionTimes(t *testing.T, db *sql.DB, project string) []time.Time {
	t.Helper()
	rows, err := db.Query(`SELECT COALESCE(completed_at, '') FROM ticks WHERE project_name = ? ORDER BY id`, project)
	if err != nil {
		t.Fatalf("query completions for %s: %v", project, err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse completed_at %q: %v", s, err)
		}
		out = append(out, ts)
	}
	return out
}

// schedGap1666MedianGap returns the median inter-tick gap for a lane, and the
// number of gaps (0 when the lane never ticked twice).
func schedGap1666MedianGap(t *testing.T, db *sql.DB, project string) (time.Duration, int) {
	t.Helper()
	times := schedGap1666CompletionTimes(t, db, project)
	if len(times) < 2 {
		return 0, 0
	}
	gaps := make([]time.Duration, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2], len(gaps)
}

// TestSCHEDGAP1666_SevenDaySimulation is the acceptance simulation: SEVEN
// simulated days of continuous board writes with THREE restarts.
//
//   - lane "cool" (72h cooldown, never interrupted): median inter-tick gap
//     must stay >= 48h. Before the gate, every restart re-nudged whatever
//     orphaned rows existed and every board write stamped a wake — the
//     resume-heavy admissions SCHED-GAP-1666 exists to stop.
//   - lane "interrupted" (72h cooldown, its tick left RUNNING at each
//     restart): MUST be resumed each time — the continuity arm, so the
//     assertion above cannot pass vacuously by refusing everything.
//   - lane "ghost" (72h cooldown, an orphan row with NO in-flight mark):
//     must never be admitted — (c)'s negative arm, live in the same run.
//   - lane "tasks": parks on a perpetual-only board, flips exactly once when
//     the first non-perpetual row lands, and does not flip again on a
//     perpetual-only write.
func TestSCHEDGAP1666_SevenDaySimulation(t *testing.T) {
	db := newTestDB(t)
	const (
		cool        = "gap1666-sim-cool"
		interrupted = "gap1666-sim-interrupted"
		ghost       = "gap1666-sim-ghost"
		tasks       = "gap1666-sim-tasks"
	)
	newRow := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"status":"pending","title":"work %s"}`, id, id)
	}
	perpetual := `{"id":"SIM-PERP","status":"pending","title":"never done","perpetual":true}`

	coolWD, coolBoard := gap1660BoardDir(t, newRow("C0"))
	intWD, intBoard := gap1660BoardDir(t, newRow("I0"))
	ghostWD, ghostBoard := gap1660BoardDir(t, newRow("G0"))
	taskWD, taskBoard := gap1660BoardDir(t, perpetual)

	sim := clock.NewSimClockAt(1000, time.Now())
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.noDeliver = true
	l.SetClock(sim)
	gw := newResumeGateway(t)
	gw.wire(l)
	w := NewBoardWakeWatcher(db, l.ForceEvaluate)
	w.SetClock(sim)
	defer l.Stop()
	defer w.Stop()

	insertGap1660Project(t, db, cool, coolWD, 72*3600, database.AdmissionModeCooldown)
	insertGap1660Project(t, db, interrupted, intWD, 72*3600, database.AdmissionModeCooldown)
	insertGap1660Project(t, db, ghost, ghostWD, 72*3600, database.AdmissionModeCooldown)
	insertGap1660Project(t, db, tasks, taskWD, 72*3600, database.AdmissionModeTasks)
	for _, name := range []string{cool, interrupted, ghost, tasks} {
		gap1660SetLastCompleted(t, db, name, time.Hour)
	}

	restartHours := map[int]bool{54: true, 102: true, 150: true}
	restarts := 0
	taskFlipAdded := false

	for hour := 6; hour <= 168; hour += 6 {
		sim.Advance(6 * time.Hour)

		// Continuous board writes: every lane's board gains a row on every
		// step. A board write must never be able to admit a cooldown lane.
		gap1660WriteBoard(t, coolBoard, newRow("C0"), newRow(fmt.Sprintf("C%d", hour)))
		gap1660WriteBoard(t, intBoard, newRow("I0"), newRow(fmt.Sprintf("I%d", hour)))
		gap1660WriteBoard(t, ghostBoard, newRow("G0"), newRow(fmt.Sprintf("G%d", hour)))
		if !taskFlipAdded {
			// The tasks lane's board carries ONLY a perpetual row until the
			// flip step below, so the lane must park on it.
			gap1660WriteBoard(t, taskBoard, perpetual)
		} else if hour == 30 {
			// The flip row was worked: the board returns to the perpetual
			// fixture, so the lane parks again instead of ticking with work.
			gap1660WriteBoard(t, taskBoard, perpetual)
		}

		// The flip step (day 1): the first NON-perpetual row lands on the
		// tasks lane's board and a wake fires.
		if hour == 24 {
			gap1660WriteBoard(t, taskBoard, perpetual, newRow("SIM-REAL-1"))
			gap1660WakeNow(t, w, tasks, taskWD, taskBoard)
			taskFlipAdded = true
			schedGap1666WaitSettled(t, db, 5*time.Second)
		}
		// A PERPETUAL-ONLY write on the same lane afterwards must not flip
		// it again: the board holds no admissible work.
		if hour == 48 {
			gap1660WriteBoard(t, taskBoard, perpetual)
			gap1660WakeNow(t, w, tasks, taskWD, taskBoard)
			schedGap1666WaitSettled(t, db, 5*time.Second)
		}

		// The restarts: the previous instance dies mid-tick for the
		// "interrupted" lane (its row is left running, the boot reap stamps
		// it as an in-flight orphan), while the "ghost" lane carries an
		// orphan mark with NO in-flight reason.
		if restartHours[hour] {
			restarts++
			orphanTickRow(t, db, fmt.Sprintf("1666-restart-%d-inflight", hour), interrupted,
				"timeout", OrphanReasonStartupReap, 0)
			orphanTickRow(t, db, fmt.Sprintf("1666-restart-%d-ghost", hour), ghost,
				"timeout", "", 0)
			clearParkedEmpty() // a fresh process starts with an empty park registry
			l.resumeOrphansAtStartup()
			schedGap1666WaitSettled(t, db, 10*time.Second)
		}

		l.evaluate()
		schedGap1666WaitSettled(t, db, 10*time.Second)
	}

	if restarts != 3 {
		t.Fatalf("simulation ran %d restart(s), want 3", restarts)
	}

	// ── The measured lane: a 72h cooldown lane over 168h ──────────────────
	median, gaps := schedGap1666MedianGap(t, db, cool)
	if gaps == 0 {
		t.Fatalf("the 72h cooldown lane never ticked twice over 7 simulated days (ticks=%d) — the cadence assertion would be vacuous",
			schedGap1666TickCount(t, db, cool))
	}
	if median < 48*time.Hour {
		t.Fatalf("72h cooldown lane median inter-tick gap = %v over 7 days with 3 restarts, want >= 48h (gaps=%d) — a restart or a board write admitted the lane ahead of its own cadence",
			median, gaps)
	}
	t.Logf("cool lane: %d tick(s), %d gap(s), median gap %v", schedGap1666TickCount(t, db, cool), gaps, median)

	// Room-to-tick floor: the gate must not have refused the lane's OWN
	// cadence — a lane at a 72h pin over 168h admits at least twice.
	if n := schedGap1666TickCount(t, db, cool); n < 2 {
		t.Fatalf("72h cooldown lane admitted only %d tick(s) over 7 days — the gate is over-strict (it must let the lane's own cadence through)", n)
	}

	// ── The continuity arm (non-vacuity): the interrupted lane WAS resumed ─
	if n := schedGap1666NudgeRows(t, db, interrupted); n < 3 {
		t.Fatalf("the interrupted lane was resumed %d time(s) across 3 restarts, want 3 — the continuity bypass (c) is not being exercised, so the cooldown assertion above proves nothing", n)
	}
	// ── The negative arm: no in-flight mark, no resume admission ─────────
	// The ghost lane is a 72h cooldown lane like the others, so it DOES tick
	// on its own cadence during the week — what the restarts must never do is
	// hand it an extra one. The mark is the continuation row the resume entry
	// would create.
	if n := schedGap1666NudgeRows(t, db, ghost); n != 0 {
		t.Fatalf("the ghost lane (orphan mark, no in-flight reason) was resumed %d time(s) — a restart must never reset a lane's cadence (c)", n)
	}

	// ── The tasks lane: park, one flip, no second flip ───────────────────
	// A tasks-mode lane with no work still ticks on its cooldown pin (the
	// SCHED-GAP-124 fallback), so the question is not "how many ticks" but
	// "how many of them were BOARD-WRITE admissions". Exactly one: the first
	// non-perpetual row flips the parked lane; the perpetual-only write
	// afterwards stamps nothing at all.
	flips := schedGap1660ReasonCount(t, db, tasks, AdmissionReasonFlipBoardEmpty)
	if flips != 1 {
		t.Fatalf("tasks lane flip:board_empty admissions = %d over the simulation, want exactly 1 (the first non-perpetual row flips; a perpetual-only write must not)", flips)
	}
	if n := schedGap1666NudgeSourceCount(t, db, tasks, NudgeSourceBoardWake); n != 1 {
		t.Fatalf("tasks lane admissions carrying nudge_source=board_wake = %d, want exactly 1 — a perpetual-only board write must not stamp an admission", n)
	}
}

// schedGap1666NudgeSourceCount counts a lane's tick rows carrying one
// nudge_source (which entry point the admission came through).
func schedGap1666NudgeSourceCount(t *testing.T, db *sql.DB, project, source string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND nudge_source = ?`,
		project, source).Scan(&n); err != nil {
		t.Fatalf("count nudge_source %s for %s: %v", source, project, err)
	}
	return n
}

// schedGap1666NudgeRows counts the continuation rows a lane's orphans
// produced (the resume entry names them <orphan-id>-nudgeN).
func schedGap1666NudgeRows(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND id LIKE '%-nudge%'`, project).Scan(&n); err != nil {
		t.Fatalf("count nudge rows for %s: %v", project, err)
	}
	return n
}

// schedGap1660ReasonCount counts a lane's tick rows carrying one admit_reason
// (the flip vocabulary is what the tasks-lane acceptance is measured on).
func schedGap1660ReasonCount(t *testing.T, db *sql.DB, project, reason string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND admit_reason = ?`,
		project, reason).Scan(&n); err != nil {
		t.Fatalf("count admit_reason %s for %s: %v", reason, project, err)
	}
	return n
}
