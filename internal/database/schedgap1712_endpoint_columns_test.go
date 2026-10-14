package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1712 at the STORAGE layer.
//
// The endpoint resolution is only real if the three tiers are storable and the
// resolved fact is recordable, and only SAFE if the migration is additive on a
// database that already holds the fleet's tick history:
//
//   - a FRESH schema carries projects.gateway_url, namespaces.gateway_url /
//     gateway_key and ticks.gateway_url / gateway_source / gateway_key_source;
//   - an EXISTING pre-1712 (v60) database is upgraded in place, the new
//     columns default to '' on the rows already there (the honest "not
//     recorded" value — never a fabricated endpoint), and the pre-existing
//     ticks keep reading their historical data;
//   - every tier round-trips through the real write paths (create, patch,
//     read back), including the explicit CLEAR ("" = inherit again).

// schedGap1712Columns lists the columns the migration must have added, by
// table, so the fresh and upgrade arms assert the same set.
var schedGap1712Columns = map[string][]string{
	"projects":   {"gateway_url"},
	"namespaces": {"gateway_url", "gateway_key"},
	"ticks":      {"gateway_url", "gateway_source", "gateway_key_source"},
}

func assertGap1712Columns(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	for table, cols := range schedGap1712Columns {
		for _, col := range cols {
			var got string
			err := db.QueryRow(
				`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&got)
			if err == sql.ErrNoRows {
				t.Errorf("%s: %s.%s is missing — migration 61 did not land", label, table, col)
				continue
			}
			if err != nil {
				t.Fatalf("%s: pragma_table_info(%s): %v", label, table, err)
			}
		}
	}
}

// TestSCHEDGAP1712_FreshSchemaCarriesEndpointColumns pins the fresh-install
// shape: every tier column exists and defaults to the honest empty.
func TestSCHEDGAP1712_FreshSchemaCarriesEndpointColumns(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	assertGap1712Columns(t, db, "fresh")

	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 61 {
		t.Fatalf("migration version = %d, want >= 61", v)
	}
}

// TestSCHEDGAP1712_UpgradeFromV60IsAdditive builds a REAL v60 database (the
// full ladder minus v61) with a tick row already in it, runs Migrate, and
// asserts the upgrade left history intact and defaulted the new columns to
// the honest empty string.
func TestSCHEDGAP1712_UpgradeFromV60IsAdditive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v60.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
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
		if m.version >= 61 {
			break // stop before the migration under test
		}
		// Mirror the real runner's tolerance: ALTER TABLE ADD COLUMN is not
		// idempotent, and early migrations re-add columns the base CREATE
		// TABLE already carries.
		if _, err := db.ExecContext(ctx, m.stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record v%d: %v", m.version, err)
		}
	}

	// Premise: the columns are genuinely ABSENT before the migration, or the
	// upgrade arm proves nothing.
	for table, cols := range schedGap1712Columns {
		for _, col := range cols {
			var name string
			if err := db.QueryRow(`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&name); err == nil {
				t.Fatalf("premise broken: %s.%s already exists at v60", table, col)
			}
		}
	}

	// A tick row from the pre-1712 era, with its historical data.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, created_at, updated_at)
		 VALUES ('legacy-lane', 'git@example.invalid:x.git', '/tmp/x', 10, 5, 7200, 1.0, 'm', 'p', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');`); err != nil {
		t.Fatalf("seed legacy project: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, commits, created_at)
		 VALUES ('legacy-tick', 'legacy-lane', 'completed', 3, '2026-01-02T00:00:00Z');`); err != nil {
		t.Fatalf("seed legacy tick: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (upgrade): %v", err)
	}
	assertGap1712Columns(t, db, "upgraded")

	var commits int
	var gwURL, gwSrc, gwKeySrc string
	if err := db.QueryRowContext(ctx,
		`SELECT commits, gateway_url, gateway_source, gateway_key_source FROM ticks WHERE id = 'legacy-tick'`).
		Scan(&commits, &gwURL, &gwSrc, &gwKeySrc); err != nil {
		t.Fatalf("read upgraded legacy tick: %v", err)
	}
	if commits != 3 {
		t.Errorf("legacy tick history changed: commits = %d, want 3", commits)
	}
	if gwURL != "" || gwSrc != "" || gwKeySrc != "" {
		t.Errorf("upgraded legacy tick endpoint = (%q, %q, %q), want honest empties (no fabricated endpoint)", gwURL, gwSrc, gwKeySrc)
	}
	var legacyProjURL string
	if err := db.QueryRowContext(ctx, `SELECT gateway_url FROM projects WHERE name = 'legacy-lane'`).Scan(&legacyProjURL); err != nil {
		t.Fatalf("read upgraded legacy project: %v", err)
	}
	if legacyProjURL != "" {
		t.Errorf("legacy project gateway_url = %q, want '' (inherit)", legacyProjURL)
	}
}

// TestSCHEDGAP1712_TiersRoundTrip drives every tier through the real write
// paths: create-with-value, patch-to-a-new-value, patch-to-empty (the
// explicit CLEAR back to inheritance), and the tick endpoint stamp.
func TestSCHEDGAP1712_TiersRoundTrip(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Namespace tier: both fields.
	if err := CreateNamespace(ctx, db, &Namespace{
		ID: "nsrt", Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
		GatewayURL: "http://ns.example:8642", GatewayKey: "fk-ns",
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns, err := GetNamespace(ctx, db, "nsrt")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.GatewayURL != "http://ns.example:8642" || ns.GatewayKey != "fk-ns" {
		t.Errorf("namespace endpoint = (%q, %q), want the created values", ns.GatewayURL, ns.GatewayKey)
	}
	// ListNamespaces shares the column list, so a drift there is caught here.
	list, err := ListNamespaces(ctx, db, false)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	found := false
	for _, n := range list {
		if n.ID == "nsrt" {
			found = true
			if n.GatewayURL != ns.GatewayURL || n.GatewayKey != ns.GatewayKey {
				t.Errorf("ListNamespaces endpoint = (%q, %q), want the same values as GetNamespace", n.GatewayURL, n.GatewayKey)
			}
		}
	}
	if !found {
		t.Fatal("ListNamespaces did not return nsrt")
	}
	// Patch: replace, then clear.
	newURL, newKey := "http://ns2.example:9000", "fk-ns2"
	if err := UpdateNamespace(ctx, db, "nsrt", NamespacePatch{GatewayURL: &newURL, GatewayKey: &newKey}); err != nil {
		t.Fatalf("UpdateNamespace (replace): %v", err)
	}
	ns, _ = GetNamespace(ctx, db, "nsrt")
	if ns.GatewayURL != newURL || ns.GatewayKey != newKey {
		t.Errorf("namespace endpoint after patch = (%q, %q), want (%q, %q)", ns.GatewayURL, ns.GatewayKey, newURL, newKey)
	}
	cleared := ""
	if err := UpdateNamespace(ctx, db, "nsrt", NamespacePatch{GatewayURL: &cleared, GatewayKey: &cleared}); err != nil {
		t.Fatalf("UpdateNamespace (clear): %v", err)
	}
	ns, _ = GetNamespace(ctx, db, "nsrt")
	if ns.GatewayURL != "" || ns.GatewayKey != "" {
		t.Errorf("namespace endpoint after clear = (%q, %q), want ''/'' (inherit again)", ns.GatewayURL, ns.GatewayKey)
	}

	// Lane tier: create, read, list, patch, clear.
	nsID := "nsrt"
	if err := CreateProject(ctx, db, &Project{
		Name: "lanert", RepoURL: "git@example.invalid:lanert.git", Workdir: "/tmp/lanert",
		Weight: 10, Priority: 5, CooldownS: 7200, Enabled: true,
		NamespaceID: &nsID, GatewayURL: "http://lane.example:8642",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	p, err := GetProject(ctx, db, "lanert")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.GatewayURL != "http://lane.example:8642" {
		t.Errorf("project GatewayURL = %q, want the created value", p.GatewayURL)
	}
	byNS, err := ListProjectsByNamespace(ctx, db, nsID)
	if err != nil {
		t.Fatalf("ListProjectsByNamespace: %v", err)
	}
	if len(byNS) != 1 || byNS[0].GatewayURL != p.GatewayURL {
		t.Errorf("ListProjectsByNamespace endpoint = %+v, want the same value as GetProject", byNS)
	}
	page, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(page) != 1 || page[0].GatewayURL != p.GatewayURL {
		t.Errorf("ListProjects endpoint = %+v, want the same value as GetProject", page)
	}
	replaced := "http://lane2.example:9000"
	if err := UpdateProject(ctx, db, "lanert", ProjectUpdates{GatewayURL: &replaced}); err != nil {
		t.Fatalf("UpdateProject (replace): %v", err)
	}
	p, _ = GetProject(ctx, db, "lanert")
	if p.GatewayURL != replaced {
		t.Errorf("project GatewayURL after patch = %q, want %q", p.GatewayURL, replaced)
	}
	if err := UpdateProject(ctx, db, "lanert", ProjectUpdates{GatewayURL: &cleared}); err != nil {
		t.Fatalf("UpdateProject (clear): %v", err)
	}
	p, _ = GetProject(ctx, db, "lanert")
	if p.GatewayURL != "" {
		t.Errorf("project GatewayURL after clear = %q, want '' (inherit)", p.GatewayURL)
	}

	// A ProjectUpdates that carries ONLY the endpoint must not read as empty
	// (or the loader would skip the pin entirely).
	only := "http://only.example:8642"
	onlyUpdates := ProjectUpdates{GatewayURL: &only}
	if onlyUpdates.IsEmpty() {
		t.Error("ProjectUpdates{GatewayURL} reported IsEmpty — the loader would skip the pin")
	}

	// Tick tier: the resolved-endpoint stamp.
	if err := CreateTick(ctx, db, &Tick{ID: "tickrt", ProjectName: "lanert"}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ticks SET gateway_url = ?, gateway_source = ?, gateway_key_source = ? WHERE id = ?`,
		"http://lane.example:8642", GatewaySourceLane, GatewaySourceNamespace, "tickrt"); err != nil {
		t.Fatalf("stamp tick endpoint: %v", err)
	}
	var gwURL, gwSrc, gwKeySrc string
	if err := db.QueryRowContext(ctx,
		`SELECT gateway_url, gateway_source, gateway_key_source FROM ticks WHERE id = ?`, "tickrt").
		Scan(&gwURL, &gwSrc, &gwKeySrc); err != nil {
		t.Fatalf("read tick endpoint: %v", err)
	}
	if gwURL != "http://lane.example:8642" || gwSrc != GatewaySourceLane || gwKeySrc != GatewaySourceNamespace {
		t.Errorf("tick endpoint = (%q, %q, %q), want the stamped values", gwURL, gwSrc, gwKeySrc)
	}
}
