package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1755 — per-family stall census (AC 2).
//
// The per-turn /v1/responses "stalled: no progress" class is counted PER LANE
// FAMILY so an operator can see which class of lane the deadline is failing and
// what deadline that family actually carries. The census answers, in one
// object on /api/v1/health:
//
//	how many ticks the family ran in the window        (ticks)
//	how many of them failed as "stalled"              (stalled_24h)
//	that ratio                                        (stall_rate)
//	the deadline in force for the family              (deadline/deadline_ms)
//
// The deadline column is the SCHED-GAP-1755 policy resolution
// (scheduler.StallFamilyPolicies), NOT a guess from the tick rows: a family
// whose deadline is disarmed reports "exempt" even if an ancient tick of that
// family once stalled. Families with no ticks in the window are still listed —
// the policy is the load-bearing half of the answer.

// stallCensusWindowHours is the rolling window the census counts over. The
// acceptance criterion is stated as a 24h window, so the field is fixed here
// rather than plumbed as a knob.
const stallCensusWindowHours = 24

// StallFamilyCensus is one family's row in the census.
type StallFamilyCensus struct {
	Family string `json:"family"`
	// Ticks is every tick the family's lanes spawned in the window.
	Ticks int `json:"ticks"`
	// Stalled24h counts ticks whose error carries the "stalled: no progress"
	// class (the per-turn deadline), the exact predicate the row's AC 3
	// quotes from ticks.error.
	Stalled24h int `json:"stalled_24h"`
	// StallRate is Stalled24h / Ticks (0 when the family ran nothing).
	StallRate float64 `json:"stall_rate"`
	// Deadline is the per-turn deadline in force for the family ("exempt" when
	// the policy disarms it), DeadlineMS the same value in milliseconds.
	Deadline   string `json:"deadline"`
	DeadlineMS int64  `json:"deadline_ms"`
	// Exempt mirrors the policy: true = no per-turn deadline for this family.
	Exempt bool `json:"exempt"`
	// Heartbeat is true when the liveness heartbeat channel backs the
	// deadline (SCHED-GAP-1755: always armed; it is the second supervisor).
	Heartbeat bool `json:"heartbeat"`
}

// StallCensus is the /api/v1/health "stalls_24h" block.
type StallCensus struct {
	WindowHours int                 `json:"window_hours"`
	Total       int                 `json:"total"`
	ByFamily    []StallFamilyCensus `json:"by_family"`
	Error       string              `json:"error,omitempty"`
}

// computeStallCensus aggregates the last stallCensusWindowHours of ticks by
// lane family and merges the resolved per-family deadlines. now is the
// component clock's instant; global is the daemon-wide per-turn knob
// (--gateway-response-timeout) the family policy resolves against.
//
// One round trip: the daemon runs a single serialized SQLite connection, so a
// per-family N+1 would serialize behind tick work (the SCHED-PERF-002 lesson).
// A query failure is NOT fatal — the health route still answers, with the
// policy half of the census and an explicit error string (a census is
// observability, never a liveness gate).
func computeStallCensus(ctx context.Context, db *sql.DB, now time.Time, global time.Duration) StallCensus {
	out := StallCensus{
		WindowHours: stallCensusWindowHours,
		ByFamily:    make([]StallFamilyCensus, 0, len(scheduler.StallFamilyPolicies(global))),
	}
	type counts struct{ ticks, stalled int }
	byFamily := map[string]counts{}

	cutoff := now.Add(-stallCensusWindowHours * time.Hour)
	if db == nil {
		// No database attached (bare unit-test server): report the policy
		// half only. A census is observability; it never fails the route.
		out.Error = "no database attached"
		for _, p := range scheduler.StallFamilyPolicies(global) {
			out.ByFamily = append(out.ByFamily, StallFamilyCensus{
				Family:     p.Family,
				Deadline:   p.DeadlineLabel,
				DeadlineMS: p.DeadlineMS,
				Exempt:     p.Exempt,
				Heartbeat:  p.Heartbeat,
			})
		}
		return out
	}
	// spawned_at is RFC3339 text. The string comparison is the index-friendly
	// coarse bound (a day of slack, so an offset carry can never drop a row);
	// julianday() is the exact bound, the same pair the budget/window queries
	// use for mixed-offset rows.
	rows, err := db.QueryContext(ctx, `
SELECT project_name,
       COUNT(*) AS ticks,
       SUM(CASE WHEN error LIKE 'stalled%' THEN 1 ELSE 0 END) AS stalled
  FROM ticks
 WHERE spawned_at >= ? AND julianday(spawned_at) >= julianday(?)
 GROUP BY project_name`,
		cutoff.Add(-24*time.Hour).UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		log.Printf("WARN: stalls_24h census query: %v", err)
		out.Error = fmt.Sprintf("census query: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var project string
			var c counts
			if err := rows.Scan(&project, &c.ticks, &c.stalled); err != nil {
				log.Printf("WARN: stalls_24h census scan: %v", err)
				out.Error = fmt.Sprintf("census scan: %v", err)
				break
			}
			fam := scheduler.StallFamilyOf(project)
			agg := byFamily[fam]
			agg.ticks += c.ticks
			agg.stalled += c.stalled
			byFamily[fam] = agg
		}
		if err := rows.Err(); err != nil {
			log.Printf("WARN: stalls_24h census rows: %v", err)
			out.Error = fmt.Sprintf("census rows: %v", err)
		}
	}

	for _, p := range scheduler.StallFamilyPolicies(global) {
		c := byFamily[p.Family]
		row := StallFamilyCensus{
			Family:     p.Family,
			Ticks:      c.ticks,
			Stalled24h: c.stalled,
			Deadline:   p.DeadlineLabel,
			DeadlineMS: p.DeadlineMS,
			Exempt:     p.Exempt,
			Heartbeat:  p.Heartbeat,
		}
		if c.ticks > 0 {
			row.StallRate = float64(c.stalled) / float64(c.ticks)
		}
		out.Total += c.stalled
		out.ByFamily = append(out.ByFamily, row)
	}
	return out
}
