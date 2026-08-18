package scheduler

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Regression tests for INFRA-012 (2026-08-01): daemon restart must NOT
// blanket-timeout running ticks. Gateway-spawned ticks have pid=0 and their
// HTTP sessions survive a daemon restart — startup cleanup must only reap
// ticks whose recorded pid no longer exists in /proc.

const deadTestPID = 2147483647 // pid_max upper bound on Linux — guaranteed nonexistent

// insertRunningTick inserts a tick row directly in 'running' status with the
// given pid (0 = gateway spawn, >0 = exec-fallback child).
func insertRunningTick(t *testing.T, db *sql.DB, tickID, project string, pid int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid)
		 VALUES (?, ?, 'running', ?, ?, ?)`, tickID, project, now, now, pid)
	if err != nil {
		t.Fatalf("insert running tick %s: %v", tickID, err)
	}
}

func tickStatusOf(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status); err != nil {
		t.Fatalf("query tick status %s: %v", tickID, err)
	}
	return status
}

func mustCreateProjectINFRA012(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	p := &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 0,
		DecayRate: 1.0,
		Model:     "test-model",
		Provider:  "test-provider",
		Enabled:   true,
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestCleanDanglingOnStartup_GatewayTickSurvives — a running tick with pid=0
// (gateway spawn) must be left 'running' by startup cleanup. Fails against
// the old blanket UPDATE (which marked ALL running ticks 'timeout').
func TestCleanDanglingOnStartup_GatewayTickSurvives(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gw-proj")
	insertRunningTick(t, db, "gw-tick-1", "gw-proj", 0)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.cleanDanglingOnStartup()

	if got := tickStatusOf(t, db, "gw-tick-1"); got != "running" {
		t.Errorf("pid=0 gateway tick status = %q, want running (must survive daemon restart)", got)
	}

	// last_tick_completed must NOT be bumped — no ticks were reaped.
	var lastCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "gw-proj").Scan(&lastCompleted); err != nil {
		t.Fatalf("query last_tick_completed: %v", err)
	}
	if lastCompleted.Valid && lastCompleted.String != "" {
		t.Errorf("last_tick_completed bumped to %q despite no reaped ticks — distorts packer urgency", lastCompleted.String)
	}
}

// TestCleanDanglingOnStartup_DeadPidReaped — a running tick with a pid that
// no longer exists in /proc must be marked 'timeout', while a pid=0 gateway
// tick for another project stays 'running'. The last_tick_completed bump must
// apply ONLY to the reaped project. The pid=0 assertion catches the old bug
// (blanket UPDATE timed out both ticks).
func TestCleanDanglingOnStartup_DeadPidReaped(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "dead-proj")
	mustCreateProjectINFRA012(t, db, "live-proj")
	insertRunningTick(t, db, "dead-tick-1", "dead-proj", deadTestPID)
	insertRunningTick(t, db, "live-tick-1", "live-proj", 0)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.cleanDanglingOnStartup()

	if got := tickStatusOf(t, db, "dead-tick-1"); got != "timeout" {
		t.Errorf("dead-pid tick status = %q, want timeout", got)
	}
	if got := tickStatusOf(t, db, "live-tick-1"); got != "running" {
		t.Errorf("pid=0 gateway tick status = %q, want running", got)
	}

	var lastCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "dead-proj").Scan(&lastCompleted); err != nil {
		t.Fatalf("query dead-proj last_tick_completed: %v", err)
	}
	if !lastCompleted.Valid || lastCompleted.String == "" {
		t.Errorf("reaped project's last_tick_completed not bumped: %+v", lastCompleted)
	}

	lastCompleted = sql.NullString{}
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "live-proj").Scan(&lastCompleted); err != nil {
		t.Fatalf("query live-proj last_tick_completed: %v", err)
	}
	if lastCompleted.Valid && lastCompleted.String != "" {
		t.Errorf("live project's last_tick_completed bumped to %q — only reaped projects should be touched", lastCompleted.String)
	}
}

// TestCleanDanglingOnStartup_NoDuplicateRespawn — packer-level regression from
// the board: after a daemon restart with an in-flight pid=0 tick, the running
// project must NOT be picked again by the namespace packer (no duplicate tick).
// With the old blanket cleanup, the tick would be timed out, the running set
// would be empty, and the project would be re-selected here.
func TestCleanDanglingOnStartup_NoDuplicateRespawn(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	nsID := "ns-a"
	ns := &database.Namespace{ID: nsID, Weight: 10, Reserved: 5, HardCap: 100, Enabled: true}
	if err := database.CreateNamespace(ctx, db, ns); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	for _, name := range []string{"p1", "p2", "p3"} {
		p := &database.Project{
			Name:        name,
			RepoURL:     "https://example.com/" + name,
			Workdir:     "/tmp/" + name,
			Weight:      5,
			Priority:    5,
			CooldownS:   0,
			DecayRate:   1.0,
			Model:       "test-model",
			Provider:    "test-provider",
			Enabled:     true,
			NamespaceID: &nsID,
		}
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}

	// p1 has an in-flight gateway tick (pid=0) from before the "restart".
	insertRunningTick(t, db, "p1-inflight", "p1", 0)

	// Simulate daemon restart: startup cleanup runs, must leave p1 running.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.cleanDanglingOnStartup()

	// Rebuild the running set exactly the way evaluate()/evalContext does.
	var running []string
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT project_name FROM ticks WHERE status = 'running'`)
	if err != nil {
		t.Fatalf("query running ticks: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan running: %v", err)
		}
		running = append(running, name)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := NewMultiPoolPacker(100, 10, nil)
	result := mp.Pack(projects, namespaces,
		NewUrgencyCalculator(time.Minute, time.Hour, 10), nil, running, time.Now())

	picked := make(map[string]bool, len(result.Projects))
	for _, p := range result.Projects {
		picked[p.Name] = true
	}
	if picked["p1"] {
		t.Errorf("project p1 (in-flight pid=0 tick) re-picked after restart — duplicate tick spawned")
	}
	if !picked["p2"] {
		t.Errorf("eligible project p2 was not picked")
	}
	if !picked["p3"] {
		t.Errorf("eligible project p3 was not picked")
	}
}

// tickOutcomeOf returns the raw outcome column for a tick (NULL when unset).
func tickOutcomeOf(t *testing.T, db *sql.DB, tickID string) sql.NullString {
	t.Helper()
	var outcome sql.NullString
	if err := db.QueryRow(`SELECT outcome FROM ticks WHERE id = ?`, tickID).Scan(&outcome); err != nil {
		t.Fatalf("query tick outcome %s: %v", tickID, err)
	}
	return outcome
}

// tickCompletedAtOf returns the raw completed_at column for a tick (NULL
// when unset). GAP-045: reaped timeout rows must carry a non-NULL
// completed_at so they are terminal for duration / failure-window math.
func tickCompletedAtOf(t *testing.T, db *sql.DB, tickID string) sql.NullString {
	t.Helper()
	var completedAt sql.NullString
	if err := db.QueryRow(`SELECT completed_at FROM ticks WHERE id = ?`, tickID).Scan(&completedAt); err != nil {
		t.Fatalf("query tick completed_at %s: %v", tickID, err)
	}
	return completedAt
}

// TestReapZombies_DeadPidReaped — a running tick whose pid no longer exists
// in /proc must be reaped by the 60s zombie reaper: status='timeout' and
// outcome left NULL. Regression for REC-ZOMBIE-OUTCOME: the old code set
// outcome='zombie_reaped', which violates the ticks outcome CHECK constraint
// ('committed','dry_run','failed','timeout'), so SQLite rejected the UPDATE
// and the tick stayed 'running' forever — reaping silently never worked.
func TestReapZombies_DeadPidReaped(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "zombie-proj")
	insertRunningTick(t, db, "zombie-tick-1", "zombie-proj", deadTestPID)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.reapZombies()

	if got := tickStatusOf(t, db, "zombie-tick-1"); got != "timeout" {
		t.Errorf("dead-pid tick status = %q, want timeout (old code: UPDATE rejected by outcome CHECK, stays running)", got)
	}
	if outcome := tickOutcomeOf(t, db, "zombie-tick-1"); outcome.Valid {
		t.Errorf("dead-pid tick outcome = %q, want NULL (CHECK constraint rejects 'zombie_reaped')", outcome.String)
	}
	// GAP-045: the dead-pid reap path must stamp completed_at like the
	// failed/completed paths — a timeout row without it reads as in-flight
	// forever in duration / failure-window / p99-latency queries.
	if completedAt := tickCompletedAtOf(t, db, "zombie-tick-1"); !completedAt.Valid || completedAt.String == "" {
		t.Errorf("dead-pid tick completed_at = %v, want stamped (GAP-045)", completedAt)
	}
}

// TestReapZombies_LivePidUntouched — a running tick whose pid is alive (the
// test process itself) must NOT be reaped by the zombie reaper.
func TestReapZombies_LivePidUntouched(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "live-proj")
	insertRunningTick(t, db, "live-tick-1", "live-proj", os.Getpid())

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.reapZombies()

	if got := tickStatusOf(t, db, "live-tick-1"); got != "running" {
		t.Errorf("live-pid tick status = %q, want running (pid %d is the test process — alive)", got, os.Getpid())
	}
}

// TestReapZombies_PidZeroUntouched — a gateway-spawned tick (pid=0) must NOT
// be reaped by the zombie reaper; reapZombies only targets pid > 0.
func TestReapZombies_PidZeroUntouched(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gw-proj")
	insertRunningTick(t, db, "gw-tick-1", "gw-proj", 0)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.reapZombies()

	if got := tickStatusOf(t, db, "gw-tick-1"); got != "running" {
		t.Errorf("pid=0 gateway tick status = %q, want running (reaper only targets pid > 0)", got)
	}
}

// TestEvalFallback_NoDuplicateRespawnAfterRestart — SCHED-GAP-030 regression
// (2026-08-11). Production topology: fleet.toml carries 0 namespaces, so
// evaluate() takes the FALLBACK packer path (packer.go) whose running set
// comes from the in-memory slot pool — EMPTY right after a daemon restart.
// cleanDanglingOnStartup leaves fresh pid=0 gateway ticks 'running', but
// without this fix the first EVAL re-picks the project and spawns a
// duplicate tick (observed live: daemon restart spawned
// coding-hermes-scheduler-2026-08-11-03-00-52 while the 02-58-39 tick was
// still executing). The DB running set must be merged into the fallback
// packer's running set.
func TestEvalFallback_NoDuplicateRespawnAfterRestart(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "p1")
	// p1 has an in-flight gateway tick (pid=0) surviving the restart.
	insertRunningTick(t, db, "p1-inflight", "p1", 0)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.ForceEvaluate()

	// evaluate() must NOT spawn a second tick for p1. Poll briefly for the
	// goroutine to finish, then assert exactly one row remains.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'p1'`).Scan(&n); err != nil {
			t.Fatalf("count p1 ticks: %v", err)
		}
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'p1'`).Scan(&n); err != nil {
		t.Fatalf("count p1 ticks: %v", err)
	}
	if n != 1 {
		t.Fatalf("p1 tick rows = %d, want 1 — fallback packer double-spawned an in-flight project after restart", n)
	}
	if got := tickStatusOf(t, db, "p1-inflight"); got != "running" {
		t.Errorf("p1-inflight status = %q, want running", got)
	}
}
