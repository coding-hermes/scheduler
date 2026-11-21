package scheduler

// SCHED-GAP-16936 piece 3 — THE CAPPED REPAIR RE-DISPATCH.
//
// THE PROBLEM. A tick that FAILED and left runnable leftovers in its worktree
// (uncommitted code, a half-landed fix) was indistinguishable, at the scheduler
// level, from a tick that failed with an empty tree: both rows read
// status=failed and the lane simply waited out its cooldown. The leftovers were
// only recoverable by an operator noticing them by hand — and the naive fix
// (retry every failure) turns a provider outage into an unbounded retry storm
// against a lane that has nothing to retry.
//
// THE RULE (three parts, all enforced here):
//
//  1. ELIGIBILITY. Only a failed tick whose recovery probe (piece 2) found
//     uncommitted content is re-dispatchable. A failure with a clean worktree
//     is NOT re-dispatched at all — there is nothing to repair, and a blind
//     retry is exactly the storm the cap exists to prevent.
//  2. THE CAP. At most MaxRepairRedispatch (2) repair attempts per failure
//     chain. The third is REFUSED with the cause named on the row.
//  3. THE LEDGER. The attempt number this tick CAUSED and the cause (the
//     recovery evidence that justified it, or the reason it was refused) ride
//     the same single finalization UPDATE as the outcome
//     (ticks.repair_attempt / ticks.repair_cause, migration v92) — no second
//     write that can silently miss a row.
//
// WHAT COUNTS AS THE CHAIN. priorRepairAttempts walks the rows inserted before
// this one for the project, stopping at the first row that is not failed or
// timed out; the chain length is the max repair_attempt seen in that tail. A
// tick that completed productively ends the chain (it is not failed, so the walk
// stops there), which is what makes "at most twice" apply per FAILURE, not per
// project lifetime. The walk is ordered by rowid — insertion order — because
// ticks.spawned_at mixes naive and offset-bearing RFC3339 spellings in the live
// table and a string comparison across the two orders rows wrongly.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
)

// MaxRepairRedispatch is the hard cap on repair re-dispatches per failure chain.
// The brief's "at most twice": attempts 1 and 2 are legal, 3 is refused.
const MaxRepairRedispatch = 2

// Repair causes recorded on ticks.repair_cause. They are log/ledger prose rather
// than a CHECK-constrained vocabulary (the column is free text by design: the
// cause carries the probe evidence), but the PREFIX is what operators grep for,
// so keep these stable.
const (
	// RepairCauseRecoverable — the tick failed and its worktree holds content.
	RepairCauseRecoverable = "recovery_available"
	// RepairCauseNothingRunnable — the tick failed with nothing to repair.
	RepairCauseNothingRunnable = "nothing_runnable"
	// RepairCauseCapReached — the failure chain already used its two attempts.
	RepairCauseCapReached = "cap_reached"
)

// RepairRedispatchDecision is the outcome of the eligibility + cap evaluation.
type RepairRedispatchDecision struct {
	// Dispatch is true when a repair re-dispatch should be launched.
	Dispatch bool
	// Attempt is the attempt number this re-dispatch represents (1-based) when
	// Dispatch is true, and the attempt number that was REFUSED when Refused is
	// true. 0 means no attempt was owed at all (not a failed tick / nothing
	// runnable).
	Attempt int
	// Cause is the ledger text: RepairCauseRecoverable + the probe evidence,
	// or RepairCauseNothingRunnable / RepairCauseCapReached + why.
	Cause string
	// Refused is true when the cap (not the evidence) blocked a dispatch that
	// was otherwise eligible — the distinction matters, because a refused
	// repair means a human now owns the leftovers.
	Refused bool
}

// decideRepairRedispatch is the pure decision: status + probe + chain length in,
// dispatch/refuse verdict out. No I/O, so the whole rule table is unit-testable.
//
//	status        the tick's final status (only TickFailed is eligible)
//	probe         piece 2's worktree verdict
//	priorAttempts the repair attempts already made in this failure chain
func decideRepairRedispatch(status TickStatus, probe RecoveryProbe, priorAttempts int) RepairRedispatchDecision {
	if status != TickFailed {
		// Not eligible. Timeouts and aborted ticks are NOT repair candidates:
		// a wall that fired is a liveness verdict, and an operator restart took
		// the surface away — retrying either would re-run the same clock.
		return RepairRedispatchDecision{}
	}
	if priorAttempts < 0 {
		priorAttempts = 0
	}
	if !probe.Recoverable() {
		// Nothing runnable: the worktree is clean, or it could not be measured.
		// A blind retry is what the cap exists to prevent — and an UNMEASURED
		// tree is not evidence of leftovers, so it fails closed too.
		cause := RepairCauseNothingRunnable + ": " + probe.State
		if probe.Detail != "" {
			cause += " (" + probe.Detail + ")"
		}
		return RepairRedispatchDecision{Cause: cause}
	}
	attempt := priorAttempts + 1
	if attempt > MaxRepairRedispatch {
		return RepairRedispatchDecision{
			Attempt: attempt,
			Refused: true,
			Cause: fmt.Sprintf("%s: %d repair attempts already made (cap %d): %s",
				RepairCauseCapReached, priorAttempts, MaxRepairRedispatch, probe.Detail),
		}
	}
	return RepairRedispatchDecision{
		Dispatch: true,
		Attempt:  attempt,
		Cause:    fmt.Sprintf("%s: %s", RepairCauseRecoverable, probe.Detail),
	}
}

// priorRepairAttempts measures the repair chain this tick belongs to: walk the
// rows inserted before it for the same project, and stop at the first row that
// is neither failed nor timed out. The chain length is the largest
// repair_attempt in that tail (a single walk, bounded).
//
// Ordering is by rowid, NOT spawned_at: the live ticks table carries both the
// naive ("2026-11-20T10:00:00") and the offset-bearing RFC3339 spelling, so a
// timestamp comparison across the two orderings puts rows in the wrong order.
//
// A DB read failure returns 0 — the safe direction is a LOWER attempt count,
// which can only ever grant a dispatch the cap would have allowed anyway on a
// healthy read (max two extra runs, never an unbounded loop).
func priorRepairAttempts(ctx context.Context, db *sql.DB, project string, tickID string) int {
	if db == nil || strings.TrimSpace(project) == "" || strings.TrimSpace(tickID) == "" {
		return 0
	}
	rows, err := db.QueryContext(ctx, `
		SELECT status, COALESCE(repair_attempt, 0)
		FROM ticks
		WHERE project_name = ?
		  AND rowid < (SELECT rowid FROM ticks WHERE id = ?)
		ORDER BY rowid DESC
		LIMIT 32
	`, project, tickID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	prior := 0
	for rows.Next() {
		var status string
		var attempt int
		if err := rows.Scan(&status, &attempt); err != nil {
			return prior
		}
		switch TickStatus(status) {
		case TickFailed, TickTimeout:
			// Still inside the failure chain.
		default:
			// A non-failed row ends the chain: this tick starts a fresh one.
			return prior
		}
		if attempt > prior {
			prior = attempt
		}
	}
	return prior
}

// repairCauseNotLaunched is appended to a booked-but-not-launched cause. It is
// what separates "the fleet decided to repair and did" from "the fleet decided
// to repair and was disarmed" — without it, a disarmed row would read exactly
// like a clean one at the column level (repair_attempt = 0).
const repairCauseNotLaunched = "; not launched (SCHEDULER_REPAIR_REDISPATCH disarmed)"

// repairRedispatchArmed reports whether the re-dispatch may LAUNCH. It is ON by
// default: the row asks for a capped repair re-dispatch, and an implementation
// that only ever fires when an operator remembers to export a variable is inert
// code (the fleet's recurring defect class), not a delivered feature.
//
// SCHEDULER_REPAIR_REDISPATCH is therefore a KILL SWITCH, not an arming flag:
// "0"/"false"/"no"/"off"/"disabled" (case-insensitive, trimmed) disarms it;
// unset or any other value leaves it armed. Disarming is not silence — the
// finalization still records the decision and the evidence it would have acted
// on, suffixed with repairCauseNotLaunched, so an operator can see what the
// fleet skipped instead of reading a row that looks like nothing was ever
// recoverable.
func repairRedispatchArmed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SCHEDULER_REPAIR_REDISPATCH"))) {
	case "0", "false", "no", "off", "disabled":
		return false
	}
	return true
}

// repairRequest is one booked repair re-dispatch the slot pool launches once its
// failing tick has released the lane (its slot refcount and its dedup
// reservation), so the repair is not swallowed by the pool's own dedup.
type repairRequest struct {
	proj      PackedProject
	attempt   int
	noDeliver bool
}

// completeTick writes a tick's terminal row through the repair-aware finalizer
// and returns the re-dispatch to launch (nil when none was booked).
//
// The recovery probe runs INSIDE Complete (which needs it before the UPDATE), so
// the row's verdict and the decision below are derived from the same
// measurement — there is no window in which the row says "clean" and the
// scheduler acts as if it were dirty.
//
// The kill switch is resolved in the FINALIZER (see repairRedispatchArmed), not
// here, because the ledger has to tell "decided and launched" from "decided and
// disarmed" and only the writer of the row can do that: a decision that never
// reached this method already carries repair_attempt = 0 plus the not-launched
// marker, and a decision that arrived here is one the fleet is entitled to run.
func (p *SlotPool) completeTick(proj PackedProject, outcome TickOutcome, noDeliver bool) *repairRequest {
	decision, err := p.lifecycle.CompleteWithRepair(outcome)
	if err != nil {
		log.Printf("SPAWN: complete %s: %v", outcome.TickID, err)
		return nil
	}
	if !decision.Dispatch {
		return nil
	}
	return &repairRequest{proj: proj, attempt: decision.Attempt, noDeliver: noDeliver}
}

// recordRepairDecision stamps the decision onto the outcome so Complete's single
// finalization UPDATE carries it, and logs the named verdict either way (a
// refusal is an operator-visible fact, not a silent no-op).
func recordRepairDecision(project, tickID string, decision RepairRedispatchDecision) {
	if decision.Cause == "" {
		return
	}
	if decision.Dispatch {
		log.Printf("REPAIR: %s tick=%s re-dispatch attempt %d/%d (%s) — SCHED-GAP-16936",
			project, tickID, decision.Attempt, MaxRepairRedispatch, decision.Cause)
		return
	}
	if decision.Refused {
		log.Printf("REPAIR: %s tick=%s REFUSED attempt %d (cap %d) — %s — SCHED-GAP-16936",
			project, tickID, decision.Attempt, MaxRepairRedispatch, decision.Cause)
		return
	}
	log.Printf("REPAIR: %s tick=%s not re-dispatched — %s — SCHED-GAP-16936",
		project, tickID, decision.Cause)
}
