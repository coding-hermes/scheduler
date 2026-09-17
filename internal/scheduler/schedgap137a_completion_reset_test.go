package scheduler_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-137a: lifecycle.Complete must reset consecutive_failures on
// TickCompleted. Live evidence (foreman, 2026-09-17): bunker/chimera-v2/crier
// ran successful local ticks but kept consecutive_failures=91 — the projects
// UPDATE in Complete only touched last_tick_completed, never the failure
// counter, so local-spawn ticks never hit spawn.go's spawn-time reset.
//
// These tests pin the COMPLETION-time reset: a successful outcome clears the
// residue; failed/timeout outcomes leave it alone (GAP-133 backoff depends on
// the counter persisting across failures).

// getConsecutiveFailures reads the backoff counter directly (the setter,
// setConsecutiveFailures, lives in sgap001_regression_test.go).
func getConsecutiveFailures(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var cf int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, name).Scan(&cf); err != nil {
		t.Fatalf("query consecutive_failures for %s: %v", name, err)
	}
	return cf
}

// runCompleteFor walks a tick through Enqueue → StartRunning → Complete with
// the given status and returns the project's consecutive_failures afterwards.
func runCompleteFor(t *testing.T, db *sql.DB, project, tickID string, status scheduler.TickStatus) int {
	t.Helper()
	lt := scheduler.NewLifecycleTracker(db)

	if err := lt.Enqueue(project, tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	outcome := scheduler.TickOutcome{
		TickID:   tickID,
		Project:  project,
		Started:  now.Add(-time.Second),
		Finished: now,
		Status:   status,
	}
	if status == scheduler.TickFailed {
		outcome.ExitCode = 1
		outcome.Error = "boom"
	}
	if err := lt.Complete(outcome); err != nil {
		t.Fatalf("Complete(%s): %v", status, err)
	}
	return getConsecutiveFailures(t, db, project)
}

// TestSCHEDGAP137A_CompletionResetsConsecutiveFailures verifies a successful
// tick clears the stale failure counter (the drain-storm residue fix).
func TestSCHEDGAP137A_CompletionResetsConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "gap137a-ok")
	setConsecutiveFailures(t, db, "gap137a-ok", 5)

	got := runCompleteFor(t, db, "gap137a-ok", "gap137a-ok-1", scheduler.TickCompleted)

	if got != 0 {
		t.Errorf("projects.consecutive_failures not reset on TickCompleted: consecutive_failures = %d, want 0", got)
	}
}

// TestSCHEDGAP137A_FailureDoesNotResetConsecutiveFailures verifies failed
// outcomes leave the counter intact — GAP-133's FailureBackoff gate reads it.
func TestSCHEDGAP137A_FailureDoesNotResetConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "gap137a-fail")
	setConsecutiveFailures(t, db, "gap137a-fail", 5)

	got := runCompleteFor(t, db, "gap137a-fail", "gap137a-fail-1", scheduler.TickFailed)

	if got != 5 {
		t.Errorf("consecutive_failures = %d after TickFailed, want 5 (failure must keep backoff counter)", got)
	}
}

// TestSCHEDGAP137A_TimeoutDoesNotResetConsecutiveFailures verifies timeout
// outcomes also leave the counter intact ("no timeout backoff" applies to
// cooldown, not to erasing the failure history).
func TestSCHEDGAP137A_TimeoutDoesNotResetConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "gap137a-timeout")
	setConsecutiveFailures(t, db, "gap137a-timeout", 5)

	got := runCompleteFor(t, db, "gap137a-timeout", "gap137a-timeout-1", scheduler.TickTimeout)

	if got != 5 {
		t.Errorf("consecutive_failures = %d after TickTimeout, want 5 (timeout must keep backoff counter)", got)
	}
}
