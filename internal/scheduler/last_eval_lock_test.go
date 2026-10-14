package scheduler

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SCHED-GAP-1621: /api/v1/status (and /health) read the loop's last-eval
// timestamp via LastEvalTime(). evaluate() holds the WRITE lock across the
// whole pack, so the getter's old RLock joined that convoy and the status
// route stalled 28-104s behind a cold pack. These tests pin the fix: the
// getter is lock-free, and the atomic mirror it reads stays coherent with
// the value evaluate() writes under the lock.

// TestLastEvalTimeDoesNotBlockOnLoopLock proves the status path does not
// block behind a long-running evaluate pass. The stand-in for "evaluate is
// mid-pack" is a goroutine holding l.mu for the duration of the call — the
// exact state a cold pack produces (write lock held for tens of seconds).
// Pre-fix, LastEvalTime() took l.mu.RLock() here and blocked until the lock
// was released; post-fix it returns immediately from the atomic mirror.
// No sleeps beyond the failure bound: the blocked-holder goroutine parks on
// a channel, and the 2s select bound is a failure timeout, not a wait.
func TestLastEvalTimeDoesNotBlockOnLoopLock(t *testing.T) {
	l := NewLoop(nil, time.Minute, time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)

	evalAt := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	l.mu.Lock()
	l.lastEval = evalAt
	l.lastEvalNs.Store(evalAt.UnixNano())
	l.mu.Unlock()

	release := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		l.mu.Lock()
		defer l.mu.Unlock()
		<-release // hold the write lock the way a mid-pack evaluate() does
	}()

	// Wait until the holder actually owns the lock (locked, then parked on
	// release). TryLock probes from this goroutine: while it SUCCEEDS the
	// holder has not yet taken the lock, so release and retry; the loop
	// exits the first time it FAILS, i.e. the holder owns the lock and the
	// convoy state is established.
	deadline := time.Now().Add(5 * time.Second)
	for l.mu.TryLock() {
		l.mu.Unlock()
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("holder goroutine never acquired the lock")
		}
		time.Sleep(time.Millisecond)
	}

	got := make(chan time.Time, 1)
	go func() { got <- l.LastEvalTime() }()

	select {
	case v := <-got:
		if !v.Equal(evalAt) {
			t.Fatalf("LastEvalTime() = %v, want %v", v, evalAt)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("LastEvalTime() blocked while Loop.mu was write-held — /api/v1/status convoys behind evaluate() again (SCHED-GAP-1621 regression)")
	}
	close(release)
	<-holderDone
}

// TestLastEvalTimeMirrorCoherentWithEvaluate runs one real evaluation pass
// and proves the lock-free mirror lands on exactly the timestamp evaluate()
// wrote to the locked field — the /api/v1/status "last_evaluation" value and
// the in-loop stall-watchdog view must never diverge.
func TestLastEvalTimeMirrorCoherentWithEvaluate(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	t.Cleanup(l.Stop)
	// Sim clock: a fixed instant, so evaluate()'s `now` is deterministic.
	fixed := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)
	l.SetClock(clock.NewFixed(fixed))
	l.SetNoExecFallback(true)

	l.evaluate()

	if got, want := l.LastEvalTime(), fixed; !got.Equal(want) {
		t.Fatalf("LastEvalTime() = %v, want the evaluate() instant %v", got, want)
	}
	l.mu.RLock()
	locked := l.lastEval
	l.mu.RUnlock()
	if !locked.Equal(l.LastEvalTime()) {
		t.Fatalf("mirror diverged from locked field: LastEvalTime()=%v, l.lastEval=%v", l.LastEvalTime(), locked)
	}
}

// TestLastEvalTimeZeroBeforeFirstEval pins the never-evaluated contract the
// board-wake WAKE-LAW test depends on: a fresh loop reports the zero time,
// not a garbage timestamp, and does so without taking the lock.
func TestLastEvalTimeZeroBeforeFirstEval(t *testing.T) {
	l := NewLoop(nil, time.Minute, time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)
	if !l.LastEvalTime().IsZero() {
		t.Fatalf("LastEvalTime() = %v on a never-evaluated loop, want zero time", l.LastEvalTime())
	}
}
