package scheduler

import (
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// CadenceWindow is the achieved-rate measurement horizon. Seven days keeps a
// weekly lane measurable while smoothing one-day tick-duration noise.
const CadenceWindow = 7 * 24 * time.Hour

// cadenceBoostUrgency is above ordinary pending/bump work but below the hard
// starvation guarantee. It changes ordering only; every existing cooldown,
// namespace, budget, load, and concurrency gate still applies.
const cadenceBoostUrgency = bumpBoostUrgency + 1e6

// effectiveCadenceTarget resolves derived-first cadence intent. An explicit
// override wins; zero explicitly opts out. Without an override, a durable
// cooldown pin is operator configuration and derives 86400/pin runs/day.
// A lane with neither has no cadence opinion and preserves legacy ordering.
func effectiveCadenceTarget(p database.Project) (float64, bool) {
	target, _, ok := database.EffectiveCadenceTarget(p)
	return target, ok
}

func cadenceAdjustedUrgency(current float64, p database.Project, achieved float64) float64 {
	target, ok := effectiveCadenceTarget(p)
	if !ok || achieved >= target || current >= starvationBoostUrgency {
		return current
	}
	deficit := (target - achieved) / target
	boosted := cadenceBoostUrgency + deficit*1e5
	if boosted > current {
		return boosted
	}
	return current
}
