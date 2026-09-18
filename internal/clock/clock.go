// Package clock is the single time choke point of the scheduler daemon.
//
// Every clock read or wait in non-test code under internal/ and cmd/ goes
// through a Clock value obtained from this package — never through the stdlib
// time package directly (time.Parse, time.Duration arithmetic and time.Date
// construction are not clock reads and stay direct). The static guard in
// stdlib_guard_test.go fails the build if a stdlib clock call reappears
// outside this package.
//
// Injection is by constructor or by setter, one Clock per long-lived
// component (Loop, Spawner, SlotPool, dashboard Generator, DuckBrain sync
// client, API server, DB helpers). There is deliberately NO package-level
// mutable default clock: two components (or two tests in one binary) must be
// able to hold different clocks.
//
// Three implementations exist:
//
//   - RealClock (Real()) — stdlib delegation. The production default; behavior
//     is byte-identical to the pre-seam direct time.Now()/time.Sleep() calls.
//   - SimClock (NewSimClock) — a virtual clock whose time is decoupled from the
//     wall clock: it can run at 10x/100x/1000x (Scale), be jumped forward
//     deterministically (Advance/AdvanceTo), or skip straight to the next armed
//     timer (SetAutoAdvance, WaitForNextTimer). This is the test-time simulator
//     and it is only ever installed through an explicit opt-in (FromEnv with
//     SCHEDULER_TIME_MODE=sim, or a test constructing one directly).
//   - FixedClock (NewFixed) — a frozen decision instant with real waits. This
//     is the ADV-R04/G6 determinism seam used by the existing clock-seam tests.
package clock

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment contract. Sim mode is a FATAL boot error unless it was
// explicitly requested by SCHEDULER_TIME_MODE=sim, so a stray variable can
// never silently put the production daemon on a fake clock.
const (
	// EnvMode selects the clock implementation: ModeReal (default) or ModeSim.
	EnvMode = "SCHEDULER_TIME_MODE"
	// EnvScale is the sim clock's speed multiplier (10 = 10x, 1000 = 1000x).
	EnvScale = "SCHEDULER_TIME_SCALE"
	// EnvStart is the sim clock's starting instant (RFC3339). Defaults to now.
	EnvStart = "SCHEDULER_TIME_START"
	// EnvAutoAdvance enables skip-to-next-armed-timer ("1"/"true").
	EnvAutoAdvance = "SCHEDULER_TIME_AUTOADVANCE"

	// ModeReal runs on the wall clock (production).
	ModeReal = "real"
	// ModeSim runs on the virtual simulator clock (tests / --simulate).
	ModeSim = "sim"

	// DefaultScale is the sim clock speed when EnvScale is unset.
	DefaultScale = 1.0
)

// Clock is the single seam through which a component reads and waits on time.
//
// Implementations must be safe for concurrent use by multiple goroutines:
// the daemon reads time from evaluation loops, HTTP handlers, tick goroutines
// and the dashboard renderer concurrently.
type Clock interface {
	// Now returns the current instant.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
	// Until returns the duration until t (negative if t has passed).
	Until(t time.Time) time.Duration
	// Sleep blocks for d.
	Sleep(d time.Duration)
	// After returns a channel that receives the current time after d.
	After(d time.Duration) <-chan time.Time
	// NewTimer returns a one-shot Timer.
	NewTimer(d time.Duration) *Timer
	// NewTicker returns a Ticker firing every d.
	NewTicker(d time.Duration) *Ticker
	// AfterFunc waits for d then calls f in its own goroutine.
	AfterFunc(d time.Duration, f func()) *Timer
}

// Timer is the clock-agnostic timer handle. C behaves exactly like
// time.Timer.C; Stop and Reset are nil-safe so a zero Timer can be used as a
// "no timer" sentinel.
type Timer struct {
	// C receives the firing instant.
	C <-chan time.Time

	stop  func() bool
	reset func(time.Duration) bool
}

// Stop prevents the Timer from firing. It reports whether the call stopped an
// armed timer (false if it had already fired or been stopped).
func (t *Timer) Stop() bool {
	if t == nil || t.stop == nil {
		return false
	}
	return t.stop()
}

// Reset arms the Timer to fire after d, reporting whether it was armed.
func (t *Timer) Reset(d time.Duration) bool {
	if t == nil || t.reset == nil {
		return false
	}
	return t.reset(d)
}

// Ticker is the clock-agnostic ticker handle. C behaves exactly like
// time.Ticker.C; Stop and Reset are nil-safe.
type Ticker struct {
	// C delivers ticks.
	C <-chan time.Time

	stop  func()
	reset func(time.Duration)
}

// Stop turns off the Ticker.
func (t *Ticker) Stop() {
	if t == nil || t.stop == nil {
		return
	}
	t.stop()
}

// Reset stops the Ticker and resets its period to d.
func (t *Ticker) Reset(d time.Duration) {
	if t == nil || t.reset == nil {
		return
	}
	t.reset(d)
}

// describe is implemented by the concrete clocks so boot can log exactly which
// clock it is running on.
type describe interface {
	Describe() string
}

// Describe renders the clock mode for the boot log line, e.g.
// "real" or "sim (scale=1000 start=… auto=false)". Unknown implementations
// report "custom" so an operator can never mistake one for the default.
func Describe(c Clock) string {
	if d, ok := c.(describe); ok {
		return d.Describe()
	}
	return "custom"
}

// ---------------------------------------------------------------------------
// RealClock
// ---------------------------------------------------------------------------

// RealClock reads and waits on the wall clock via the stdlib time package.
// It is the production default and its behavior is identical to the direct
// time.Now()/time.Since()/time.Sleep() calls it replaced.
type RealClock struct{}

// Real returns the production clock.
func Real() Clock { return RealClock{} }

// Describe implements describe.
func (RealClock) Describe() string { return ModeReal }

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// Since implements Clock.
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Until implements Clock.
func (RealClock) Until(t time.Time) time.Duration { return time.Until(t) }

// Sleep implements Clock.
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// After implements Clock.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer implements Clock.
func (RealClock) NewTimer(d time.Duration) *Timer {
	t := time.NewTimer(d)
	return &Timer{
		C:     t.C,
		stop:  t.Stop,
		reset: t.Reset,
	}
}

// NewTicker implements Clock.
func (RealClock) NewTicker(d time.Duration) *Ticker {
	t := time.NewTicker(d)
	return &Ticker{
		C:     t.C,
		stop:  t.Stop,
		reset: t.Reset,
	}
}

// AfterFunc implements Clock.
func (RealClock) AfterFunc(d time.Duration, f func()) *Timer {
	t := time.AfterFunc(d, f)
	return &Timer{
		C:     t.C,
		stop:  t.Stop,
		reset: t.Reset,
	}
}

// ---------------------------------------------------------------------------
// FixedClock
// ---------------------------------------------------------------------------

// fixedClock freezes Now at one instant (the ADV-R04/G6 decision-instant seam)
// while waits stay real. It preserves the exact semantics the existing
// clock-seam tests rely on: evaluate() sees a constant instant, so cooldown
// arithmetic is deterministic, and nothing else changes behavior.
type fixedClock struct {
	at   time.Time
	real Clock
}

// NewFixed returns a clock whose Now is pinned to at and whose waits, timers
// and tickers delegate to the real wall clock.
func NewFixed(at time.Time) Clock {
	return fixedClock{at: at, real: Real()}
}

// Describe implements describe.
func (c fixedClock) Describe() string { return "fixed " + c.at.UTC().Format(time.RFC3339) }

// Now implements Clock.
func (c fixedClock) Now() time.Time { return c.at }

// Since implements Clock. The fixed instant is the "now" side of the
// subtraction, mirroring stdlib time.Since against a frozen clock.
func (c fixedClock) Since(t time.Time) time.Duration { return c.at.Sub(t) }

// Until implements Clock.
func (c fixedClock) Until(t time.Time) time.Duration { return t.Sub(c.at) }

// Sleep implements Clock.
func (c fixedClock) Sleep(d time.Duration) { c.real.Sleep(d) }

// After implements Clock.
func (c fixedClock) After(d time.Duration) <-chan time.Time { return c.real.After(d) }

// NewTimer implements Clock.
func (c fixedClock) NewTimer(d time.Duration) *Timer { return c.real.NewTimer(d) }

// NewTicker implements Clock.
func (c fixedClock) NewTicker(d time.Duration) *Ticker { return c.real.NewTicker(d) }

// AfterFunc implements Clock.
func (c fixedClock) AfterFunc(d time.Duration, f func()) *Timer { return c.real.AfterFunc(d, f) }

// ---------------------------------------------------------------------------
// FromEnv
// ---------------------------------------------------------------------------

// FromEnv resolves the process clock from the environment.
//
//	SCHEDULER_TIME_MODE        real (default) | sim
//	SCHEDULER_TIME_SCALE       positive float, default 1.0 (sim only)
//	SCHEDULER_TIME_START       RFC3339 instant (sim only, default now)
//	SCHEDULER_TIME_AUTOADVANCE 0|1, default 0 (sim only)
//
// It FAILS CLOSED: an unparseable mode, a non-positive scale, a bad start
// instant or a bad autoadvance token is an error, not a silent fallback to
// the wall clock. Callers at a boot boundary must treat the error as fatal.
func FromEnv() (Clock, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvMode)))
	switch mode {
	case "", ModeReal:
		// Real mode ignores the sim-only variables on purpose: they have no
		// meaning on the wall clock, and reading them here would only invite
		// a half-configured sim.
		return Real(), nil
	case ModeSim:
		scale := DefaultScale
		if raw := strings.TrimSpace(os.Getenv(EnvScale)); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("clock: %s=%q: %w", EnvScale, raw, err)
			}
			if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
				return nil, fmt.Errorf("clock: %s=%q: must be a positive finite multiplier", EnvScale, raw)
			}
			scale = v
		}
		start := time.Now()
		if raw := strings.TrimSpace(os.Getenv(EnvStart)); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Errorf("clock: %s=%q: want RFC3339: %w", EnvStart, raw, err)
			}
			start = t
		}
		auto := false
		if raw := strings.TrimSpace(os.Getenv(EnvAutoAdvance)); raw != "" {
			v, err := parseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("clock: %s=%q: %w", EnvAutoAdvance, raw, err)
			}
			auto = v
		}
		sc := NewSimClockAt(scale, start)
		sc.SetAutoAdvance(auto)
		return sc, nil
	default:
		return nil, fmt.Errorf("clock: %s=%q: want %q or %q", EnvMode, mode, ModeReal, ModeSim)
	}
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("want 0/1 or true/false")
	}
}
