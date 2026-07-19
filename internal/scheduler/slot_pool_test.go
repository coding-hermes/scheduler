package scheduler_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// project creates a basic Project for testing.
func project(name string, weight, priority int) *database.Project {
	return &database.Project{
		Name:     name,
		RepoURL:  "https://example.com/" + name,
		Workdir:  "/tmp/" + name,
		Weight:   weight,
		Priority: priority,
		Model:    "test",
		Provider: "test",
		Enabled:  true,
	}
}

// packed wraps a Project into a PackedProject for SlotPool.Spawn.
func packed(p *database.Project) scheduler.PackedProject {
	return scheduler.PackedProject{
		Name:     p.Name,
		RepoURL:  p.RepoURL,
		Workdir:  p.Workdir,
		Weight:   p.Weight,
		Priority: float64(p.Priority),
		Model:    p.Model,
		Provider: p.Provider,
	}
}

// ── SlotPool Concurrency Tests ──

func TestSlotPool_AcquireRelease(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 1)
	pool := scheduler.NewSlotPool(1, 10*time.Second, sp, lc)

	if !pool.Acquire(context.Background(), "test") {
		t.Fatal("Acquire should succeed")
	}
	if pool.Available() != 0 {
		t.Error("slot should be occupied")
	}

	pool.Release()
	if pool.Available() != 1 {
		t.Error("slot should be free after Release")
	}
}

func TestSlotPool_AcquireTimeout(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 1)
	pool := scheduler.NewSlotPool(1, 10*time.Second, sp, lc)

	if !pool.Acquire(context.Background(), "a") {
		t.Fatal("first Acquire")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pool.Acquire(ctx, "b") {
		t.Error("cancelled context should fail")
	}
}

func TestSlotPool_RunningSet(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 5)
	pool := scheduler.NewSlotPool(5, 10*time.Second, sp, lc)

	for _, n := range []string{"alpha", "beta", "gamma"} {
		if !pool.Acquire(context.Background(), n) {
			t.Fatalf("Acquire %s", n)
		}
	}

	rs := pool.RunningSet()
	if len(rs) != 3 {
		t.Fatalf("len = %d, want 3", len(rs))
	}
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if !rs[n] {
			t.Errorf("missing %s", n)
		}
	}
}

// ── Goroutine Leak Tests ──

func TestSlotPool_NoGoroutineLeak(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 2)
	pool := scheduler.NewSlotPool(2, 10*time.Second, sp, lc)

	before := runtime.NumGoroutine()

	c1 := pool.SlotFreed()
	c2 := pool.SlotFreed()
	c3 := pool.SlotFreed()

	if c1 != c2 || c2 != c3 {
		t.Fatal("SlotFreed() returned different channels — goroutine leak!")
	}

	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+2 {
		t.Errorf("goroutines grew from %d to %d — possible leak", before, after)
	}
}

// ── Event-Driven Tests ──

func TestSlotPool_SlotFreedFiresOnRelease(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 2)
	pool := scheduler.NewSlotPool(2, 10*time.Second, sp, lc)

	// Fill both slots.
	pool.Acquire(context.Background(), "a")
	pool.Acquire(context.Background(), "b")

	ch := pool.SlotFreed()
	drainCh(ch, 50*time.Millisecond)

	pool.Release()

	select {
	case <-ch:
		// Pass — SlotFreed fired.
	case <-time.After(2 * time.Second):
		t.Fatal("SlotFreed did not fire within 2s of Release")
	}
}

func TestSlotPool_SlotFreedMultipleReleases(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 5)
	pool := scheduler.NewSlotPool(5, 10*time.Second, sp, lc)

	pool.Acquire(context.Background(), "a")
	pool.Acquire(context.Background(), "b")
	pool.Acquire(context.Background(), "c")

	ch := pool.SlotFreed()
	drainCh(ch, 50*time.Millisecond)

	pool.Release()
	pool.Release()
	pool.Release()

	fired := 0
	for fired < 3 {
		select {
		case <-ch:
			fired++
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/3 events received", fired)
		}
	}
}

func drainCh(ch <-chan struct{}, timeout time.Duration) {
	for {
		select {
		case <-ch:
		case <-time.After(timeout):
			return
		}
	}
}
