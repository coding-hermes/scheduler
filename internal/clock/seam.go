package clock

import "sync/atomic"

// Seam is a race-safe holder for one component's Clock.
//
// Its zero value reads as the WALL CLOCK, so a component is correct before
// SetClock is ever called, and a test may swap in a simulator at any point
// without a data race. Embedding the seam (rather than a bare Clock field)
// is what keeps a nil clock from ever meaning "no time": Get never returns
// nil, and Set(nil) is a no-op so a nil argument cannot silently move a
// component onto a simulated clock.
type Seam struct{ v atomic.Value }

// Set installs c as the component's clock. nil keeps the current seam.
func (s *Seam) Set(c Clock) {
	if c == nil {
		return
	}
	s.v.Store(c)
}

// Get returns the installed clock, or the wall clock when none was installed.
func (s *Seam) Get() Clock {
	if c, ok := s.v.Load().(Clock); ok && c != nil {
		return c
	}
	return Real()
}

// Installed reports whether an explicit clock was installed.
func (s *Seam) Installed() bool {
	_, ok := s.v.Load().(Clock)
	return ok
}
