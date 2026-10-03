package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1674 acceptance at the STORAGE layer.
//
// The builder no-artifact guard's abort is only real if the database stores
// it: outcome='aborted:no_artifact' must be admitted by the CHECK vocabulary,
// the guard's per-namespace config columns must round-trip, and the existing
// vocabularies must keep rejecting everything outside them. v50 rebuilds the
// ticks table to widen the outcome CHECK (SQLite cannot modify a CHECK in
// place — v37's procedure, carried forward); v51 adds the namespace guard
// columns; v52 adds ticks.guard_nudged_at.
//
// The upgrade test mirrors SCHED-GAP-203B's harness: it rebuilds a PRE-1674
// database from current source (stripping the new tokens) rather than
// hand-copying an old schema, and asserts its own premise before claiming
// the migration changed the answer.

// schedGap1674V49DB builds a genuine pre-1674 (v49) database: the full ladder
// except v50+, with migration 1's outcome CHECK restored to its pre-1674
// vocabulary.
func schedGap1674V49DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v49.db")
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
		if m.version >= 50 {
			break // stop before v50 — the migration under test
		}
		stmt := m.stmt
		if m.version == 1 || m.version == 37 {
			// Reconstruct the pre-1674 outcome CHECK from current source.
			// BOTH migration 1 (fresh installs) and migration 37's rebuild
			// statement (the v203b table rebuild) carry the CHECK — the
			// strip must hit both, or the rebuild silently re-adds the
			// token and the premise arm below is vacuous (measured: the
			// one-strip harness built a v49 database that already admitted
			// the guard verdict).
			stmt = strings.ReplaceAll(stmt, ",'aborted:no_artifact'", "")
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Mirror Migrate's duplicate-column tolerance (see the v37
			// harness note: replaying migration 1 can legitimately hit it).
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

// TestSCHEDGAP1674_UpgradeV49_AdmitsAbortOutcomeWithoutLosingRows pins the
// v50 rebuild: an existing fleet database migrates in place, the guard
// outcome is admitted, every old value survives byte-for-byte, and the old
// rejections still hold.
func TestSCHEDGAP1674_UpgradeV49_AdmitsAbortOutcomeWithoutLosingRows(t *testing.T) {
	db := schedGap1674V49DB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		 VALUES ('alpha', 'https://example.com/alpha', '/tmp/alpha', '2026-09-29T00:00:00Z', '2026-09-29T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Every pre-1674 column carried, so a rebuild that shifted a column
	// cannot hide behind zero/NULL values. Column list = v37's rebuild list
	// + the two v48 dispatch columns.
	allCols := []string{
		"id", "project_name", "session_id", "pid", "status", "outcome", "spawned_at",
		"completed_at", "exit_code", "commits", "files_changed", "tokens_in", "tokens_out",
		"cost_usd", "urgency", "weight_used", "error", "created_at", "heartbeat_at",
		"orphaned_at", "orphan_reason", "nudge_count", "code_commits", "board_commits",
		"bump", "worker_count", "wave_recovery", "gateway_trace", "cost_source",
		"failure_reason", "slot_wait_ms", "admit_reason", "nudge_source",
		"dispatch_outcome", "dispatch_reason",
	}
	cols, marks := make([]string, 0, len(allCols)), make([]string, 0, len(allCols))
	for _, c := range allCols {
		cols = append(cols, c)
		marks = append(marks, "?")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (`+strings.Join(cols, ",")+`) VALUES (`+strings.Join(marks, ",")+")",
		"alpha-2026-09-29-00-00-01", "alpha", "sess-1", 0, "failed", "failed",
		"2026-09-29T00:00:01Z", "2026-09-29T00:30:01Z", -1, 3, 7, 1000, 200,
		0.42, 1.5, 10, "gateway unreachable: refused",
		"2026-09-29T00:00:00Z", "2026-09-29T00:10:00Z", nil, nil,
		0, 3, 1, 1, 0, 0, `{"mode":"idle"}`, "gateway", "gateway_transport", 1234, "ok", "startup",
		"yes", "dispatched",
	); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	// Premise: the pre-1674 schema really rejects the new outcome token.
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET outcome = 'aborted:no_artifact' WHERE id = 'alpha-2026-09-29-00-00-01'`); err == nil {
		t.Fatal("premise: the pre-1674 ticks.outcome CHECK accepted 'aborted:no_artifact' — this test is not exercising the v50 rebuild")
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to v%d: %v", latestMigration, err)
	}
	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}

	// The guard verdict is admitted — both columns, the shape the guard
	// writes (status=failed + outcome=aborted:no_artifact).
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'failed', outcome = 'aborted:no_artifact' WHERE id = 'alpha-2026-09-29-00-00-01'`); err != nil {
		t.Fatalf("post-migration guard-outcome write failed: %v — the rebuild did not widen the CHECK vocabulary", err)
	}
	var gotStatus, gotOutcome string
	if err := db.QueryRowContext(ctx,
		`SELECT status, outcome FROM ticks WHERE id = 'alpha-2026-09-29-00-00-01'`).
		Scan(&gotStatus, &gotOutcome); err != nil {
		t.Fatalf("read back guard-aborted row: %v", err)
	}
	if gotStatus != "failed" || gotOutcome != "aborted:no_artifact" {
		t.Errorf("row after guard write = (%q,%q), want (failed,aborted:no_artifact)", gotStatus, gotOutcome)
	}

	// The old rejections still hold — the vocabulary widened by ONE token.
	for _, bogus := range []string{"aborted", "no_artifact", "bogus"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE ticks SET outcome = ? WHERE id = 'alpha-2026-09-29-00-00-01'`, bogus); err == nil {
			t.Errorf("the rebuilt ticks.outcome CHECK accepted %q — the vocabulary widened into a free-for-all", bogus)
		}
	}
	// And the data survived the rebuild: the seed row's other columns read
	// back unchanged after the deliberate guard write above.
	var commits int
	var dispatch string
	if err := db.QueryRowContext(ctx,
		`SELECT commits, dispatch_outcome FROM ticks WHERE id = 'alpha-2026-09-29-00-00-01'`).
		Scan(&commits, &dispatch); err != nil {
		t.Fatalf("read back columns: %v", err)
	}
	if commits != 3 || dispatch != "yes" {
		t.Errorf("rebuilt row lost column data: commits=%d dispatch_outcome=%q, want 3/yes", commits, dispatch)
	}

	// tick_workers survived the parent DROP (the v37 cascade hazard).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tick_workers (tick_id, task_id, branch) VALUES ('alpha-2026-09-29-00-00-01', 'SCHED-GAP-1674', 'wt/sched-gap-1674')`); err != nil {
		t.Fatalf("seed tick_worker after rebuild: %v", err)
	}
	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d after the migration, want 1", fk)
	}
	var violations int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Errorf("foreign_key_check reported %d violations after the rebuild", violations)
	}
	// The indexes the rebuild must restore.
	for _, idx := range []string{"idx_ticks_project_spawned", "idx_ticks_status", "idx_ticks_status_completed", "idx_ticks_project_spawned_cost", "idx_ticks_status_running"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name = ?`, idx).Scan(&n); err != nil {
			t.Fatalf("index lookup %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s missing after the rebuild", idx)
		}
	}
}

// TestSCHEDGAP1674_FreshDBAdmitsAbortOutcome pins the fresh-install half: a
// brand-new database accepts the guard outcome without ever running the
// rebuild (migration 1 carries the current vocabulary) and still rejects
// values outside it.
func TestSCHEDGAP1674_FreshDBAdmitsAbortOutcome(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("fresh1674")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("fresh1674")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET status = 'failed', outcome = 'aborted:no_artifact' WHERE id = ?`, tk.ID); err != nil {
		t.Fatalf("fresh DB rejected status=failed/outcome=aborted:no_artifact: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET outcome = 'bogus' WHERE id = ?`, tk.ID); err == nil {
		t.Error("fresh DB accepted outcome='bogus'")
	}
}

// TestSCHEDGAP1674_NamespaceGuardConfigRoundTrip pins acceptance 4's storage
// half: the namespace guard knobs round-trip through Create/Get/Update and
// the writer validates its vocabulary.
func TestSCHEDGAP1674_NamespaceGuardConfigRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	reporter := "reporter"
	window := "45m"
	floor := "40"
	if err := CreateNamespace(ctx, db, &Namespace{
		ID: "satellite-family", Weight: 10,
		ReporterClass:        reporter,
		NoArtifactWindow:     window,
		NoArtifactReconFloor: floor,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns, err := GetNamespace(ctx, db, "satellite-family")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.ReporterClass != "reporter" || ns.NoArtifactWindow != "45m" || ns.NoArtifactReconFloor != "40" {
		t.Errorf("create round-trip = (%q,%q,%q), want (reporter,45m,40)",
			ns.ReporterClass, ns.NoArtifactWindow, ns.NoArtifactReconFloor)
	}

	// Update path: clear the exemption, change the knobs.
	empty := ""
	if err := UpdateNamespace(ctx, db, "satellite-family", NamespacePatch{
		ReporterClass: &empty,
	}); err != nil {
		t.Fatalf("UpdateNamespace clear reporter_class: %v", err)
	}
	ns, _ = GetNamespace(ctx, db, "satellite-family")
	if ns.ReporterClass != "" {
		t.Errorf("reporter_class after clear = %q, want empty", ns.ReporterClass)
	}

	// Invalid values are rejected, never stored.
	bad := "yes-please"
	if err := UpdateNamespace(ctx, db, "satellite-family", NamespacePatch{ReporterClass: &bad}); err == nil {
		t.Error("UpdateNamespace accepted reporter_class=\"yes-please\"")
	}
	badWindow := "twenty minutes"
	if err := UpdateNamespace(ctx, db, "satellite-family", NamespacePatch{NoArtifactWindow: &badWindow}); err == nil {
		t.Error("UpdateNamespace accepted unparseable no_artifact_window")
	}
	negFloor := "0"
	if err := UpdateNamespace(ctx, db, "satellite-family", NamespacePatch{NoArtifactReconFloor: &negFloor}); err == nil {
		t.Error("UpdateNamespace accepted no_artifact_recon_floor=0")
	}
}
