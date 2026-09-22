package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-219 — the two tests the brief requires, pinned in the loader
// package where ApplyFleetConfig lives.

// TestSCHEDGAP219_APICooldownSurvivesRestart is the SCHED-GAP-121 regression,
// test-pinned under the new law: set a cooldown through the API write path
// (database.UpdateProject — the exact path PUT /api/v1/projects/{name}
// takes), then simulate a full boot/loader cycle over a fleet.toml that
// carries a DIFFERENT value, and assert the API value survived. Under the
// OLD law this exact scenario snapped the value back to the toml number
// (hermes-dagger 900 vs OPERATOR_7200).
func TestSCHEDGAP219_APICooldownSurvivesRestart(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "fleet.toml")
	// The seed file carries the pre-ruling value (the OPERATOR_7200 shape).
	if err := os.WriteFile(tomlPath, []byte(`
[[projects]]
name = "dagger"
repo_url = "local:/tmp/dagger"
workdir = "/tmp/dagger"
cooldown_s = 7200
enabled = true
`), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}

	// First boot: the seed creates the row.
	cfg, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (boot 1): %v", err)
	}

	// Operator ruling via the API write path: 900s (Bane's 2026-09-15
	// dagger speed ruling, the exact value that used to drift back).
	ruling := 900
	if err := database.UpdateProject(ctx, db, "dagger", database.ProjectUpdates{CooldownS: &ruling}); err != nil {
		t.Fatalf("UpdateProject (API): %v", err)
	}

	// Second boot over the SAME toml (still 7200 — nobody regenerated it).
	cfg2, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig (boot 2): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg2); err != nil {
		t.Fatalf("ApplyFleetConfig (boot 2): %v", err)
	}

	p, err := database.GetProject(ctx, db, "dagger")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownS != 900 {
		t.Errorf("SCHED-GAP-121 REGRESSION: API-set cooldown did not survive the restart — cooldown_s = %d, want 900", p.CooldownS)
	}
	// The toml value imports as the operator pin — but a pin import never
	// lowers an existing pin, and the first boot already pinned 7200, so
	// the 900 toml value must NOT have replaced it.
	if p.CooldownPinS == nil || *p.CooldownPinS != 7200 {
		t.Errorf("pin integrity: cooldown_pin_s = %v, want 7200 (a lower toml value must not lower an operator pin)", p.CooldownPinS)
	}
	if p.CooldownPinBy != database.CooldownPinImportBy {
		t.Errorf("pin provenance: cooldown_pin_by = %q, want %q", p.CooldownPinBy, database.CooldownPinImportBy)
	}
}

// TestSCHEDGAP219_SeedAppliesOnceThenPinWins covers the brief's second
// required proof: a fleet.toml value with NO DB row is applied (the seed
// creates the row); a fleet.toml value DIFFERENT from an existing operator
// pin loses — the pin wins.
func TestSCHEDGAP219_SeedAppliesOnceThenPinWins(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[[projects]]
name = "lane"
repo_url = "local:/tmp/lane"
workdir = "/tmp/lane"
cooldown_s = 43200
enabled = true
`), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}

	// 1. No DB row: the seed applies — the row is created with the toml
	//    cooldown, and the operator pin is recorded.
	cfg, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (seed): %v", err)
	}
	p, err := database.GetProject(ctx, db, "lane")
	if err != nil {
		t.Fatalf("GetProject (seed): %v", err)
	}
	if p.CooldownS != 43200 {
		t.Errorf("seed: cooldown_s = %d, want 43200 (no DB row — the seed applies)", p.CooldownS)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 43200 {
		t.Errorf("seed: cooldown_pin_s = %v, want 43200 (first import stamps the pin)", p.CooldownPinS)
	}

	// 2. The operator RAISES the pin via the API write path, then a restart
	//    carries a DIFFERENT (lower) toml value: the pin must win.
	higher := 86400
	if err := database.UpdateProject(ctx, db, "lane", database.ProjectUpdates{CooldownPinS: &higher}); err != nil {
		t.Fatalf("UpdateProject (pin raise): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}
	p, err = database.GetProject(ctx, db, "lane")
	if err != nil {
		t.Fatalf("GetProject (restart): %v", err)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 86400 {
		t.Errorf("PIN WINS FAILED: cooldown_pin_s = %v, want 86400 — the toml's 43200 must not lower an operator pin", p.CooldownPinS)
	}
	if p.CooldownS != 86400 {
		// The API pin-set path snaps cooldown_s UP to the pin; the restart
		// must not have pulled it back down to the toml's 43200.
		t.Errorf("cooldown after restart = %d, want 86400 (the restart must not apply the lower toml value)", p.CooldownS)
	}
}

// TestSCHEDGAP219_PauseSurvivesSeedRestart is the enabled-side of the law:
// a project paused through the API stays paused across a boot over a seed
// file that says enabled = true.
func TestSCHEDGAP219_PauseSurvivesSeedRestart(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[[projects]]
name = "paused"
repo_url = "local:/tmp/paused"
workdir = "/tmp/paused"
cooldown_s = 21600
enabled = true
`), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}

	cfg, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (boot 1): %v", err)
	}

	// Pause via the API write path (enabled=false, GAP-044 provenance).
	disabled := false
	if err := database.UpdateProject(ctx, db, "paused", database.ProjectUpdates{Enabled: &disabled}); err != nil {
		t.Fatalf("UpdateProject (pause): %v", err)
	}

	// Boot 2 over the unchanged seed.
	cfg2, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig (boot 2): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg2); err != nil {
		t.Fatalf("ApplyFleetConfig (boot 2): %v", err)
	}

	p, err := database.GetProject(ctx, db, "paused")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.Enabled {
		t.Errorf("SCHED-GAP-219 FAILED: a pause was undone by a restart over a seed file that says enabled=true")
	}
}
