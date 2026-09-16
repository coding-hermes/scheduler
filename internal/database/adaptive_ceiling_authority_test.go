package database

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// ADV-R10 — adaptive-ceiling single authority. When no explicit
// cooldown_ceiling_s is set, the default ceiling is DERIVED as 8 × the
// effective cooldown floor (the fleet.toml pin shape). A second hardcoded
// weekly (604800) default must never re-enter circulation.

// TestADVR10_DerivedCeilingIsEightTimesFloor pins the one derivation rule:
// DefaultAdaptiveCooldownCeiling(floor) = 8 × floor.
func TestADVR10_DerivedCeilingIsEightTimesFloor(t *testing.T) {
	cases := []struct {
		floor int
		want  int
	}{
		{43200, 345600}, // spec flagship: 12h floor -> 4d cap, NOT the old 7d
		{21600, 172800}, // fleet 6h pin -> 2d
		{900, 7200},     // 15m operator pin -> 2h
		{600, 4800},
		{0, 0}, // dynamic/no-floor project: nothing derivable, never escalates
	}
	for _, tc := range cases {
		if got := DefaultAdaptiveCooldownCeiling(tc.floor); got != tc.want {
			t.Errorf("DefaultAdaptiveCooldownCeiling(%d) = %d, want %d (8 × floor)",
				tc.floor, got, tc.want)
		}
	}
}

// TestADVR10_EnableDerivesCeilingFromFloor verifies the false→true enable
// transition derives the ceiling from the floor (43200 -> 345600) instead of
// parking the project at the old weekly 604800 default.
func TestADVR10_EnableDerivesCeilingFromFloor(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("advr10-derived")
	p.CooldownS = 43200
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	enable := true
	if err := UpdateProject(ctx, db, "advr10-derived", ProjectUpdates{AdaptiveCooldown: &enable}); err != nil {
		t.Fatalf("UpdateProject(adaptive=true): %v", err)
	}

	p, err := GetProject(ctx, db, "advr10-derived")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownFloorS != 43200 {
		t.Errorf("cooldown_floor_s = %d, want 43200 (snapshotted from cooldown_s)", p.CooldownFloorS)
	}
	if p.CooldownCeilingS != 345600 {
		t.Errorf("cooldown_ceiling_s = %d, want 345600 (8 × floor 43200, derived default — NOT 604800)", p.CooldownCeilingS)
	}
}

// TestADVR10_EnableExplicitFloorAlsoDerivesCeiling: an explicit floor with no
// explicit ceiling derives the ceiling from THAT floor, not from cooldown_s.
func TestADVR10_EnableExplicitFloorAlsoDerivesCeiling(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("advr10-explicit-floor")
	p.CooldownS = 21600
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	enable := true
	floor := 43200
	if err := UpdateProject(ctx, db, "advr10-explicit-floor", ProjectUpdates{
		AdaptiveCooldown: &enable, CooldownFloorS: &floor,
	}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	p, err := GetProject(ctx, db, "advr10-explicit-floor")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownFloorS != 43200 {
		t.Errorf("cooldown_floor_s = %d, want 43200 (explicit)", p.CooldownFloorS)
	}
	if p.CooldownCeilingS != 345600 {
		t.Errorf("cooldown_ceiling_s = %d, want 345600 (8 × explicit floor 43200)", p.CooldownCeilingS)
	}
}

// TestADVR10_EnableExplicitCeilingWins pins the explicit-wins rule: an
// explicit cooldown_ceiling_s lands verbatim and is never overwritten by the
// derived default (behavior preserved from pre-ADV-R10).
func TestADVR10_EnableExplicitCeilingWins(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("advr10-explicit-ceiling")
	p.CooldownS = 43200
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	enable := true
	ceiling := 999000 // deliberately NOT 8 × anything
	if err := UpdateProject(ctx, db, "advr10-explicit-ceiling", ProjectUpdates{
		AdaptiveCooldown: &enable, CooldownCeilingS: &ceiling,
	}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	p, err := GetProject(ctx, db, "advr10-explicit-ceiling")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownCeilingS != 999000 {
		t.Errorf("cooldown_ceiling_s = %d, want 999000 (explicit ceiling wins over derived 8×floor)", p.CooldownCeilingS)
	}
}

// TestADVR10_NoSecondHardcodedWeeklyCeiling fails the moment a hardcoded
// 604800 (the retired weekly default) reappears in any non-test Go source
// file under internal/ or cmd/ — comments included. The derived default is
// the ONLY ceiling default; grep-provable by construction.
func TestADVR10_NoSecondHardcodedWeeklyCeiling(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // .../internal/database/x_test.go -> repo root

	pattern := regexp.MustCompile(`604800`)
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(repoRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if pattern.Match(b) {
				rel, _ := filepath.Rel(repoRoot, path)
				offenders = append(offenders, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("hardcoded 604800 (second weekly-ceiling default) found in non-test sources: %s — the only ceiling authority is DefaultAdaptiveCooldownCeiling (8 × floor, ADV-R10)",
			strings.Join(offenders, ", "))
	}
}
