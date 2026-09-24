package scheduler

import "strings"

// SCHED-GAP-1608 — operator-induced drain reaps are TRANSPORT events, not
// lane faults. The drain-restart deploy cannot converge on a busy fleet:
// pausing stops new admission but in-flight ticks keep heartbeating, so a
// controlled restart eventually times out the drain and stamps every live
// tick failed (loop.go abortInFlightTicks → orphan_reason='drain_timeout').
// Measured 2026-09-24: a busy fleet held 10-12 live ticks for hours; one
// restart eventually reaped 40 ticks as drain_timeout. Requiring
// active_ticks=0 before any restart therefore cannot converge — and the
// reaped rows must never count against the lanes.
//
// This file is the SINGLE AUTHORITY for that exclusion (same contract shape
// as failureclass.go's SCHED-GAP-173 single authority): every surface that
// turns a failed/timeout tick row into lane failure accounting — the
// auto-disable enforcer (CheckFailureRateAutoDisable) and the read-only
// status surface (computeProjectFailureRates) — classifies rows through
// the FailureIsLaneAttributableT tuple verdict. Never restate the
// carve-out with a second predicate.

// OrphanReasonOperatorRestart is the explicit orphan reason row
// abortInFlightTicks stamps when the in-flight drain-abort is acknowledged
// as operator-induced (the same event the legacy rows — and only those —
// carried implicitly through the abort error text). It is the distinct
// reason the drain-restart runbook quotes verbatim around operator
// approval, and one of the two rows operatorAcknowledgedReap excludes.
const OrphanReasonOperatorRestart = "operator_restart"

// operatorAcknowledgedReap is the package-private verdict: is this FAILED
// row's terminal stamp an operator-acknowledged reap (drain timeout or the
// explicit operator-restart reason) rather than a project-attributable
// failure?
func operatorAcknowledgedReap(orphanReason string) bool {
	return orphanReason == OrphanReasonDrainTimeout ||
		orphanReason == OrphanReasonOperatorRestart
}

// OperatorAcknowledgedReap is the exported single authority the two
// classification surfaces and their regression tests import: did this
// failed row get orphan-stamped by an operator-acknowledged reap path
// (drain_timeout / operator_restart) rather than by something the project
// caused? Only those two orphan_reason spellings qualify — every other
// value (empty, startup_reap, zombie_reap) is the safe default: the row
// keeps its usual accounting.
func OperatorAcknowledgedReap(orphanReason string) bool {
	return operatorAcknowledgedReap(orphanReason)
}

// IsOperatorRestartReap reports whether a failed tick's PERSISTED error text
// is the shutdown-drain abort marker (the one operator-induced failure class
// whose ONLY persisted signature used to be the orphan_reason column, before
// SCHED-GAP-1608 stamped it there explicitly). It compares against
// OrphanAbortMarker (failureclass.go) — the exactly-once wording constant;
// it must not restate the abort text. Never restate the abort wording
// anywhere else.
func IsOperatorRestartReap(errText string) bool {
	return strings.Contains(strings.ToLower(errText), OrphanAbortMarker)
}

// FailureIsLaneAttributableT is the EXPORTED shared verdict every
// failure-accounting surface applies instead of its own logic (its
// package-private spelling is failureIsLaneAttributableT; internal/scheduler
// callers use that). A tick row counts against its lane unless BOTH (a) it
// is a failed/timeout row and (b) the row's terminal stamp carries only
// equipment- or operator-side blame:
//   - orphan_reason drain_timeout / operator_restart — an operator-induced
//     drain reap (SCHED-GAP-1608): the owner was alive, an operator killed
//     the process;
//   - the legacy shape: no structural stamp, but the persisted error text
//     is the drain abort — the exactly-one-wording form of the same reap
//     (back-compat for every row stamped before operator_restart existed);
//   - "failed" (spawn/session failure) with a harness-class error — the
//     gateway refused the spawn and the tick never reached the project
//     (SCHED-GAP-134-class noise, failureclass.go's single marker list).
//
// Comparisons are by TUPLE, never per-column: the same SURFACE structure
// applies to 'failed' and 'timeout' rows alike ('timeout' with an
// orphan-provenance stamp is excluded exactly like its 'failed' sibling;
// harness text on a 'timeout' keeps the enforcer's own arithmetic — those
// are bookkeeping-dead rows, not operator reaps).
func FailureIsLaneAttributableT(status, errText, orphanReason string) bool {
	if status != "failed" && status != "timeout" {
		return true
	}
	if operatorAcknowledgedReap(orphanReason) {
		return false
	}
	if IsOperatorRestartReap(errText) {
		return false
	}
	if status == "failed" && harnessFailure(errText) {
		return false
	}
	return true
}
