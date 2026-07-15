package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Create minimal schema for tests.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			name TEXT PRIMARY KEY,
			enabled INTEGER DEFAULT 1,
			min_interval INTEGER DEFAULT 1800,
			max_interval INTEGER DEFAULT 7200
		);
		CREATE TABLE IF NOT EXISTS ticks (
			tick_id TEXT PRIMARY KEY,
			project_name TEXT,
			status TEXT DEFAULT 'queued',
			completed_at TEXT,
			started_at TEXT
		);
		CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			severity TEXT,
			component TEXT,
			message TEXT,
			details TEXT,
			created_at TEXT
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func insertProject(t *testing.T, db *sql.DB, name string, minI, maxI int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (name, enabled, min_interval, max_interval) VALUES (?, 1, ?, ?)`,
		name, minI, maxI)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

func insertTick(t *testing.T, db *sql.DB, tickID, project, status string, completedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ticks (tick_id, project_name, status, completed_at) VALUES (?, ?, ?, ?)`,
		tickID, project, status, completedAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick %s: %v", tickID, err)
	}
}

func countEventsBySeverity(t *testing.T, db *sql.DB, severity string) int {
	t.Helper()
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity = ?`, severity).Scan(&count)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// TestAlertEscalator_CheckSchedulerHealth_NotEvaluating emits CRITICAL when
// lastEval is zero (never ran).
func TestAlertEscalator_CheckSchedulerHealth_NotEvaluating(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckSchedulerHealth(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 1 {
		t.Errorf("expected 1 CRITICAL event, got %d", n)
	}
}

// TestAlertEscalator_CheckSchedulerHealth_Stale emits CRITICAL when lastEval
// is more than 10 minutes ago.
func TestAlertEscalator_CheckSchedulerHealth_Stale(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	stale := time.Now().Add(-15 * time.Minute)
	err := escalator.CheckSchedulerHealth(context.Background(), stale)
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 1 {
		t.Errorf("expected 1 CRITICAL event for stale eval, got %d", n)
	}
}

// TestAlertEscalator_CheckSchedulerHealth_Recent does NOT emit when lastEval
// is recent.
func TestAlertEscalator_CheckSchedulerHealth_Recent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	recent := time.Now().Add(-1 * time.Minute)
	err := escalator.CheckSchedulerHealth(context.Background(), recent)
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 0 {
		t.Errorf("expected 0 CRITICAL events for recent eval, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation emits MEDIUM for a project with no tick
// in more than 2x its max_interval.
func TestAlertEscalator_CheckStarvation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "test-proj", 1800, 3600) // 1h max interval
	// Last tick was 3 hours ago (> 2x 3600 = 2h).
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-001", "test-proj", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckStarvation(context.Background())
	if err != nil {
		t.Fatalf("CheckStarvation: %v", err)
	}

	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("expected 1 MEDIUM event for starved project, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_RecentTick does NOT emit when the project
// had a recent tick.
func TestAlertEscalator_CheckStarvation_RecentTick(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "active-proj", 1800, 3600)
	recent := time.Now().Add(-10 * time.Minute) // well within 2h threshold
	insertTick(t, db, "tick-002", "active-proj", "completed", recent)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckStarvation(context.Background())
	if err != nil {
		t.Fatalf("CheckStarvation: %v", err)
	}

	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 0 {
		t.Errorf("expected 0 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures emits HIGH when a project
// has more than 3 consecutive failed ticks.
func TestAlertEscalator_CheckConsecutiveFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "failing-proj", 1800, 3600)
	now := time.Now()
	for i := 0; i < 4; i++ {
		insertTick(t, db, "fail-"+string(rune('0'+i)), "failing-proj", "failed",
			now.Add(-time.Duration(4-i)*time.Minute))
	}

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckConsecutiveFailures(context.Background())
	if err != nil {
		t.Fatalf("CheckConsecutiveFailures: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("expected 1 HIGH event, got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures_BrokenStreak does NOT emit when
// a completed tick breaks the failure streak.
func TestAlertEscalator_CheckConsecutiveFailures_BrokenStreak(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "recovering-proj", 1800, 3600)
	now := time.Now()
	// 2 failures, then 1 success, then 2 more failures.
	insertTick(t, db, "f-1", "recovering-proj", "failed", now.Add(-5*time.Minute))
	insertTick(t, db, "f-2", "recovering-proj", "failed", now.Add(-4*time.Minute))
	insertTick(t, db, "ok-1", "recovering-proj", "completed", now.Add(-3*time.Minute))
	insertTick(t, db, "f-3", "recovering-proj", "failed", now.Add(-2*time.Minute))
	insertTick(t, db, "f-4", "recovering-proj", "failed", now.Add(-1*time.Minute))

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckConsecutiveFailures(context.Background())
	if err != nil {
		t.Fatalf("CheckConsecutiveFailures: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (streak broken), got %d", n)
	}
}

// TestAlertEscalator_RunAll runs all checks without error.
func TestAlertEscalator_RunAll(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	// Recent eval — should not emit CRITICAL.
	err := escalator.RunAll(context.Background(), time.Now().Add(-1*time.Minute))
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	// RunAll should not error even with no projects.
}
