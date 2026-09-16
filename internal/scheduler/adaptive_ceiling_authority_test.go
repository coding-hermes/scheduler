package scheduler

import (
	"testing"
)

// ADV-R10 — adaptive-ceiling single authority at the RUNTIME layer: when a
// project row carries cooldown_ceiling_s = 0 (no explicit ceiling), the
// escalation cap falls back to the derived default 8 × floor, never to a
// hardcoded weekly value.

// TestADVR10_RuntimeDerivedCeilingCapsEscalation drives a 43200s-floor
// project (ceiling column 0) through the escalator and proves it caps at
// 345600 (8 × floor), NOT 604800. Tick 4 is the discriminator: the retired
// weekly default would read 604800 there.
func TestADVR10_RuntimeDerivedCeilingCapsEscalation(t *testing.T) {
	db := slowdownTestDB(t)
	insertAdaptiveProject(t, db, "advr10-cap", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 43200, floorS: 43200, ceilingS: 0, threshold: 1}) // threshold 1 = escalate on first no-progress tick

	want := []int{86400, 172800, 345600, 345600} // 2x steps capped at 8 × 43200
	for i, w := range want {
		if !adaptiveCooldown(db, "advr10-cap", "", noProgressOutcome("advr10-cap")) {
			t.Fatalf("tick %d: adaptiveCooldown returned false", i+1)
		}
		cd, _, _, _, _, _ := readAdaptiveState(t, db, "advr10-cap")
		if cd != w {
			t.Fatalf("tick %d: cooldown = %d, want %d (43200 × 2^%d capped at 345600 = 8 × floor, NOT 604800)", i+1, cd, w, i+1)
		}
	}
}

// TestADVR10_RuntimeFloorlessRowNeverEscalates pins the floorless-row
// semantic: a hand-edited row with floor 0 AND ceiling 0 derives ceiling 0
// (nothing stable to derive from), so the streak is tracked but cooldown_s
// never escalates. This is the documented dynamic-project behavior; the
// enable path always writes a floor, so properly-enabled rows never land
// here.
func TestADVR10_RuntimeFloorlessRowNeverEscalates(t *testing.T) {
	db := slowdownTestDB(t)
	insertAdaptiveProject(t, db, "advr10-nofloor", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 0, ceilingS: 0, threshold: 1})

	for i := 0; i < 4; i++ {
		if !adaptiveCooldown(db, "advr10-nofloor", "", noProgressOutcome("advr10-nofloor")) {
			t.Fatalf("tick %d: adaptiveCooldown returned false", i+1)
		}
		cd, _, _, _, streak, _ := readAdaptiveState(t, db, "advr10-nofloor")
		if cd != 600 {
			t.Fatalf("tick %d: cooldown = %d, want 600 (floorless row must never escalate)", i+1, cd)
		}
		if streak != i+1 {
			t.Fatalf("tick %d: streak = %d, want %d (streak still tracked)", i+1, streak, i+1)
		}
	}
}

// TestADVR10_ExplicitCeilingPreservesWeeklyEscalation pins acceptance #4:
// a row carrying an EXPLICIT 604800 ceiling keeps the full legacy escalation
// chain verbatim — explicit ceilings are untouched by the derived default.
func TestADVR10_ExplicitCeilingPreservesWeeklyEscalation(t *testing.T) {
	db := slowdownTestDB(t)
	insertAdaptiveProject(t, db, "advr10-explicit-weekly", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1})

	want := []int{1200, 2400, 4800, 9600, 19200, 38400, 76800, 153600, 307200, 604800, 604800}
	for i, w := range want {
		if !adaptiveCooldown(db, "advr10-explicit-weekly", "", noProgressOutcome("advr10-explicit-weekly")) {
			t.Fatalf("tick %d: adaptiveCooldown returned false", i+1)
		}
		cd, _, _, _, _, _ := readAdaptiveState(t, db, "advr10-explicit-weekly")
		if cd != w {
			t.Fatalf("tick %d: cooldown = %d, want %d (explicit weekly ceiling chain preserved)", i+1, cd, w)
		}
	}
}
