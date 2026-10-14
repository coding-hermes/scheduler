package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1655 acceptance at the STORAGE layer.
//
// The no_work outcome is only real if the database stores it. The outcome
// vocabulary lives in a CHECK constraint (SQLite cannot alter one in place),
// so migration v55 rebuilds the ticks table with the widened CHECK — v50's
// procedure verbatim (own-transaction, foreign_keys toggled, full copy,
// rename, index restoration). Mirrors the SCHED-GAP-203B test's premise
// discipline: this file asserts its own premise (the PRE-v55 schema must
// REJECT 'no_work') before claiming the migration changed the answer.

// schedGap1655V54DB builds a genuine pre-1655 (v54) database: the full
// ladder except v55. v50 already rebuilt ticks, so the source of truth for
// the pre-55 outcome CHECK is migration 50's statement with the new token
// stripped — reconstructed from current source, never hand-copied.
func schedGap1655V54DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v54.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	applyTestDurabilityOff(t, db) // QA-CHS-182: test-only fsync bypass
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version   INTEGER PRIMARY KEY,
    desc      TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}

	for _, m := range migrations {
		if m.version >= 55 {
			break // stop before v55 — the migration under test
		}
		stmt := m.stmt
		if m.version == 50 {
			// Reconstruct the pre-1655 outcome CHECK from current
			// source: drop the token the rebuild exists to add.
			stmt = strings.ReplaceAll(stmt, ",'no_work'", "")
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Mirror Migrate's tolerance (see schedgap203b): revised
			// CREATE TABLEs replay as "duplicate column name".
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

// TestSCHEDGAP1655_V54SchemaRejectsNoWork asserts the premise: the pre-55
// outcome CHECK must REJECT 'no_work', or the migration proves nothing.
func TestSCHEDGAP1655_V54SchemaRejectsNoWork(t *testing.T) {
	db := schedGap1655V54DB(t)
	ctx := context.Background()
	// One project to satisfy the FK.
	if _, err := db.ExecContext(ctx, `INSERT INTO projects (name, repo_url, workdir, enabled, created_at, updated_at)
		VALUES ('p', 'https://example.com/p', '/tmp/p', 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, outcome, created_at)
		VALUES ('t1', 'p', 'completed', 'no_work', datetime('now'))`)
	if err == nil {
		t.Fatal("pre-v55 schema ACCEPTED outcome='no_work' — the premise is broken (nothing for v55 to change)")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("expected a CHECK violation, got: %v", err)
	}
}

// TestSCHEDGAP1655_UpgradeV54_AdmitsNoWorkWithoutLosingRows is the
// load-bearing acceptance: migrate the v54 database to HEAD and prove
// (a) existing outcome rows survive byte-identically, and (b) a tick with
// outcome='no_work' now stores without a CHECK violation.
func TestSCHEDGAP1655_UpgradeV54_AdmitsNoWorkWithoutLosingRows(t *testing.T) {
	db := schedGap1655V54DB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO projects (name, repo_url, workdir, enabled, created_at, updated_at)
		VALUES ('p', 'https://example.com/p', '/tmp/p', 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Legacy rows across the whole pre-existing outcome vocabulary.
	for i, outcome := range []string{"committed", "dry_run", "failed", "timeout", "deferred", "aborted:no_artifact"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, outcome, created_at)
			VALUES (?, 'p', 'completed', ?, datetime('now'))`, "legacy-"+outcome, outcome); err != nil {
			t.Fatalf("seed legacy tick %s: %v", outcome, err)
		}
		_ = i
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate v54 -> v55: %v", err)
	}

	// (a) every legacy row survived with its outcome intact.
	rows, err := db.QueryContext(ctx, `SELECT id, outcome FROM ticks WHERE id LIKE 'legacy-%' ORDER BY id`)
	if err != nil {
		t.Fatalf("read legacy rows post-migration: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, outcome string
		if err := rows.Scan(&id, &outcome); err != nil {
			t.Fatalf("scan legacy row: %v", err)
		}
		got[id] = outcome
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate legacy rows: %v", err)
	}
	for _, outcome := range []string{"committed", "dry_run", "failed", "timeout", "deferred", "aborted:no_artifact"} {
		if got["legacy-"+outcome] != outcome {
			t.Errorf("row legacy-%s outcome = %q, want %q (the rebuild lost or mutated a row)", outcome, got["legacy-"+outcome], outcome)
		}
	}
	if len(got) != 6 {
		t.Errorf("legacy rows post-migration = %d, want 6", len(got))
	}

	// (b) the widened CHECK admits no_work.
	if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, outcome, created_at)
		VALUES ('new-work', 'p', 'completed', 'no_work', datetime('now'))`); err != nil {
		t.Fatalf("post-v55 INSERT with outcome='no_work' failed: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE outcome = 'no_work'`).Scan(&n); err != nil {
		t.Fatalf("count no_work rows: %v", err)
	}
	if n != 1 {
		t.Errorf("no_work rows = %d, want 1", n)
	}
}

// TestSCHEDGAP1655_CountProjectOutcomes pins deliverable 4's aggregate:
// the split zero-fills the full vocabulary, NoWork counts the zero-tool
// verdict, and outcome-less terminal rows land in Unset (unmeasured),
// never in a bucket.
func TestSCHEDGAP1655_CountProjectOutcomes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("split-p")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	type seed struct {
		id      string
		status  string
		outcome any // string or nil
	}
	for _, s := range []seed{
		{"t1", "completed", "no_work"},
		{"t2", "completed", "no_work"},
		{"t3", "completed", "committed"},
		{"t4", "failed", "failed"},
		{"t5", "timeout", nil},
	} {
		if s.outcome == nil {
			if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, created_at)
				VALUES (?, 'split-p', ?, datetime('now'))`, s.id, s.status); err != nil {
				t.Fatalf("seed %s: %v", s.id, err)
			}
			continue
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, outcome, created_at)
			VALUES (?, 'split-p', ?, ?, datetime('now'))`, s.id, s.status, s.outcome); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	split, err := CountProjectOutcomes(ctx, db, "split-p")
	if err != nil {
		t.Fatalf("CountProjectOutcomes: %v", err)
	}
	if split.NoWork != 2 {
		t.Errorf("NoWork = %d, want 2", split.NoWork)
	}
	if split.Total != 4 {
		t.Errorf("Total = %d, want 4 (outcome-bearing terminal ticks)", split.Total)
	}
	if split.Unset != 1 {
		t.Errorf("Unset = %d, want 1 (the outcome-less terminal row)", split.Unset)
	}
	if split.Buckets["no_work"] != 2 || split.Buckets["committed"] != 1 || split.Buckets["failed"] != 1 {
		t.Errorf("buckets = %v, want no_work=2 committed=1 failed=1", split.Buckets)
	}
	for _, o := range ProjectOutcomeVocabulary {
		if _, ok := split.Buckets[o]; !ok {
			t.Errorf("bucket %q missing — the vocabulary must be zero-filled, not sparse", o)
		}
	}
}
