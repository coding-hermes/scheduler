package database

import (
	"context"
	"database/sql"
	"fmt"
)

// SCHED-GAP-1653 dispatch accountability — the write side.
//
// Every terminal tick records whether the tick dispatched a foreman
// worker (dispatch_outcome) and the reason from the closed vocabulary
// (dispatch_reason). The write happens ONCE, at finalization: the tick
// completion paths (lifecycle.Complete for live ticks, the sim spawner
// for dry-runs) stamp the outcome the SPAWN PATH already decided —
// this file never re-derives it. Rows terminal before migration v48
// keep '' / '' (honest: predates accountability).

// RecordTickDispatch persists the SCHED-GAP-1653 dispatch accountability
// pair on a terminal tick: outcome ∈ {yes, no} and reason from
// DispatchReasons ("dispatched" pairs with yes; the five no-reasons with
// no). The vocabulary is validated HERE as well as by the migration's
// CHECK constraint, so a caller bug fails with a field-named error at
// the decision site instead of a raw constraint violation.
//
// Best-effort by contract: the stamp is observability and the stand-down
// gate's evidence, never a lifecycle gate — an unknown id or a write
// error is returned and logged by the caller, and can never change the
// tick's terminal status.
func RecordTickDispatch(ctx context.Context, db *sql.DB, tickID, outcome, reason string) error {
	if outcome != DispatchYes && outcome != DispatchNo {
		return fmt.Errorf("record tick dispatch %q: dispatch_outcome must be %q or %q, got %q",
			tickID, DispatchYes, DispatchNo, outcome)
	}
	if !DispatchReasonIsValid(reason) {
		return fmt.Errorf("record tick dispatch %q: dispatch_reason %q is outside the SCHED-GAP-1653 vocabulary",
			tickID, reason)
	}
	if (outcome == DispatchYes) != (reason == DispatchReasonDispatched) {
		return fmt.Errorf("record tick dispatch %q: outcome %q must pair with the matching reason side (yes↔dispatched, no↔a no-reason), got %q",
			tickID, outcome, reason)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE ticks SET dispatch_outcome = ?, dispatch_reason = ? WHERE id = ?
	`, outcome, reason, tickID)
	if err != nil {
		return fmt.Errorf("record tick dispatch %q: %w", tickID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for tick %q: %w", tickID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, tickID)
	}
	return nil
}

// DispatchCount is one lane's tick split by dispatch_reason
// (SCHED-GAP-1653): total terminal ticks with the fields recorded, how
// many dispatched a worker, and the no-dispatch count with its reasons
// spelled out. Coverage names the rows still carrying ” (legacy ticks
// and anything the finalization stamp missed) so the operator can see
// whether the accountability fields themselves are trustworthy for this
// lane — a lane whose coverage sits near zero is reporting absence of
// measurement, not absence of work.
type DispatchCount struct {
	Total      int            `json:"total"`       // terminal ticks with a dispatch decision recorded
	Dispatched int            `json:"dispatched"`  // dispatched a foreman worker (reason=dispatched)
	NoDispatch int            `json:"no_dispatch"` // no dispatch, by reason below
	Reasons    map[string]int `json:"reasons"`     // every vocabulary bucket, zero-filled
	Coverage   int            `json:"coverage"`    // terminal ticks still carrying '' (legacy / unrecorded)
}

// CountProjectDispatchReasons aggregates one project's TERMINAL ticks by
// dispatch_reason (SCHED-GAP-1653). Every vocabulary bucket is present —
// zero-filled — so a consumer can render the full split without merging
// its own keys; Coverage counts terminal rows whose pair is still ”.
// The query runs through the (project_name, spawned_at) covering index
// prefix with a status filter, so it stays a bounded index scan even on
// the fat live fleet database.
func CountProjectDispatchReasons(ctx context.Context, db *sql.DB, project string) (DispatchCount, error) {
	dc := DispatchCount{Reasons: make(map[string]int, len(DispatchReasons))}
	for _, r := range DispatchReasons {
		dc.Reasons[r] = 0
	}
	rows, err := db.QueryContext(ctx, `
		SELECT status, dispatch_outcome, dispatch_reason, COUNT(*)
		FROM ticks
		WHERE project_name = ? AND status IN ('completed', 'failed', 'timeout', 'deferred')
		GROUP BY dispatch_outcome, dispatch_reason
	`, project)
	if err != nil {
		return dc, fmt.Errorf("count dispatch reasons for %q: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status          string
			outcome, reason string
			n               int
		)
		if err := rows.Scan(&status, &outcome, &reason, &n); err != nil {
			return dc, fmt.Errorf("scan dispatch reason row for %q: %w", project, err)
		}
		// 'running' can appear transiently in the group (a row flipped
		// terminal between the WHERE and the scan is impossible in one
		// statement, but a status-bearing group from a legacy vocabulary
		// must never silently join Total) — guard by status, not trust.
		if status != "completed" && status != "failed" && status != "timeout" && status != "deferred" {
			continue
		}
		switch {
		case outcome == DispatchYes && reason == DispatchReasonDispatched:
			dc.Dispatched += n
			dc.Total += n
		case outcome == DispatchNo && DispatchReasonIsValid(reason):
			dc.Reasons[reason] += n
			dc.NoDispatch += n
			dc.Total += n
		default:
			dc.Coverage += n
		}
	}
	if err := rows.Err(); err != nil {
		return dc, fmt.Errorf("iterate dispatch reason rows for %q: %w", project, err)
	}
	return dc, nil
}
