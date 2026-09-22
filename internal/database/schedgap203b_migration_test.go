package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-203-B acceptance at the STORAGE layer.
//
// The deferred tick status is only real if the database will store it. The
// ticks status/outcome vocabularies live in CHECK constraints
// (status IN ('queued','running','completed','failed','timeout'), outcome IN
// ('committed','dry_run','failed','timeout')), and SQLite cannot modify a CHECK
// in place — so admitting 'deferred' required a table REBUILD (migration v37).
// A worker can verify the scheduler half of SCHED-GAP-203 and still ship a
// fleet where every deferral write fails with "CHECK constraint failed", which
// is exactly the shape this file pins down.
//
// The upgrade test deliberately rebuilds a PRE-203 database from the current
// source (migration 1's statement with the new token stripped) rather than
// hand-copying an old schema: a hand-copy drifts silently, while this version
// asserts its own premise (the old DDL must REJECT 'deferred') before claiming
// the migration is what changed the answer.

// schedGap203bV36DB builds a genuine pre-203 (v36) database: the full ladder
// except v37, with migration 1's ticks CHECKs restored to their pre-203
// vocabulary. The caller gets a *sql.DB plus a cleanup.
func schedGap203bV36DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v36.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	// The migrations bookkeeping table is created by Migrate itself (not by
	// migration 1), so the harness mirrors that bootstrap here.
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version   INTEGER PRIMARY KEY,
    desc      TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}

	for _, m := range migrations {
		if m.version >= 37 {
			break // stop before v37 — the migration under test
		}
		stmt := m.stmt
		if m.version == 1 {
			// Reconstruct the pre-203 CHECK vocabularies from the current
			// source: drop the token the rebuild exists to add.
			stmt = strings.ReplaceAll(stmt, ",'deferred'", "")
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Mirror Migrate's own tolerance: migration 1's CREATE TABLE is
			// periodically revised to carry columns that later migrations also
			// add, so a replay hits "duplicate column name" — the runner treats
			// that as success, and this harness must agree or it would be
			// reconstructing a schema the real runner never produces.
			if !strings.Contains(err.Error(), "duplicate column name") {
				t.Fatalf("apply v%d (%s): %v", m.version, m.desc, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record v%d: %v", m.version, err)
		}
	}
	return db
}

// TestSCHEDGAP203B_UpgradeV36_AdmitsDeferredWithoutLosingRows is the load-bearing
// test of the rebuild: an EXISTING fleet database (73k+ ticks rows in
// production, with tick_workers children that cascade on parent delete) must
// migrate in place — gaining the 'deferred' vocabulary, losing no tick row, no
// tick_workers attribution row, and no column value.
func TestSCHEDGAP203B_UpgradeV36_AdmitsDeferredWithoutLosingRows(t *testing.T) {
	db := schedGap203bV36DB(t)
	ctx := context.Background()

	// --- seed a fleet-shaped database -------------------------------------
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		 VALUES ('alpha', 'https://example.com/alpha', '/tmp/alpha', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Two ticks: one carrying every non-default column, so a rebuild that lost
	// or shifted a column cannot hide behind zero/NULL values.
	var cols, marks []string
	allCols := []string{
		"id", "project_name", "session_id", "pid", "status", "outcome", "spawned_at",
		"completed_at", "exit_code", "commits", "files_changed", "tokens_in", "tokens_out",
		"cost_usd", "urgency", "weight_used", "error", "created_at", "heartbeat_at",
		"orphaned_at", "orphan_reason", "nudge_count", "code_commits", "board_commits",
		"bump", "worker_count", "wave_recovery", "gateway_trace", "cost_source",
		"failure_reason", "slot_wait_ms", "admit_reason", "nudge_source",
	}
	cols = make([]string, 0, len(allCols))
	marks = make([]string, 0, len(allCols))
	for _, c := range allCols {
		cols = append(cols, c)
		marks = append(marks, "?")
	}
	seedTick := func(args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ticks (`+strings.Join(cols, ",")+`) VALUES (`+strings.Join(marks, ",")+`)`,
			args...); err != nil {
			t.Fatalf("seed tick: %v", err)
		}
	}
	seedTick(
		"alpha-2026-09-20-00-00-01", "alpha", "sess-1", 4321, "failed", "failed",
		"2026-09-20T00:00:01Z", "2026-09-20T01:00:01Z", 1, 3, 7, 1000, 200,
		0.42, 1.5, 10, "gateway unreachable and exec fallback disabled: gateway transient error: sse stream ended without a terminal event",
		"2026-09-20T00:00:00Z", "2026-09-20T00:30:00Z", "2026-09-20T01:00:02Z", "startup_reap",
		2, 3, 1, 1, 4, 1, `{"mode":"idle"}`, "gateway", "gateway_transport", 1234, "ok", "board_wake",
	)
	seedTick(
		"alpha-2026-09-20-00-00-02", "alpha", nil, 0, "completed", "committed",
		"2026-09-20T02:00:01Z", "2026-09-20T02:30:01Z", 0, 0, 0, 0, 0, 0.0, 0.0, 0,
		nil, "2026-09-20T02:00:00Z", nil, nil, nil, 0, 0, 0, 0, 0, 0, "", "measured", "", 0, "", "",
	)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tick_workers (tick_id, task_id, branch) VALUES ('alpha-2026-09-20-00-00-01', 'AUG-1', 'wt/aug-1')`); err != nil {
		t.Fatalf("seed tick_worker: %v", err)
	}

	// --- premise: the pre-203 schema really rejects the new vocabulary ----
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'deferred' WHERE id = 'alpha-2026-09-20-00-00-02'`); err == nil {
		t.Fatal("premise: the pre-203 ticks.status CHECK accepted 'deferred' — this test is not exercising the v37 rebuild")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET outcome = 'deferred' WHERE id = 'alpha-2026-09-20-00-00-02'`); err == nil {
		t.Fatal("premise: the pre-203 ticks.outcome CHECK accepted 'deferred'")
	}

	// Snapshot the full column image so the comparison below is exhaustive.
	type rowImage map[string]any
	scanAll := func() map[string]rowImage {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(allCols, ",")+` FROM ticks ORDER BY id`)
		if err != nil {
			t.Fatalf("scan ticks: %v", err)
		}
		defer rows.Close()
		out := map[string]rowImage{}
		for rows.Next() {
			vals := make([]any, len(allCols))
			ptrs := make([]any, len(allCols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan tick row: %v", err)
			}
			img := rowImage{}
			id := ""
			for i, c := range allCols {
				img[c] = normaliseSQLValue(vals[i])
				if c == "id" {
					id, _ = vals[i].(string)
				}
			}
			out[id] = img
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate ticks: %v", err)
		}
		return out
	}
	before := scanAll()

	// --- migrate -----------------------------------------------------------
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to v%d: %v", latestMigration, err)
	}

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Errorf("migration version = %d, want %d", v, latestMigration)
	}
	var recorded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM migrations WHERE version = 37`).Scan(&recorded); err != nil {
		t.Fatalf("migrations row for v37: %v", err)
	}
	if recorded != 1 {
		t.Errorf("migrations rows for version 37 = %d, want 1", recorded)
	}

	// --- the new vocabulary is admitted -----------------------------------
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'deferred', outcome = 'deferred' WHERE id = 'alpha-2026-09-20-00-00-02'`); err != nil {
		t.Fatalf("post-migration UPDATE to status=deferred/outcome=deferred failed: %v — the rebuild did not widen the CHECK vocabularies", err)
	}
	var gotStatus, gotOutcome string
	if err := db.QueryRowContext(ctx,
		`SELECT status, outcome FROM ticks WHERE id = 'alpha-2026-09-20-00-00-02'`).
		Scan(&gotStatus, &gotOutcome); err != nil {
		t.Fatalf("read back deferred row: %v", err)
	}
	if gotStatus != "deferred" || gotOutcome != "deferred" {
		t.Errorf("row after deferred write = (%q,%q), want (deferred,deferred)", gotStatus, gotOutcome)
	}

	// --- and the old rejections still hold --------------------------------
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'bogus' WHERE id = 'alpha-2026-09-20-00-00-02'`); err == nil {
		t.Error("the rebuilt ticks.status CHECK accepted 'bogus' — the vocabulary was widened into a free-for-all")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET outcome = 'bogus' WHERE id = 'alpha-2026-09-20-00-00-02'`); err == nil {
		t.Error("the rebuilt ticks.outcome CHECK accepted 'bogus'")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'deferred' WHERE id = 'alpha-2026-09-20-00-00-02'`); err != nil {
		t.Fatalf("re-defer after the bogus probe failed: %v", err)
	}

	// --- no data lost, no column shifted ----------------------------------
	after := scanAll()
	if len(after) != len(before) {
		t.Fatalf("tick rows after migration = %d, want %d", len(after), len(before))
	}
	for id, bimg := range before {
		aimg, ok := after[id]
		if !ok {
			t.Errorf("tick %s disappeared in the rebuild", id)
			continue
		}
		for _, c := range allCols {
			if c == "status" || c == "outcome" {
				continue // asserted above (the deliberate deferred write)
			}
			if bimg[c] != aimg[c] {
				t.Errorf("tick %s column %s = %v after migration, want %v (rebuild lost or shifted data)", id, c, aimg[c], bimg[c])
			}
		}
	}

	// --- tick_workers survived the parent DROP ----------------------------
	var workers int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tick_workers`).Scan(&workers); err != nil {
		t.Fatalf("count tick_workers: %v", err)
	}
	if workers != 1 {
		t.Errorf("tick_workers rows after the rebuild = %d, want 1 — the parent DROP fired ON DELETE CASCADE, which is why the migration disables foreign_keys", workers)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tick_workers (tick_id, task_id, branch) VALUES ('alpha-2026-09-20-00-00-99', 'AUG-2', 'wt/aug-2')`); err == nil {
		t.Error("the rebuilt ticks table no longer enforces the tick_workers FK (an orphan tick_worker was accepted)")
	}

	// --- indexes and enforcement restored ---------------------------------
	for _, idx := range []string{"idx_ticks_project_spawned", "idx_ticks_status", "idx_ticks_status_completed"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name = ?`, idx).Scan(&n); err != nil {
			t.Fatalf("index lookup %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after the rebuild", idx)
		}
	}
	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d after the migration, want 1 — the rebuild must hand the connection back with enforcement ON", fk)
	}
	var violations int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Errorf("foreign_key_check reported %d violations after the rebuild", violations)
	}
}

// TestSCHEDGAP203B_FreshDBAdmitsDeferred pins the fresh-install half: a brand
// new database must accept 'deferred' on BOTH columns without ever running the
// rebuild (migration 1 carries the current vocabulary), and must still reject
// values outside it.
func TestSCHEDGAP203B_FreshDBAdmitsDeferred(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("fresh")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("fresh")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'deferred', outcome = 'deferred' WHERE id = ?`, tk.ID); err != nil {
		t.Fatalf("fresh DB rejected status=deferred/outcome=deferred: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'bogus' WHERE id = ?`, tk.ID); err == nil {
		t.Error("fresh DB accepted status='bogus'")
	}
}

// normaliseSQLValue makes driver value types comparable across two reads of the
// same row ([]byte → string) so the exhaustive column comparison above cannot
// report a false difference.
func normaliseSQLValue(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
