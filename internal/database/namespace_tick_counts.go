package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LoadNamespaceTickCounts returns completed-tick counts per namespace over the
// trailing window ending at now (SCHED-GAP-1686). Ticks are joined to their
// lane's namespace through projects; a lane with no namespace is excluded (it
// is not governed by a namespace ceiling). A tick counts as consumption once
// it COMPLETED within the window — a queued or still-running tick has a NULL
// completed_at and is excluded by the comparison (NULL never satisfies >=).
// now is taken as a parameter (like LoadCadenceRates) so the call site threads
// the loop's single decision instant; no clock read happens here.
func LoadNamespaceTickCounts(ctx context.Context, db *sql.DB, now time.Time, window time.Duration) (map[string]int, error) {
	if window <= 0 {
		return nil, fmt.Errorf("cadence ceiling window must be positive")
	}
	rows, err := db.QueryContext(ctx, `
SELECT p.namespace_id, COUNT(*)
FROM ticks t
JOIN projects p ON p.name = t.project_name
WHERE p.namespace_id IS NOT NULL
  AND t.completed_at IS NOT NULL
  AND t.completed_at >= ?
GROUP BY p.namespace_id`, now.Add(-window).Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("load namespace tick counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var nsID string
		var n int
		if err := rows.Scan(&nsID, &n); err != nil {
			return nil, fmt.Errorf("scan namespace tick count: %w", err)
		}
		counts[nsID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate namespace tick counts: %w", err)
	}
	return counts, nil
}
