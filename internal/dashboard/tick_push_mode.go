package dashboard

import "github.com/coding-hermes/scheduler/internal/scheduler"

// SCHED-GAP-1594 — the /health panel's live-vs-pushed indicator.
//
// The card names one of two states:
//
//	push       — the spawner pushes each tick's commits at tick exit
//	             (the SCHED-GAP-1694 fleet default); the dashboard AND the
//	             remote both carry the tick's work.
//	local-only — --disable-tick-push is armed: the daemon is WEB-PRIMARY,
//	             ticks update this dashboard (tick rows, metrics) only, and
//	             commits stay on local disk until the fleet-strand-push cron
//	             or an operator pushes.
//
// It reads scheduler.TickPushDisabledDefault — the same package state the
// spawner's push decision consumes — so the indicator can never disagree
// with what Wait() does at tick close-out.

// tickPushMode returns "local-only" or "push" from the scheduler's
// process-wide per-tick push state.
func tickPushMode() string {
	if scheduler.TickPushDisabledDefault() {
		return "local-only"
	}
	return "push"
}
