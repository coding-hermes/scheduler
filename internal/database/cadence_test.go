package database

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

func TestCadenceTargetRoundTripAndAchievedRate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	target := 4.0
	p := sampleProject("cadence-lane")
	p.TargetRunsPerDay = &target
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := GetProject(ctx, db, p.Name)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.TargetRunsPerDay == nil || *got.TargetRunsPerDay != target {
		t.Fatalf("target round trip = %#v, want %v", got.TargetRunsPerDay, target)
	}

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for i, age := range []time.Duration{time.Hour, 48 * time.Hour} {
		tick := &Tick{ID: "cadence-" + string(rune('a'+i)), ProjectName: p.Name, Status: StatusQueued}
		if err := CreateTick(ctx, db, tick); err != nil {
			t.Fatalf("CreateTick[%d]: %v", i, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE ticks SET spawned_at = ?, status = 'running' WHERE id = ?`, now.Add(-age).Format(time.RFC3339), tick.ID); err != nil {
			t.Fatalf("stamp spawned_at[%d]: %v", i, err)
		}
	}
	rates, err := LoadCadenceRates(ctx, db, now, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("LoadCadenceRates: %v", err)
	}
	want := 2.0 / 7.0
	if got := rates[p.Name]; got != want {
		t.Fatalf("achieved rate = %v, want %v", got, want)
	}
}

func TestEffectiveCadenceTargetNoOpinionWithoutOverrideOrPin(t *testing.T) {
	p := *sampleProject("no-target")
	p.CooldownPinS = nil
	p.TargetRunsPerDay = nil
	if target, source, ok := EffectiveCadenceTarget(p); ok || target != 0 || source != "none" {
		t.Fatalf("target = (%v,%q,%v), want (0,none,false)", target, source, ok)
	}
}

// TestLoadCadenceRates_WindowFollowsClockSeam pins the achieved-rate horizon to
// the CALLER's clock — the SCHED-GAP-169 seam the loop threads in as
// evaluate()'s single `now` — never a wall-clock read. A run inside the
// trailing window counts; advancing the sim clock past the window drops it out,
// which is exactly how a lane that stopped running decays back to zero achieved
// runs/day (SOL-CADENCE / SCHED-GAP-1668).
//
// The horizon itself is the caller's parameter: scheduler.CadenceWindow and the
// API's cadenceWindow are both pinned to 7d by their own packages' tests, and
// this layer measures over whatever window it is handed.
func TestLoadCadenceRates_WindowFollowsClockSeam(t *testing.T) {
	const window = 7 * 24 * time.Hour // the horizon the loop and API pass in
	db := newTestDB(t)
	// A manual sim clock: it moves only when this test says so, so the window
	// boundaries below are exact rather than wall-clock dependent.
	clk := clock.NewManualSimClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	ctx := clock.WithClock(context.Background(), clk)

	p := sampleProject("cadence-seam")
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tick := &Tick{ID: "cadence-seam-1", ProjectName: p.Name, Status: StatusQueued}
	if err := CreateTick(ctx, db, tick); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	// Spawned six days before the sim instant: inside the 7d window.
	if _, err := db.ExecContext(ctx, `UPDATE ticks SET spawned_at = ?, status = 'running' WHERE id = ?`,
		clk.Now().Add(-6*24*time.Hour).Format(time.RFC3339), tick.ID); err != nil {
		t.Fatalf("stamp spawned_at: %v", err)
	}

	rates, err := LoadCadenceRates(ctx, db, clk.Now(), window)
	if err != nil {
		t.Fatalf("LoadCadenceRates: %v", err)
	}
	if got, want := rates[p.Name], 1.0/7.0; got != want {
		t.Fatalf("achieved rate at the seam instant = %v, want %v", got, want)
	}

	// Advance the SEAM two days rather than hand-building a later instant: the
	// run is now 8 days old and must leave the measurement entirely.
	clk.Advance(2 * 24 * time.Hour)
	rates, err = LoadCadenceRates(ctx, db, clk.Now(), window)
	if err != nil {
		t.Fatalf("LoadCadenceRates after advance: %v", err)
	}
	if got := rates[p.Name]; got != 0 {
		t.Fatalf("achieved rate after a 2d seam advance = %v, want 0 (the run is now 8d old)", got)
	}

	// A non-positive window is refused, never silently treated as unbounded.
	if _, err := LoadCadenceRates(ctx, db, clk.Now(), 0); err == nil {
		t.Fatal("LoadCadenceRates(0) = nil error, want a refusal")
	}
}
