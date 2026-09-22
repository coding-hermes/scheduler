package config

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── SCHED-GAP-107: bump survives fleet-cooldown-policy --apply regen ──────

// TestApplyFleetConfig_BumpActivePreservesCooldown (SCHED-GAP-107): when a
// project has an active bump, ApplyFleetConfig must NOT re-pin cooldown_s /
// cooldown_floor_s / cooldown_ceiling_s from fleet.toml. The bump owns those
// fields until its auto-revert restores the saved pre-bump values; a regen
// re-pin would clobber the bump cooldown and break the auto-revert snapshot.
func TestApplyFleetConfig_BumpActivePreservesCooldown(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Seed a project from fleet.toml with cooldown=43200.
	seedCfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "bump-regen", RepoURL: "https://github.com/example/bump-regen",
				Workdir: "/home/kara/bump-regen", CooldownS: 43200,
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, seedCfg); err != nil {
		t.Fatalf("ApplyFleetConfig (seed): %v", err)
	}

	// Enable the project so a bump is valid.
	enabled := true
	if err := database.UpdateProject(ctx, db, "bump-regen", database.ProjectUpdates{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateProject enable: %v", err)
	}

	// Bump the project: 5 ticks at 7200s cooldown.
	bumped, err := database.BumpProject(ctx, db, "bump-regen", 5, 7200, "test bump")
	if err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	if !bumped.BumpActive {
		t.Fatal("BumpActive should be true after bump")
	}
	if bumped.CooldownS != 7200 {
		t.Errorf("CooldownS after bump = %d, want 7200", bumped.CooldownS)
	}
	if bumped.BumpSavedCooldownS != 43200 {
		t.Errorf("BumpSavedCooldownS = %d, want 43200", bumped.BumpSavedCooldownS)
	}

	// Simulate fleet-cooldown-policy.py --apply regen: ApplyFleetConfig re-pins
	// from fleet.toml (which carries cooldown_s=43200). The bump must survive.
	if err := ApplyFleetConfig(ctx, db, seedCfg); err != nil {
		t.Fatalf("ApplyFleetConfig (regen): %v", err)
	}

	p, err := database.GetProject(ctx, db, "bump-regen")
	if err != nil {
		t.Fatalf("GetProject after regen: %v", err)
	}
	if !p.BumpActive {
		t.Error("BumpActive should still be true after regen — bump must survive policy --apply")
	}
	if p.CooldownS != 7200 {
		t.Errorf("CooldownS after regen = %d, want 7200 (bump owns cooldown while active; regen must not clobber)", p.CooldownS)
	}
	if p.BumpRemainingTicks != 5 {
		t.Errorf("BumpRemainingTicks after regen = %d, want 5", p.BumpRemainingTicks)
	}
	if p.BumpSavedCooldownS != 43200 {
		t.Errorf("BumpSavedCooldownS after regen = %d, want 43200 (snapshot must survive regen)", p.BumpSavedCooldownS)
	}
}

// TestApplyFleetConfig_BumpInactiveRepinsNormally (SCHED-GAP-107): when no
// bump is active, ApplyFleetConfig re-pins cooldown_s from fleet.toml as usual
// — the bump guard does not interfere with the normal regen path.
func TestApplyFleetConfig_BumpInactiveRepinsNormally(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "normal", RepoURL: "https://github.com/example/normal",
				Workdir: "/home/kara/normal", CooldownS: 3600,
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (seed): %v", err)
	}

	// Change cooldown via API (simulating a foreman self-PUT).
	cd := 7200
	if err := database.UpdateProject(ctx, db, "normal", database.ProjectUpdates{CooldownS: &cd}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	// SCHED-GAP-219: the DB is the cooldown authority — a restart does NOT
	// re-pin cooldown from fleet.toml. The API value survives; the toml's
	// 3600 is recorded as the row's operator pin instead (a floor, never a
	// live-value overwrite).
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}

	p, err := database.GetProject(ctx, db, "normal")
	if err != nil {
		t.Fatalf("GetProject after restart: %v", err)
	}
	if p.CooldownS != 7200 {
		t.Errorf("CooldownS after restart = %d, want 7200 (API-set cooldown must survive the restart — SCHED-GAP-219)", p.CooldownS)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 3600 {
		t.Errorf("CooldownPinS after restart = %v, want 3600 (the fleet.toml value imports as the operator pin)", p.CooldownPinS)
	}
}
