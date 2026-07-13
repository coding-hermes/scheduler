package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

func ctx() context.Context { return context.Background() }

func mustCreateProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	mustCreateProjectAt(t, db, name, 10, 5, 900, 1.0)
}

// TestLifecycle_EnqueueStartComplete walks a tick through the full lifecycle.
func TestLifecycle_EnqueueStartComplete(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)

	tickID := "alpha-2026-01-01-00-00-00"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	tick, err := database.GetTick(ctx(), db, tickID)
	if err != nil {
		t.Fatalf("GetTick after enqueue: %v", err)
	}
	if tick.Status != database.StatusQueued {
		t.Errorf("after enqueue status = %q, want queued", tick.Status)
	}

	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	tick, _ = database.GetTick(ctx(), db, tickID)
	if tick.Status != database.StatusRunning {
		t.Errorf("after StartRunning status = %q, want running", tick.Status)
	}

	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Minute),
		Finished: now,
		Status:   scheduler.TickCompleted,
		ExitCode: 0,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tick, _ = database.GetTick(ctx(), db, tickID)
	if tick.Status != database.StatusCompleted {
		t.Errorf("after Complete status = %q, want completed", tick.Status)
	}
	if tick.CompletedAt == "" {
		t.Errorf("CompletedAt not set")
	}
}

// TestLifecycle_CompleteUpdatesLastTickCompleted verifies the project timestamp is updated
// on successful completion. We query the raw column since it isn't in the Go struct.
func TestLifecycle_CompleteUpdatesLastTickCompleted(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-1"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Second),
		Finished: now,
		Status:   scheduler.TickCompleted,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var lastCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "alpha").Scan(&lastCompleted); err != nil {
		t.Fatalf("query last_tick_completed: %v", err)
	}
	if !lastCompleted.Valid || lastCompleted.String == "" {
		t.Errorf("last_tick_completed not updated after successful tick: %+v", lastCompleted)
	}
}

// TestLifecycle_CompleteFailureDoesNotUpdateTimestamp verifies failed ticks don't bump it.
func TestLifecycle_CompleteFailureDoesNotUpdateTimestamp(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-1"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Second),
		Finished: now,
		Status:   scheduler.TickFailed,
		ExitCode: 1,
		Error:    "boom",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var lastCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "alpha").Scan(&lastCompleted); err != nil {
		t.Fatalf("query last_tick_completed: %v", err)
	}
	if lastCompleted.Valid {
		t.Errorf("last_tick_completed = %q, want NULL (failed ticks should not update)", lastCompleted.String)
	}
}

// TestLifecycle_EnqueueDuplicate verifies duplicate tick ID is rejected (PK conflict).
func TestLifecycle_EnqueueDuplicate(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-dup"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	err := lt.Enqueue("alpha", tickID)
	if err == nil {
		t.Fatal("second Enqueue with same tick ID succeeded; expected error")
	}
}

// TestLifecycle_CleanupStale marks running ticks as timeout when old enough.
func TestLifecycle_CleanupStale(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-stale"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	// Backdate the spawned_at to 2 hours ago.
	oldSpawned := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE ticks SET spawned_at = ? WHERE id = ?`, oldSpawned, tickID); err != nil {
		t.Fatalf("backdate spawned_at: %v", err)
	}

	// Clean up ticks older than 1 hour.
	n, err := lt.CleanupStale(time.Hour)
	if err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if n != 1 {
		t.Errorf("CleanupStale returned %d, want 1", n)
	}

	tick, err := database.GetTick(ctx(), db, tickID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if tick.Status != database.StatusTimeout {
		t.Errorf("after CleanupStale status = %q, want timeout", tick.Status)
	}
}

// TestLifecycle_CleanupStaleIgnoresRecent verifies fresh running ticks are left alone.
func TestLifecycle_CleanupStaleIgnoresRecent(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-fresh"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	// Tick just started → CleanupStale with 1h window should leave it alone.
	n, err := lt.CleanupStale(time.Hour)
	if err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if n != 0 {
		t.Errorf("CleanupStale returned %d, want 0 (tick is recent)", n)
	}
	tick, _ := database.GetTick(ctx(), db, tickID)
	if tick.Status != database.StatusRunning {
		t.Errorf("recent tick status = %q, want running (untouched)", tick.Status)
	}
}

// TestLifecycle_ExportSession_Placeholder verifies the placeholder behavior.
func TestLifecycle_ExportSession_Placeholder(t *testing.T) {
	db := newTestDB(t)
	lt := scheduler.NewLifecycleTracker(db)

	stats, err := lt.ExportSession("nonexistent-session")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if stats.SessionID != "nonexistent-session" {
		t.Errorf("SessionID = %q, want placeholder echo", stats.SessionID)
	}
}

// TestLifecycle_StartRunningMissingTick verifies StartRunning on unknown ID does not error
// (UPDATE that matches no rows is not an error in SQLite).
func TestLifecycle_StartRunningMissingTick(t *testing.T) {
	db := newTestDB(t)
	lt := scheduler.NewLifecycleTracker(db)

	if err := lt.StartRunning("does-not-exist"); err != nil {
		t.Errorf("StartRunning on missing tick returned %v, want nil", err)
	}
}

// TestLifecycle_CompleteMissingTick verifies Complete does not panic on missing row.
func TestLifecycle_CompleteMissingTick(t *testing.T) {
	db := newTestDB(t)
	lt := scheduler.NewLifecycleTracker(db)

	err := lt.Complete(scheduler.TickOutcome{
		TickID:  "missing",
		Project: "nope",
		Status:  scheduler.TickCompleted,
	})
	if err != nil {
		t.Logf("Complete on missing tick returned %v (acceptable)", err)
	}
}

// TestLifecycle_CompleteNegativeExitCode verifies exit_code < 0 is stored as NULL.
func TestLifecycle_CompleteNegativeExitCode(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-sigkill"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Second),
		Finished: now,
		Status:   scheduler.TickTimeout,
		ExitCode: -1, // sentinel for "killed by signal"
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var exitCode sql.NullInt64
	var status string
	if err := db.QueryRow(`SELECT status, exit_code FROM ticks WHERE id = ?`, tickID).Scan(&status, &exitCode); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exitCode.Valid {
		t.Errorf("exit_code = %d (valid), want NULL for negative exit_code", exitCode.Int64)
	}
	if status != "timeout" {
		t.Errorf("status = %q, want timeout", status)
	}
}

// TestLifecycle_CompleteWithEmptyError verifies empty error message is stored as NULL.
func TestLifecycle_CompleteWithEmptyError(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-ok"

	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-time.Second),
		Finished: now,
		Status:   scheduler.TickCompleted,
		Error:    "   ", // whitespace only — should be treated as empty
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var errStr sql.NullString
	if err := db.QueryRow(`SELECT error FROM ticks WHERE id = ?`, tickID).Scan(&errStr); err != nil {
		t.Fatalf("query: %v", err)
	}
	if errStr.Valid {
		t.Errorf("error = %q, want NULL (whitespace-only error should be stored as NULL)", errStr.String)
	}
}