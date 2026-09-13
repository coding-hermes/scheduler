package database

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// SCHED-GAP-107-VER verification suite (DB layer). These tests deliberately
// re-pin the migration-v26 contract at a finer grain than bump_test.go and
// extend it with the checks that suite lacks: the recorded v26 migration row
// and column DEFAULTs, snapshot exactness for an adaptive-OFF project (the
// all-zero saved-policy shape), full bump-field zeroing on ClearBump, and
// errors.Is mapping for the missing-project error.

// TestVerify_Migrate26_BumpColumnsAndDefaults pins migration v26 exactly:
// the version row is RECORDED in the migrations table (not merely "version
// >= 26"), all nine projects.bump_* columns plus ticks.bump exist, and each
// column carries its documented DEFAULT so pre-bump rows read as inert.
func TestVerify_Migrate26_BumpColumnsAndDefaults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The bump migration is specifically version 26 (recorded, with its
	// description) — a later renumbering must not silently absorb it.
	var desc string
	err := db.QueryRowContext(ctx,
		`SELECT desc FROM migrations WHERE version = 26`).Scan(&desc)
	if err != nil {
		t.Fatalf("migration v26 row missing: %v", err)
	}
	if !strings.Contains(desc, "task bump") || !strings.Contains(desc, "SCHED-GAP-107") {
		t.Errorf("migration v26 desc = %q, want the SCHED-GAP-107 task-bump description", desc)
	}

	wantDefaults := map[string]string{
		"bump_active":                  "0",
		"bump_remaining_ticks":         "0",
		"bump_cooldown_s":              "0",
		"bump_reason":                  "''",
		"bump_saved_cooldown_s":        "0",
		"bump_saved_floor_s":           "0",
		"bump_saved_ceiling_s":         "0",
		"bump_saved_no_progress_ticks": "0",
		"bump_started_at":              "''",
	}
	for col, wantDflt := range wantDefaults {
		var n int
		var dflt interface{}
		if err := db.QueryRowContext(ctx,
			`SELECT count(*), max(dflt_value) FROM pragma_table_info('projects') WHERE name = ?`, col,
		).Scan(&n, &dflt); err != nil {
			t.Fatalf("pragma_table_info(projects) for %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("projects.%s missing after Migrate (count=%d)", col, n)
			continue
		}
		got, _ := dflt.(string)
		if got != wantDflt {
			t.Errorf("projects.%s DEFAULT = %s, want %s", col, got, wantDflt)
		}
	}
	var n int
	var tickDflt interface{}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), max(dflt_value) FROM pragma_table_info('ticks') WHERE name = 'bump'`,
	).Scan(&n, &tickDflt); err != nil {
		t.Fatalf("pragma_table_info(ticks) for bump: %v", err)
	}
	if n != 1 {
		t.Fatalf("ticks.bump missing after Migrate (count=%d)", n)
	}
	if got, _ := tickDflt.(string); got != "0" {
		t.Errorf("ticks.bump DEFAULT = %s, want 0", got)
	}
}

// TestVerify_BumpProject_SnapshotExactAdaptiveOff covers the snapshot shape
// bump_test.go skips: an adaptive-OFF project (floor=0, ceiling=0, streak=0).
// Every saved column must equal the pre-bump value verbatim — including the
// zeros — and ClearBump must restore those zeros exactly (a bump can never
// ratchet an adaptive policy into existence) and zero ALL bump fields.
func TestVerify_BumpProject_SnapshotExactAdaptiveOff(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("verify-adaptive-off")
	p.CooldownS = 86400
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Deliberately leave adaptive policy columns at their zero defaults.

	got, err := BumpProject(ctx, db, "verify-adaptive-off", 3, 7200, "verify")
	if err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Immediate cooldown effect — the bump governs selection right away.
	if got.CooldownS != 7200 {
		t.Fatalf("cooldown_s = %d after bump, want 7200 (immediate effect)", got.CooldownS)
	}
	// Snapshot exactness against the pre-bump values (all zeros for the
	// adaptive policy, 86400 for the cooldown).
	if got.BumpSavedCooldownS != 86400 || got.BumpSavedFloorS != 0 ||
		got.BumpSavedCeilingS != 0 || got.BumpSavedNoProgress != 0 {
		t.Fatalf("snapshot wrong for adaptive-off project: saved_cd=%d floor=%d ceiling=%d streak=%d, want 86400/0/0/0",
			got.BumpSavedCooldownS, got.BumpSavedFloorS, got.BumpSavedCeilingS, got.BumpSavedNoProgress)
	}
	if !got.BumpActive || got.BumpRemainingTicks != 3 || got.BumpCooldownS != 7200 ||
		got.BumpReason != "verify" || got.BumpStartedAt == "" {
		t.Fatalf("bump state fields wrong: %+v", got)
	}

	if err := ClearBump(ctx, db, "verify-adaptive-off"); err != nil {
		t.Fatalf("ClearBump: %v", err)
	}
	after, err := GetProject(ctx, db, "verify-adaptive-off")
	if err != nil {
		t.Fatalf("GetProject after clear: %v", err)
	}
	if after.CooldownS != 86400 || after.CooldownFloorS != 0 ||
		after.CooldownCeilingS != 0 || after.NoProgressTicks != 0 {
		t.Fatalf("restore wrong: cd=%d floor=%d ceiling=%d streak=%d, want 86400/0/0/0",
			after.CooldownS, after.CooldownFloorS, after.CooldownCeilingS, after.NoProgressTicks)
	}
	// Every bump field is zeroed — no residue for the next bump to collide with.
	if after.BumpActive || after.BumpRemainingTicks != 0 || after.BumpCooldownS != 0 ||
		after.BumpReason != "" || after.BumpStartedAt != "" ||
		after.BumpSavedCooldownS != 0 || after.BumpSavedFloorS != 0 ||
		after.BumpSavedCeilingS != 0 || after.BumpSavedNoProgress != 0 {
		t.Fatalf("bump fields not fully cleared: %+v", after)
	}
}

// TestVerify_BumpProject_MissingMapsToErrProjectNotFound pins the error
// IDENTITY (not just non-nil): callers map ErrProjectNotFound to 404 and the
// already-active case to 409, so the sentinel must survive wrapping.
func TestVerify_BumpProject_MissingMapsToErrProjectNotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := BumpProject(ctx, db, "ghost", 5, 7200, "x"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("BumpProject on missing project: err = %v, want ErrProjectNotFound", err)
	}
	if err := ClearBump(ctx, db, "ghost"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("ClearBump on missing project: err = %v, want ErrProjectNotFound", err)
	}

	// Contrast: an existing project with no active bump maps to the OTHER
	// sentinel so the API can answer 409 rather than 404.
	if err := CreateProject(ctx, db, sampleProject("quiet")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := ClearBump(ctx, db, "quiet"); !errors.Is(err, ErrNoActiveBump) {
		t.Fatalf("ClearBump with no active bump: err = %v, want ErrNoActiveBump", err)
	}
}
