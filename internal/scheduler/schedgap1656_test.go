package scheduler

// SCHED-GAP-1656 — board-aware admission for BUILDER lanes.
//
// The defect this pins: a BUILDER lane in admission_mode=tasks whose board
// holds ZERO dispatchable rows still burns a slot, a cooldown and a full LLM
// session once its wall-clock pin elapses. SCHED-GAP-1655 closed the
// COOLDOWN-mode half of that waste (the no_work gate, nowork.go); the tasks
// half had no board gate at all — the SCHED-GAP-124 waiver only fires when
// work EXISTS, and a drained tasks lane simply fell through to its pin
// (packer.go / packer_select.go / multipool_packer.go all share the shape).
//
// The gate added here (tasksBuilderAdmissionBlocked, nowork.go) defers such a
// lane with the EXISTING tasks_no_work reason (brief point 2 — the frozen
// deferrals vocabulary is not extended): the lane is parked, its
// classification already names tasks_no_work, and the SCHED-GAP-1660
// parked-empty flip (plus the board-wake watcher, a tasks-mode privilege per
// SCHED-GAP-1695/1727) admits it the moment real work lands — so deferring
// costs no latency, only the wasted session.
//
// Reporter/parasite lanes keep their timer cadence BY DESIGN (SCHED-GAP-1655
// deliverable 3, laneClass): an empty board never defers them.
//
// Every subtest drives the REAL evaluate() (pinned clock, sim mode) or the
// real gate function — the same harness as admission_decision_test.go.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// admitNewLoopNS builds a loop on the NAMESPACE packer path
// (MultiPoolPacker.Pack → packer_select.go) with the same sim-mode and pinned
// clock seams admitNewLoop installs on the flat path.
func admitNewLoopNS(t *testing.T, db *sql.DB, now time.Time) *Loop {
	t.Helper()
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4, true)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(now))
	return l
}

// admitInsertCompletedTick seeds one terminal tick row.
//
// The namespace packer path resolves lastCompleted from the TICKS table
// (MultiPoolPacker.Pack takes evalContext's MAX(completed_at) snapshot), NOT
// from projects.last_tick_completed the flat packer reads — so a namespace-path
// fixture needs the tick row or the whole cooldown/admission block is skipped
// ("never completed: nothing to pace against") and the lane is packed
// unconditionally.
func admitInsertCompletedTick(t *testing.T, db *sql.DB, tickID, project string, completed time.Time) {
	t.Helper()
	ts := completed.UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, created_at)
		VALUES (?, ?, 'completed', ?, ?, ?)`, tickID, project, ts, ts, ts); err != nil {
		t.Fatalf("insert completed tick %s for %s: %v", tickID, project, err)
	}
}

// TestSCHEDGAP1656_TasksBuilderEmptyBoardDefers is AC 1 + AC 3: a tasks-mode
// BUILDER lane on an owned, drained board defers with reason tasks_no_work
// and does NOT spawn, while the same lane with one dispatchable row is
// admitted normally, and a cooldown-mode REPORTER lane on the identical empty
// board keeps ticking.
func TestSCHEDGAP1656_TasksBuilderEmptyBoardDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("tasks builder with an empty board defers and does not dispatch", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t) // owned board, zero dispatchable rows
		admitInsertProject(t, db, admitProjectSpec{
			// The pin has ELAPSED (+90s vs 60s): before this row the lane
			// fell straight through to the pack and burned the session.
			Name: "gap1656-tasks-idle", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-tasks-idle")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1656-tasks-idle = %d, want exactly 1: %v", len(lines), lines)
		}
		admitAssertLineShape(t, lines[0])
		if got := admitField(lines[0], "reason"); got != AdmissionReasonTasksNoWork {
			t.Fatalf("reason = %q, want %q (the existing tasks no-work reason; no new vocabulary): %s",
				got, AdmissionReasonTasksNoWork, lines[0])
		}
		// The deferral means NO session: the sim spawner records every spawn.
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none (deferred, not dispatched)", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonTasksNoWork] != 1 {
			t.Errorf("admission_counters[tasks_no_work] = %d, want 1 (%v)",
				counters[AdmissionReasonTasksNoWork], counters)
		}
		// The pass-over is persisted (SCHED-GAP-157 deferrals table), so the
		// waste is queryable per lane, not just logged.
		if n := countDeferralsFor(t, db, "gap1656-tasks-idle", AdmissionReasonTasksNoWork); n != 1 {
			t.Errorf("deferrals rows with reason tasks_no_work for gap1656-tasks-idle = %d, want 1", n)
		}
	})

	t.Run("tasks builder with a dispatchable row dispatches normally", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t, `{"id":"G1656-1","status":"pending","title":"real work"}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1656-tasks-busy", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-tasks-busy")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1656-tasks-busy = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (dispatchable row → the tasks waiver admits): %s",
				got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1656-tasks-busy" {
			t.Errorf("simulated selection = %v, want [gap1656-tasks-busy]", got)
		}
	})

	t.Run("cooldown reporter lane with an empty board still ticks", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t) // the same drained board the builder was deferred on
		// -sync is a REPORTER suffix (laneClass): its product is a periodic
		// report, so the timer IS the design (SCHED-GAP-1655 deliverable 3).
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1656-fleet-sync", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-fleet-sync")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1656-fleet-sync = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (a reporter lane keeps its timer cadence on an empty board): %s",
				got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1656-fleet-sync" {
			t.Errorf("simulated selection = %v, want [gap1656-fleet-sync]", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonTasksNoWork] != 0 {
			t.Errorf("admission_counters[tasks_no_work] = %d, want 0 (reporter exempt)",
				counters[AdmissionReasonTasksNoWork])
		}
	})

	t.Run("cooldown builder keeps the SCHED-GAP-1655 no_work reason (no cross-talk)", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1656-cooldown-builder", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd, // cooldown mode (default) + builder class + empty board
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-cooldown-builder")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1656-cooldown-builder = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonNoWork {
			t.Fatalf("reason = %q, want %q (SCHED-GAP-1655's cooldown-mode gate, untouched): %s",
				got, AdmissionReasonNoWork, lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none", got)
		}
	})

	t.Run("tasks builder whose board holds only a perpetual row defers", func(t *testing.T) {
		db := newTestDB(t)
		// A perpetual fixture is NOT dispatchable work (boardOpenRows excludes
		// it) — the parked-empty shape SCHED-GAP-1660 flips back to work.
		wd := admitWorkdirWithBoard(t, `{"id":"NEVER-DONE","status":"pending","perpetual":true}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1656-tasks-perpetual", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-tasks-perpetual")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonTasksNoWork {
			t.Fatalf("reason = %q, want %q (perpetual-only board holds no dispatchable row): %s",
				got, AdmissionReasonTasksNoWork, lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none", got)
		}
	})

	t.Run("tasks builder with a missing board is fail-open (pin decides)", func(t *testing.T) {
		db := newTestDB(t)
		// TempDir with no .coding-hermes/ at all: an evidence gap, not proof
		// of an empty board — the fleet-wide fail-open convention applies (a
		// missing board never deferred a tick before this row).
		wd := t.TempDir()
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1656-tasks-noboard", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1656-tasks-noboard")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (unreadable board is fail-open — the pin decides): %s",
				got, AdmissionReasonOK, lines[0])
		}
	})
}

// TestSCHEDGAP1656_NamespacePathGate pins the SAME gate on the namespace
// packer path (packer_select.go) — the second of the three selection paths
// whose tasks branch must consult the board before the pin.
func TestSCHEDGAP1656_NamespacePathGate(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "gap1656ns", 0, "cooldown")
	wd := admitWorkdirWithBoard(t)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1656-ns-idle", NS: "gap1656ns", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	admitInsertCompletedTick(t, db, "tick-ns-idle-1", "gap1656-ns-idle", now.Add(-90*time.Second))
	l := admitNewLoopNS(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1656-ns-idle")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for gap1656-ns-idle = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonTasksNoWork {
		t.Fatalf("reason = %q, want %q (namespace packer path): %s", got, AdmissionReasonTasksNoWork, lines[0])
	}
	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Errorf("spawned %v, want none (namespace path deferred the drained builder)", got)
	}

	// Non-vacuity: the same namespace path admits the lane the moment a
	// dispatchable row exists.
	wd2 := admitWorkdirWithBoard(t, `{"id":"G1656-NS-1","status":"pending","title":"work"}`)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1656-ns-busy", NS: "gap1656ns", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd2,
	})
	admitInsertCompletedTick(t, db, "tick-ns-busy-1", "gap1656-ns-busy", now.Add(-90*time.Second))
	l2 := admitNewLoopNS(t, db, now)
	admitCaptureLog(t)
	l2.evaluate()
	if got := simSelectedProjects(t, db); len(got) != 1 || got[0] != "gap1656-ns-busy" {
		t.Errorf("simulated selection = %v, want [gap1656-ns-busy] (work on the board is admitted)", got)
	}
}

// TestSCHEDGAP1656_GateTable pins the gate's conjunction at the function
// level: tasks mode × builder class × OWNED board × read-and-empty. Every
// other shape is transparent — including the ownership refusal (SCHED-GAP-141:
// a lane that only reads a foreign board is time-based and must keep its
// cooldown), and both fail-open shapes.
func TestSCHEDGAP1656_GateTable(t *testing.T) {
	emptyBoard := admitWorkdirWithBoard(t)
	withWork := admitWorkdirWithBoard(t, `{"id":"G1656-T-1","status":"pending"}`)

	cases := []struct {
		name      string
		project   string
		workdir   string
		mode      string
		ownership string
		want      bool
	}{
		{"tasks builder owned empty board → blocked", "proj", emptyBoard, "tasks", "", true},
		{"tasks builder owned board with work → proceeds", "proj", withWork, "tasks", "", false},
		{"tasks reporter empty board → proceeds (timer by design)", "proj-sync", emptyBoard, "tasks", "", false},
		{"tasks builder foreign board → proceeds (ownership refused, GAP-141)", "proj", emptyBoard, "tasks", "shared", false},
		{"tasks builder no board → proceeds (fail-open)", "proj", t.TempDir(), "tasks", "", false},
		{"tasks builder empty workdir → proceeds (no evidence)", "proj", "", "tasks", "", false},
		{"cooldown builder empty board → gate transparent (1655 owns this shape)", "proj", emptyBoard, "cooldown", "", false},
		{"cooldown reporter empty board → proceeds", "proj-sync", emptyBoard, "cooldown", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tasksBuilderAdmissionBlocked(tc.project, tc.workdir, tc.mode, tc.ownership, "")
			if got != tc.want {
				t.Errorf("tasksBuilderAdmissionBlocked(%q, wd=%q, mode=%q, own=%q) = %v, want %v",
					tc.project, tc.workdir, tc.mode, tc.ownership, got, tc.want)
			}
		})
	}
}
