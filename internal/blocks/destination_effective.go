package blocks

import (
	"sort"
	"strings"
)

// Delivery-time destination decisions (SCHED-GAP-17084).
//
// InventoryDestinations is the AUDIT twin of this file: it classifies a whole
// fleet in one pass and never carries a destination value. DecideDelivery is
// the single-project, DELIVERY-TIME sibling — same status vocabulary, plus the
// effective target the scheduler must actually use, which it returns out of
// band (Effective has no JSON tag) because a report's destination is private.
//
// Ownership rule (identical to the inventory): a group's destination_policy is
// authoritative only when it is itself approved. A project owned by ZERO
// authorized groups is left exactly as it is — no approved mapping exists, so
// nothing can be called drift, and refusing it would silence a lane for a
// policy that was never approved. A project owned by MORE THAN ONE group where
// at least one policy is approved is refused: agreement between groups does not
// establish which owner approved the target.
const (
	// DestinationAligned — the current target IS the approved destination.
	DestinationAligned = "aligned"
	// DestinationInherited — no target is set, so delivery inherits the
	// approved group destination (lane inheritance). The inventory reports
	// this same state as "unset" because it has no delivery to inherit into.
	DestinationInherited = "inherited"
	// DestinationDrift — the current target differs from the approved one.
	DestinationDrift = "drift"
	// DestinationConflict — more than one owning group carries an approved
	// policy, so ownership of the target is ambiguous.
	DestinationConflict = "conflict"
	// DestinationUnknown — no approved policy owns this project; delivery is
	// unchanged (the historical behaviour).
	DestinationUnknown = "unknown"
	// DestinationUnapproved — an exception exists for the project but was
	// never approved, so it cannot authorize a route.
	DestinationUnapproved = "unapproved"
	// DestinationInvalid — the approved destination is not a telegram
	// chat+thread pair, so the approval itself is broken.
	DestinationInvalid = "invalid"
)

// DeliveryDecision is the delivery-time verdict for one project's target.
type DeliveryDecision struct {
	// Allowed reports whether delivery may proceed at all.
	Allowed bool `json:"allowed"`
	// Status is one of the Destination* constants above.
	Status string `json:"status"`
	// Origin names the owning group (and ":exception" for an approved
	// exception). Several owners are comma-joined in name order.
	Origin string `json:"origin,omitempty"`
	// Effective is the target delivery must use. It equals the project's
	// current value unless the decision inherited the group destination.
	// Deliberately excluded from JSON: destinations are private.
	Effective string `json:"-"`
}

// DecideDelivery classifies one project's current delivery target against the
// approved destination policies and returns the effective target.
//
// It never mutates its inputs and never invents a route: an unapproved group
// policy, an unapproved exception, an invalid approved target, a conflicting
// membership or a target that disagrees with the approval all resolve to
// Allowed=false with an empty Effective value.
// ch:trace row=SCHED-GAP-17084
func DecideDelivery(groups []Group, project, current string) DeliveryDecision {
	decision := DeliveryDecision{Allowed: true, Status: DestinationUnknown, Effective: current}

	var owners []string
	for _, group := range groups {
		if containsString(group.Projects, project) {
			owners = append(owners, group.Name)
		}
	}
	if len(owners) == 0 {
		return decision
	}
	sort.Strings(owners)
	decision.Origin = strings.Join(owners, ",")

	// Only an APPROVED policy can be authoritative. Everything else leaves
	// delivery untouched, which is the pre-17084 behaviour.
	authorized := false
	var policy DestinationPolicy
	for _, owner := range owners {
		for _, group := range groups {
			if group.Name != owner || group.DestinationPolicy == nil || !group.DestinationPolicy.Approved {
				continue
			}
			authorized = true
			policy = *group.DestinationPolicy
		}
	}
	if !authorized {
		return decision
	}
	if len(owners) > 1 {
		decision.Allowed = false
		decision.Status = DestinationConflict
		decision.Effective = ""
		return decision
	}
	if !validTelegramDestination(policy.Destination) {
		decision.Allowed = false
		decision.Status = DestinationInvalid
		decision.Effective = ""
		return decision
	}

	effective := policy.Destination
	if exception, exists := policy.Exceptions[project]; exists {
		if !exception.Approved {
			decision.Allowed = false
			decision.Status = DestinationUnapproved
			decision.Effective = ""
			return decision
		}
		if !validTelegramDestination(exception.Destination) {
			decision.Allowed = false
			decision.Status = DestinationInvalid
			decision.Effective = ""
			return decision
		}
		effective = exception.Destination
		decision.Origin += ":exception"
	}

	switch current {
	case "":
		decision.Status = DestinationInherited
		decision.Effective = effective
	case effective:
		decision.Status = DestinationAligned
		decision.Effective = current
	default:
		decision.Allowed = false
		decision.Status = DestinationDrift
		decision.Effective = ""
	}
	return decision
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
