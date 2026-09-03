package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// GAP-044 disable-provenance tests. Disabled projects previously had no
// record of how/when/why they were disabled (ch-delta: enabled=false,
// no reason, no event). Every disable path must now stamp
// disabled_at/disabled_by/disabled_reason and the API must surface them.

func TestUpdateProject_DisableStampsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("proj-a")
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := UpdateProject(ctx, db, "proj-a", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable project: %v", err)
	}

	got, err := GetProject(ctx, db, "proj-a")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Enabled {
		t.Fatal("project still enabled")
	}
	if got.DisabledBy != "api" {
		t.Fatalf("DisabledBy = %q, want \"api\"", got.DisabledBy)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty — must be stamped on disable")
	}
	if !strings.Contains(got.DisabledReason, "API") {
		t.Fatalf("DisabledReason = %q, want default API reason", got.DisabledReason)
	}
}

func TestUpdateProject_DisableHonorsExplicitProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-b")); err != nil {
		t.Fatalf("create project: %v", err)
	}

	by := "api-pause"
	reason := "paused via POST /pause"
	if err := UpdateProject(ctx, db, "proj-b", ProjectUpdates{
		Enabled:        BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		t.Fatalf("pause project: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-b")
	if got.DisabledBy != "api-pause" {
		t.Fatalf("DisabledBy = %q, want \"api-pause\"", got.DisabledBy)
	}
	if got.DisabledReason != "paused via POST /pause" {
		t.Fatalf("DisabledReason = %q, want explicit reason", got.DisabledReason)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty")
	}
}

func TestUpdateProject_ResumeClearsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-c")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := UpdateProject(ctx, db, "proj-c", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if err := UpdateProject(ctx, db, "proj-c", ProjectUpdates{Enabled: BoolPtr(true)}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-c")
	if !got.Enabled {
		t.Fatal("project not re-enabled")
	}
	if got.DisabledBy != "" || got.DisabledAt != "" || got.DisabledReason != "" {
		t.Fatalf("provenance not cleared on resume: by=%q at=%q reason=%q",
			got.DisabledBy, got.DisabledAt, got.DisabledReason)
	}
}

func TestUpdateProject_NonEnabledUpdateLeavesProvenanceUntouched(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-d")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := UpdateProject(ctx, db, "proj-d", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before, _ := GetProject(ctx, db, "proj-d")

	cd := 7200
	if err := UpdateProject(ctx, db, "proj-d", ProjectUpdates{CooldownS: &cd}); err != nil {
		t.Fatalf("cooldown update: %v", err)
	}

	after, _ := GetProject(ctx, db, "proj-d")
	if after.DisabledBy != before.DisabledBy || after.DisabledAt != before.DisabledAt || after.DisabledReason != before.DisabledReason {
		t.Fatalf("non-enabled update changed provenance: %+v -> %+v", before, after)
	}
}

func TestDeleteProject_StampsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-e")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// The API only allows DELETE on already-disabled projects (409
	// otherwise) — simulate the pre-disabled legacy state, including a
	// row disabled BEFORE provenance columns existed (NULLs).
	if _, err := db.ExecContext(ctx, `UPDATE projects SET enabled = 0 WHERE name = 'proj-e'`); err != nil {
		t.Fatalf("legacy disable: %v", err)
	}

	if err := DeleteProject(ctx, db, "proj-e"); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-e")
	if got.Enabled {
		t.Fatal("project still enabled after delete")
	}
	if got.DisabledBy != "api-delete" {
		t.Fatalf("DisabledBy = %q, want \"api-delete\" (legacy backfill)", got.DisabledBy)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty — legacy backfill must stamp it")
	}
	if !strings.Contains(got.DisabledReason, "DELETE") {
		t.Fatalf("DisabledReason = %q, want api-delete reason", got.DisabledReason)
	}
}

func TestDeleteProject_KeepsExistingProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-f")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	by := "api-pause"
	reason := "paused earlier"
	if err := UpdateProject(ctx, db, "proj-f", ProjectUpdates{
		Enabled:        BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before, _ := GetProject(ctx, db, "proj-f")

	if err := DeleteProject(ctx, db, "proj-f"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, _ := GetProject(ctx, db, "proj-f")
	if after.DisabledBy != "api-pause" || after.DisabledReason != "paused earlier" {
		t.Fatalf("delete overwrote existing provenance: by=%q reason=%q", after.DisabledBy, after.DisabledReason)
	}
	if after.DisabledAt != before.DisabledAt {
		t.Fatal("delete changed existing DisabledAt")
	}
}

func TestProjectJSON_ExposesDisableProvenance(t *testing.T) {
	// The wire format must carry the provenance fields so /api/v1/projects
	// and /api/v1/projects/{name} surface them.
	p := Project{
		Name:           "proj-g",
		Enabled:        false,
		DisabledAt:     "2026-08-13T21:30:00Z",
		DisabledBy:     "auto-disable",
		DisabledReason: "failure rate 95.0% (19/20 ticks)",
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"disabled_at"`, `"disabled_by"`, `"disabled_reason"`} {
		if !strings.Contains(string(out), key) {
			t.Fatalf("JSON missing %s: %s", key, out)
		}
	}
}

func TestMigration12_AddsProvenanceColumns(t *testing.T) {
	db := newTestDB(t) // InitDB runs all migrations incl. v12
	ctx := context.Background()
	cols, err := db.QueryContext(ctx, `PRAGMA table_info(projects)`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer cols.Close()
	seen := map[string]bool{}
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		seen[name] = true
	}
	for _, want := range []string{"disabled_at", "disabled_by", "disabled_reason"} {
		if !seen[want] {
			t.Fatalf("migration v12 missing column %s", want)
		}
	}
}

// applyMigrationsUpTo runs the migrations slice through version upto and
// records them in the migrations table — the pre-upgrade state an existing
// install has before Migrate applies the rest.
func applyMigrationsUpTo(t *testing.T, db *sql.DB, upto int) {
	t.Helper()
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
		if m.version > upto {
			break
		}
		if _, err := db.ExecContext(ctx, m.stmt); err != nil {
			// Mirror Migrate()'s tolerance: SQLite ALTER TABLE ADD COLUMN
			// is not idempotent and the initial CREATE TABLE already
			// carries columns later migrations re-add (worker_model etc.).
			if !strings.Contains(err.Error(), "duplicate column name") {
				t.Fatalf("apply migration %d (%s): %v", m.version, m.desc, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}
}

// TestMigration13_BackfillsLegacyDisableProvenance (DOGFOOD-010) simulates a
// v12 install whose pre-GAP-044 disabled rows carry empty provenance, then
// runs Migrate and verifies the backfill: legacy rows get
// disabled_by='legacy' / disabled_reason='pre-GAP-044 disable' / a
// disabled_at, rows that already have provenance are untouched, and the
// migration is idempotent.
func TestMigration13_BackfillsLegacyDisableProvenance(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Pre-upgrade state: schema at v12.
	applyMigrationsUpTo(t, db, 12)

	// insertV12Project inserts a row using ONLY columns that existed at
	// schema v12 (the era this test simulates). CreateProject now writes the
	// post-v12 adaptive-cooldown columns (migration v22) and would fail
	// against this frozen old table — a mid-upgrade binary never creates
	// projects before Migrate() has caught the schema up, so raw SQL is the
	// faithful way to seed the legacy state.
	insertV12Project := func(name string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO projects
			(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
			 model, provider, worker_model, worker_provider, fallback_model,
			 fallback_provider, no_global_fallback, idle_model, idle_provider,
			 daily_budget_usd, weekly_budget_usd, final_budget_usd,
			 prompt, prompt_mode, command, gateway_key, namespace_id, deliver,
			 enabled, created_at, updated_at,
			 last_tick_started, last_tick_completed, consecutive_failures,
			 disabled_at, disabled_by, disabled_reason)
			VALUES (?, 'https://github.com/example/' || ?, '/tmp/work/' || ?,
			 10, 5, 900, 1.0, 'deepseek-v4-pro', 'deepseek-foreman',
			 '', '', '', '', 0, '', '', 0.0, 0.0, 0.0,
			 '', 'append', '', '', NULL, '',
			 1, datetime('now'), datetime('now'),
			 NULL, NULL, 0, NULL, '', '')`,
			name, name, name)
		if err != nil {
			t.Fatalf("insert legacy v12 project %s: %v", name, err)
		}
	}

	// A legacy disabled row (disabled before GAP-044 — empty provenance).
	insertV12Project("legacy-a")
	if _, err := db.ExecContext(ctx,
		`UPDATE projects SET enabled = 0, disabled_by = '', disabled_reason = '', disabled_at = NULL WHERE name = 'legacy-a'`); err != nil {
		t.Fatalf("legacy disable legacy-a: %v", err)
	}
	// A row that already carries provenance — must NOT be touched.
	insertV12Project("stamped-a")
	if _, err := db.ExecContext(ctx,
		`UPDATE projects SET enabled = 0, disabled_by = 'api-pause', disabled_reason = 'paused earlier', disabled_at = '2026-08-14T00:00:00Z' WHERE name = 'stamped-a'`); err != nil {
		t.Fatalf("disable stamped-a: %v", err)
	}
	// An enabled row — must NOT be touched.
	insertV12Project("enabled-a")

	// Upgrade: Migrate applies v13 (backfill) and any later migrations.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (v13): %v", err)
	}
	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}

	legacy, err := GetProject(ctx, db, "legacy-a")
	if err != nil {
		t.Fatalf("get legacy-a: %v", err)
	}
	if legacy.DisabledBy != "legacy" {
		t.Errorf("legacy-a DisabledBy = %q, want \"legacy\"", legacy.DisabledBy)
	}
	if legacy.DisabledReason != "pre-GAP-044 disable" {
		t.Errorf("legacy-a DisabledReason = %q, want \"pre-GAP-044 disable\"", legacy.DisabledReason)
	}
	if legacy.DisabledAt == "" {
		t.Error("legacy-a DisabledAt empty — backfill must stamp it")
	}

	stamped, err := GetProject(ctx, db, "stamped-a")
	if err != nil {
		t.Fatalf("get stamped-a: %v", err)
	}
	if stamped.DisabledBy != "api-pause" || stamped.DisabledReason != "paused earlier" || stamped.DisabledAt != "2026-08-14T00:00:00Z" {
		t.Errorf("stamped-a provenance overwritten: by=%q reason=%q at=%q",
			stamped.DisabledBy, stamped.DisabledReason, stamped.DisabledAt)
	}

	enabled, err := GetProject(ctx, db, "enabled-a")
	if err != nil {
		t.Fatalf("get enabled-a: %v", err)
	}
	if !enabled.Enabled {
		t.Error("enabled-a was disabled by the backfill")
	}

	// Idempotency: a second Migrate is a no-op (version guard) and the
	// statement itself only matches empty-provenance rows anyway.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}
