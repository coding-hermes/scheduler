package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-160 — never-run lane (NULL last_tick_completed) inside a capped
// shared namespace: ordering + starvation proof.
//
// The row was filed on the premise that nine PM lanes had never run (NULL
// last_tick_completed) and could therefore be starved by the shared `pm`
// namespace / concurrency-cap rotation. This fixture reproduces that state
// deterministically and pins the verified rules below.
//
// PREMISE WIRING (why a never-run lane is "absent", not "NULL-checked"): the
// live namespace selection path never reads the projects.last_tick_completed
// column — MultiPoolPacker.Pack consumes the ticks-derived map built by
// Loop.evalContext (tick_process.go):
//
//	SELECT project_name, MAX(completed_at) FROM ticks WHERE status != 'running'
//	GROUP BY project_name
//
// A never-run lane is simply ABSENT from that map, so both effects of a NULL
// last_tick_completed show up here as "no map entry": urgency falls back to
// created_at (urgency.go:63-73) and the cooldown gate is skipped (both gates
// are written `if lt, ok := lastCompleted[name]; ok`). The fixture builds the
// state end-to-end — a project row with NULL last_tick_completed, zero tick
// rows for the never-run lane, and one REAL completed tick row for the
// competitor — then derives the map from the ticks table exactly as the live
// loop does.
//
// VERIFIED RULES (SCHED-GAP-160 closure: no production change was warranted —
// the fixture reproduces neither starvation nor an incorrect ordering):
//
//  1. No phantom cooldown. A never-run lane has no last completion, so the
//     cooldown gate cannot apply to it: it is admissible on the first
//     evaluation with namespace cap/budget headroom, even while a
//     higher-urgency competitor in the same capped namespace is still inside
//     its cooldown. The flat path agrees (packer.go's gate is `lastTickAt !=
//     nil && ...`), and isOverdue/isStarving both use created_at as the
//     "last attempt" fallback for a project that has never run.
//
//  2. Deterministic ordering on an exact tie. Equal urgency and priority is
//     decided by the nil-last-tick tie-break in packer_select.go ("Older
//     last-tick = higher priority" / `if !iOk && jOk { return true }`): the
//     never-run lane sorts first. It is NOT a blanket preference — with a
//     strictly older last completion the run lane wins on urgency.
//
//  3. No silent starvation. The never-run lane can legitimately LOSE a cycle
//     to an eligible competitor with higher urgency, but its wait is bounded
//     by the S-GAP-001 fairness window: once elapsed-since-created exceeds
//     StarvationWindow(cooldownS) the starvation tier (1e12) forces it ahead
//     of any organically-scored project, so it is admitted no later than the
//     first cycle past that window. Two buckets are pinned below: the live
//     `pm` lane shape (priority 3, cooldown_s=86400 → window 3 x cooldown =
//     72h; 14 of the 15 enabled pm lanes carry it) and the sub-hour bucket
//     (cooldown <= 1h → window floors at 1h).
const (
	sg160NS = "pm"
	// Lane names are deliberately ordered AGAINST the nil-last-tick rule: the
	// never-run lane sorts AFTER the competitor lexically, so the sort's final
	// name fallback cannot produce "never-run lane selected first" — only the
	// nil-last-tick tie-break can (see the tie case below).
	sg160NeverLane = "pm-z-never-run"
	sg160RunLane   = "pm-a-recent-run"
)

// sg160Fixture is the deterministic SCHED-GAP-160 fixture.
type sg160Fixture struct {
	db           *sql.DB
	projects     []database.Project
	namespaces   []database.Namespace
	mp           *scheduler.MultiPoolPacker
	calc         *scheduler.UrgencyCalculator
	createdAt    time.Time // never-run lane created_at (its only age reference)
	tickID       string    // the run lane's single completed tick row
	runCooldownS int       // the run lane's cooldown (hostile-sweep cadence)
}

// sg160NewFixture creates: one capped namespace (max_concurrent=1) holding a
// never-run lane (NULL last_tick_completed + zero tick rows) and a run lane
// (one real completed tick row). Priority and cooldown are parameters because
// each case tunes which gate is supposed to bind.
func sg160NewFixture(t *testing.T, now time.Time, neverPriority, runPriority, neverCooldown, runCooldown int) *sg160Fixture {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)

	mustCreateNamespace(t, db, &database.Namespace{
		ID: sg160NS, Weight: 10, Reserved: 1, HardCap: 100,
		MaxConcurrent: 1, // the shared cap: ONE tick in this namespace per cycle
		Enabled:       true,
	})

	created := now.Add(-10 * time.Minute)
	nsID := sg160NS

	never := makeProject(sg160NeverLane, 1, neverPriority, neverCooldown, 1.0)
	never.Workdir = "/tmp/schedgap160-never-run" // no board → no pending-task boost
	never.CreatedAt = created.Format(time.RFC3339)
	never.NamespaceID = &nsID
	// last_tick_completed is deliberately left unset → NULL in the row.
	if err := database.CreateProject(ctx, db, never); err != nil {
		t.Fatalf("CreateProject %s: %v", sg160NeverLane, err)
	}

	run := makeProject(sg160RunLane, 1, runPriority, runCooldown, 1.0)
	run.Workdir = "/tmp/schedgap160-recent-run"
	run.CreatedAt = now.Add(-48 * time.Hour).Format(time.RFC3339)
	run.NamespaceID = &nsID
	if err := database.CreateProject(ctx, db, run); err != nil {
		t.Fatalf("CreateProject %s: %v", sg160RunLane, err)
	}

	tickID := database.NextTickID(context.Background(), sg160RunLane)
	if err := database.CreateTick(ctx, db, &database.Tick{ID: tickID, ProjectName: sg160RunLane}); err != nil {
		t.Fatalf("CreateTick %s: %v", sg160RunLane, err)
	}
	if err := database.CompleteTick(ctx, db, tickID, database.OutcomeCommitted, 0, ""); err != nil {
		t.Fatalf("CompleteTick %s: %v", sg160RunLane, err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	// Hermetic: no board files under the fixture workdirs, but pin the counter
	// so a stray /tmp board can never inject a pending-task boost.
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(0))

	f := &sg160Fixture{
		db: db, projects: projects, namespaces: namespaces,
		mp: mp, calc: defaultUrgencyCalc(), createdAt: created, tickID: tickID,
		runCooldownS: runCooldown,
	}
	sg160AssertPremises(t, f)
	return f
}

// sg160AssertPremises fails loudly if the fixture does not actually hold the
// state the case names — a fixture that silently set last_tick_completed (or
// created a tick row for the never-run lane) would make every assertion below
// vacuous.
func sg160AssertPremises(t *testing.T, f *sg160Fixture) {
	t.Helper()

	var neverLTC sql.NullString
	if err := f.db.QueryRow(
		`SELECT last_tick_completed FROM projects WHERE name = ?`, sg160NeverLane,
	).Scan(&neverLTC); err != nil {
		t.Fatalf("read %s.last_tick_completed: %v", sg160NeverLane, err)
	}
	if neverLTC.Valid {
		t.Fatalf("premise broken: %s.last_tick_completed = %q, want NULL", sg160NeverLane, neverLTC.String)
	}

	var neverTicks, runTicks int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, sg160NeverLane,
	).Scan(&neverTicks); err != nil {
		t.Fatalf("count ticks for %s: %v", sg160NeverLane, err)
	}
	if neverTicks != 0 {
		t.Fatalf("premise broken: %s has %d tick row(s), want 0 (never run)", sg160NeverLane, neverTicks)
	}
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, sg160RunLane,
	).Scan(&runTicks); err != nil {
		t.Fatalf("count ticks for %s: %v", sg160RunLane, err)
	}
	if runTicks != 1 {
		t.Fatalf("premise broken: %s has %d tick row(s), want 1", sg160RunLane, runTicks)
	}
}

// sg160SetRunLaneCompletedAt moves the run lane's single completed tick to the
// given instant (CompleteTick stamps "now", so the deterministic timestamp is
// written directly). The never-run lane is never given a tick row.
func sg160SetRunLaneCompletedAt(t *testing.T, f *sg160Fixture, ts time.Time) {
	t.Helper()
	if _, err := f.db.Exec(
		`UPDATE ticks SET completed_at = ? WHERE id = ?`, ts.UTC().Format(time.RFC3339), f.tickID,
	); err != nil {
		t.Fatalf("set %s completed_at: %v", sg160RunLane, err)
	}
}

// sg160LastCompletedFromTicks mirrors Loop.evalContext's completion query —
// the ONLY recency source the namespace selection path consumes.
func sg160LastCompletedFromTicks(t *testing.T, f *sg160Fixture) map[string]time.Time {
	t.Helper()
	rows, err := f.db.Query(
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status != 'running' GROUP BY project_name`)
	if err != nil {
		t.Fatalf("query last completed: %v", err)
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var name, ts string
		if err := rows.Scan(&name, &ts); err != nil {
			t.Fatalf("scan last completed: %v", err)
		}
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatalf("parse completed_at %q: %v", ts, err)
		}
		out[name] = parsed
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate last completed: %v", err)
	}
	return out
}

// sg160Pack runs one selection cycle and enforces the non-vacuity invariant
// every case depends on: the shared namespace holds exactly one project, so a
// "no project was selected" result fails the case instead of silently passing
// an empty assertion.
func sg160Pack(t *testing.T, f *sg160Fixture, now time.Time) string {
	t.Helper()
	last := sg160LastCompletedFromTicks(t, f)
	if _, present := last[sg160NeverLane]; present {
		t.Fatalf("premise broken: %s present in the ticks-derived lastCompleted map", sg160NeverLane)
	}
	res := f.mp.Pack(f.projects, f.namespaces, f.calc, last, nil, now)
	if len(res.NamespaceTicks) != 1 {
		t.Fatalf("namespace tick rows = %d, want 1", len(res.NamespaceTicks))
	}
	if res.NamespaceTicks[0].JobCount != 1 {
		t.Fatalf("namespace %s jobs = %d, want exactly 1 (cap=1) — fixture was vacuous",
			sg160NS, res.NamespaceTicks[0].JobCount)
	}
	if len(res.Projects) != 1 {
		t.Fatalf("selected %d project(s), want exactly 1", len(res.Projects))
	}
	return res.Projects[0].Name
}

// TestPacker_NeverRunLaneNilLastTickAdmissionAndOrdering pins the two static
// rules for a NULL last_tick_completed lane under a shared namespace cap
// (rules 1 and 2 in the file header).
func TestPacker_NeverRunLaneNilLastTickAdmissionAndOrdering(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	t.Run("admitted_while_higher_urgency_competitor_is_in_cooldown", func(t *testing.T) {
		// Never lane: priority 5, cooldown 900s, created 10m ago (age 600s —
		// still INSIDE its own cooldown, which must not gate a lane that has
		// never completed). Competitor: priority 9, completed 60s ago, so it
		// is 900s-cooldown-blocked.
		f := sg160NewFixture(t, now, 5, 9, 900, 900)
		sg160SetRunLaneCompletedAt(t, f, now.Add(-60*time.Second))

		neverUrg := f.calc.ComputeUrgency(5, 1.0, now, nil, f.createdAt)
		runUrg := f.calc.ComputeUrgency(9, 1.0, now, ptrTime(now.Add(-60*time.Second)), now.Add(-48*time.Hour))
		if runUrg <= neverUrg {
			t.Fatalf("premise broken: competitor urgency %v must exceed never-run urgency %v for this case to prove the gate (not the score) decided", runUrg, neverUrg)
		}

		got := sg160Pack(t, f, now)
		if got != sg160NeverLane {
			t.Errorf("selected %q, want %q — a never-run lane must not be held by a cooldown it has no completion to satisfy (competitor was cooldown-blocked at urgency %v > %v)",
				got, sg160NeverLane, runUrg, neverUrg)
		}
	})

	t.Run("exact_urgency_tie_prefers_never_run_then_run_lane_wins_when_older", func(t *testing.T) {
		// Identical priority/cooldown/decay and identical elapsed (10m each)
		// produce an EXACT urgency tie; cooldown 300s < 600s elapsed keeps
		// both lanes eligible, so only the tie-break can decide.
		f := sg160NewFixture(t, now, 5, 5, 300, 300)
		sg160SetRunLaneCompletedAt(t, f, now.Add(-10*time.Minute))

		neverUrg := f.calc.ComputeUrgency(5, 1.0, now, nil, f.createdAt)
		runUrg := f.calc.ComputeUrgency(5, 1.0, now, ptrTime(now.Add(-10*time.Minute)), now.Add(-48*time.Hour))
		if neverUrg != runUrg {
			t.Fatalf("premise broken: urgency tie expected, got never=%v run=%v", neverUrg, runUrg)
		}

		if got := sg160Pack(t, f, now); got != sg160NeverLane {
			t.Errorf("tie broken by %q, want %q (nil last-tick sorts first)", got, sg160NeverLane)
		}

		// Negative control: same lane, strictly older last completion → the
		// run lane wins on urgency, proving the tie-break above is not a
		// blanket preference for never-run lanes.
		sg160SetRunLaneCompletedAt(t, f, now.Add(-20*time.Minute))
		olderUrg := f.calc.ComputeUrgency(5, 1.0, now, ptrTime(now.Add(-20*time.Minute)), now.Add(-48*time.Hour))
		if olderUrg <= neverUrg {
			t.Fatalf("premise broken: run lane at 20m urgency %v must exceed never-run %v", olderUrg, neverUrg)
		}
		if got := sg160Pack(t, f, now); got != sg160RunLane {
			t.Errorf("selected %q, want %q (older last completion outranks the tie-break)", got, sg160RunLane)
		}
	})
}

// TestPacker_NeverRunLaneBoundedWaitNoSilentStarvation walks the hostile
// steady state for a never-run lane: a competitor that re-admits on its own
// cooldown cadence and therefore is eligible with strictly higher organic
// urgency in EVERY cycle. The never-run lane loses those cycles but must be
// admitted no later than the first cycle past its fairness window — for the
// live `pm` lanes (15 enabled, all priority 3, 14 of them cooldown_s=86400)
// that window is StarvationWindow(86400) = 3 x cooldown = 72h; the shorter
// cooldown bucket (<= 1h -> window 1h) is covered by a second case.
func TestPacker_NeverRunLaneBoundedWaitNoSilentStarvation(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name                       string
		neverPriority, runPriority int
		cooldown                   int
	}{
		// The live pm shape: priority 3, 24h cooldown, cap-1 namespace.
		{"live_pm_lane_shape_24h_cooldown", 3, 9, 86400},
		// The sub-hour bucket: cooldown <= 1h → window floors at 1h.
		{"sub_hour_cooldown", 5, 9, 900},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sg160NewFixture(t, now, tc.neverPriority, tc.runPriority, tc.cooldown, tc.cooldown)
			window := scheduler.StarvationWindow(tc.cooldown)
			// Probe step = window/60, so the bound is asserted to within 1/60
			// of the fairness window without 4000+ pack calls.
			step := window / 60
			if step < time.Minute {
				step = time.Minute
			}

			firstAdmission, losses := sg160SweepHostile(t, f, window, step, 120)
			if firstAdmission == 0 {
				t.Fatalf("never-run lane was never admitted within %d probe cycles of %v (cooldown %ds, window %v) — silent starvation reproduced",
					120, step, tc.cooldown, window)
			}
			t.Logf("never-run lane (cooldown %ds, window %v): lost %d cycle(s) to an eligible higher-urgency competitor, first admitted at age %v",
				tc.cooldown, window, losses, firstAdmission)
			if losses == 0 {
				t.Fatal("fixture was not hostile: the competitor never won a cycle, so the starvation bound was untested")
			}
			if firstAdmission <= window {
				t.Errorf("never-run lane admitted at age %v, want strictly after the %v fairness window", firstAdmission, window)
			}
			if firstAdmission > window+step {
				t.Errorf("never-run lane admitted at age %v, want no later than %v (window + one probe step) — wait is not bounded by the fairness window",
					firstAdmission, window+step)
			}
		})
	}
}

// sg160SweepHostile runs the hostile steady-state sweep and returns the age at
// which the never-run lane was first admitted plus the number of cycles an
// eligible higher-urgency competitor won before that.
//
// Hostile model: at every probe the competitor completed `its cooldown + 100s`
// earlier — i.e. it reliably re-admitted at its own cooldown cadence, so it is
// eligible (feedback: past its cooldown) and carries strictly higher organic
// urgency in each cycle below the window.
func sg160SweepHostile(t *testing.T, f *sg160Fixture, window, step time.Duration, maxSteps int) (firstAdmission time.Duration, losses int) {
	t.Helper()
	lag := time.Duration(f.runCooldownS)*time.Second + 100*time.Second

	for i := 1; i <= maxSteps; i++ {
		cycle := f.createdAt.Add(time.Duration(i) * step)
		age := cycle.Sub(f.createdAt)
		sg160SetRunLaneCompletedAt(t, f, cycle.Add(-lag))

		last := sg160LastCompletedFromTicks(t, f)
		if _, present := last[sg160NeverLane]; present {
			t.Fatalf("step %d: %s present in the ticks-derived map", i, sg160NeverLane)
		}
		res := f.mp.Pack(f.projects, f.namespaces, f.calc, last, nil, cycle)
		if len(res.NamespaceTicks) != 1 || res.NamespaceTicks[0].JobCount != 1 || len(res.Projects) != 1 {
			t.Fatalf("step %d (age %v): expected exactly 1 selected project under the shared cap=1, got jobs=%v projects=%d — assertion would be vacuous",
				i, age, res.NamespaceTicks, len(res.Projects))
		}
		proj := res.Projects[0]

		if firstAdmission == 0 {
			if proj.Name == sg160NeverLane {
				// First admission: it must be the starvation tier that did
				// it, and it must not happen before the window elapsed.
				firstAdmission = age
				if proj.Urgency < 1e12 {
					t.Errorf("step %d (age %v): never-run lane admitted with urgency %v, want >= 1e12 (starvation tier)",
						i, age, proj.Urgency)
				}
				continue
			}
			if age > window {
				t.Errorf("step %d (age %v): %q still holds the slot past the %v fairness window — the never-run lane is starving",
					i, age, proj.Name, window)
			}
			losses++
			continue
		}
		if proj.Name != sg160NeverLane {
			t.Errorf("step %d (age %v): %q re-took the slot after the never-run lane was admitted at age %v",
				i, age, proj.Name, firstAdmission)
		}
	}
	return firstAdmission, losses
}

func ptrTime(t time.Time) *time.Time { return &t }
