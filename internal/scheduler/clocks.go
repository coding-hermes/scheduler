package scheduler

import "github.com/coding-hermes/scheduler/internal/clock"

// clockSeam is this package's name for the shared, race-safe clock holder
// (SCHED-GAP-169): the zero value reads as the WALL CLOCK, so a component works
// before SetClock is ever called and a test can swap in a simulator at any
// point without a data race. There is deliberately no package-level clock: each
// component owns its own seam, and Loop.SetClock propagates one clock to every
// component it owns so a whole tree can be driven by a single simulator.
type clockSeam = clock.Seam

// Clock returns the loop's clock, never nil (the zero value of the seam reads
// as the wall clock). Surfaces that must share the loop's timeline — the API
// server's uptime, the dashboard's generated_at stamps — read it here instead
// of holding a second, unsynchronized seam.
func (l *Loop) Clock() clock.Clock { return l.clock() }
