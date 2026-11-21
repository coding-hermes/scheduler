package database

// SCHED-GAP-16936 acceptance at the STORAGE layer.
//
// Migration v92 adds the two measurements the scheduler could not express
// before: the dry-tick worktree verdict (ticks.recovery_state, a closed
// vocabulary enforced by a CHECK, plus ticks.recovery_detail carrying the
// evidence) and the capped repair re-dispatch ledger (ticks.repair_attempt,
// ticks.repair_cause). The columns are only real if the database stores AND
// constrains them, and only safe if an existing fleet database migrates in
// place with every pre-v92 value intact.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestSCHEDGAP16936_V92RecoveryAndRepairColumns pins the schema contract:
// defaults, the closed vocabulary, and the non-negative attempt counter.
func TestSCHEDGAP16936_V92RecoveryAndRepairColumns(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 92 {
		t.Fatalf("migration version = %d, want >= 92 (SCHED-GAP-16936)", v)
	}
	if v != latestMigration {
		t.Errorf("migration version = %d, latestMigration = %d — the ladder and its constant disagree", v, latestMigration)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at) VALUES ('sched16936', 'https://github.com/example/x', '/tmp/w', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('sched16936-t1','sched16936','queued','2026-11-20T00:00:00Z')`); err != nil {
		t.Fatalf("enqueue tick: %v", err)
	}

	// Defaults: '' is the honest "not owed" for the verdict, 0 for the counter.
	var state, detail, cause string
	var attempt int
	if err := db.QueryRowContext(ctx,
		`SELECT recovery_state, recovery_detail, repair_attempt, repair_cause FROM ticks WHERE id='sched16936-t1'`,
	).Scan(&state, &detail, &attempt, &cause); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if state != "" || detail != "" || cause != "" || attempt != 0 {
		t.Errorf("v92 defaults = (%q, %q, %d, %q), want (\"\", \"\", 0, \"\")", state, detail, attempt, cause)
	}

	// The vocabulary admits every member, with evidence and a counter.
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET recovery_state='recovery_available', recovery_detail='modified=1 sample=a.go',
		        repair_attempt=2, repair_cause='recovery_available: modified=1'
		 WHERE id='sched16936-t1'`); err != nil {
		t.Fatalf("update to the booked shape: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT recovery_state, repair_attempt FROM ticks WHERE id='sched16936-t1'`,
	).Scan(&state, &attempt); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if state != "recovery_available" || attempt != 2 {
		t.Errorf("round-trip = (%q, %d), want (recovery_available, 2)", state, attempt)
	}
	for _, member := range []string{"no_changes", "recovery_available", "not_probed", ""} {
		if _, err := db.ExecContext(ctx,
			`UPDATE ticks SET recovery_state=? WHERE id='sched16936-t1'`, member); err != nil {
			t.Errorf("recovery_state=%q refused by the CHECK: %v", member, err)
		}
	}

	// Outside the vocabulary is refused: the column cannot become free text.
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET recovery_state='clean' WHERE id='sched16936-t1'`); err == nil {
		t.Error("recovery_state='clean' was accepted — the closed vocabulary is not enforced")
	}
	// A negative attempt count is refused (a counter, not a delta).
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET repair_attempt=-1 WHERE id='sched16936-t1'`); err == nil {
		t.Error("repair_attempt=-1 was accepted — the non-negative CHECK is missing")
	}
}

// schedGap16936V91DB builds a genuine pre-16936 (v91) database: the full ladder
// except v92. The premise ("the columns do not exist yet") is asserted before
// any claim about what v92 changed.
func schedGap16936V91DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v91.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	applyTestDurabilityOff(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version    INTEGER PRIMARY KEY,
    desc       TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}
	for _, m := range migrations {
		if m.version >= 92 {
			break // stop before v92 — the migration under test
		}
		if _, err := db.ExecContext(ctx, m.stmt); err != nil {
			// Mirror Migrate's tolerance: revised CREATE TABLEs replay as
			// "duplicate column name".
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

// TestSCHEDGAP16936_UpgradeFromV91KeepsRows pins the additive contract: an
// existing fleet database migrates in place, every pre-v92 value survives
// byte-for-byte, and the new columns read their honest defaults on old rows.
func TestSCHEDGAP16936_UpgradeFromV91KeepsRows(t *testing.T) {
	db := schedGap16936V91DB(t)
	ctx := context.Background()

	// PREMISE: v91 is where this database stops, and the v92 columns are absent.
	if v, err := MigrationVersion(ctx, db); err != nil || v != 91 {
		t.Fatalf("premise: version = %d, err = %v, want 91", v, err)
	}
	if countColumns(t, db, "ticks", "recovery_state") != 0 {
		t.Fatal("premise broken: ticks.recovery_state exists before v92")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at) VALUES ('legacy', 'https://github.com/example/legacy', '/tmp/w', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at,
                   commits, files_changed, error, reason_code, created_at)
VALUES ('legacy-1','legacy','completed','dry_run','2026-11-01T00:00:00Z','2026-11-01T00:10:00Z',
        0, 0, 'no_changes', 'no_work', '2026-11-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy tick: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to v%d: %v", latestMigration, err)
	}
	if v, err := MigrationVersion(ctx, db); err != nil || v != latestMigration {
		t.Fatalf("post-migrate version = %d, err = %v, want %d", v, err, latestMigration)
	}

	var status, outcome, errText, code, state, detail, cause string
	var attempt int
	if err := db.QueryRowContext(ctx, `
		SELECT status, outcome, COALESCE(error,''), reason_code,
		       recovery_state, recovery_detail, repair_attempt, repair_cause
		FROM ticks WHERE id='legacy-1'`).
		Scan(&status, &outcome, &errText, &code, &state, &detail, &attempt, &cause); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if status != "completed" || outcome != "dry_run" || errText != "no_changes" || code != "no_work" {
		t.Errorf("legacy values changed: status=%q outcome=%q error=%q reason_code=%q",
			status, outcome, errText, code)
	}
	// A legacy row is not retro-annotated: '' is the honest "no verdict owed",
	// never a fabricated no_changes.
	if state != "" || detail != "" || cause != "" || attempt != 0 {
		t.Errorf("legacy v92 defaults = (%q, %q, %d, %q), want empty", state, detail, attempt, cause)
	}
}

// countColumns returns how many of the named columns exist on a table (works on
// both pre- and post-migration shapes).
func countColumns(t *testing.T, db *sql.DB, table string, columns ...string) int {
	t.Helper()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
	args := make([]interface{}, 0, len(columns)+1)
	args = append(args, table)
	for _, c := range columns {
		args = append(args, c)
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name IN (`+placeholders+`)`, args...,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	return n
}
