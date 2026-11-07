package database

import "fmt"

// SCHED-GAP-17000 — dry-run throttle constants.
//
// These live in the database package because three surfaces need the SAME
// numbers and must not drift: the scheduler (which suspends on them), the config
// loader (which validates a fleet.toml value), and the API (which validates a
// PUT). scheduler and config both import database; neither imports the other.
const (
	// DryRunWarnStreak is where "do something" starts: five consecutive
	// no-artifact ticks is a lane being spawned for nothing, not a lane
	// having one bad tick.
	DryRunWarnStreak = 5

	// DefaultDryRunThrottle is the suspension threshold when a project sets
	// no dry_run_throttle of its own. Ten is the owner's number.
	DefaultDryRunThrottle = 10
)

// DryRunThrottleSettingError validates an operator-supplied throttle. Zero is
// legal (means "use the default"); anything below the warn streak is not,
// because a suspension that fires before its own warning line teaches an
// operator nothing except that the scheduler is erratic.
func DryRunThrottleSettingError(cfg int) error {
	if cfg == 0 {
		return nil
	}
	if cfg < DryRunWarnStreak {
		return fmt.Errorf("dry_run_throttle must be 0 (default %d) or >= %d (the warn streak); got %d — a throttle below the warning line suspends a lane with no warning",
			DefaultDryRunThrottle, DryRunWarnStreak, cfg)
	}
	return nil
}

// EffectiveDryRunThrottle resolves the stored value to the threshold in force.
func EffectiveDryRunThrottle(cfg int) int {
	if cfg >= DryRunWarnStreak {
		return cfg
	}
	return DefaultDryRunThrottle
}
