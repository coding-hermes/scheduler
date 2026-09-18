package clock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// start462 is the fixed instant every test anchors on. Constructed, never read
// from the wall clock — so nothing here can turn wall-clock-flaky.
func start462() time.Time { return time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC) }

// TestSimClock_AdvanceFiresTimersInDeadlineOrder is the core ordering
// contract: Advance fires every timer whose deadline falls inside the jumped
// window, earliest first, registering nothing that was not yet due — and a
// frozen Manual clock proves the ORDER, not just the set.
func TestSimClock_AdvanceFiresTimersInDeadlineOrder(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	var mu sync.Mutex
	var order []string
	record := func(name string) func() {
		return func() { mu.Lock(); order = append(order, name); mu.Unlock() }
	}

	// Registered out of deadline order on purpose.
	c.AfterFunc(30*time.Minute, record("late-30m"))
	c.AfterFunc(5*time.Minute, record("early-5m"))
	c.AfterFunc(10*time.Minute, record("mid-10m"))
	c.AfterFunc(90*time.Minute, record("beyond-90m"))

	if n := c.PendingTimers(); n != 4 {
		t.Fatalf("PendingTimers() = %d, want 4", n)
	}

	// Jump inside the window that covers none of the deadlines.
	c.Advance(4 * time.Minute)
	waitFor(t, 100*time.Millisecond, func() bool { return firedCount(&mu, &order) == 0 },
		"a 4m jump fired a timer whose deadline is 5m out")

	// Jump exactly onto the first deadline.
	c.Advance(1 * time.Minute)
	if got := c.Now(); !got.Equal(start462().Add(5 * time.Minute)) {
		t.Fatalf("Now() after jump = %v, want the exact deadline %v", got, start462().Add(5*time.Minute))
	}
	waitFor(t, time.Second, func() bool { return firedCount(&mu, &order) == 1 }, "5m timer did not fire")
	if got := firstN(&mu, &order, 1); len(got) != 1 || got[0] != "early-5m" {
		t.Fatalf("first fire = %v, want [early-5m]", got)
	}

	// Jump across both remaining in-window deadlines in ONE advance: they must
	// fire in deadline order (10m then 30m), never in registration order.
	c.Advance(31 * time.Minute)
	waitFor(t, time.Second, func() bool { return firedCount(&mu, &order) == 3 }, "mid/late timers did not fire")
	if got := firstN(&mu, &order, 3); got[1] != "mid-10m" || got[2] != "late-30m" {
		t.Fatalf("fire order = %v, want [early-5m mid-10m late-30m]", got)
	}

	// The 90m timer is still armed and must NOT have fired.
	if n := c.PendingTimers(); n != 1 {
		t.Fatalf("PendingTimers() = %d, want 1 (the 90m timer)", n)
	}
	if next, ok := c.NextTimer(); !ok || !next.Equal(start462().Add(90*time.Minute)) {
		t.Fatalf("NextTimer() = %v/%v, want the 90m deadline", next, ok)
	}
}

// TestSimClock_ScaleShortensRealWaits proves the 10x/100x/1000x contract: a
// blocked wait costs d/scale of REAL time while virtual Now() advances the
// full d, so production arithmetic is unchanged but reached sooner.
func TestSimClock_ScaleShortensRealWaits(t *testing.T) {
	const virtualWait = 20 * time.Second
	const scale = 1000.0

	c := NewSimClockAt(scale, start462())
	defer c.Close()
	if got := c.Scale(); got != scale {
		t.Fatalf("Scale() = %v, want %v", got, scale)
	}

	realStart := time.Now()
	c.Sleep(virtualWait)
	realElapsed := time.Since(realStart)
	virtualElapsed := c.Since(start462())

	if realElapsed >= virtualWait {
		t.Fatalf("Sleep(%v) at scale %v took %v of real time — not scaled", virtualWait, scale, realElapsed)
	}
	if virtualElapsed != virtualWait {
		t.Fatalf("virtual elapsed = %v, want exactly %v", virtualElapsed, virtualWait)
	}
	// Generous ceiling: the point is "far below real time", not a benchmark.
	if realElapsed > virtualWait/10 {
		t.Fatalf("Sleep(%v) at scale %v took %v real — expected well under %v", virtualWait, scale, realElapsed, virtualWait/10)
	}
}

// TestSimClock_TickerTicksAtScaledCadence drives a real ticker through the sim
// and asserts BOTH the virtual cadence (ticks are exactly period apart) and
// that the run costs a fraction of the virtual span in real time.
func TestSimClock_TickerTicksAtScaledCadence(t *testing.T) {
	const period = 15 * time.Millisecond
	c := NewSimClockAt(200, start462())
	defer c.Close()

	tk := c.NewTicker(period)
	defer tk.Stop()

	var stamps []time.Time
	realStart := time.Now()
	for len(stamps) < 3 {
		select {
		case at := <-tk.C:
			stamps = append(stamps, at)
		case <-time.After(10 * time.Second):
			t.Fatalf("ticker stalled after %d ticks (sim clock broken?)", len(stamps))
		}
	}
	realElapsed := time.Since(realStart)

	// Virtual cadence is unaffected by the scale factor.
	for i := 1; i < len(stamps); i++ {
		if d := stamps[i].Sub(stamps[i-1]); d != period {
			t.Fatalf("tick %d spacing = %v, want exactly %v", i, d, period)
		}
	}
	// Each tick costs period/scale of real time.
	if budget := period * 20; realElapsed > budget {
		t.Fatalf("3 ticks at scale 200 took %v real, want well under %v", realElapsed, budget)
	}
	if delta := c.Since(start462()); delta < 3*period {
		t.Fatalf("virtual time after 3 ticks = %v, want >= %v", delta, 3*period)
	}
}

// TestSimClock_AfterFuncFiresOnceAndIsOneShot pins the one-shot contract: the
// callback runs exactly once, Stop() afterwards reports "already fired", and a
// second jump does not re-run it.
func TestSimClock_AfterFuncFiresOnceAndIsOneShot(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	var calls int64
	tm := c.AfterFunc(2*time.Minute, func() { atomic.AddInt64(&calls, 1) })
	if tm.C != nil {
		t.Fatalf("AfterFunc timer C = %v, want nil (stdlib AfterFunc semantics)", tm.C)
	}

	c.Advance(2 * time.Minute)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&calls) == 1 }, "AfterFunc callback did not run")
	if n := c.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() after firing = %d, want 0", n)
	}
	if tm.Stop() {
		t.Fatal("Stop() on an already-fired AfterFunc timer returned true")
	}

	c.Advance(24 * time.Hour)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("AfterFunc ran %d times, want exactly 1 (one-shot)", got)
	}
}

// TestSimClock_AfterFuncStopPreventsFiring pins the other side: a stopped
// callback never runs, however far the clock is jumped.
func TestSimClock_AfterFuncStopPreventsFiring(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	var calls int64
	tm := c.AfterFunc(time.Hour, func() { atomic.AddInt64(&calls, 1) })
	if !tm.Stop() {
		t.Fatal("Stop() on an armed AfterFunc timer returned false")
	}
	c.Advance(72 * time.Hour)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("stopped AfterFunc ran %d times, want 0", got)
	}
}

// TestSimClock_WaitForNextTimerUnblocksOnNextDeadline is the test-driver
// check-in primitive: park until the next deadline, then land exactly on it.
func TestSimClock_WaitForNextTimerUnblocksOnNextDeadline(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	// No timer armed: the call must BLOCK, not return a zero deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	early := make(chan time.Time, 1)
	go func() {
		d, _ := c.WaitForNextTimer(ctx)
		early <- d
	}()
	select {
	case d := <-early:
		t.Fatalf("WaitForNextTimer returned %v before any timer was armed", d)
	case <-time.After(100 * time.Millisecond):
	}

	want := start462().Add(3 * time.Minute)
	tm := c.NewTimer(3 * time.Minute)
	defer tm.Stop()

	deadline, ok := c.WaitForNextTimer(ctx)
	if !ok {
		t.Fatal("WaitForNextTimer returned ok=false with a timer armed")
	}
	if !deadline.Equal(want) {
		t.Fatalf("WaitForNextTimer deadline = %v, want %v", deadline, want)
	}
	// It skips virtual time to JUST before the deadline: the timer has not
	// fired yet, and the caller's Advance lands exactly on it.
	if now := c.Now(); !now.Before(deadline) {
		t.Fatalf("Now() = %v, want strictly before the returned deadline %v", now, deadline)
	}
	select {
	case at := <-tm.C:
		t.Fatalf("timer fired during WaitForNextTimer (at %v) — caller owns the Advance", at)
	default:
	}
	c.Advance(deadline.Sub(c.Now()))
	select {
	case at := <-tm.C:
		if !at.Equal(want) {
			t.Fatalf("fired at %v, want %v", at, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Advance to the returned deadline did not fire the timer")
	}

	// Context cancellation releases a waiter with ok=false.
	cancel()
	if _, ok := c.WaitForNextTimer(ctx); ok {
		t.Fatal("WaitForNextTimer returned ok=true on a cancelled context")
	}
}

// TestSimClock_RunUntilIdleDrainsOneShots proves the drain helper: every armed
// one-shot fires in deadline order, and the call returns instead of spinning.
func TestSimClock_RunUntilIdleDrainsOneShots(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	var order []string
	var mu sync.Mutex
	for _, spec := range []struct {
		d    time.Duration
		name string
	}{
		{time.Hour, "h"},
		{time.Minute, "m"},
		{time.Second, "s"},
	} {
		spec := spec
		c.AfterFunc(spec.d, func() { mu.Lock(); order = append(order, spec.name); mu.Unlock() })
	}

	if n := c.RunUntilIdle(context.Background()); n != 3 {
		t.Fatalf("RunUntilIdle fired %d deadlines, want 3", n)
	}
	waitFor(t, time.Second, func() bool { return firedCount(&mu, &order) == 3 }, "not all callbacks ran")
	if got := firstN(&mu, &order, 3); got[0] != "s" || got[1] != "m" || got[2] != "h" {
		t.Fatalf("drain order = %v, want [s m h]", got)
	}
	if n := c.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() after RunUntilIdle = %d, want 0", n)
	}
	// A ticker never idles: the drain must return rather than spin forever.
	tk := c.NewTicker(time.Minute)
	defer tk.Stop()
	if n := c.RunUntilIdle(context.Background()); n != 0 {
		t.Fatalf("RunUntilIdle fired %d deadlines with only a ticker armed, want 0", n)
	}
}

// TestSimClock_TimerResetRearms pins Reset semantics on both the one-shot and
// the API level: a reset timer fires at its NEW deadline and never fires twice.
func TestSimClock_TimerResetRearms(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	tm := c.NewTimer(time.Minute)
	if !tm.Reset(10 * time.Minute) {
		t.Fatal("Reset on an armed timer returned false")
	}
	c.Advance(time.Minute)
	select {
	case at := <-tm.C:
		t.Fatalf("timer fired at its ORIGINAL deadline (%v) after Reset", at)
	default:
	}
	c.Advance(9 * time.Minute)
	select {
	case at := <-tm.C:
		if !at.Equal(start462().Add(10 * time.Minute)) {
			t.Fatalf("fired at %v, want %v", at, start462().Add(10*time.Minute))
		}
	case <-time.After(time.Second):
		t.Fatal("Reset timer did not fire at its new deadline")
	}
	if n := c.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() = %d after a one-shot fired, want 0", n)
	}
}

// TestSimClock_TickerResyncOnHugeJump pins the catch-up bound: a 24h jump over
// a 1s ticker must not replay 86,400 periods, and the ticker must stay usable
// afterwards.
func TestSimClock_TickerResyncOnHugeJump(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	tk := c.NewTicker(time.Second)
	defer tk.Stop()

	done := make(chan time.Time, 8)
	go func() {
		for at := range tk.C {
			done <- at
		}
	}()

	start := time.Now()
	c.Advance(24 * time.Hour)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("24h jump over a 1s ticker took %v — the backlog was replayed instead of resynced", elapsed)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ticker delivered nothing after the resync jump")
	}
	// Still armed and still usable.
	if n := c.PendingTimers(); n != 1 {
		t.Fatalf("PendingTimers() = %d, want 1 (the ticker stays armed)", n)
	}
	next, ok := c.NextTimer()
	if !ok || !next.After(c.Now()) {
		t.Fatalf("NextTimer() = %v/%v, want a future deadline (ticker re-armed)", next, ok)
	}
}

// TestSimClock_AdvanceIsMonotonic pins that virtual time never runs backwards
// and that a zero/negative advance is a silent no-op.
func TestSimClock_AdvanceIsMonotonic(t *testing.T) {
	c := NewManualSimClock(start462())
	defer c.Close()

	c.Advance(time.Hour)
	c.Advance(0)
	c.Advance(-time.Hour)
	if got := c.Now(); !got.Equal(start462().Add(time.Hour)) {
		t.Fatalf("Now() = %v, want %v (negative/zero advances are no-ops)", got, start462().Add(time.Hour))
	}
	c.SetNow(start462())
	if got := c.Now(); !got.Equal(start462()) {
		t.Fatalf("SetNow did not move the clock: %v", got)
	}
}

// TestSimClock_CloseReleasesWaiters pins the shutdown contract: Close stops the
// driver and unblocks a parked WaitForNextTimer.
func TestSimClock_CloseReleasesWaiters(t *testing.T) {
	c := NewSimClockAt(1000, start462())
	ctx := context.Background()

	done := make(chan bool, 1)
	go func() {
		_, ok := c.WaitForNextTimer(ctx)
		done <- ok
	}()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("WaitForNextTimer returned ok=true after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a parked WaitForNextTimer")
	}
}

// TestSimClock_SleepScalesWithScaleChange pins the "10x then 100x" story: the
// multiplier is live, not constructor-frozen, and a runtime change takes effect
// on the next wait.
func TestSimClock_SleepScalesWithScaleChange(t *testing.T) {
	c := NewSimClockAt(1, start462())
	defer c.Close()

	c.SetScale(0) // ignored: a typo must not freeze the clock
	if got := c.Scale(); got != 1 {
		t.Fatalf("Scale() = %v after SetScale(0), want 1 (invalid values ignored)", got)
	}
	c.SetScale(1000)
	if got := c.Scale(); got != 1000 {
		t.Fatalf("Scale() = %v after SetScale(1000), want 1000", got)
	}
	realStart := time.Now()
	c.Sleep(30 * time.Second)
	if elapsed := time.Since(realStart); elapsed > 3*time.Second {
		t.Fatalf("Sleep(30s) at scale 1000 took %v real", elapsed)
	}
	if got := c.Since(start462()); got != 30*time.Second {
		t.Fatalf("virtual elapsed = %v, want 30s", got)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func firedCount(mu *sync.Mutex, order *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*order)
}

func firstN(mu *sync.Mutex, order *[]string, n int) []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, n)
	for i := 0; i < n && i < len(*order); i++ {
		out = append(out, (*order)[i])
	}
	return out
}

func waitFor(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout after %v: %s", budget, msg)
}
