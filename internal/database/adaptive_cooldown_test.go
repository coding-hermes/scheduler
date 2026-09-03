package database

import (
	"context"
	"testing"
)

// TestMigrate_AdaptiveCooldownColumns pins migration v22: the projects table
// must gain the adaptive-cooldown policy + runtime columns so per-project
// auto slow-down / speed-up persists across restarts.
func TestMigrate_AdaptiveCooldownColumns(t *testing.T) {
	db := newTestDB(t)

	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 22 {
		t.Errorf("MigrationVersion = %d, want >= 22 (adaptive cooldown migration)", v)
	}
	for _, col := range []string{
		"adaptive_cooldown", "cooldown_floor_s", "cooldown_ceiling_s",
		"no_progress_threshold", "no_progress_ticks", "board_rows_seen",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('projects') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(projects) for %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("projects.%s missing after Migrate (count=%d) — migration v22 not applied", col, n)
		}
	}
}

// TestUpdateProject_AdaptiveEnableNormalizes verifies the false→true enable
// transition in UpdateProject fills effective policy values (floor snapshotted
// from cooldown_s, ceiling/threshold = built-in defaults) and clears the
// runtime streak to a clean slate.
func TestUpdateProject_AdaptiveEnableNormalizes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("adaptive-test")
	p.CooldownS = 7200
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Dirty the runtime streak as if a previous adaptive era left residue.
	if _, err := db.Exec(`UPDATE projects SET no_progress_ticks = 7, board_rows_seen = 42 WHERE name = 'adaptive-test'`); err != nil {
		t.Fatalf("seed streak: %v", err)
	}

	enable := true
	if err := UpdateProject(ctx, db, "adaptive-test", ProjectUpdates{AdaptiveCooldown: &enable}); err != nil {
		t.Fatalf("UpdateProject(adaptive=true): %v", err)
	}

	p, err := GetProject(ctx, db, "adaptive-test")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !p.AdaptiveCooldown {
		t.Error("adaptive_cooldown = false, want true")
	}
	if p.CooldownFloorS != 7200 {
		t.Errorf("cooldown_floor_s = %d, want 7200 (snapshotted from cooldown_s at enable)", p.CooldownFloorS)
	}
	if p.CooldownCeilingS != DefaultAdaptiveCooldownCeilingS {
		t.Errorf("cooldown_ceiling_s = %d, want %d (built-in weekly default)", p.CooldownCeilingS, DefaultAdaptiveCooldownCeilingS)
	}
	if p.NoProgressThreshold != DefaultAdaptiveCooldownThreshold {
		t.Errorf("no_progress_threshold = %d, want %d (built-in default)", p.NoProgressThreshold, DefaultAdaptiveCooldownThreshold)
	}
	if p.NoProgressTicks != 0 {
		t.Errorf("no_progress_ticks = %d, want 0 (streak cleared on enable)", p.NoProgressTicks)
	}
	if p.BoardRowsSeen != AdaptiveUnseenBoardRows {
		t.Errorf("board_rows_seen = %d, want %d (baseline reset on enable)", p.BoardRowsSeen, AdaptiveUnseenBoardRows)
	}

	// A later explicit threshold/ceiling override is respected.
	threshold := 3
	ceiling := 100000
	if err := UpdateProject(ctx, db, "adaptive-test", ProjectUpdates{
		NoProgressThreshold: &threshold, CooldownCeilingS: &ceiling,
	}); err != nil {
		t.Fatalf("UpdateProject(threshold/ceiling): %v", err)
	}
	p, _ = GetProject(ctx, db, "adaptive-test")
	if p.NoProgressThreshold != 3 || p.CooldownCeilingS != 100000 {
		t.Errorf("override not applied: threshold=%d ceiling=%d, want 3/100000", p.NoProgressThreshold, p.CooldownCeilingS)
	}
}

// TestUpdateProject_AdaptiveDisableLeavesPolicy verifies disabling the flag is
// a pure off-switch: policy numbers and the streak are left in place (a fresh
// re-enable re-normalizes anyway).
func TestUpdateProject_AdaptiveDisableLeavesPolicy(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("adaptive-off")
	p.CooldownS = 3600
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	enable := true
	if err := UpdateProject(ctx, db, "adaptive-off", ProjectUpdates{AdaptiveCooldown: &enable}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET no_progress_ticks = 4 WHERE name = 'adaptive-off'`); err != nil {
		t.Fatalf("seed streak: %v", err)
	}

	disable := false
	if err := UpdateProject(ctx, db, "adaptive-off", ProjectUpdates{AdaptiveCooldown: &disable}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	p, err := GetProject(ctx, db, "adaptive-off")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.AdaptiveCooldown {
		t.Error("adaptive_cooldown = true after disable, want false")
	}
	if p.CooldownFloorS != 3600 {
		t.Errorf("cooldown_floor_s = %d, want 3600 (policy retained while off)", p.CooldownFloorS)
	}
	if p.NoProgressTicks != 4 {
		t.Errorf("no_progress_ticks = %d, want 4 (streak untouched by disable)", p.NoProgressTicks)
	}
}
