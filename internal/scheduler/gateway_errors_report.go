package scheduler

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// SCHED-GAP-1654 — the aggregation half of the gateway-availability surface.
//
// noteGatewayAvailabilityError (spawn.go) writes one event per tick lost to
// gateway unavailability (component gateway_availability, class in the
// details' error_class field). This file is the single COUNT of those rows,
// shared by every reader surface so their numbers cannot drift:
//
//   - GET /api/v1/gateway-errors (internal/api/gateway_errors.go) serves it
//     as JSON, and
//   - the gateway_errors MCP tool (internal/mcp/handlers.go) returns the same
//     JSON to AI agents — the CTL-003 parity guard requires a data route to
//     ship with a tool, and two independent COUNT queries would be two
//     chances to disagree.
//
// No new table, no new column: the events table is already the durable audit
// surface (and is published live through /api/v1/events/stream, CTL-002), so
// the availability history is queryable by class/project with json_extract
// and survives restarts — which a process-lifetime counter would not.

// GatewayErrorsWindow is the fixed reporting window. 24h matches the operator
// question this surface was built for ("what did the gateway cost us
// overnight?"), and a fixed window keeps the numbers comparable between two
// reads — a caller-supplied range would make "is this worse than yesterday"
// unanswerable from the output alone. The response states the window and the
// exact cutoff it applied.
const GatewayErrorsWindow = 24 * time.Hour

// GatewayErrorProjectCount is one project's row of GatewayErrorsReport.
type GatewayErrorProjectCount struct {
	Project string           `json:"project"`
	Total   int64            `json:"total"`
	ByClass map[string]int64 `json:"by_class"`
}

// GatewayErrorsReport is the aggregated availability-loss answer, identical
// on the REST wire and in the MCP tool output.
type GatewayErrorsReport struct {
	GeneratedAt string                     `json:"generated_at"`
	WindowHours int                        `json:"window_hours"`
	Cutoff      string                     `json:"cutoff"`
	Total       int64                      `json:"total"`
	ByClass     map[string]int64           `json:"by_class"`
	ByProject   []GatewayErrorProjectCount `json:"by_project"`
}

// GatewayErrorsReport builds the availability-loss report for the window
// ending at now (UTC), counting gateway_availability events in [cutoff, now).
//
// Every known class is present in ByClass (and in each row's ByClass) even at
// zero — a reader never has to distinguish "no losses" from a missing field —
// and ByProject is ordered by total desc, then project name asc, so two reads
// of the same window render identically. An empty window is a legitimate
// answer: total 0 with the three classes at 0 and ByProject an empty
// (non-nil) slice.
//
// A DB failure surfaces as the error; a counting surface must never answer
// 200 with fabricated zeros.
func BuildGatewayErrorsReport(ctx context.Context, db *sql.DB, now time.Time) (*GatewayErrorsReport, error) {
	now = now.UTC()
	cutoff := now.Add(-GatewayErrorsWindow)

	byClass := map[string]int64{}
	for _, class := range GatewayErrorClassOrder {
		byClass[class] = 0
	}
	perProject := map[string]map[string]int64{}

	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(json_extract(details, '$.project'), '') AS project,
       COALESCE(json_extract(details, '$.error_class'), '') AS error_class,
       COUNT(*)
FROM events
WHERE component = ?
  AND julianday(created_at) >= julianday(?)
GROUP BY project, error_class`,
		GatewayAvailabilityEventComponent, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var total int64
	for rows.Next() {
		var project, class string
		var n int64
		if err := rows.Scan(&project, &class, &n); err != nil {
			return nil, err
		}
		if class == "" {
			// Defensive: the component is written only by the spawn path's
			// classifier, so a class-less row is not a shape this report
			// produced. Name it rather than silently dropping the count.
			class = "unknown"
		}
		if project == "" {
			// Also defensive, same rationale: the emitter always carries the
			// lane name. An honest label beats inventing one.
			project = "(unattributed)"
		}
		byClass[class] += n
		if perProject[project] == nil {
			perProject[project] = map[string]int64{}
		}
		perProject[project][class] += n
		total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]GatewayErrorProjectCount, 0, len(perProject))
	for project, classes := range perProject {
		row := GatewayErrorProjectCount{Project: project, ByClass: map[string]int64{}}
		for class, n := range classes {
			row.ByClass[class] = n
			row.Total += n
		}
		// Make every known class explicit per project too, so a lane with
		// only 503s still shows rate_limited_429: 0 rather than an absent key.
		for _, class := range GatewayErrorClassOrder {
			if _, ok := row.ByClass[class]; !ok {
				row.ByClass[class] = 0
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Project < out[j].Project
	})

	return &GatewayErrorsReport{
		GeneratedAt: now.Format(time.RFC3339),
		WindowHours: int(GatewayErrorsWindow.Hours()),
		Cutoff:      cutoff.Format(time.RFC3339),
		Total:       total,
		ByClass:     byClass,
		ByProject:   out,
	}, nil
}
