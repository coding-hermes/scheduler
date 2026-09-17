package scheduler

import (
	"context"
	"database/sql"
	"strings"
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
			cooldown_s INTEGER DEFAULT 1800,
			workdir TEXT DEFAULT '',
			updated_at TEXT,
			disabled_at TEXT,
			disabled_by TEXT,
			disabled_reason TEXT
		);
		CREATE TABLE IF NOT EXISTS ticks (
			id TEXT PRIMARY KEY,
			project_name TEXT,
			status TEXT DEFAULT 'queued',
			completed_at TEXT,
			spawned_at TEXT,
			started_at TEXT,
			error TEXT DEFAULT ''
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

func insertProject(t *testing.T, db *sql.DB, name string, cooldown int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (name, enabled, cooldown_s) VALUES (?, 1, ?)`,
		name, cooldown)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

func insertTick(t *testing.T, db *sql.DB, tickID, project, status string, completedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at) VALUES (?, ?, ?, ?, ?)`,
		tickID, project, status, completedAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
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
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

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
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

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
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

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
// in more than 2x its cooldown.
func TestAlertEscalator_CheckStarvation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "test-proj", 1800) // 1h max interval
	// Last tick was 3 hours ago (> 2x 3600 = 2h).
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-001", "test-proj", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

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

	insertProject(t, db, "active-proj", 1800)
	recent := time.Now().Add(-10 * time.Minute) // well within 2h threshold
	insertTick(t, db, "tick-002", "active-proj", "completed", recent)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

	err := escalator.CheckStarvation(context.Background())
	if err != nil {
		t.Fatalf("CheckStarvation: %v", err)
	}

	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 0 {
		t.Errorf("expected 0 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_Throttle proves SCHED-GAP-014: consecutive
// CheckStarvation calls emit at most one MEDIUM event per starvationThrottleWindow
// per project, even though the escalator is constructed fresh each time (as it
// is in production at tick_process.go:134).
func TestAlertEscalator_CheckStarvation_Throttle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "starved-proj", 1800) // 2x cooldown = 1h
	oldTick := time.Now().Add(-3 * time.Hour)  // well beyond 1h threshold
	insertTick(t, db, "tick-old", "starved-proj", "completed", oldTick)

	// First call — should emit (first crossing of the threshold).
	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Fatalf("after first call: expected 1 MEDIUM event, got %d", n)
	}

	// Second call — fresh escalator (mirrors production), same project still
	// starved. Throttle must suppress: still 1 event.
	escalator2 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("after second call (throttled): expected 1 MEDIUM event, got %d", n)
	}

	// Third call — still throttled.
	escalator3 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator3.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #3: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("after third call (throttled): expected 1 MEDIUM event, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_ThrottleExpires proves that after the
// throttle window passes, a new starvation event IS emitted (not suppressed
// forever). We simulate this by manually backdating the first event's
// created_at to before the throttle window.
func TestAlertEscalator_CheckStarvation_ThrottleExpires(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "expiring-proj", 1800) // 2x cooldown = 1h
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-old", "expiring-proj", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Fatalf("after first call: expected 1 MEDIUM event, got %d", n)
	}

	// Backdate the emitted event to 31 minutes ago — just past the 30-min window.
	_, err := db.Exec(`UPDATE events SET created_at = ? WHERE severity = 'MEDIUM' AND component = 'escalation'`,
		time.Now().Add(-31*time.Minute).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("backdate event: %v", err)
	}

	// Second call — throttle has expired, should emit again.
	escalator2 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Errorf("after throttle expires: expected 2 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_ThrottleDistinctProjects proves the
// throttle is per-project: two starved projects each emit once on the first
// call, and neither emits again on the second call.
func TestAlertEscalator_CheckStarvation_ThrottleDistinctProjects(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "proj-a", 1800)
	insertProject(t, db, "proj-b", 1800)
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-a", "proj-a", "completed", oldTick)
	insertTick(t, db, "tick-b", "proj-b", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Fatalf("after first call: expected 2 MEDIUM events (one per project), got %d", n)
	}

	// Second call — both throttled.
	escalator2 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Errorf("after second call (throttled): expected 2 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures emits HIGH when a project
// has more than 3 consecutive failed ticks.
func TestAlertEscalator_CheckConsecutiveFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "failing-proj", 1800)
	now := time.Now()
	for i := 0; i < 4; i++ {
		insertTick(t, db, "fail-"+string(rune('0'+i)), "failing-proj", "failed",
			now.Add(-time.Duration(4-i)*time.Minute))
	}

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

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

	insertProject(t, db, "recovering-proj", 1800)
	now := time.Now()
	// 2 failures, then 1 success, then 2 more failures.
	insertTick(t, db, "f-1", "recovering-proj", "failed", now.Add(-5*time.Minute))
	insertTick(t, db, "f-2", "recovering-proj", "failed", now.Add(-4*time.Minute))
	insertTick(t, db, "ok-1", "recovering-proj", "completed", now.Add(-3*time.Minute))
	insertTick(t, db, "f-3", "recovering-proj", "failed", now.Add(-2*time.Minute))
	insertTick(t, db, "f-4", "recovering-proj", "failed", now.Add(-1*time.Minute))

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

	err := escalator.CheckConsecutiveFailures(context.Background())
	if err != nil {
		t.Fatalf("CheckConsecutiveFailures: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (streak broken), got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures_Throttle proves SCHED-GAP-137c:
// consecutive CheckConsecutiveFailures calls emit at most one HIGH event per
// consecutiveFailureThrottleWindow per project, even though the escalator is
// constructed fresh each time (as it is in production at tick_process.go:134).
func TestAlertEscalator_CheckConsecutiveFailures_Throttle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "stuck-proj", 1800)
	now := time.Now()
	for i := 0; i < 4; i++ {
		insertTick(t, db, "stuck-fail-"+string(rune('0'+i)), "stuck-proj", "failed",
			now.Add(-time.Duration(4-i)*time.Minute))
	}

	// First call — should emit (first crossing of the >3 threshold).
	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator.CheckConsecutiveFailures(context.Background()); err != nil {
		t.Fatalf("CheckConsecutiveFailures #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Fatalf("after first call: expected 1 HIGH event, got %d", n)
	}

	// Second call — fresh escalator (mirrors production), same project still
	// stuck. Throttle must suppress: still 1 event.
	escalator2 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator2.CheckConsecutiveFailures(context.Background()); err != nil {
		t.Fatalf("CheckConsecutiveFailures #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("after second call (throttled): expected 1 HIGH event, got %d", n)
	}

	// Third call — still throttled.
	escalator3 := NewAlertEscalator(db, events, autoDisablePolicy{})
	if err := escalator3.CheckConsecutiveFailures(context.Background()); err != nil {
		t.Fatalf("CheckConsecutiveFailures #3: %v", err)
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("after third call (throttled): expected 1 HIGH event, got %d", n)
	}
}

// TestAlertEscalator_CheckDuplicateWorkdirs emits HIGH when two ENABLED
// projects share the same workdir (case-insensitive), and stays silent for
// unique or disabled duplicates.
func TestAlertEscalator_CheckDuplicateWorkdirs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Two enabled projects sharing a workdir (different case).
	insertProject(t, db, "HEADING", 43200)
	insertProject(t, db, "heading", 900)
	_, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/heading", "HEADING")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}
	_, err = db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/heading", "heading")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}

	// Unique workdir project — must NOT trigger.
	insertProject(t, db, "solo", 900)
	_, err = db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/solo", "solo")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}

	// Disabled duplicate — must NOT trigger.
	insertProject(t, db, "archived", 900)
	_, err = db.Exec(`UPDATE projects SET workdir = ?, enabled = 0 WHERE name = ?`, "/home/kara/shared", "archived")
	if err != nil {
		t.Fatalf("set archived: %v", err)
	}

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

	if err := escalator.CheckDuplicateWorkdirs(context.Background()); err != nil {
		t.Fatalf("CheckDuplicateWorkdirs: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("expected 1 HIGH event for the duplicate pair, got %d", n)
	}
	var msg string
	err = db.QueryRow(`SELECT message FROM events WHERE severity = 'HIGH'`).Scan(&msg)
	if err != nil {
		t.Fatalf("read HIGH event: %v", err)
	}
	if !strings.Contains(msg, "duplicate workdir") {
		t.Errorf("event message = %q, want duplicate-workdir mention", msg)
	}
}

// TestAlertEscalator_RunAll runs all checks without error.
func TestAlertEscalator_RunAll(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events, autoDisablePolicy{})

	// Recent eval — should not emit CRITICAL.
	err := escalator.RunAll(context.Background(), time.Now().Add(-1*time.Minute))
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	// RunAll should not error even with no projects.
}

// --- CheckFailureRateAutoDisable (SCHED-GAP-018) -------------------------

// helper: check if a project is enabled in the DB.
func projectEnabled(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var enabled int
	err := db.QueryRow(`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&enabled)
	if err != nil {
		t.Fatalf("query enabled for %s: %v", name, err)
	}
	return enabled == 1
}

// TestAutoDisable_DisablesHighFailureProject: a project with 96% failure rate
// over 100 ticks, threshold=0.95, min_ticks=50 → should be disabled + event.
func TestAutoDisable_DisablesHighFailureProject(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "failing-proj", 1800)
	now := time.Now()
	// 96 failed ticks, 4 completed ticks = 96% failure rate over 100 ticks.
	for i := 0; i < 96; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "failing-proj", "failed",
			now.Add(-time.Duration(100-i)*time.Second))
	}
	for i := 0; i < 4; i++ {
		insertTick(t, db, "ok-"+string(rune('A'+i)), "failing-proj", "completed",
			now.Add(-time.Duration(4-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.95, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if projectEnabled(t, db, "failing-proj") {
		t.Error("expected failing-proj to be disabled")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("expected 1 HIGH auto-disable event, got %d", n)
	}
	// GAP-044: auto-disable must stamp provenance (who/when/why).
	var disabledBy, disabledAt, disabledReason string
	if err := db.QueryRow(
		`SELECT COALESCE(disabled_by,''), COALESCE(disabled_at,''), COALESCE(disabled_reason,'') FROM projects WHERE name = 'failing-proj'`,
	).Scan(&disabledBy, &disabledAt, &disabledReason); err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	if disabledBy != "auto-disable" {
		t.Errorf("disabled_by = %q, want \"auto-disable\"", disabledBy)
	}
	if disabledAt == "" {
		t.Error("disabled_at empty — auto-disable must stamp a timestamp")
	}
	if !strings.Contains(disabledReason, "96.0%") {
		t.Errorf("disabled_reason = %q, want failure stats including 96.0%%", disabledReason)
	}
}

// TestAutoDisable_NoOpBelowThreshold: 80% failure rate, threshold=0.95 → no-op.
func TestAutoDisable_NoOpBelowThreshold(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "borderline", 1800)
	now := time.Now()
	for i := 0; i < 80; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "borderline", "failed",
			now.Add(-time.Duration(80-i)*time.Second))
	}
	for i := 0; i < 20; i++ {
		insertTick(t, db, "ok-"+string(rune('A'+i)), "borderline", "completed",
			now.Add(-time.Duration(20-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.95, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if !projectEnabled(t, db, "borderline") {
		t.Error("expected borderline to remain enabled (below threshold)")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events, got %d", n)
	}
}

// TestAutoDisable_NoOpBelowMinTicks: failure rate above threshold but only
// 30 ticks total (< min_ticks=50) → no-op.
func TestAutoDisable_NoOpBelowMinTicks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "small-sample", 1800)
	now := time.Now()
	// 29 failures + 1 completed = 96.7% but only 30 ticks total.
	for i := 0; i < 29; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "small-sample", "failed",
			now.Add(-time.Duration(30-i)*time.Second))
	}
	insertTick(t, db, "ok-1", "small-sample", "completed", now)

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.95, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if !projectEnabled(t, db, "small-sample") {
		t.Error("expected small-sample to remain enabled (below min_ticks)")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (below min_ticks), got %d", n)
	}
}

// TestAutoDisable_FeatureOff: threshold=0 → no-op even with 100% failure.
func TestAutoDisable_FeatureOff(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "all-failing", 1800)
	now := time.Now()
	for i := 0; i < 100; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "all-failing", "failed",
			now.Add(-time.Duration(100-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0} // feature off
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if !projectEnabled(t, db, "all-failing") {
		t.Error("expected all-failing to remain enabled (feature off)")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (feature off), got %d", n)
	}
}

// TestAutoDisable_SkipsAlreadyDisabled: disabled project with 100% failure
// rate → skipped (no event, stays disabled).
func TestAutoDisable_SkipsAlreadyDisabled(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "already-off", 1800)
	_, err := db.Exec(`UPDATE projects SET enabled = 0 WHERE name = 'already-off'`)
	if err != nil {
		t.Fatalf("disable project: %v", err)
	}
	now := time.Now()
	for i := 0; i < 100; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "already-off", "failed",
			now.Add(-time.Duration(100-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.95, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if projectEnabled(t, db, "already-off") {
		t.Error("already-disabled project should not be re-enabled")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (already disabled), got %d", n)
	}
}

// TestAutoDisable_WindowBounds: with a small window (e.g. 10), only the last
// 10 ticks matter — a project that was failing early but recently recovered
// should NOT be disabled.
func TestAutoDisable_WindowBounds(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "recovered", 1800)
	now := time.Now()
	// 90 old failures (outside the window of 10).
	for i := 0; i < 90; i++ {
		insertTick(t, db, "old-fail-"+string(rune('A'+i)), "recovered", "failed",
			now.Add(-time.Duration(100-i)*time.Minute))
	}
	// 10 recent successes (inside the window of 10).
	for i := 0; i < 10; i++ {
		insertTick(t, db, "recent-ok-"+string(rune('A'+i)), "recovered", "completed",
			now.Add(-time.Duration(10-i)*time.Second))
	}

	events := NewEventLogger(db)
	// min_ticks=5 so the 10 recent ticks pass the sample-size guard; window=10.
	policy := autoDisablePolicy{failureRate: 0.50, window: 10, minTicks: 5}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	// The 10 most recent ticks are all completed → 0% failure rate in window.
	if !projectEnabled(t, db, "recovered") {
		t.Error("expected recovered to remain enabled (window excludes old failures)")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events, got %d", n)
	}
}

// TestAutoDisable_EventDetails verifies the HIGH event contains the expected
// detail fields.
func TestAutoDisable_EventDetails(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "doomed", 1800)
	now := time.Now()
	for i := 0; i < 100; i++ {
		insertTick(t, db, "fail-"+string(rune('A'+i)), "doomed", "failed",
			now.Add(-time.Duration(100-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.95, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	var details string
	err := db.QueryRow(`SELECT details FROM events WHERE severity = 'HIGH' AND component = 'auto-disable'`).Scan(&details)
	if err != nil {
		t.Fatalf("query auto-disable event: %v", err)
	}
	// Verify key fields are present in the JSON details.
	for _, key := range []string{`"project"`, `"failure_rate"`, `"failed"`, `"total"`, `"window"`, `"threshold"`} {
		if !strings.Contains(details, key) {
			t.Errorf("auto-disable event details missing %s: %s", key, details)
		}
	}
	if !strings.Contains(details, `"doomed"`) {
		t.Errorf("auto-disable event details should reference project 'doomed': %s", details)
	}
}

// --- SCHED-GAP-134: harness failures must not feed project health ----------

// insertTickE inserts a tick with an error string (insertTick has no error
// column support; harness-failure classification reads ticks.error).
func insertTickE(t *testing.T, db *sql.DB, tickID, project, status, errText string, completedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ticks (id, project_name, status, error, completed_at, spawned_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tickID, project, status, errText, completedAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick %s: %v", tickID, err)
	}
}

// TestAutoDisable_IgnoresHarnessFailures (SCHED-GAP-134): 100 failed ticks whose
// errors are all gateway-outage classes must NOT auto-disable the project — the
// project never got to run. Regression test for the 2026-09-16 mass-disable.
func TestAutoDisable_IgnoresHarnessFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "gatewayblocked", 1800)
	now := time.Now()
	errText := `gateway unreachable and exec fallback disabled: gateway POST: HTTP 503: invalid_request_error: Gateway is draining existing work; retry shortly.`
	for i := 0; i < 100; i++ {
		insertTickE(t, db, "fail-"+string(rune('A'+i%26))+string(rune('a'+i/26)), "gatewayblocked", "failed", errText,
			now.Add(-time.Duration(100-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.90, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if !projectEnabled(t, db, "gatewayblocked") {
		t.Error("expected gatewayblocked to stay ENABLED — all failures were harness-side (SCHED-GAP-134)")
	}
	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected no HIGH auto-disable event, got %d", n)
	}
}

// TestAutoDisable_StillCountsProjectFailures: harness-failure exclusion must not
// blunt the breaker — a project failing on its own terms is still disabled.
func TestAutoDisable_StillCountsProjectFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "broken", 1800)
	now := time.Now()
	for i := 0; i < 100; i++ {
		insertTickE(t, db, "fail-"+string(rune('A'+i%26))+string(rune('a'+i/26)), "broken", "failed",
			"tick timeout after 7200s: no board progress", now.Add(-time.Duration(100-i)*time.Second))
	}

	events := NewEventLogger(db)
	policy := autoDisablePolicy{failureRate: 0.90, window: 100, minTicks: 50}
	escalator := NewAlertEscalator(db, events, policy)

	if err := escalator.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if projectEnabled(t, db, "broken") {
		t.Error("expected broken to be disabled — its failures are project-side")
	}
}

// TestHarnessFailureClassification pins the marker list.
func TestHarnessFailureClassification(t *testing.T) {
	yes := []string{
		`gateway unreachable and exec fallback disabled: gateway POST: Post "http://127.0.0.1:8642/v1/responses": dial tcp 127.0.0.1:8642: connect: connection refused`,
		`gateway unreachable and exec fallback disabled: gateway POST: HTTP 503: invalid_request_error: Gateway is draining existing work; retry shortly.`,
		`aborted by graceful shutdown — drain timeout`,
		`Invalid gateway API key (API_SERVER_KEY)`,
	}
	no := []string{
		``,
		`tick timeout after 7200s`,
		`exit status 2`,
		`judge rejected the criteria evidence`,
	}
	for _, s := range yes {
		if !harnessFailure(s) {
			t.Errorf("harnessFailure(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if harnessFailure(s) {
			t.Errorf("harnessFailure(%q) = true, want false", s)
		}
	}
}
