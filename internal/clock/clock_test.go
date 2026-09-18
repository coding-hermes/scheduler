package clock

import (
	"os"
	"testing"
	"time"
)

// TestRealClock_DelegatesToStdlib pins that the production clock is a faithful
// stdlib delegation — a divergence here would silently change daemon timing.
func TestRealClock_DelegatesToStdlib(t *testing.T) {
	c := Real()
	if got, want := Describe(c), ModeReal; got != want {
		t.Fatalf("Describe(Real()) = %q, want %q", got, want)
	}

	base := time.Now()
	if d := c.Now().Sub(base); d < -time.Second || d > time.Second {
		t.Fatalf("Real().Now() = %v, want within a second of the wall clock %v", c.Now(), base)
	}
	if d := c.Since(base); d < 0 {
		t.Fatalf("Real().Since(past) = %v, want >= 0", d)
	}
	if d := c.Until(base.Add(time.Hour)); d <= 0 || d > time.Hour {
		t.Fatalf("Real().Until(now+1h) = %v, want (0, 1h]", d)
	}

	// After / NewTimer / AfterFunc / Sleep / NewTicker must all really wait.
	select {
	case <-c.After(10 * time.Millisecond):
	case <-time.After(2 * time.Second):
		t.Fatal("Real().After(10ms) never fired")
	}
	tm := c.NewTimer(10 * time.Millisecond)
	select {
	case <-tm.C:
	case <-time.After(2 * time.Second):
		t.Fatal("Real().NewTimer(10ms) never fired")
	}
	if tm.Stop() {
		t.Fatal("Stop() on an already-fired Real timer returned true")
	}
	fired := make(chan struct{}, 1)
	c.AfterFunc(10*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("Real().AfterFunc(10ms) never ran")
	}
	start := time.Now()
	c.Sleep(20 * time.Millisecond)
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("Real().Sleep returned immediately")
	}
	tk := c.NewTicker(10 * time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C:
	case <-time.After(2 * time.Second):
		t.Fatal("Real().NewTicker(10ms) never ticked")
	}
}

// TestFixedClock_PinsNowKeepsRealWaits pins the ADV-R04/G6 semantics the
// clock-seam tests depend on: Now is frozen at the constructed instant while
// every wait stays real.
func TestFixedClock_PinsNowKeepsRealWaits(t *testing.T) {
	at := start462()
	c := NewFixed(at)

	if got := c.Now(); !got.Equal(at) {
		t.Fatalf("fixed Now() = %v, want %v", got, at)
	}
	if got := c.Since(at.Add(-90 * time.Second)); got != 90*time.Second {
		t.Fatalf("fixed Since(at-90s) = %v, want 90s", got)
	}
	if got := c.Until(at.Add(45 * time.Second)); got != 45*time.Second {
		t.Fatalf("fixed Until(at+45s) = %v, want 45s", got)
	}
	select {
	case <-c.After(5 * time.Millisecond):
	case <-time.After(2 * time.Second):
		t.Fatal("fixed clock's After did not use the real clock")
	}
	if got := Describe(c); got != "fixed "+at.UTC().Format(time.RFC3339) {
		t.Fatalf("Describe(fixed) = %q", got)
	}
}

// TestFromEnv_RealMode covers the production default: unset and explicit real
// both yield the wall clock, and the sim-only variables are ignored.
func TestFromEnv_RealMode(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{EnvMode: "real"},
		{EnvMode: "REAL"},
		{EnvMode: " real "},
		// A half-configured sim must not leak into real mode.
		{EnvMode: "real", EnvScale: "not-a-number", EnvStart: "nonsense", EnvAutoAdvance: "maybe"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		c, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv(%v) = %v, want the real clock", env, err)
		}
		if _, ok := c.(RealClock); !ok {
			t.Fatalf("FromEnv(%v) = %T, want RealClock", env, c)
		}
		if got := Describe(c); got != ModeReal {
			t.Fatalf("Describe = %q, want %q", got, ModeReal)
		}
	}
}

// TestFromEnv_SimMode covers the simulator contract: scale, start instant and
// skip-forward are all read from the environment, with sane defaults.
func TestFromEnv_SimMode(t *testing.T) {
	t.Setenv(EnvMode, ModeSim)
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv(sim) = %v", err)
	}
	sim, ok := c.(*SimClock)
	if !ok {
		t.Fatalf("FromEnv(sim) = %T, want *SimClock", c)
	}
	defer sim.Close()
	if got := sim.Scale(); got != DefaultScale {
		t.Fatalf("default scale = %v, want %v", got, DefaultScale)
	}
	if sim.AutoAdvance() {
		t.Fatal("auto-advance must default OFF")
	}
	if !sim.Driven() {
		t.Fatal("env sim clock must be driver-enabled (scaled real waits)")
	}
	if got := time.Since(sim.Now()); got > time.Minute || got < -time.Minute {
		t.Fatalf("default start = %v, want ~now", sim.Now())
	}

	t.Setenv(EnvScale, "1000")
	t.Setenv(EnvStart, "2031-05-04T03:02:01Z")
	t.Setenv(EnvAutoAdvance, "1")
	c2, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv(sim, explicit) = %v", err)
	}
	sim2 := c2.(*SimClock)
	defer sim2.Close()
	if sim2.Scale() != 1000 {
		t.Fatalf("scale = %v, want 1000", sim2.Scale())
	}
	if !sim2.Now().Equal(start462()) {
		t.Fatalf("start = %v, want %v", sim2.Now(), start462())
	}
	if !sim2.AutoAdvance() {
		t.Fatal("auto-advance should be on with SCHEDULER_TIME_AUTOADVANCE=1")
	}
	if got := Describe(sim2); got == ModeReal {
		t.Fatalf("Describe(sim) = %q, must identify sim mode for the boot log", got)
	}
}

// TestFromEnv_FailsClosed pins the doctrine that a stray/typo'd variable is a
// boot error rather than a silent fallback to the wall clock.
func TestFromEnv_FailsClosed(t *testing.T) {
	cases := []map[string]string{
		{EnvMode: "simulation"},
		{EnvMode: "SIM"}, // accepted (case-insensitive) — checked separately below
		{EnvMode: ModeSim, EnvScale: "zero"},
		{EnvMode: ModeSim, EnvScale: "0"},
		{EnvMode: ModeSim, EnvScale: "-10"},
		{EnvMode: ModeSim, EnvStart: "yesterday"},
		{EnvMode: ModeSim, EnvAutoAdvance: "sometimes"},
	}
	for _, env := range cases {
		for k, v := range env {
			t.Setenv(k, v)
		}
		c, err := FromEnv()
		if env[EnvMode] == "SIM" {
			if err != nil {
				t.Fatalf("FromEnv(%v) = %v, want case-insensitive acceptance", env, err)
			}
			if sim, ok := c.(*SimClock); ok {
				sim.Close()
			}
			os.Unsetenv(EnvMode)
			continue
		}
		if err == nil {
			t.Fatalf("FromEnv(%v) = %T, want a hard error (fail closed)", env, c)
		}
		os.Unsetenv(EnvMode)
		os.Unsetenv(EnvScale)
		os.Unsetenv(EnvStart)
		os.Unsetenv(EnvAutoAdvance)
	}
}

// TestDescribe_UnknownImplementation reports "custom" so an operator can never
// mistake a foreign clock for the default.
func TestDescribe_UnknownImplementation(t *testing.T) {
	if got := Describe(customClock{Real()}); got != "custom" {
		t.Fatalf("Describe(custom) = %q, want \"custom\"", got)
	}
}

// customClock satisfies Clock without implementing describe — the shape a
// third-party/test clock has.
type customClock struct{ Clock }
