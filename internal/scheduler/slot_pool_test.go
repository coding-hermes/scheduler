package scheduler_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
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

	pool.Release("test")
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

// ── SCHED-GAP-021: project-aware release ──

// assertRunningSet checks that RunningSet contains exactly the given names.
func assertRunningSet(t *testing.T, pool *scheduler.SlotPool, want ...string) {
	t.Helper()
	rs := pool.RunningSet()
	if len(rs) != len(want) {
		t.Fatalf("RunningSet = %v (len %d), want exactly %v (len %d)", rs, len(rs), want, len(want))
	}
	for _, n := range want {
		if !rs[n] {
			t.Errorf("RunningSet missing %s (got %v)", n, rs)
		}
	}
}

// TestSlotPool_ReleaseOutOfOrderPreservesMarkers is THE regression test for
// SCHED-GAP-021 (live proof: ring-runner double-spawn 2026-08-09 22:07 while
// its 21:45 tick was still running). The old FIFO Release() popped the oldest
// acquisition regardless of which project completed, so a short tick finishing
// while a long-running project held its slot evicted the LONG-RUNNING
// project's marker — EVAL's dedup then saw it as free and spawned a duplicate.
// Release must free ONLY the completing project's marker.
func TestSlotPool_ReleaseOutOfOrderPreservesMarkers(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 3)
	pool := scheduler.NewSlotPool(3, 10*time.Second, sp, lc)

	// A acquires first and runs long; B and C acquire after.
	for _, n := range []string{"a-long", "b-short", "c-short"} {
		if !pool.Acquire(context.Background(), n) {
			t.Fatalf("Acquire %s", n)
		}
	}
	assertRunningSet(t, pool, "a-long", "b-short", "c-short")

	// B completes first — out of acquisition order. Only B's marker may go;
	// A's (the FIFO-oldest) must be PRESERVED.
	pool.Release("b-short")
	assertRunningSet(t, pool, "a-long", "c-short")
	if pool.Running() != 2 {
		t.Errorf("Running = %d after releasing b-short, want 2", pool.Running())
	}

	// C completes next. A's marker must STILL be there.
	pool.Release("c-short")
	assertRunningSet(t, pool, "a-long")
	if pool.Running() != 1 {
		t.Errorf("Running = %d after releasing c-short, want 1", pool.Running())
	}

	// A finally completes. Pool empty.
	pool.Release("a-long")
	assertRunningSet(t, pool)
	if pool.Running() != 0 {
		t.Errorf("Running = %d after releasing a-long, want 0", pool.Running())
	}
	if pool.Available() != 3 {
		t.Errorf("Available = %d after all releases, want 3", pool.Available())
	}
}

// TestSlotPool_ReleaseUnknownNameIsNoOp pins the other half of SCHED-GAP-021:
// releasing a name that holds no slot must NOT pop another project's marker
// (that silent eviction was the bug) and must not signal SlotFreed.
func TestSlotPool_ReleaseUnknownNameIsNoOp(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 2)
	pool := scheduler.NewSlotPool(2, 10*time.Second, sp, lc)

	pool.Acquire(context.Background(), "a")
	pool.Acquire(context.Background(), "b")

	ch := pool.SlotFreed()
	drainCh(ch, 50*time.Millisecond)

	// "ghost" holds no slot — on a FULL pool this is exactly where the old
	// code popped the FIFO-oldest marker.
	pool.Release("ghost")

	assertRunningSet(t, pool, "a", "b")
	if pool.Running() != 2 {
		t.Errorf("Running = %d after ghost release, want 2", pool.Running())
	}
	if pool.Available() != 0 {
		t.Errorf("Available = %d after ghost release, want 0", pool.Available())
	}
	select {
	case <-ch:
		t.Error("SlotFreed fired for a no-op release — no slot was freed")
	case <-time.After(150 * time.Millisecond):
		// Pass — no event.
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

	pool.Release("a")

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

	pool.Release("a")
	pool.Release("b")
	pool.Release("c")

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

// ── ReleaseAll tests ──

func TestSlotPool_ReleaseAll(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 5)
	pool := scheduler.NewSlotPool(5, 10*time.Second, sp, lc)

	// Acquire 3 slots.
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if !pool.Acquire(context.Background(), n) {
			t.Fatalf("Acquire %s", n)
		}
	}
	if pool.Running() != 3 {
		t.Fatalf("Running = %d, want 3 after acquires", pool.Running())
	}

	// Release all should drain the semaphore and signal.
	pool.ReleaseAll()

	if pool.Running() != 0 {
		t.Errorf("Running = %d after ReleaseAll, want 0", pool.Running())
	}
	if pool.Available() != 5 {
		t.Errorf("Available = %d after ReleaseAll, want 5", pool.Available())
	}
}

func TestSlotPool_ReleaseAll_Empty(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 3)
	pool := scheduler.NewSlotPool(3, 10*time.Second, sp, lc)

	// ReleaseAll on empty pool is a no-op and should not block.
	pool.ReleaseAll()

	if pool.Running() != 0 {
		t.Errorf("Running = %d after ReleaseAll on empty pool, want 0", pool.Running())
	}
	if pool.Available() != 3 {
		t.Errorf("Available = %d after ReleaseAll on empty pool, want 3", pool.Available())
	}
}
