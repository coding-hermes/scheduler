package scheduler

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1726-TEST-TIMEOUT — the suite reports goroutines stuck in
// evalDrain when it exceeds its 120s budget (loop.go:912). evalDrain is the
// SCHED-GAP-1575-A coalescing goroutine: the FIRST ForceEvaluate() on a Loop
// starts it, and it is parked on l.stopCh until Loop.Stop() closes. Tests
// that build a Loop, drive SpawnNow/ForceEvaluate (or a live BoardWakeWatcher
// wired to ForceEvaluate), and never Stop() leave that goroutine parked for
// the rest of the binary's lifetime — the stack fragments the failure report
// prints. This file pins the drain's lifecycle (the parked goroutine is gone
// the moment Stop returns) and adds a suite-exit census so any future leak
// is reported with a count at the very end of the run, not buried in a
// timeout dump.
func TestMain(m *testing.M) {
	code := m.Run()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	drains := strings.Count(string(buf[:n]), "scheduler.(*Loop).evalDrain(")
	if drains > 0 {
		fmt.Fprintf(os.Stderr, "SCHED-GAP-1726 goroutine census: %d goroutine(s) still parked in evalDrain at suite exit\n", drains)
	}
	os.Exit(code)
}

// TestSCHEDGAP1726_EvalDrainExitsOnStop pins the drain lifecycle: the
// evalDrain goroutine started by the first ForceEvaluate is gone by the time
// Stop() returns — no goroutine stays parked in evalDrain for a stopped
// loop. The count is a delta (other tests' in-flight drains are static under
// -p 1 sequential execution and cancel out).
func TestSCHEDGAP1726_EvalDrainExitsOnStop(t *testing.T) {
	countDrains := func() int {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		return strings.Count(string(buf[:n]), "scheduler.(*Loop).evalDrain(")
	}

	db := newTestDB(t)
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.noDeliver = true
	loop.ForceEvaluate()

	// Wait until the wake has been consumed so the drain is parked in its
	// select (the goroutine must exist before Stop, or the delta is vacuous).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && loop.LastEvalTime().IsZero() {
		time.Sleep(time.Millisecond)
	}
	before := countDrains()
	if before == 0 {
		t.Fatal("no evalDrain goroutine found after ForceEvaluate — the lifecycle pin cannot observe the drain")
	}

	loop.Stop()
	if after := countDrains(); after != before-1 {
		t.Fatalf("evalDrain goroutines after Stop = %d, want %d — a drain survived Stop()", after, before-1)
	}
}
