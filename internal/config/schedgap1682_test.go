package config

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1682 acceptance at the CONFIG layer (A4's TOML arm).
//
// The `noop_allowed` key under a [[projects]] entry parses, applies and
// validates through the real loader: absent key = the row is untouched
// (NULL = the lane-class derivation survives), explicit true/false pins
// GatewayKey-conditionally, and an API-set override survives a restart
// with a keyless entry.

// writeFleetToml1682 writes a minimal fleet.toml with one project entry.
func writeFleetToml1682(t *testing.T, projectBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.toml")
	content := "[[projects]]\nname = \"noop-1682-lane\"\nrepo_url = \"https://example.com/l\"\nworkdir = \"/tmp/noop-1682-lane\"\n" + projectBody
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	return path
}

// noop1682RealDB is a fresh fully-migrated in-memory database.
func noop1682RealDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSCHEDGAP1682_FleetTomlNoopAllowedParses(t *testing.T) {
	path := writeFleetToml1682(t, "noop_allowed = false\n")
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(cfg.Projects))
	}
	if cfg.Projects[0].NoopAllowed == nil {
		t.Fatal("noop_allowed is nil, want &false (the key parsed)")
	}
	if *cfg.Projects[0].NoopAllowed {
		t.Fatal("noop_allowed = true, want false")
	}
}

func TestSCHEDGAP1682_FleetTomlNoopAllowedApplies(t *testing.T) {
	db := noop1682RealDB(t)
	ctx := context.Background()

	// Seed the row so the loader takes the UPDATE (pin) path.
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "noop-1682-lane", RepoURL: "https://example.com/l", Workdir: "/tmp/noop-1682-lane",
		Weight: 10, Priority: 5, CooldownS: 900,
		Enabled: true, CreatedAt: "2026-10-10T00:00:00Z", UpdatedAt: "2026-10-10T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Absent key: the row's NULL (derived) survives.
	path := writeFleetToml1682(t, "")
	if err := ApplyFleetConfig(ctx, db, mustLoadFleet1682(t, path)); err != nil {
		t.Fatalf("apply (keyless): %v", err)
	}
	p, err := database.GetProject(ctx, db, "noop-1682-lane")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.NoopAllowed != nil {
		t.Fatalf("keyless entry overwrote the row: noop_allowed = %v, want nil (derived)", *p.NoopAllowed)
	}

	// Explicit false: pinned.
	f := false
	path = writeFleetToml1682(t, "noop_allowed = false\n")
	if err := ApplyFleetConfig(ctx, db, mustLoadFleet1682(t, path)); err != nil {
		t.Fatalf("apply (false): %v", err)
	}
	p, err = database.GetProject(ctx, db, "noop-1682-lane")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.NoopAllowed == nil || *p.NoopAllowed {
		t.Fatalf("noop_allowed = %v, want &false", p.NoopAllowed)
	}

	// An API-set true SURVIVES a restart with a keyless entry (the
	// GatewayKey-conditional pin).
	if err := database.UpdateProject(ctx, db, "noop-1682-lane", database.ProjectUpdates{NoopAllowed: &f}); err != nil {
		t.Fatalf("api update: %v", err)
	}
	path = writeFleetToml1682(t, "")
	if err := ApplyFleetConfig(ctx, db, mustLoadFleet1682(t, path)); err != nil {
		t.Fatalf("apply (keyless, after api): %v", err)
	}
	p, err = database.GetProject(ctx, db, "noop-1682-lane")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.NoopAllowed == nil {
		t.Fatal("keyless restart wiped the API-set override, want it preserved")
	}
}

func mustLoadFleet1682(t *testing.T, path string) *FleetConfig {
	t.Helper()
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	return cfg
}

// TestSCHEDGAP1682_CreatePathCarriesOverride pins the create half: a
// brand-new lane's fleet.toml entry lands the override at creation (the
// create path is what fleet.toml uses for a project it has not seen).
func TestSCHEDGAP1682_CreatePathCarriesOverride(t *testing.T) {
	db := noop1682RealDB(t)
	ctx := context.Background()

	path := writeFleetToml1682(t, "noop_allowed = true\n")
	if err := ApplyFleetConfig(ctx, db, mustLoadFleet1682(t, path)); err != nil {
		t.Fatalf("apply (create): %v", err)
	}
	p, err := database.GetProject(ctx, db, "noop-1682-lane")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.NoopAllowed == nil || !*p.NoopAllowed {
		t.Fatalf("noop_allowed = %v, want &true (created with the override)", p.NoopAllowed)
	}
}
