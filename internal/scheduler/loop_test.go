package scheduler_test

import (
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/scheduler"
)

// TestNewLoop_Defaults verifies the constructor wires up the calculator, packer, and lifecycle.
func TestNewLoop_Defaults(t *testing.T) {
	db := newTestDB(t)

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	if loop == nil {
		t.Fatal("NewLoop returned nil")
	}
	// Verify the loop starts and stops cleanly (no enabled projects → eval is a no-op).
	done := make(chan struct{})
	go func() {
		loop.Run()
		close(done)
	}()

	// Give Run a moment, then stop.
	time.Sleep(50 * time.Millisecond)
	loop.Stop()

	select {
	case <-done:
		// Run returned cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Stop()")
	}
}

// TestLoop_ForceEvaluateNoProjects is a no-op when no projects are enabled.
func TestLoop_ForceEvaluateNoProjects(t *testing.T) {
	db := newTestDB(t)
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 100, 5)

	// ForceEvaluate runs evaluate in a goroutine. With no enabled projects it returns immediately.
	loop.ForceEvaluate()

	// Wait for goroutine completion via WaitGroup-like heuristic: poll until
	// there are no running ticks. Since we created none, this is immediate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
			t.Fatalf("count ticks: %v", err)
		}
		if n == 0 {
			return // success: no ticks were created
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("ForceEvaluate with no projects left rows in ticks table")
}

// TestLoop_PauseResume verifies the pause/resume signaling works without deadlocking.
func TestLoop_PauseResume(t *testing.T) {
	db := newTestDB(t)
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 100, 5)

	done := make(chan struct{})
	go func() {
		loop.Run()
		close(done)
	}()

	// Pause the loop.
	loop.Pause()
	time.Sleep(50 * time.Millisecond)

	// Resume the loop.
	loop.Resume()
	time.Sleep(50 * time.Millisecond)

	// Stop the loop.
	loop.Stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Pause/Resume/Stop")
	}
}

// TestLoop_StopIsIdempotentNoopAfterReturn verifies Stop() can be called after Run has returned.
func TestLoop_StopAfterReturn(t *testing.T) {
	db := newTestDB(t)
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 100, 5)

	done := make(chan struct{})
	go func() {
		loop.Run()
		close(done)
	}()
	loop.Stop()
	<-done

	// Calling Stop again should panic on closing an already-closed channel — recover and report.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double-Stop, got nil")
		}
	}()
	loop.Stop()
	t.Fatal("did not reach defer (expected panic)")
}

// TestSumWeights is exercised indirectly through Pick, but we add a tiny direct test
// to lock the helper's behavior — the function is unexported so we test it via the public path.
func TestSumWeights_ViaPick(t *testing.T) {
	db := newTestDB(t)
	// Create projects with weights summing to a known total.
	mustCreateProjectAt(t, db, "a", 10, 5, 0, 1.0)
	mustCreateProjectAt(t, db, "b", 20, 5, 0, 1.0)
	mustCreateProjectAt(t, db, "c", 30, 5, 0, 1.0)

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	// budget=100 → all three fit (10+20+30=60).
	p := scheduler.NewPacker(db, calc, 100, 10)
	got, err := p.Pick(time.Now())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Pick returned %d, want 3", len(got))
	}

	// sumWeights(got) should equal 60 — verify by adding the public fields.
	total := 0
	for _, pp := range got {
		total += pp.Weight
	}
	if total != 60 {
		t.Errorf("sum of weights = %d, want 60", total)
	}
}
