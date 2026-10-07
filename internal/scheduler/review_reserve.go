package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"sync"
)

// SCHED-GAP-225 — review-lane reserved slots.
//
// The global concurrency budget (--max-concurrent) is one flat pool: when
// enough code lanes are eligible (or already running) the evaluation pass
// fills every slot before a review lane is ever considered, and the -review
// satellite families starve. The measured fleet shape was 148 candidates
// against --max-concurrent 10.
//
// Mechanism: the LAST N slots of the admission window cannot be taken by
// code lanes. Every selection path (the flat packer's greedy pack, the
// multi-pool namespace path's Phase 2, its borrow re-pack, and packFlat)
// admits lanes in urgency order up to
//
//	code ceiling = max_concurrent - effective_reserve
//
// and only lanes whose name ends in the -review role suffix may cross that
// ceiling (up to the full cap). Review lanes below the ceiling compete
// unchanged — the reserve is a floor for their admission, never a boost.
//
// The reserve is DYNAMIC within one pack pass: effective_reserve =
// min(reserve, review lanes still unpacked in the candidate list), decayed
// as review lanes pack. An unused reservation never idles a slot — with no
// waiting review lane the ceiling collapses onto the full cap and selection
// is byte-identical to the legacy behavior.
//
// Precedence and layering (same three-layer model as every other knob):
// TOML [scheduler] review_lane_reserved_slots < env
// SCHEDULER_REVIEW_LANE_RESERVED_SLOTS < flag --review-lane-reserved-slots.
// The flag's fleet default is 2; the LIBRARY default here is 0 (disabled),
// exactly the tasks-pacing precedent (SCHED-GAP-136): embedding tests and
// library consumers keep byte-identical selection until the daemon arms the
// flag. 0 anywhere disables the reservation.
//
// The knob follows the SetLoadGateThreshold pattern: a package-level value
// wired ONCE from the daemon entry point (last writer wins), read at pack
// time so the packers need no constructor churn.

// ReviewLaneReservedSlotsFlagDefault is the --review-lane-reserved-slots
// fleet default (mirrored in cmd/schedulerd/main.go's flag declaration).
const ReviewLaneReservedSlotsFlagDefault = 2

// reviewReserveLibraryDefault is the library/embedding default: no
// reservation. The fleet binary arms 2 via the flag default.
const reviewReserveLibraryDefault = 0

var (
	reviewLaneReservedSlots = reviewReserveLibraryDefault
	reviewReserveMu         sync.RWMutex
)

// SetReviewLaneReservedSlots installs the review-lane slot reservation
// (SCHED-GAP-225): how many slots of the --max-concurrent admission window
// code lanes may not take. Called once from main/loop wiring; last writer
// wins. A negative value normalizes to 0 (disabled) — there is no meaning
// for a negative reserve and silently keeping one would hide a config typo.
func SetReviewLaneReservedSlots(n int) {
	if n < 0 {
		n = 0
	}
	reviewReserveMu.Lock()
	defer reviewReserveMu.Unlock()
	reviewLaneReservedSlots = n
}

// IsReviewLane reports whether the lane name carries the -review role
// suffix — the satellite family the reserved band protects. Suffix match,
// so "myproj-reviews" and "review-bot" (a foreman whose name merely starts
// with review) are NOT review lanes.
func IsReviewLane(name string) bool {
	return strings.HasSuffix(name, "-review")
}

// reviewLaneSlotCeiling returns the admission ceiling for ONE lane
// (SCHED-GAP-225): the full maxConcurrent for a -review lane, otherwise
// maxConcurrent minus the effective reserve. A reserve at or above the cap
// clamps to maxConcurrent-1 — a misconfigured reserve can never starve code
// lanes entirely, and a 1-slot fleet cannot reserve anything.
func reviewLaneSlotCeiling(name string, reserved, maxConcurrent int) int {
	if IsReviewLane(name) {
		return maxConcurrent
	}
	if reserved <= 0 || maxConcurrent < 1 {
		return maxConcurrent
	}
	eff := reserved
	if eff > maxConcurrent-1 {
		eff = maxConcurrent - 1
	}
	return maxConcurrent - eff
}

// dynamicReviewReserve is the reserve in force mid-pass: min(reserved,
// eligibleReview - packedReview), floored at 0 — the reservation exists
// only while a review lane is still waiting for a slot.
func dynamicReviewReserve(reserved, eligibleReview, packedReview int) int {
	if reserved < 0 {
		reserved = 0
	}
	if eligibleReview < reserved {
		reserved = eligibleReview
	}
	reserved -= packedReview
	if reserved < 0 {
		reserved = 0
	}
	return reserved
}

// reviewReserveRemaining reads the live knob and decays it by the review
// lanes already packed this pass. The packers call this at pass start and
// after every review-lane admission.
func reviewReserveRemaining(eligibleReview, packedReview int) int {
	reviewReserveMu.RLock()
	r := reviewLaneReservedSlots
	reviewReserveMu.RUnlock()
	return dynamicReviewReserve(r, eligibleReview, packedReview)
}

// countEligibleReviewLanes counts enabled, not-running -review lanes from
// the projects table (SCHED-GAP-225). The zero-select mirror and the ADMIT
// classifier use it to decide whether the reservation is actually in force
// this pass (the dynamic reserve exists only while a review lane is still
// waiting for a slot). Best-effort: a query failure returns 0, which reads
// as "no reserve in force" — the mirrors then fall back to the legacy
// full-cap verdicts instead of inventing a ceiling from bad data.
func countEligibleReviewLanes(db *sql.DB, runningSet map[string]bool) int {
	if db == nil {
		return 0
	}
	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM projects WHERE enabled = 1 AND name LIKE '%-review'`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if !IsReviewLane(name) || (runningSet != nil && runningSet[name]) {
			continue
		}
		n++
	}
	return n
}
