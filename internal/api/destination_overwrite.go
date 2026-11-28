package api

import (
	"fmt"
	"sort"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

// Destination-policy overwrite alert (SCHED-GAP-17084).
//
// Once an approved destination_policy is authoritative for delivery (see
// internal/scheduler/deliver_destination_guard.go), a PUT that rewrites that
// policy silently re-routes every lane the group owns — the exact class of
// "someone edited the mapping and reports started landing somewhere else"
// drift this row was filed about. The store must stay editable (the API is the
// only writer of groups.jsonl), so this is an ALERT, not a refusal: the write
// proceeds exactly as before and the change is named in the daemon log.
//
// The description never quotes a destination: it reports which parts of the
// approval moved, because a chat id in a log line is a leak like any other.

// destinationPolicyOverwrite describes how a PUT changes an existing approval.
// It returns "" when nothing about the authority moved (no prior policy, no
// patch, or a byte-equivalent policy).
func destinationPolicyOverwrite(prior, next *blocks.DestinationPolicy) string {
	if prior == nil || next == nil {
		return ""
	}
	var changes []string
	if prior.Approved != next.Approved {
		if next.Approved {
			changes = append(changes, "approval granted")
		} else {
			changes = append(changes, "approval withdrawn — delivery is no longer enforced for this group")
		}
	}
	if prior.Destination != next.Destination {
		switch {
		case prior.Destination == "":
			changes = append(changes, "destination set")
		case next.Destination == "":
			changes = append(changes, "destination cleared")
		default:
			changes = append(changes, "approved destination replaced")
		}
	}
	if diff := exceptionsDiff(prior.Exceptions, next.Exceptions); diff != "" {
		changes = append(changes, diff)
	}
	if len(changes) == 0 {
		return ""
	}
	parts := ""
	for i, change := range changes {
		if i > 0 {
			parts += "; "
		}
		parts += change
	}
	return parts
}

// exceptionsDiff summarizes a change to the per-project exception map without
// naming any destination. Additions/removals are counted, and an exception
// whose approval state or destination changed is counted as "redefined".
func exceptionsDiff(prior, next map[string]blocks.DestinationException) string {
	names := make([]string, 0, len(prior)+len(next))
	seen := map[string]bool{}
	for name := range prior {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for name := range next {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var added, removed, redefined []string
	for _, name := range names {
		before, hadBefore := prior[name]
		after, hasAfter := next[name]
		switch {
		case !hadBefore && hasAfter:
			added = append(added, name)
		case hadBefore && !hasAfter:
			removed = append(removed, name)
		case before != after:
			redefined = append(redefined, name)
		}
	}
	out := ""
	if len(added) > 0 {
		out += fmt.Sprintf("%d exception(s) added", len(added))
	}
	if len(removed) > 0 {
		out += fmt.Sprintf("%s%d exception(s) removed", separator(out), len(removed))
	}
	if len(redefined) > 0 {
		out += fmt.Sprintf("%s%d exception(s) redefined (authority and/or target changed)", separator(out), len(redefined))
	}
	return out
}

func separator(existing string) string {
	if existing == "" {
		return ""
	}
	return "; "
}
