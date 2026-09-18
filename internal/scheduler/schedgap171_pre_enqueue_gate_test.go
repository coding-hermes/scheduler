package scheduler

// SCHED-GAP-171 — the POOL half of the defer-not-drop contract: the load gate
// is consulted BEFORE a row is created, so a deferral cannot strand one.
//
// THE DEFECT (first-hand code + log evidence, 2026-09-18). SlotPool.spawn
// consulted LoadGateShouldDefer (SCHED-GAP-125) and RETURNED. For the
// evaluation path that was harmless; for every caller that enqueues the row
// BEFORE handing it to the pool it was not — Loop.SpawnNow did exactly that,
// and the row it had just inserted (the row whose id the API returns) stayed
// in status='queued'. Nothing in the daemon dispatches an EXISTING row: the
// packer re-picks a project and can strand another row, SpawnNow answers
// ErrProjectRunning ("project already has a tick in flight"), and the orphan
// scan counts the stranded row as in-flight (so the lane is neither running
// nor eligible). Live shape: 5 rows queued >1h while load was 14-20, plus the
// 7 zombies reconciled by hand the same morning (SCHED-GAP-145 class). The
// nudge half of the same defect was fixed in SCHED-GAP-146 (resumeOrphans now
// checks the gate before it spends a nudge); this file pins the POOL half.
//
// THE FIX. The deferral moved to the caller, BEFORE any row exists:
//
//	Loop.evaluate   (tick_process.go) — pre-enqueue check per packed project;
//	Loop.SpawnNow   (loop.go)         — after the enqueue that makes the
//	                                    returned id resolvable, the deferral
//	                                    is REPORTED (`load_gate_deferred`
//	                                    with the tick id) and the row stays
//	                                    queued — the API contract is intact;
//	resumeOrphans   (session_resume.go) — unchanged (SCHED-GAP-146).
//
// SlotPool.spawn no longer contains the branch (AC5): with all three entry
// points pre-checking, the pool has nothing left to gate.
//
// WHAT EACH TEST PINS, and why it is not vacuous:
//
//  1. TestSchedGap171_EvalSkipsEnqueueOnDeferredGate — a real evaluation pass
//     under load creates NO row and charges NOTHING, and the wait is
//     observable as an event (project + namespace + load_1m + threshold), not
//     only as a log line. The deferral log line is asserted FIRST: without it,
//     "nothing changed" would also be satisfied by a pass that never reached
//     the project.
//  2. TestSchedGap171_EvalProceedsOnOpenGate — the regression guard for the
//     open path: gate off ⇒ enqueue + spawn happen, no deferral event is
//     emitted, and the tick completes.
//  3. TestSchedGap171_SpawnNowDeferralEmitsEvent — the preserved API contract:
//     the returned id is a REAL stored row (status queued), no slot is taken,
//     the mock gateway is never reached, and the event carries the tick id.
//     This is the case that used to leave a zombie row with no explanation.
//  4. TestSchedGap171_RepeatedDeferralDoesNotAccumulate — two deferred passes
//     leave the queued-row count unchanged (0 → 0 → 0): the anti-accumulation
//     property of the 2026-09-18 defect.
//
// The gate-armed tests fail closed on the machinery: every one asserts its own
// PREMISE (LoadGateShouldDefer true, namespace cap 0 so no other gate can be
// the reason) before driving the path under test. The SpawnNow fixture wires a
// HELD gateway so a gate that fails to bite spawns the mock, never a real
// foreman process.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// schedGap171Fixture builds ONE enabled project in an UNLIMITED namespace on a
// pinned clock: the load gate is then the only gate that can act, so whatever
// the test observes is the gate under test.
func schedGap171Fixture(t *testing.T, name string) (db *sql.DB, l *Loop, ns string, now time.Time) {
	t.Helper()
	now = fixedEvalNow()
	db = newTestDB(t)
	ns = "schedgap171-ns"
	// cap 0 = unlimited (each test re-checks this premise).
	admitInsertNamespace(t, db, ns, 0, "cooldown")
	admitInsertProject(t, db, admitProjectSpec{
		Name: name, NS: ns, CooldownS: 60,
		Last: now.Add(-5 * time.Minute), Workdir: t.TempDir(),
	})
	l = NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetClock(clock.NewFixed(now)) // deterministic selection instant
	l.noDeliver = true
	return db, l, ns, now
}

// schedGap171AssertNoOtherGate fails the test when a gate other than the load
// gate could explain why nothing ran.
func schedGap171AssertNoOtherGate(t *testing.T, db *sql.DB, ns string) {
	t.Helper()
	if got := namespaceCapDB(db, ns); got != 0 {
		t.Fatalf("premise: namespace %s max_concurrent = %d, want 0 (unlimited) — the cap gate must not be the gate under test", ns, got)
	}
}

// schedGap171AssertGateTripped fails the test when the fixture does not
// actually trip the load gate (otherwise every "deferred" assertion below
// would be satisfied by a gate that never fired).
func schedGap171AssertGateTripped(t *testing.T, db *sql.DB, ns string) {
	t.Helper()
	if !LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: LoadGateShouldDefer(%q) = false with threshold %.0f and a load reading of 15.0 — the fixture does not trip the gate",
			ns, loadGateThreshold())
	}
}

// schedGap171Event is one decoded load-gate deferral event.
type schedGap171Event struct {
	Component string
	Severity  string
	Message   string
	Details   map[string]any
}

// schedGap171DeferralEvents returns every load-gate deferral event, oldest
// first. The selection is the machine-detectable marker
// (`reason=load_gate_deferred`) rather than a log line or a message substring,
// so a deferral is queryable on its own after the fact.
func schedGap171DeferralEvents(t *testing.T, db *sql.DB) []schedGap171Event {
	t.Helper()
	rows, err := db.Query(`SELECT component, severity, message, details FROM events
		WHERE json_extract(details, '$.reason') = 'load_gate_deferred' ORDER BY id`)
	if err != nil {
		t.Fatalf("query load-gate deferral events: %v", err)
	}
	defer rows.Close()
	var out []schedGap171Event
	for rows.Next() {
		var e schedGap171Event
		var raw string
		if err := rows.Scan(&e.Component, &e.Severity, &e.Message, &raw); err != nil {
			t.Fatalf("scan load-gate deferral event: %v", err)
		}
		if err := json.Unmarshal([]byte(raw), &e.Details); err != nil {
			t.Fatalf("decode deferral event details %q: %v", raw, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate load-gate deferral events: %v", err)
	}
	return out
}

// schedGap171DetailFloat reads a numeric detail and fails the test when it is
// absent or not a number (a silently missing field is the failure mode this
// helper exists to make loud).
func schedGap171DetailFloat(t *testing.T, e schedGap171Event, key string) float64 {
	t.Helper()
	f, ok := e.Details[key].(float64)
	if !ok {
		t.Fatalf("deferral event %q has no numeric %q (details=%v)", e.Message, key, e.Details)
	}
	return f
}

// schedGap171AssertDeferralEvent pins the payload contract of one deferral
// event: the message, the component, and every field a caller needs to
// attribute the deferral — project, namespace, both gate readings, and the
// tick id when (and only when) a row already existed.
func schedGap171AssertDeferralEvent(t *testing.T, e schedGap171Event, project, ns, tickID string, load, threshold float64) {
	t.Helper()
	if got := e.Message; got != "load gate deferred "+project {
		t.Errorf("deferral event message = %q, want %q", got, "load gate deferred "+project)
	}
	if got := e.Component; got != "load_gate" {
		t.Errorf("deferral event component = %q, want %q (the deferral is the gateway's decision, not the slot pool's)", got, "load_gate")
	}
	if got, _ := e.Details["project"].(string); got != project {
		t.Errorf("deferral event project = %q, want %q", got, project)
	}
	if got, _ := e.Details["namespace"].(string); got != ns {
		t.Errorf("deferral event namespace = %q, want %q (without it a deferral cannot be attributed to the namespace's load_gate opt-out)", got, ns)
	}
	if got := schedGap171DetailFloat(t, e, "load_1m"); got != load {
		t.Errorf("deferral event load_1m = %v, want %v", got, load)
	}
	if got := schedGap171DetailFloat(t, e, "threshold"); got != threshold {
		t.Errorf("deferral event threshold = %v, want %v", got, threshold)
	}
	if got, _ := e.Details["deferred"].(bool); !got {
		t.Errorf("deferral event deferred = %v, want true — the marker is what lets a caller tell a deferral from a spawn", e.Details["deferred"])
	}
	if tickID == "" {
		// The evaluation path defers BEFORE a row exists: reporting a tick id
		// there would name a row that was never created.
		if v, ok := e.Details["tick_id"]; ok {
			t.Errorf("deferral event carries tick_id=%v, want no tick_id — the evaluation path defers before any row exists", v)
		}
		return
	}
	if got, _ := e.Details["tick_id"].(string); got != tickID {
		t.Errorf("deferral event tick_id = %q, want %q (the id is how the API caller correlates the deferral with the row that stays queued)", got, tickID)
	}
}

// TestSchedGap171_EvalSkipsEnqueueOnDeferredGate (AC1): with the gate tripped,
// an evaluation pass neither enqueues a row nor charges the lane, and the
// deferral is observable as an event carrying both gate readings.
func TestSchedGap171_EvalSkipsEnqueueOnDeferredGate(t *testing.T) {
	const name = "sg171-eval-deferred"
	db, l, ns, _ := schedGap171Fixture(t, name)

	// A held mock gateway: a gate that failed to bite spawns the mock and the
	// assertions below report it, instead of the test launching a real foreman.
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	restore := gap146ArmLoadGate(15.0) // threshold 12, load 15.0
	defer restore()

	schedGap171AssertGateTripped(t, db, ns)
	schedGap171AssertNoOtherGate(t, db, ns)
	want := gap146ReadState(t, db, name)

	logbuf := admitCaptureLog(t)
	l.evaluate()

	// The deferral is REAL — the pass reached the packed project and the gate
	// refused it. Every "unchanged" assertion below depends on this line.
	if got := gap146LogCount(logbuf, "LOAD-GATE: deferring "+name); got != 1 {
		t.Fatalf("LOAD-GATE deferral lines for %s = %d, want 1 — the evaluation pass must consult the gate before spawning the packed project",
			name, got)
	}

	// AC1(c) — the deferral event, with project + namespace + load_1m +
	// threshold.
	evs := schedGap171DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("load-gate deferral events = %d, want 1 (a caller must be able to see the deferral after the fact)", len(evs))
	}
	schedGap171AssertDeferralEvent(t, evs[0], name, ns, "", 15.0, 12.0)

	// AC1(a) — NO row was created: the row-creating call (SlotPool.Spawn)
	// is never reached, and no queued row can be stranded.
	if got := gap146TickRows(t, db, name, ""); got != 0 {
		t.Errorf("tick rows for %s = %d, want 0 — a pre-enqueue deferral creates no row, not even a queued one", name, got)
	}
	if got := gap146TickRows(t, db, name, "queued"); got != 0 {
		t.Errorf("queued rows for %s = %d, want 0 — a queued row nothing dispatches IS the 2026-09-18 defect", name, got)
	}
	if got := gap146TickCountAll(t, db); got != 0 {
		t.Errorf("tick rows in the whole DB = %d, want 0 — the deferral must not touch the ticks table at all", got)
	}

	// AC1(b) — nothing charged. nudge_count is a ticks column, so "unchanged"
	// is the conjunction of "no tick row exists" (above) and "no nudge row was
	// created or charged" here; the lane's scheduling state is the third half.
	if got := countNudgeRows(t, db, name); got != 0 {
		t.Errorf("nudge rows for %s = %d, want 0 — a load deferral spends no retry budget", name, got)
	}
	if got := l.namespaceInflight(context.Background(), ns); got != 0 {
		t.Errorf("namespace in-flight = %d, want 0 — a stranded queued row inflates this count and defers the lane's siblings", got)
	}
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0 — a deferred project takes no global slot", got)
	}
	if l.slotPool.RunningSet()[name] {
		t.Errorf("%s is in RunningSet after a deferral — the gate returns before any reservation, so no phantom occupancy may survive it", name)
	}
	if got := gw.spawns.Load(); got != 0 {
		t.Errorf("the held gateway saw %d spawn(s), want 0 — a deferred project must not reach the spawner", got)
	}
	gap146AssertStateUnchanged(t, db, name, want, "SG171 evaluation deferral")
}

// TestSchedGap171_EvalProceedsOnOpenGate (AC3): with the gate open the
// evaluation path enqueues and spawns exactly as before, and emits no
// deferral event — the regression guard for the path the gate must not touch.
func TestSchedGap171_EvalProceedsOnOpenGate(t *testing.T) {
	const name = "sg171-eval-open"
	db, l, _, _ := schedGap171Fixture(t, name)

	// Threshold 0 = the default (gate fully disabled).
	gap146ArmLoadGateOff(t)

	gw := newHeldResumeGateway(t)
	gw.wire(l) // a gate that failed to bite must spawn the MOCK, never a real foreman

	l.evaluate()

	waitUntil(t, 20*time.Second, "the packed project to be enqueued and started", func() bool {
		return gap146TickRows(t, db, name, "running") == 1
	})
	// Barrier: the spawn is parked on the held gateway, so "it started" is an
	// observation rather than a race against an instant mock.
	waitForHeldSpawn(t, gw, 5*time.Second)

	if got := gap146TickRows(t, db, name, ""); got != 1 {
		t.Errorf("tick rows for %s = %d, want 1 — an open gate must enqueue the selected project", name, got)
	}
	if got := gap146TickRows(t, db, name, "queued"); got != 0 {
		t.Errorf("queued rows for %s = %d, want 0 — an open gate dispatches the row; it does not park it", name, got)
	}
	if evs := schedGap171DeferralEvents(t, db); len(evs) != 0 {
		t.Errorf("load-gate deferral events with the gate OPEN = %d, want 0 (first: %+v)", len(evs), evs[0])
	}

	gw.releaseAll()
	waitForSettled(t, db, 20*time.Second)

	if got := gap146TickRows(t, db, name, "completed"); got != 1 {
		t.Errorf("completed rows for %s = %d, want 1 — the open path must run the tick to completion", name, got)
	}
	if got := gap146TickRows(t, db, name, "failed"); got != 0 {
		t.Errorf("failed rows for %s = %d, want 0", name, got)
	}
}

// TestSchedGap171_SpawnNowDeferralEmitsEvent (AC2): the API path keeps its
// contract — the returned id is a real stored row sitting `queued` — and the
// deferral is reported with that id instead of being silent.
func TestSchedGap171_SpawnNowDeferralEmitsEvent(t *testing.T) {
	const name = "sg171-spawnnow-deferred"
	db, l, ns, _ := schedGap171Fixture(t, name)

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	gw := newHeldResumeGateway(t)
	gw.wire(l) // premise guard: a gate bypass spawns this mock, not a foreman

	schedGap171AssertGateTripped(t, db, ns)
	schedGap171AssertNoOtherGate(t, db, ns)
	want := gap146ReadState(t, db, name)

	p, err := database.GetProject(context.Background(), db, name)
	if err != nil {
		t.Fatalf("GetProject %s: %v", name, err)
	}

	tickID, err := l.SpawnNow(*p)
	if err != nil {
		t.Fatalf("SpawnNow under a down gate returned an error (%v) — the API contract requires a stored row even when the spawn is deferred", err)
	}
	if tickID == "" {
		t.Fatal("SpawnNow returned an empty tick id — the API contract requires a real stored row")
	}

	// The returned id resolves, and the row is queued: not started, not failed.
	if got := gap146TickStatus(t, db, tickID); got != "queued" {
		t.Errorf("tick %s status = %q, want %q — the deferred API row stays queued (defer, not drop)", tickID, got, "queued")
	}
	if got := gap146TickRows(t, db, name, "running"); got != 0 {
		t.Errorf("running rows for %s = %d, want 0 — a deferred spawn must not start", name, got)
	}
	if got := gap146TickRows(t, db, name, "failed"); got != 0 {
		t.Errorf("failed rows for %s = %d, want 0 — a busy host must not charge the lane a failure", name, got)
	}
	if got := gap146TickNudgeCount(t, db, tickID); got != 0 {
		t.Errorf("nudge_count on %s = %d, want 0 — a deferral spends no retry budget", tickID, got)
	}

	// The deferral is visible post-hoc, WITH the tick id.
	evs := schedGap171DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("load-gate deferral events = %d, want 1 (a deferred API spawn must be distinguishable from a spawn that never happened)", len(evs))
	}
	schedGap171AssertDeferralEvent(t, evs[0], name, ns, tickID, 15.0, 12.0)

	// Nothing was handed to the pool and nothing reached the gateway.
	if got := gw.spawns.Load(); got != 0 {
		t.Errorf("the held gateway saw %d spawn(s), want 0 — a deferred SpawnNow never reaches the pool", got)
	}
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0", got)
	}
	if l.slotPool.RunningSet()[name] {
		t.Errorf("%s is in RunningSet after a deferred SpawnNow — no reservation may be taken for a spawn that was refused", name)
	}
	gap146AssertStateUnchanged(t, db, name, want, "SG171 SpawnNow deferral")
}

// TestSchedGap171_RepeatedDeferralDoesNotAccumulate (AC4): deferred passes are
// idempotent with respect to the queue — the count stays unchanged, because a
// pre-enqueue deferral never leaves a row to accumulate. (Re-admission once
// the gate clears is owned by TestGAP146_DeferDoesNotDoubleCountAcrossEvaluations.)
func TestSchedGap171_RepeatedDeferralDoesNotAccumulate(t *testing.T) {
	const name = "sg171-repeat"
	db, l, ns, _ := schedGap171Fixture(t, name)

	gw := newHeldResumeGateway(t)
	gw.wire(l)

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	schedGap171AssertGateTripped(t, db, ns)
	schedGap171AssertNoOtherGate(t, db, ns)
	want := gap146ReadState(t, db, name)

	logbuf := admitCaptureLog(t)

	queued0 := countTicksInStatus(t, db, "queued")
	l.evaluate()
	queued1 := countTicksInStatus(t, db, "queued")
	l.evaluate()
	queued2 := countTicksInStatus(t, db, "queued")

	if queued1 != queued0 || queued2 != queued1 {
		t.Errorf("queued rows across two deferred passes = %d -> %d -> %d, want the count UNCHANGED — a deferral that leaves a row behind is the 2026-09-18 accumulation defect",
			queued0, queued1, queued2)
	}
	if queued0 != 0 || queued1 != 0 || queued2 != 0 {
		t.Errorf("queued rows across two deferred passes = %d -> %d -> %d, want 0 throughout — the evaluation path defers BEFORE the enqueue, so no row exists to accumulate",
			queued0, queued1, queued2)
	}

	// Both passes deferred the SAME project (not merely "some pass deferred"):
	// a deferral that consumed the cooldown or the attempt clock would make the
	// packer skip the project and the fleet would see a silent stall.
	if got := gap146LogCount(logbuf, "LOAD-GATE: deferring "+name); got != 2 {
		t.Fatalf("LOAD-GATE deferral lines across 2 passes = %d, want 2 — the project must be re-picked on every pass", got)
	}
	if got := gap146TickCountAll(t, db); got != 0 {
		t.Errorf("tick rows after 2 deferred passes = %d, want 0", got)
	}
	if evs := schedGap171DeferralEvents(t, db); len(evs) != 2 {
		t.Errorf("load-gate deferral events after 2 passes = %d, want 2", len(evs))
	}
	if got := gw.spawns.Load(); got != 0 {
		t.Errorf("the held gateway saw %d spawn(s), want 0 — no deferred pass may reach the spawner", got)
	}
	gap146AssertStateUnchanged(t, db, name, want, "SG171 repeated deferral")
}
