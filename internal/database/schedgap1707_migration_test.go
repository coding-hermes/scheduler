package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1707 acceptance at the STORAGE layer: the v64 columns must exist
// with the documented defaults, admit the partial-telemetry vocabulary, and
// keep the -1 sentinel for unmeasured wave convergence.
//
// Harness mirrors the heartbeat_at v10 shape: a migrated database (fresh
// InitDB) must carry the columns; an INSERT that names them must round-trip;
// the defaults must keep a legacy-shaped row honest (partial=0, '' reason,
// silence 0, workers_terminal -1).

// TestMigrate_TelemetryPartialColumns pins migration v64: the four timeout-
// visibility columns exist on ticks with the documented column defaults.
func TestMigrate_TelemetryPartialColumns(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('ticks')
		 WHERE name IN ('telemetry_partial','telemetry_partial_reason','session_silence_s','workers_terminal')`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(ticks): %v", err)
	}
	if n != 4 {
		t.Errorf("ticks timeout-visibility columns = %d, want 4 — migration v64 not applied", n)
	}

	// Defaults on a legacy-shaped insert: not partial, no reason, no
	// silence, unmeasured convergence (-1, never a fabricated 0).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		 VALUES ('sgap1707-mig', 'https://example.com/sgap1707', '/tmp/sgap1707', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		 VALUES ('sgap1707-t1', 'sgap1707-mig', 'running', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert tick: %v", err)
	}
	var partial int
	var reason string
	var silence int64
	var workers int
	if err := db.QueryRow(
		`SELECT telemetry_partial, telemetry_partial_reason, session_silence_s, workers_terminal
		 FROM ticks WHERE id = 'sgap1707-t1'`,
	).Scan(&partial, &reason, &silence, &workers); err != nil {
		t.Fatalf("read v64 defaults: %v", err)
	}
	if partial != 0 || reason != "" || silence != 0 {
		t.Errorf("v64 defaults = (%d, %q, %d), want (0, \"\", 0) — defaults drifted", partial, reason, silence)
	}
	if workers != -1 {
		t.Errorf("workers_terminal default = %d, want -1 (unmeasured sentinel)", workers)
	}

	// The vocabulary admits a watchdog-shaped row: partial + named reason
	// + silence + measured convergence.
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status='timeout', telemetry_partial=1,
		        telemetry_partial_reason='session_silent', session_silence_s=5400,
		        workers_terminal=2
		 WHERE id = 'sgap1707-t1'`,
	); err != nil {
		t.Fatalf("update to watchdog shape: %v", err)
	}
	if err := db.QueryRow(
		`SELECT telemetry_partial, telemetry_partial_reason, session_silence_s, workers_terminal
		 FROM ticks WHERE id = 'sgap1707-t1'`,
	).Scan(&partial, &reason, &silence, &workers); err != nil {
		t.Fatalf("re-read watchdog shape: %v", err)
	}
	if partial != 1 || reason != "session_silent" || silence != 5400 || workers != 2 {
		t.Errorf("watchdog shape round-trip = (%d, %q, %d, %d), want (1, session_silent, 5400, 2)",
			partial, reason, silence, workers)
	}
}

// TestSCHEDGAP1707_MigrationVersionPinned keeps v64 present in the ladder for this
// row's build (the generic version test already reads latestMigration; this
// arm names the number so a silent renumber shows up here first).
func TestSCHEDGAP1707_MigrationVersionPinned(t *testing.T) {
	db := newTestDB(t)
	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 64 {
		t.Errorf("migration version = %d, want at least 64 (SCHED-GAP-1707's v64 must be applied)", v)
	}
}

// TestSCHEDGAP1707_UpgradeKeepsRows pins the additive-migration contract:
// an existing fleet database migrates in place and every pre-v64 value on a
// terminal tick survives byte-for-byte (the harness mirrors schedgap203b's
// stripped-rebuild shape, but v64 is plain ALTERs, so the strip only stops
// the ladder before v64).
func TestSCHEDGAP1707_UpgradeKeepsRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v63.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	applyTestDurabilityOff(t, db)
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
		if m.version >= 64 {
			break // stop before v64 — the migration under test
		}
		if _, err := db.ExecContext(ctx, m.stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				t.Fatalf("apply v%d: %v", m.version, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record v%d: %v", m.version, err)
		}
	}

	// Seed a v63-era terminal tick with real telemetry.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		 VALUES ('alpha', 'https://example.com/alpha', '/tmp/alpha', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, session_id, status, outcome, spawned_at, completed_at,
		    exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, cost_source, created_at)
		 VALUES ('alpha-t1', 'alpha', 'sess-1', 'completed', 'committed', '2026-10-01T00:01:00Z', '2026-10-01T01:01:00Z',
		    0, 3, 7, 1000, 200, 0.42, 'measured', '2026-10-01T00:01:00Z')`); err != nil {
		t.Fatalf("seed v63 tick: %v", err)
	}

	// Apply the FULL ladder (v64 included).
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to v64: %v", err)
	}

	// Every old value survives; the new columns read their honest defaults.
	var tin, tout int64
	var cost float64
	var src string
	var partial int
	var reason string
	var silence int64
	var workers int
	if err := db.QueryRow(
		`SELECT tokens_in, tokens_out, cost_usd, cost_source,
		        telemetry_partial, telemetry_partial_reason, session_silence_s, workers_terminal
		 FROM ticks WHERE id = 'alpha-t1'`,
	).Scan(&tin, &tout, &cost, &src, &partial, &reason, &silence, &workers); err != nil {
		t.Fatalf("read upgraded tick: %v", err)
	}
	if tin != 1000 || tout != 200 || cost != 0.42 || src != "measured" {
		t.Errorf("pre-v64 telemetry shifted: tokens=%d/%d cost=%v source=%s, want 1000/200/0.42/measured", tin, tout, cost, src)
	}
	if partial != 0 || reason != "" || silence != 0 || workers != -1 {
		t.Errorf("upgraded row not honest-default: (%d,%q,%d,%d), want (0,\"\",0,-1)", partial, reason, silence, workers)
	}
}
