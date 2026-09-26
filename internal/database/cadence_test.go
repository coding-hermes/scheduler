package database

import (
	"context"
	"testing"
	"time"
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
