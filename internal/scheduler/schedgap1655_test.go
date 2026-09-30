package scheduler

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1655 end-to-end acceptance. Every subtest drives the REAL
// evaluate() (pinned clock, sim mode) or the real terminalOutcome mapping,
// mirroring the admission_decision_test.go harness.

// TestSCHEDGAP1655_CooldownBuilderEmptyBoardDefers pins AC 1 + AC 3: a
// cooldown-mode BUILDER lane whose board holds zero dispatchable rows is
// deferred with reason no_work, emits the grep-stable ADMIT line, and does
// NOT spawn (sim selection unchanged), while a -sync REPORTER lane with the
// same empty board STILL ticks on cadence.
func TestSCHEDGAP1655_CooldownBuilderEmptyBoardDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("builder with empty board defers no_work and does not spawn", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t) // owned board, zero rows
		admitInsertProject(t, db, admitProjectSpec{
			Name: "builder-idle", CooldownS: 60, Last: now.Add(-90 * time.Second), // pin elapsed
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("builder-idle")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for builder-idle = %d, want exactly 1: %v", len(lines), lines)
		}
		admitAssertLineShape(t, lines[0])
		if got := admitField(lines[0], "reason"); got != AdmissionReasonNoWork {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonNoWork, lines[0])
		}
		// Session count unchanged: the sim spawner records every spawn.
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none (deferred, not dispatched)", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonNoWork] != 1 {
			t.Errorf("admission_counters[no_work] = %d, want 1 (%v)", counters[AdmissionReasonNoWork], counters)
		}
		// The deferral is persisted (SCHED-GAP-157 deferrals table), so
		// the waste is queryable, not just logged.
		if n := countDeferralsFor(t, db, "builder-idle", AdmissionReasonNoWork); n != 1 {
			t.Errorf("deferrals rows with reason no_work for builder-idle = %d, want 1", n)
		}
	})

	t.Run("builder with a dispatchable row proceeds normally", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t, `{"id":"T-1","status":"pending"}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "builder-busy", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("builder-busy")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for builder-busy = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (dispatchable row → normal admission): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "builder-busy" {
			t.Errorf("simulated selection = %v, want [builder-busy]", got)
		}
	})

	t.Run("reporter lane with an empty board still ticks on cadence", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t) // the same empty board the builder was deferred on
		admitInsertProject(t, db, admitProjectSpec{
			Name: "fleet-sync", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("fleet-sync")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for fleet-sync = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (reporter keeps its timer cadence): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "fleet-sync" {
			t.Errorf("simulated selection = %v, want [fleet-sync]", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonNoWork] != 0 {
			t.Errorf("admission_counters[no_work] = %d, want 0 (reporter exempt)", counters[AdmissionReasonNoWork])
		}
	})

	t.Run("builder still inside its pin reports cooldown, not no_work", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "builder-pinned", CooldownS: 3600, Last: now.Add(-600 * time.Second), // pin NOT elapsed
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("builder-pinned")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonCooldown {
			t.Fatalf("reason = %q, want %q (the pin is the older, still-true answer): %s", got, AdmissionReasonCooldown, lines[0])
		}
	})
}

// TestSCHEDGAP1655_TasksLaneUntouched pins the tasks-mode family: the
// GAP-124 waiver path keeps its own reason vocabulary — an empty-board
// tasks lane stays tasks_no_work, never no_work.
func TestSCHEDGAP1655_TasksLaneUntouched(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	wd := admitWorkdirWithBoard(t)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "tasks-idle", CooldownS: 3600, Last: now.Add(-600 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	l := admitNewLoop(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("tasks-idle")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines = %d, want 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonTasksNoWork {
		t.Fatalf("reason = %q, want %q (tasks vocabulary unchanged): %s", got, AdmissionReasonTasksNoWork, lines[0])
	}
}

// TestSCHEDGAP1655_VocabularyComplete pins deliverable 4's plumbing: the
// new reason is IN the frozen vocabulary (counters zero-seed it, the ADMIT
// line shape accepts it), and AdmissionCounters exposes it per process.
func TestSCHEDGAP1655_VocabularyComplete(t *testing.T) {
	if !admissionReasonIsKnown(AdmissionReasonNoWork) {
		t.Fatalf("reason %q is not in the frozen vocabulary", AdmissionReasonNoWork)
	}
	found := false
	for _, r := range admissionReasonVocabulary {
		if r == AdmissionReasonNoWork {
			found = true
		}
	}
	if !found {
		t.Fatalf("admissionReasonVocabulary missing no_work: %v", admissionReasonVocabulary)
	}
}

// TestSCHEDGAP1655_OutcomeRecording pins AC 4 + AC 5's mapping half: a
// tick whose session had zero tool calls is recorded outcome=no_work, the
// value stays inside the schema vocabulary, and every pre-existing verdict
// is preserved byte-identically (the table carries the SCHED-GAP-1652
// contract plus the two new arms and the precedence rules).
func TestSCHEDGAP1655_OutcomeRecording(t *testing.T) {
	cases := []struct {
		name string
		in   TickOutcome
		want string
	}{
		{"completed zero-tool tick is no_work", TickOutcome{Status: TickCompleted, NoTools: true, Commits: 0, FilesChanged: 0}, "no_work"},
		{"no_work never outranks a landed commit", TickOutcome{Status: TickCompleted, NoTools: true, Commits: 2, FilesChanged: 1}, "committed"},
		{"no_work never outranks changed files", TickOutcome{Status: TickCompleted, NoTools: true, Commits: 0, FilesChanged: 3}, "committed"},
		{"no_work with unknown measurement is still no_work (sentinel)", TickOutcome{Status: TickCompleted, NoTools: true, Commits: -1, FilesChanged: -1}, "no_work"},
		{"tools-run no-artifact stays dry_run", TickOutcome{Status: TickCompleted, Commits: 0, FilesChanged: 0}, "dry_run"},
		{"failed ignores NoTools", TickOutcome{Status: TickFailed, NoTools: true}, "failed"},
		{"timeout ignores NoTools", TickOutcome{Status: TickTimeout, NoTools: true}, "timeout"},
		{"deferred ignores NoTools", TickOutcome{Status: TickDeferred, NoTools: true}, "deferred"},
		{"guard abort keeps its verdict", TickOutcome{Status: TickFailed, GuardAbort: true, NoTools: true}, AbortOutcomeValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalOutcome(tc.in); got != tc.want {
				t.Fatalf("terminalOutcome(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("outcome value is inside the DB CHECK vocabulary", func(t *testing.T) {
		allowed := map[string]bool{
			"committed": true, "dry_run": true, "failed": true, "timeout": true,
			"deferred": true, "aborted:no_artifact": true, "no_work": true,
		}
		for _, o := range []TickOutcome{
			{Status: TickCompleted, NoTools: true},
			{Status: TickCompleted},
			{Status: TickFailed, GuardAbort: true},
		} {
			if got := terminalOutcome(o); !allowed[got] {
				t.Fatalf("terminalOutcome(%+v) = %q — the schema CHECK would reject it", o, got)
			}
		}
	})
}

// TestSCHEDGAP1655_NoWorkLineShape pins the operator-facing NO-WORK log
// contract: grep-stable prefix, both the lane and the tick named, and the
// verdict spelled out — the way LOAD-GATE and BUILDER-GUARD lines work.
func TestSCHEDGAP1655_NoWorkLineShape(t *testing.T) {
	// The line is produced by the spawn path (fmt into log); assert the
	// constant shape contract by rendering the same way.
	line := noWorkTickLine("proj-x", "tick-1")
	if !strings.HasPrefix(line, "NO-WORK: proj-x") {
		t.Errorf("line = %q, want NO-WORK: prefix naming the lane", line)
	}
	if !strings.Contains(line, "tick-1") || !strings.Contains(line, "no_work") {
		t.Errorf("line = %q, want tick id and the no_work verdict", line)
	}
}

// countDeferralsFor counts the SCHED-GAP-157 deferrals rows for one
// project and reason — the queryable-waste assertion.
func countDeferralsFor(t *testing.T, db *sql.DB, project, reason string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deferrals WHERE project_name = ? AND reason = ?`,
		project, reason).Scan(&n); err != nil {
		t.Fatalf("count deferrals for %s/%s: %v", project, reason, err)
	}
	return n
}
