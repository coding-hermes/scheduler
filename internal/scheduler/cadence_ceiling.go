package scheduler

import (
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1686 — per-namespace cadence ceiling.
//
// Owner-measured problem (2026-09-30): enabled satellite namespaces were
// over-served against the cadence their own cooldowns imply (pm 2.22x,
// releases 1.97x) while others (docs/review/perf) never ran. Two mechanisms
// lift a namespace above its own cadence — the starvation-guarantee boost and
// board-driven bumps — and neither had a ceiling on the result.
//
// Fix: a namespace may not consume more than CadenceCeilingFactor of its
// cadence-implied demand within CadenceCeilingWindow. Cadence-implied demand
// is the sum over the namespace's enabled lanes of window / max(cooldown_s, 1).
// The ceiling only DEFERS a lane (like every other gate — `continue` before the
// pack); it never drops, penalizes, or consumes cooldown.

const (
	// CadenceCeilingWindow is the rolling window the per-namespace cadence
	// ceiling measures against (SCHED-GAP-1686): 24 hours.
	CadenceCeilingWindow = 24 * time.Hour

	// CadenceCeilingFactor is the multiple of cadence-implied demand a
	// namespace may not exceed within CadenceCeilingWindow (~150%).
	CadenceCeilingFactor = 1.5
)

// Boost-cause vocabulary (SCHED-GAP-1686): which urgency boost set a lane's
// urgency in the scoring loop. The cadence-ceiling event names the cause so
// the over-serving mechanism is visible rather than inferred from a tick
// census.
const (
	BoostCauseOrganic    = "organic"
	BoostCauseStarvation = "starvation"
	BoostCausePending    = "pending"
	BoostCauseBump       = "bump"
)

// cadenceImpliedDemand returns the cadence-implied tick demand for a set of
// enabled lanes over window: the sum over lanes of window / max(cooldown_s, 1).
// A cooldown of 0 (or negative) reads as 1 so the term stays finite.
func cadenceImpliedDemand(lanes []database.Project, window time.Duration) float64 {
	secs := window.Seconds()
	var demand float64
	for _, p := range lanes {
		cd := p.CooldownS
		if cd < 1 {
			cd = 1
		}
		demand += secs / float64(cd)
	}
	return demand
}

// cadenceCeilingFor returns the tick-count ceiling for a namespace: its
// cadence-implied demand scaled by CadenceCeilingFactor.
func cadenceCeilingFor(implied float64) float64 {
	return implied * CadenceCeilingFactor
}

// cadenceCeilingDefers reports whether the cadence ceiling defers a lane,
// given the namespace's trailing-window completed-tick count and the number of
// lanes already selected THIS cycle. The ceiling is a hard cap: a lane is
// deferred once tickCount+selectedCount reaches the ceiling, so a namespace
// can never push its rolling-window consumption past the ceiling in one pack.
func cadenceCeilingDefers(tickCount, selectedCount int, ceiling float64) bool {
	return float64(tickCount+selectedCount) >= ceiling
}

// projectRootName resolves the project-family root of a lane by following
// Parent links to termination. Cycle- and dangling-safe, mirroring
// database.BuildLaneTree: a self-link, a parent that does not resolve in the
// snapshot, or a detected cycle makes the lane its own root.
func projectRootName(name string, parentOf map[string]string) string {
	seen := make(map[string]struct{})
	for {
		if _, dup := seen[name]; dup {
			return name // cycle — stop at the first repeated name
		}
		seen[name] = struct{}{}
		parent, ok := parentOf[name]
		if !ok || parent == "" || parent == name {
			return name
		}
		if _, ok := parentOf[parent]; !ok {
			// The parent does not resolve in this snapshot — this lane is the
			// top of its fragment (BuildLaneTree's dangling root).
			return name
		}
		name = parent
	}
}
