package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EffectiveCadenceTarget resolves derived-first cadence intent. An explicit
// override wins; zero explicitly opts out. Without an override, a durable
// cooldown pin is operator configuration and derives 86400/pin runs/day.
func EffectiveCadenceTarget(p Project) (float64, string, bool) {
	if p.TargetRunsPerDay != nil {
		if *p.TargetRunsPerDay <= 0 {
			return 0, "disabled", false
		}
		return *p.TargetRunsPerDay, "override", true
	}
	if p.CooldownPinS == nil || *p.CooldownPinS <= 0 {
		return 0, "none", false
	}
	return 86400 / float64(*p.CooldownPinS), "cooldown_pin", true
}

// LoadCadenceRates returns achieved starts/day by lane over window. Spawned
// ticks count as real runs whether they later complete, fail, or time out.
func LoadCadenceRates(ctx context.Context, db *sql.DB, now time.Time, window time.Duration) (map[string]float64, error) {
	if window <= 0 {
		return nil, fmt.Errorf("cadence window must be positive")
	}
	rows, err := db.QueryContext(ctx, `
SELECT project_name, COUNT(*)
FROM ticks
WHERE spawned_at IS NOT NULL AND spawned_at >= ?
GROUP BY project_name`, now.Add(-window).Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("load cadence rates: %w", err)
	}
	defer rows.Close()

	rates := make(map[string]float64)
	days := window.Hours() / 24
	for rows.Next() {
		var name string
		var runs int
		if err := rows.Scan(&name, &runs); err != nil {
			return nil, fmt.Errorf("scan cadence rate: %w", err)
		}
		rates[name] = float64(runs) / days
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cadence rates: %w", err)
	}
	return rates, nil
}
