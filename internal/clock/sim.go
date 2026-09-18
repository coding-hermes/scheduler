package clock

import (
	"container/heap"
	"context"
	"strconv"
	"sync"
	"time"
)

// SimClock is the test-time simulator: a virtual clock whose instant is
// decoupled from the wall clock.
//
// Semantics (the "check in points" contract):
//
//   - Virtual now starts at the configured start instant (default: wall clock
//     at construction). It is MONOTONIC — it never runs backwards.
//   - Scale (SetScale / SCHEDULER_TIME_SCALE) is the speed multiplier. A
//     blocked Sleep(d)/After(d)/timer wait costs d/scale of REAL time while
//     virtual now advances the full d. Sleep(1h) at scale 1000 therefore blocks
//     3.6s of real time and leaves Now() one hour further on, so every
//     production comparison (cooldown, deadline, tick timeout) keeps its normal
//     arithmetic — it is just reached 1000x sooner.
//   - AutoAdvance (SetAutoAdvance / SCHEDULER_TIME_AUTOADVANCE=1) is "skip to
//     the next armed timer": when a timer is armed, virtual time jumps straight
//     to its deadline with no real-time cost at all. Default off, because a
//     ticker-driven loop under auto-advance runs at maximum speed.
//   - Advance / AdvanceTo jump virtual time deterministically and fire every
//     timer whose deadline falls inside the jumped window, in (deadline,
//     registration-sequence) order. This is the primitive a Go test drives
//     directly instead of sleeping.
//   - WaitForNextTimer is the deterministic check-in point: it blocks until a
//     timer is armed and skips virtual time to just before that deadline,
//     returning the deadline so the caller fires it with Advance.
//
// Firing rules, deliberately matching stdlib behavior:
//
//   - channel timers/tickers use a buffered(1) channel and the send is
//     non-blocking, exactly like time.Ticker drops ticks nobody is reading;
//   - AfterFunc bodies run SYNCHRONOUSLY inside the advance that reached their
//     deadline, in (deadline, registration-sequence) order. This is a
//     deliberate difference from stdlib (which fires them in their own
//     goroutines, in no guaranteed order): a deterministic simulator must let
//     a test observe a callback's effects as soon as Advance returns. The cost
//     is that a slow callback delays the virtual clock, so simulator callbacks
//     must be short and must only READ the clock.
//   - a periodic timer that has missed more than maxTickerCatchUp periods in
//     one jump is resynced to now+period instead of replaying the backlog, so
//     a 24h jump over a 1s ticker stays O(maxTickerCatchUp) instead of O(86400).
type SimClock struct {
	mu     sync.Mutex
	now    time.Time
	start  time.Time
	scale  float64
	auto   bool
	driven bool
	seq    uint64
	// timers holds every armed timer and ticker ordered by (deadline, seq).
	timers timerHeap
	// wake is signalled whenever the armed set changes or virtual time moves.
	// Buffered(1): a missed signal is recovered by the next loop iteration.
	wake chan struct{}
	// closed is closed by Close and stops the driver goroutine.
	closed    chan struct{}
	closeOnce sync.Once

	// fireMu serializes callback dispatch so two concurrent advances cannot
	// interleave the firing order.
	fireMu sync.Mutex

	wg sync.WaitGroup
}

// maxTickerCatchUp bounds how many missed periods a single jump replays for a
// periodic timer before it is resynced to now+period.
const maxTickerCatchUp = 1000

// autoYieldEvery bounds the number of zero-cost jumps the driver makes under
// AutoAdvance before it yields the CPU for one real millisecond, so a ticker
// loop cannot starve the rest of the process.
const autoYieldEvery = 256

// simTimer is one armed timer, one-shot (period == 0) or periodic.
type simTimer struct {
	deadline time.Time
	seq      uint64
	period   time.Duration
	ch       chan time.Time
	fn       func()
	stopped  bool
	done     bool
	// index/inHeap track heap membership for O(log n) Stop.
	index  int
	inHeap bool
}

// NewSimClock returns a simulator clock running at the given speed multiplier,
// anchored at the wall-clock instant of construction. Scale must be > 0; the
// clock starts in scale mode (AutoAdvance off).
func NewSimClock(scale float64) *SimClock {
	return NewSimClockAt(scale, time.Now())
}

// NewSimClockAt is NewSimClock with an explicit start instant (the equivalent
// of SCHEDULER_TIME_START).
func NewSimClockAt(scale float64, start time.Time) *SimClock {
	if scale <= 0 {
		scale = DefaultScale
	}
	c := &SimClock{
		now:    start,
		start:  start,
		scale:  scale,
		driven: true,
		wake:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.run()
	return c
}

// NewManualSimClock returns a DORMANT simulator: virtual time moves only when
// the caller drives it with Advance/AdvanceTo/WaitForNextTimer. Blocking waits
// (Sleep/After/timers) therefore stay blocked until the driver advances the
// clock. This is the fully deterministic mode a Go test uses when it wants to
// decide the instant each phase runs rather than letting the driver run at a
// speed multiplier.
func NewManualSimClock(start time.Time) *SimClock {
	c := NewSimClockAt(DefaultScale, start)
	c.SetDriven(false)
	return c
}

// Driven reports whether the driver goroutine advances virtual time on its own.
func (c *SimClock) Driven() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.driven
}

// SetDriven turns the auto-advancement driver on or off. With the driver off,
// only Advance/AdvanceTo/WaitForNextTimer move virtual time.
func (c *SimClock) SetDriven(on bool) {
	c.mu.Lock()
	c.driven = on
	c.mu.Unlock()
	c.signal()
}

// Describe implements describe.
func (c *SimClock) Describe() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "sim (scale=" + formatFloat(c.scale) +
		" start=" + c.start.Format(time.RFC3339) +
		" auto=" + boolString(c.auto) +
		" driven=" + boolString(c.driven) + ")"
}

// Close stops the simulator's driver goroutine and releases every waiter.
// Subsequent Advance calls still work; blocking waits return immediately.
func (c *SimClock) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.signal()
	})
	c.wg.Wait()
}

// Scale returns the current speed multiplier.
func (c *SimClock) Scale() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scale
}

// SetScale sets the speed multiplier (10, 100, 1000, ...). Values <= 0 are
// ignored so a typo cannot freeze or rewind the clock.
func (c *SimClock) SetScale(s float64) {
	if s <= 0 {
		return
	}
	c.mu.Lock()
	c.scale = s
	c.mu.Unlock()
	c.signal()
}

// AutoAdvance reports whether skip-to-next-armed-timer is enabled.
func (c *SimClock) AutoAdvance() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auto
}

// SetAutoAdvance enables or disables skip-to-next-armed-timer.
func (c *SimClock) SetAutoAdvance(on bool) {
	c.mu.Lock()
	c.auto = on
	c.mu.Unlock()
	c.signal()
}

// Advance moves virtual time forward by d, firing every timer whose deadline
// falls in the jumped window in (deadline, registration-sequence) order.
// Non-positive durations are no-ops (virtual time never runs backwards).
func (c *SimClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	due := c.advanceLocked(d)
	c.mu.Unlock()
	c.fire(due)
	c.signal()
}

// AdvanceTo moves virtual time to t (no-op when t is not in the future).
func (c *SimClock) AdvanceTo(t time.Time) {
	c.mu.Lock()
	d := t.Sub(c.now)
	c.mu.Unlock()
	c.Advance(d)
}

// SetNow moves virtual time to t unconditionally, without firing anything.
// It exists for test fixtures that need a specific instant; it is the only
// operation that may move virtual time backwards.
func (c *SimClock) SetNow(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
	c.signal()
}

// PendingTimers reports how many timers/tickers are currently armed. Stopped
// timers are not counted.
func (c *SimClock) PendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.done {
			n++
		}
	}
	return n
}

// NextTimer returns the deadline of the earliest armed timer, if any.
func (c *SimClock) NextTimer() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.peekLocked()
	if t == nil {
		return time.Time{}, false
	}
	return t.deadline, true
}

// WaitForNextTimer blocks until the clock has an armed timer, then skips
// virtual time forward to just before that timer's deadline and returns the
// deadline. The timer itself has NOT fired: the caller fires it with
// Advance(deadline.Sub(Now())), which is exactly the deadline returned.
//
// This is the check-in primitive for test drivers: park here instead of
// sleeping for a fixed wall-clock period, then assert on the run at the exact
// instant the next timer was due.
//
// ok is false when ctx is done or the clock was Closed.
func (c *SimClock) WaitForNextTimer(ctx context.Context) (time.Time, bool) {
	for {
		c.mu.Lock()
		next := c.peekLocked()
		now := c.now
		c.mu.Unlock()

		if next == nil {
			select {
			case <-ctx.Done():
				return time.Time{}, false
			case <-c.closed:
				return time.Time{}, false
			case <-c.wake:
				continue
			}
		}

		deadline := next.deadline
		if !deadline.After(now) {
			return deadline, true
		}
		// Stop one nanosecond short of the deadline so nothing fires here and
		// the caller's Advance lands exactly on it.
		skip := deadline.Sub(now) - time.Nanosecond
		if skip > 0 {
			c.mu.Lock()
			if c.now.Before(deadline) {
				if d := deadline.Sub(c.now) - time.Nanosecond; d > 0 {
					c.advanceToLocked(c.now.Add(d))
				}
			}
			c.mu.Unlock()
			c.signal()
		}
		return deadline, true
	}
}

// RunUntilIdle advances through every pending one-shot timer in deadline order
// and returns the number of deadlines it fired. It stops early when the next
// armed timer is periodic (a ticker never idles), when ctx is done, when the
// clock is Closed, or after maxSteps deadlines — so it can never spin.
func (c *SimClock) RunUntilIdle(ctx context.Context) int {
	const maxSteps = 100000
	fired := 0
	for fired < maxSteps {
		select {
		case <-ctx.Done():
			return fired
		case <-c.closed:
			return fired
		default:
		}
		c.mu.Lock()
		next := c.peekLocked()
		if next == nil || next.period > 0 {
			c.mu.Unlock()
			return fired
		}
		deadline := next.deadline
		var due []*simTimer
		if deadline.After(c.now) {
			due = c.advanceLocked(deadline.Sub(c.now))
		} else {
			// Already due (a callback registered it exactly at now).
			due = c.advanceLocked(time.Nanosecond)
		}
		c.mu.Unlock()
		c.fire(due)
		fired += len(due)
		if len(due) == 0 {
			return fired
		}
	}
	return fired
}

// ---------------------------------------------------------------------------
// Clock interface
// ---------------------------------------------------------------------------

// Now implements Clock.
func (c *SimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Since implements Clock.
func (c *SimClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// Until implements Clock.
func (c *SimClock) Until(t time.Time) time.Duration { return t.Sub(c.Now()) }

// Sleep blocks for d of virtual time, costing d/scale of real time (or nothing
// under AutoAdvance).
func (c *SimClock) Sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.closed:
	}
}

// After implements Clock.
func (c *SimClock) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C }

// NewTimer implements Clock.
func (c *SimClock) NewTimer(d time.Duration) *Timer {
	ch := make(chan time.Time, 1)
	t := &simTimer{ch: ch}
	c.mu.Lock()
	t.deadline = c.now.Add(d)
	t.seq = c.seq
	c.seq++
	heap.Push(&c.timers, t)
	c.mu.Unlock()
	c.signal()
	return &Timer{
		C: ch,
		stop: func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if t.stopped || t.done {
				return false
			}
			t.stopped = true
			return true
		},
		reset: func(nd time.Duration) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if nd <= 0 {
				nd = time.Nanosecond
			}
			wasActive := !t.stopped && !t.done
			t.stopped = false
			t.done = false
			t.deadline = c.now.Add(nd)
			t.seq = c.seq
			c.seq++
			if !t.inHeap {
				heap.Push(&c.timers, t)
			} else {
				heap.Fix(&c.timers, t.index)
			}
			return wasActive
		},
	}
}

// NewTicker implements Clock.
func (c *SimClock) NewTicker(d time.Duration) *Ticker {
	if d <= 0 {
		d = time.Nanosecond
	}
	ch := make(chan time.Time, 1)
	t := &simTimer{ch: ch, period: d}
	c.mu.Lock()
	t.deadline = c.now.Add(d)
	t.seq = c.seq
	c.seq++
	heap.Push(&c.timers, t)
	c.mu.Unlock()
	c.signal()
	return &Ticker{
		C: ch,
		stop: func() {
			c.mu.Lock()
			t.stopped = true
			c.mu.Unlock()
		},
		reset: func(nd time.Duration) {
			if nd <= 0 {
				nd = time.Nanosecond
			}
			c.mu.Lock()
			t.stopped = false
			t.period = nd
			t.deadline = c.now.Add(nd)
			t.seq = c.seq
			c.seq++
			if !t.inHeap {
				heap.Push(&c.timers, t)
			} else {
				heap.Fix(&c.timers, t.index)
			}
			c.mu.Unlock()
			c.signal()
		},
	}
}

// AfterFunc implements Clock. f runs synchronously in deadline order when its
// deadline is reached (see the SimClock firing rules).
func (c *SimClock) AfterFunc(d time.Duration, f func()) *Timer {
	ch := make(chan time.Time, 1)
	t := &simTimer{ch: ch, fn: f}
	c.mu.Lock()
	t.deadline = c.now.Add(d)
	t.seq = c.seq
	c.seq++
	heap.Push(&c.timers, t)
	c.mu.Unlock()
	c.signal()
	return &Timer{
		C: nil, // mirrors time.AfterFunc: no channel, the callback is the signal
		stop: func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if t.stopped || t.done {
				return false
			}
			t.stopped = true
			return true
		},
		reset: func(nd time.Duration) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if nd <= 0 {
				nd = time.Nanosecond
			}
			wasActive := !t.stopped && !t.done
			t.stopped = false
			t.done = false
			t.deadline = c.now.Add(nd)
			t.seq = c.seq
			c.seq++
			if !t.inHeap {
				heap.Push(&c.timers, t)
			} else {
				heap.Fix(&c.timers, t.index)
			}
			return wasActive
		},
	}
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// run is the driver goroutine: it is the only thing that advances virtual time
// on its own, and it does so only when a timer is armed.
func (c *SimClock) run() {
	defer c.wg.Done()
	var jumps int
	for {
		c.mu.Lock()
		next := c.peekLocked()
		if next == nil {
			c.mu.Unlock()
			select {
			case <-c.wake:
				continue
			case <-c.closed:
				return
			}
		}
		if !c.driven {
			// Manual mode: virtual time moves only under Advance/AdvanceTo.
			c.mu.Unlock()
			select {
			case <-c.wake:
				continue
			case <-c.closed:
				return
			}
		}
		wait := next.deadline.Sub(c.now)
		realWait := time.Duration(0)
		if !c.auto {
			if wait > 0 {
				realWait = time.Duration(float64(wait) / c.scale)
			}
			if realWait <= 0 {
				realWait = time.Millisecond
			}
		} else if jumps >= autoYieldEvery {
			// Skip mode: yield the CPU periodically so a ticker loop cannot
			// starve every other goroutine in the process.
			jumps = 0
			realWait = time.Millisecond
		}
		deadline := next.deadline
		c.mu.Unlock()

		if realWait > 0 {
			rt := time.NewTimer(realWait)
			select {
			case <-rt.C:
			case <-c.wake:
				rt.Stop()
				continue
			case <-c.closed:
				rt.Stop()
				return
			}
		}

		c.mu.Lock()
		if !c.now.Before(deadline) {
			// Somebody else advanced past this deadline already.
			c.mu.Unlock()
			jumps++
			continue
		}
		due := c.advanceToLocked(deadline)
		c.mu.Unlock()
		c.fire(due)
		jumps++
	}
}

// advanceLocked adds d to virtual now and drains every timer that became due,
// in (deadline, seq) order. Tickers are re-armed. Callers must hold c.mu.
func (c *SimClock) advanceLocked(d time.Duration) []*simTimer {
	return c.advanceToLocked(c.now.Add(d))
}

// advanceToLocked sets virtual now to t (never backwards) and drains every
// timer due at t. Callers must hold c.mu.
func (c *SimClock) advanceToLocked(t time.Time) []*simTimer {
	if t.After(c.now) {
		c.now = t
	}
	var due []*simTimer
	for {
		next := c.peekLocked()
		if next == nil || next.deadline.After(c.now) {
			break
		}
		heap.Pop(&c.timers)
		next.done = true
		due = append(due, next)
		if next.stopped || next.period <= 0 {
			continue
		}
		reArm := next.deadline.Add(next.period)
		if c.now.Sub(reArm) > next.period*maxTickerCatchUp {
			// The jump skipped more periods than we are willing to replay:
			// resync the ticker to now+period (stdlib drops the backlog too).
			reArm = c.now.Add(next.period)
		}
		next.deadline = reArm
		next.seq = c.seq
		c.seq++
		next.done = false
		next.stopped = false
		heap.Push(&c.timers, next)
	}
	return due
}

// peekLocked discards stopped/done heap entries and returns the earliest armed
// timer, or nil. Callers must hold c.mu.
func (c *SimClock) peekLocked() *simTimer {
	for len(c.timers) > 0 {
		top := c.timers[0]
		if top.stopped || top.done {
			heap.Pop(&c.timers)
			continue
		}
		return top
	}
	return nil
}

// fire dispatches the drained timers outside the clock lock so a callback that
// reads the clock cannot deadlock the driver.
func (c *SimClock) fire(due []*simTimer) {
	if len(due) == 0 {
		return
	}
	c.fireMu.Lock()
	defer c.fireMu.Unlock()
	for _, t := range due {
		if t.stopped {
			continue
		}
		at := t.deadline
		if t.fn != nil {
			// Synchronous, in dispatch order: a deterministic simulator must
			// let a test observe the effects of a callback the moment Advance
			// returns. Callbacks may READ the clock (Now/Since) but must not
			// call Advance/AdvanceTo or Sleep on it — that would re-enter the
			// dispatcher.
			t.fn()
			continue
		}
		select {
		case t.ch <- at:
		default:
			// Buffered(1) and nobody reading: drop, exactly like stdlib.
		}
	}
}

// signal pokes the driver without ever blocking.
func (c *SimClock) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// timer heap
// ---------------------------------------------------------------------------

type timerHeap []*simTimer

func (h timerHeap) Len() int { return len(h) }

func (h timerHeap) Less(i, j int) bool {
	if h[i].deadline.Equal(h[j].deadline) {
		return h[i].seq < h[j].seq
	}
	return h[i].deadline.Before(h[j].deadline)
}

func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *timerHeap) Push(x any) {
	t := x.(*simTimer)
	t.index = len(*h)
	t.inHeap = true
	*h = append(*h, t)
}

func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	t.index = -1
	t.inHeap = false
	return t
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
