package database

import (
	"context"
	"testing"
)

// TestMigrate_BumpColumns pins migration v26 (SCHED-GAP-107): the projects
// table gains the bump state + snapshot columns and the ticks table gains
// the bump flag, so a bump survives daemon restarts and bump ticks are
// distinguishable in yield analysis.
func TestMigrate_BumpColumns(t *testing.T) {
	db := newTestDB(t)

	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 26 {
		t.Errorf("MigrationVersion = %d, want >= 26 (bump migration)", v)
	}
	for _, col := range []string{
		"bump_active", "bump_remaining_ticks", "bump_cooldown_s", "bump_reason",
		"bump_saved_cooldown_s", "bump_saved_floor_s", "bump_saved_ceiling_s",
		"bump_saved_no_progress_ticks", "bump_started_at",
	} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('projects') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(projects) for %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("projects.%s missing after Migrate (count=%d) — migration v26 not applied", col, n)
		}
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('ticks') WHERE name = 'bump'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(ticks) for bump: %v", err)
	}
	if n != 1 {
		t.Errorf("ticks.bump missing after Migrate (count=%d) — migration v26 not applied", n)
	}
}

// TestBumpProject_SnapshotAndImmediateEffect verifies the DB-layer bump
// contract: saved state captured, bump fields set, cooldown applied now.
func TestBumpProject_SnapshotAndImmediateEffect(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("bump-db-test")
	p.CooldownS = 43200
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET adaptive_cooldown = 1,
		cooldown_floor_s = 21600, cooldown_ceiling_s = 604800, no_progress_ticks = 8
		WHERE name = 'bump-db-test'`); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	got, err := BumpProject(ctx, db, "bump-db-test", 5, 7200, "db test")
	if err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	if !got.BumpActive || got.BumpRemainingTicks != 5 || got.BumpCooldownS != 7200 {
		t.Fatalf("bump fields: %+v", got)
	}
	if got.CooldownS != 7200 {
		t.Fatalf("cooldown_s = %d, want 7200 immediately", got.CooldownS)
	}
	if got.BumpSavedCooldownS != 43200 || got.BumpSavedFloorS != 21600 ||
		got.BumpSavedCeilingS != 604800 || got.BumpSavedNoProgress != 8 {
		t.Fatalf("snapshot wrong: saved_cd=%d floor=%d ceiling=%d streak=%d",
			got.BumpSavedCooldownS, got.BumpSavedFloorS, got.BumpSavedCeilingS, got.BumpSavedNoProgress)
	}
	if got.BumpReason != "db test" || got.BumpStartedAt == "" {
		t.Fatalf("reason/started_at not stamped: %q %q", got.BumpReason, got.BumpStartedAt)
	}

	// Second bump → error mentioning an active bump.
	if _, err := BumpProject(ctx, db, "bump-db-test", 5, 7200, "again"); err == nil {
		t.Fatal("second BumpProject should fail")
	}

	// Clear restores verbatim.
	if err := ClearBump(ctx, db, "bump-db-test"); err != nil {
		t.Fatalf("ClearBump: %v", err)
	}
	got, err = GetProject(ctx, db, "bump-db-test")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.BumpActive || got.CooldownS != 43200 || got.NoProgressTicks != 8 ||
		got.CooldownFloorS != 21600 || got.CooldownCeilingS != 604800 {
		t.Fatalf("restore wrong: active=%v cd=%d streak=%d floor=%d ceiling=%d",
			got.BumpActive, got.CooldownS, got.NoProgressTicks, got.CooldownFloorS, got.CooldownCeilingS)
	}
}

// TestBumpProject_NotFound pins the error mapping for unknown projects.
func TestBumpProject_NotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := BumpProject(context.Background(), db, "ghost", 5, 7200, "x"); err == nil {
		t.Fatal("BumpProject on missing project should fail")
	}
	if err := ClearBump(context.Background(), db, "ghost"); err == nil {
		t.Fatal("ClearBump on missing project should fail")
	}
}
