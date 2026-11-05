package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── PERF-001-R3-R3 regression pins ────────────────────────────────────────
//
// The row's boot-window starvation, measured on a byte copy of the live
// 16.7 GB scheduler DB (2026-11-05): /health answered 504 at 1.001s at
// t+1s AND t+5s because countActiveTicks queued behind the boot
// orphan-resume pass on the single serialized SQLite connection
// (SetMaxOpenConns(1)); /status answered 5.17s at t+5s for the same
// reason; the first dashboard render degraded to a 40 KB partial page.
//
// These tests pin the two mechanisms that close it:
//
//  1. the DB-backed read surfaces route through a mode=ro pool, so a
//     handler cannot queue behind a held write connection;
//  2. /health's active_ticks + stall census come from the warm snapshot,
//     so the hot path issues no cold SELECT at all.

// seedBootStarvationFixture builds a scheduler.db with one project, one
// running tick and one namespace, re-opens a second write handle, and
// returns (writeDB, dbPath). The caller opens the read pool against
// dbPath.
func seedBootStarvationFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler.db")
	writeDB, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = writeDB.Close() })
	if _, err := writeDB.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('perf-lane', 'https://example.invalid/perf-lane', '/tmp/perf-lane', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := writeDB.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
VALUES ('perf-tick', 'perf-lane', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed tick: %v", err)
	}
	if _, err := writeDB.Exec(`INSERT INTO namespaces (id, created_at, updated_at)
VALUES ('perf-ns', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	return writeDB, dbPath
}

// holdBootWriter opens a write transaction on writeDB, occupying the write
// pool's ONLY connection exactly like the boot orphan-resume pass does
// while it emits its events (loop.go → EmitHighEvent → EventLogger.Emit).
// The returned rollback releases it.
func holdBootWriter(t *testing.T, writeDB *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := writeDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx (boot-writer hold): %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('perf-hold', 'hold', 'hold', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("boot-writer hold (insert): %v", err)
	}
	return tx
}

// TestBootStarvation_HealthAnswersWhileBootWriterHoldsConnection pins arm 1
// at the unit level: with the write pool's single connection held inside a
// write transaction (the boot pass), GET /api/v1/health on a Server with a
// read pool answers 200 well inside its 1s-class budget. The pre-row shape
// (handler reads on the shared pool) blocks until the transaction releases
// — the measured 504-at-1.001s — so a regression to the shared handle turns
// this into a client-timeout failure, not a vacuous pass.
func TestBootStarvation_HealthAnswersWhileBootWriterHoldsConnection(t *testing.T) {
	writeDB, dbPath := seedBootStarvationFixture(t)
	ro, err := database.OpenReadPool(dbPath, 2)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	loop := scheduler.NewLoop(writeDB, time.Minute, time.Hour, 10, 0, 5)
	srv := NewServer(writeDB, loop)
	srv.SetReadDB(ro)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tx := holdBootWriter(t, writeDB)
	defer func() { _ = tx.Rollback() }()

	// The health budget is 1s in production; allow 3s here so a loaded
	// host cannot flake the pin — still 2s short of any real queueing
	// behind the held transaction, which never releases on its own.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health while the boot-writer holds the connection: %v (this is the PERF-001-R3-R3 starvation shape)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/health = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if body["active_ticks"] != float64(1) {
		t.Fatalf("active_ticks = %v, want 1", body["active_ticks"])
	}
}

// TestBootStarvation_HealthSnapshotServesWithoutColdSelect pins the second
// mechanism: once the warm snapshot exists, /api/v1/health answers
// correctly even with the read pool CLOSED — proving the hot path issues
// no SELECT for active_ticks or the stall census. A regression that
// recomputes inline would answer active_ticks 0 (countActiveTicks swallows
// the closed-pool error), so the assertion is load-bearing.
func TestBootStarvation_HealthSnapshotServesWithoutColdSelect(t *testing.T) {
	writeDB, dbPath := seedBootStarvationFixture(t)
	ro, err := database.OpenReadPool(dbPath, 2)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}

	loop := scheduler.NewLoop(writeDB, time.Minute, time.Hour, 10, 0, 5)
	srv := NewServer(writeDB, loop)
	srv.SetReadDB(ro)

	// Warm the snapshot exactly as the boot refresher does (one compute,
	// synchronously — the goroutine loop adds nothing to this pin).
	srv.refreshHealthSnapshot()

	// Close the pool: any SELECT on the hot path from here on fails.
	_ = ro.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health with closed read pool: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/health = %d, want 200 from the warm snapshot", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if body["active_ticks"] != float64(1) {
		t.Fatalf("active_ticks = %v, want 1 served from the warm snapshot (0 would mean the hot path recomputed against the closed pool — the cold SELECT this pin forbids)", body["active_ticks"])
	}
	stall, ok := body["stall_census"].(map[string]interface{})
	if !ok || stall == nil {
		t.Fatalf("stall_census missing or wrong shape: %v", body["stall_census"])
	}
}

// TestBootStarvation_StatusProjectsCadenceWhileBootWriterHoldsConnection
// pins arm 2's shape: the three heavy read endpoints all answer 200 while
// the boot-writer holds the write connection, on a Server with a read pool.
func TestBootStarvation_StatusProjectsCadenceWhileBootWriterHoldsConnection(t *testing.T) {
	writeDB, dbPath := seedBootStarvationFixture(t)
	ro, err := database.OpenReadPool(dbPath, 4)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	loop := scheduler.NewLoop(writeDB, time.Minute, time.Hour, 10, 0, 5)
	srv := NewServer(writeDB, loop)
	srv.SetReadDB(ro)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tx := holdBootWriter(t, writeDB)
	defer func() { _ = tx.Rollback() }()

	client := &http.Client{Timeout: 6 * time.Second}
	for _, path := range []string{"/api/v1/status", "/api/v1/projects", "/api/v1/cadence"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s while the boot-writer holds the connection: %v (starvation shape)", path, err)
		}
		if resp.StatusCode != 200 {
			body := make([]byte, 512)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()
			t.Fatalf("GET %s = %d, want 200 (body: %s)", path, resp.StatusCode, body[:n])
		}
		resp.Body.Close()
	}
}

// TestBootStarvation_WithoutReadPool_HealthSnapshotAbsent pins the
// fallback: a Server with NO read pool and NO snapshot (every pre-existing
// test's shape) still computes /health inline — the refactor must not
// change servers that were never given a pool.
func TestBootStarvation_WithoutReadPool_HealthSnapshotAbsent(t *testing.T) {
	writeDB, dbPath := seedBootStarvationFixture(t)
	ro, err := database.OpenReadPool(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	loop := scheduler.NewLoop(writeDB, time.Minute, time.Hour, 10, 0, 5)
	srv := NewServer(writeDB, loop) // read pool NEVER installed
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/health = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if body["active_ticks"] != float64(1) {
		t.Fatalf("active_ticks = %v, want 1 (inline compute on the shared handle)", body["active_ticks"])
	}
}
