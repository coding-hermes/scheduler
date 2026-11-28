package scheduler

// Delivery-time destination guard (SCHED-GAP-17084).
//
// SCHED-GAP-17083 taught the scheduler to STORE an approved destination policy
// per deploy group and to PREVIEW a migration against it. Nothing consumed it:
// internal/scheduler delivered to whatever projects.deliver said, so a lane
// whose group had an approved mapping could still report into a different
// thread forever and no code path noticed. This file is that consumer — the
// single choke point every tick report and timeout alert already passes
// through.
//
// Contract:
//
//   - No policy source installed → delivery is byte-identical to pre-17084
//     (deliverOutputWithMode returns the caller's target unchanged).
//   - No APPROVED policy owns the project → unchanged. An unapproved policy is
//     not authority, and refusing on one would silence a lane for a decision
//     nobody made.
//   - Approved policy owns the project → the approved destination is
//     authoritative: an identical target delivers, an empty target INHERITS
//     the group destination (lane inheritance), and a different target is
//     refused with a drift alert instead of being delivered to the wrong
//     thread.
//   - Conflicting ownership, an unapproved exception, or an invalid approved
//     target refuses: ownership of the route cannot be established.
//
// The refusal is FAIL-CLOSED for the target but FAIL-OPEN for the harness: an
// unreadable policy store logs one line a day and lets the report through,
// because a broken config file must never silence the fleet. Every refusal is
// logged at most once per project per UTC day and never quotes a destination —
// the operator is pointed at the preview endpoint instead.

import (
	"log"
	"sync"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
)

// Alert-class prefixes. Greppable by class, sanitized by construction: a line
// names the lane, the tick, the owning group(s) and the decision status — never
// a chat id, thread id or token.
const (
	destAlertDrift      = "DEST-DRIFT"
	destAlertConflict   = "DEST-CONFLICT"
	destAlertUnapproved = "DEST-UNAPPROVED"
	destAlertInvalid    = "DEST-INVALID"
	destAlertUnknown    = "DEST-UNRESOLVED"
	destAlertUnreadable = "DEST-UNAVAILABLE"
)

var (
	destinationPolicyMu     sync.Mutex
	destinationPolicySource func() ([]blocks.Group, error)

	// destAlertDay gates the once-per-project-per-day alerts. IN MEMORY ONLY,
	// like the unconfigured-target warning: re-arming on a daemon restart is
	// the cheap direction of the trade.
	destAlertMu  sync.Mutex
	destAlertDay = map[string]string{}
)

// SetDestinationPolicySource installs the group-policy reader the delivery
// guard consults (cmd/schedulerd points it at the same groups.jsonl the deploy
// block API writes). nil disables the guard entirely.
func SetDestinationPolicySource(src func() ([]blocks.Group, error)) {
	destinationPolicyMu.Lock()
	defer destinationPolicyMu.Unlock()
	destinationPolicySource = src
}

// currentDestinationPolicySource reads the installed source under the lock.
func currentDestinationPolicySource() func() ([]blocks.Group, error) {
	destinationPolicyMu.Lock()
	defer destinationPolicyMu.Unlock()
	return destinationPolicySource
}

// resetDestinationAlertState clears the once-per-day gate (tests).
func resetDestinationAlertState() {
	destAlertMu.Lock()
	destAlertDay = map[string]string{}
	destAlertMu.Unlock()
}

// warnDestinationOnce logs one destination alert for a project at most once per
// UTC day and reports whether this call emitted it.
func warnDestinationOnce(clk clock.Clock, project, tickID, class, detail string) bool {
	day := clk.Now().UTC().Format("2006-01-02")
	key := class + "|" + project

	destAlertMu.Lock()
	prev, seen := destAlertDay[key]
	alreadyWarnedToday := seen && prev == day
	if !alreadyWarnedToday {
		destAlertDay[key] = day
	}
	destAlertMu.Unlock()

	if alreadyWarnedToday {
		return false
	}
	log.Printf("%s: %s tick=%s — %s (logged once/day)", class, project, tickID, detail)
	return true
}

// resolveDeliveryDestination runs the approved-policy guard for one delivery
// and returns the target delivery must use plus whether it may proceed.
//
// With no policy source installed it is a pure pass-through (the pre-17084
// path), so every existing deployment and test is unaffected.
func resolveDeliveryDestination(clk clock.Clock, project, tickID, deliver string) (string, bool) {
	source := currentDestinationPolicySource()
	if source == nil {
		return deliver, true
	}
	groups, err := source()
	if err != nil {
		// Fail OPEN on the harness, never silently: an unreadable store is a
		// configuration fault, and dropping every report because of it would
		// hide the fault behind a fleet-wide silence.
		warnDestinationOnce(clk, project, tickID, destAlertUnreadable,
			"approved destination policy unreadable; delivering to the configured target unchanged")
		return deliver, true
	}
	decision := blocks.DecideDelivery(groups, project, deliver)
	if decision.Allowed {
		return decision.Effective, true
	}
	hint := "see GET /api/v1/groups/" + decision.Origin + "/deploy (destination_preview) for the approved mapping"
	switch decision.Status {
	case blocks.DestinationDrift:
		warnDestinationOnce(clk, project, tickID, destAlertDrift,
			"delivery refused: configured target is not the approved group destination (origin="+decision.Origin+"); "+hint)
	case blocks.DestinationConflict:
		warnDestinationOnce(clk, project, tickID, destAlertConflict,
			"delivery refused: project belongs to more than one authorized group (origin="+decision.Origin+"); resolve ownership, then retry")
	case blocks.DestinationUnapproved:
		warnDestinationOnce(clk, project, tickID, destAlertUnapproved,
			"delivery refused: the project's destination exception is not approved (origin="+decision.Origin+")")
	case blocks.DestinationInvalid:
		warnDestinationOnce(clk, project, tickID, destAlertInvalid,
			"delivery refused: the approved destination is not a telegram chat+thread pair (origin="+decision.Origin+")")
	default:
		warnDestinationOnce(clk, project, tickID, destAlertUnknown,
			"delivery refused: "+decision.Status+" (origin="+decision.Origin+")")
	}
	return "", false
}
