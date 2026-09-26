package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestSimTickID_UniqueWithinSameSecond locks the DOGFOOD-007 contract:
// two simulated ticks for the same project generated within the same
// second must get distinct IDs. The old sim-<project>-<HHMMSS> format
// collided and crashed --sim-count with "UNIQUE constraint failed:
// ticks.id".
func TestSimTickID_UniqueWithinSameSecond(t *testing.T) {
	loop := &Loop{}
	now := time.Now()
	first := loop.simTickID("heavy-alpha", now)
	second := loop.simTickID("heavy-alpha", now)
	if first == second {
		t.Fatalf("simTickID collision: %q == %q", first, second)
	}
}

// TestRunBulkSim_UniqueTickIDs end-to-end: RunBulkSim with a count larger
// than the fixture's enabled-project count wraps project picks across
// 500ms ticker fires, which is exactly the same-second duplicate scenario
// that crashed pre-DOGFOOD-007. Every generated tick must land in the DB
// exactly once (no UNIQUE constraint error, no duplicate rows).
func TestRunBulkSim_UniqueTickIDs(t *testing.T) {
	db := newTestDB(t)
	fixture := NewSimFixture(db)
	if err := fixture.Setup(nil, fixture.TestProjects()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 8)
	loop.SetSimulation(0.85)

	const count = 40 // 8 per 500ms fire; > len(projects) → wrap collisions without the fix
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := loop.RunBulkSim(ctx, count); err != nil {
		t.Fatalf("RunBulkSim: %v", err)
	}

	var total, distinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT id) FROM ticks`).Scan(&total, &distinct); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if total != count || distinct != count {
		t.Fatalf("ticks total=%d distinct=%d, want both = %d", total, distinct, count)
	}
}

// TestEvaluate_SimulateModeSpawnsSimulatedTicks: with SetSimulation set,
// an evaluation must route through the sim spawner (tick IDs prefixed
// "sim-"), never the real spawner (DOGFOOD-007: --simulate daemon mode
// previously spawned real foremen — or failed with "no gateway client and
// exec fallback disabled" in a scratch env).
func TestEvaluate_SimulateModeSpawnsSimulatedTicks(t *testing.T) {
	db := newTestDB(t)
	fixture := NewSimFixture(db)
	if err := fixture.Setup(nil, fixture.TestProjects()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 8)
	loop.SetSimulation(0.85)

	loop.ForceEvaluate()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
			t.Fatalf("count ticks: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("evaluate() spawned no ticks in sim mode")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var bad int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE id NOT LIKE 'sim-%'`).Scan(&bad); err != nil {
		t.Fatalf("count non-sim ticks: %v", err)
	}
	if bad != 0 {
		t.Fatalf("%d ticks did not use the sim spawner", bad)
	}
}
