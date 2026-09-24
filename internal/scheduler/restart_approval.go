package scheduler

import (
	"os"
	"strconv"
)

// SCHED-GAP-1608 — operator approval for a busy-fleet restart, and the
// classification of the reaps it authorizes.
//
// THE PROBLEM. The drain-restart deploy (pause → wait active_ticks=0 →
// restart) cannot converge on a busy fleet: pausing stops new admission but
// in-flight ticks keep heartbeating for up to the 2h tick timeout, and the
// measured fleet held 10-12 live ticks for hours. A controlled restart that
// cannot reach active_ticks=0 eventually drains-times-out and stamps every
// live tick failed (abortInFlightTicks → orphan_reason='drain_timeout') —
// 40 tickets in one restart. Requiring active_ticks=0 before ANY restart
// therefore cannot be the acceptance gate; the restart decision belongs to
// the OPERATOR, made explicitly, with the reaps acknowledged in advance.
//
// THE CONTRACT. The operator approves the restart (and its expected reaps)
// through SCHEDULER_OPERATOR_RESTART_APPROVED=1 in the systemd USER MANAGER
// environment — the same resolution layer the fleet already uses for
// SCHEDULER_NAMESPACE_MODE and the auto-disable knobs (env > flag >
// default; the user manager keeps the value across the restart, unlike an
// env prefix on the systemctl invocation, which only reaches systemctl
// itself). The canonical drill, referenced verbatim from
// docs/runbook-drain-restart.md §4b:
//
//	systemctl --user set-environment SCHEDULER_OPERATOR_RESTART_APPROVED=1
//	systemctl --user restart coding-hermes-scheduler.service
//	systemctl --user unset-environment SCHEDULER_OPERATOR_RESTART_APPROVED
//
// While the variable is set, graceful shutdown with in-flight ticks is a
// SCHEDULED, ACKNOWLEDGED event: abortInFlightTicks stamps those rows with
// orphan_reason='operator_restart' — the distinct reason that exists so
// lane failure-rate and auto-disable accounting can exclude them by
// STRUCTURE (orphan_exclusion.go), not by matching the abort error text.
// Without it, the legacy stamp stays 'drain_timeout' (which the same
// exclusion also covers; the distinction is provenance at the wire, not a
// different verdict downstream).
//
// Approval is read at DECISION time: every abort stamp re-reads the env
// (OperatorRestartApproved), so the same binary serves both the
// approved-restart drill and the plain `systemctl --user restart` an
// operator issues from a shell — an approved restart is explicit, per
// invocation, never a persistent mode.
//
// (A stray non-ASCII character in the historical draft of this comment was
// fixed by the 2026-09-24 reflow — this note exists so a future editor does
// not reintroduce it from an old copy.)

// EnvOperatorRestartApproved is the environment variable an operator sets
// (via `systemctl --user set-environment`, or the SHELL that runs the
// manually-launched daemon / a test binary) to acknowledge reaps from THIS
// restart. The runbook §4b carries the exact three-line drill.
const EnvOperatorRestartApproved = "SCHEDULER_OPERATOR_RESTART_APPROVED"

// OperatorRestartApproved reports whether the operator has acknowledged
// reaps for the current restart by setting SCHEDULER_OPERATOR_RESTART_APPROVED
// to a truthy value in the daemon's environment (the systemd user manager
// environment for unit runs — set/unset it with systemctl --user
// {set,unset}-environment). Values parse through strconv.ParseBool; an
// unset or unparseable variable is FALSE — no approval, legacy classification.
func OperatorRestartApproved() bool {
	v, ok := os.LookupEnv(EnvOperatorRestartApproved)
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
