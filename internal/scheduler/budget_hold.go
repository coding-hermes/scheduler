package scheduler

// SCHED-GAP-1582 — weight-budget enforcement with a visible reason.
//
// The defect this closes: when a namespace's demand oversubscribed its
// allocation, CalcEffectiveWeight RESCALED every member project down so the
// arithmetic always fit (that scaling is the allocator's fair-share contract
// and stays). The consequence was that an oversubscribed namespace was
// indistinguishable from a right-sized one — its selection output shrank
// silently, nothing surfaced, and the only operator-facing hint was a
// dashboard bar that overflowed by construction.
//
// What is added here, per the row:
//
//  1. Oversubscription is DETECTED per cycle (oversubscription, below) and
//     carried on the pack result — NamespaceTickData.Demand +
//     Overcommitted, persisted to namespace_ticks (migration v43) through
//     the existing InsertNamespaceTick write path, so "was this namespace
//     over budget" is a column on the utilization history, not an inference
//     from used == allocated.
//  2. The pass-over is RECORDED per lane with a reason: every candidate the
//     greedy pack queued because it did not fit gets one deferrals row
//     through the SCHED-GAP-157 path (reason "budget", the existing
//     AdmissionReasonBudget vocabulary entry) — no parallel observability
//     channel is invented.
//  3. A HIGH event is emitted once per oversubscribed namespace per cycle
//     through the shared EventLogger, so the state reaches
//     GET /api/v1/events/stream (CTL-002) like the gateway-health and
//     zero-select events.
//
// The policy decision (whether an oversubscribed namespace SHOULD be allowed
// to oversubscribe, and how the operator opts in) is SCHED-GAP-1584 and is
// deliberately not made here: the state is made truthful and observable,
// nothing more. Selection order and the greedy arithmetic are untouched.

import (
	"context"
	"log"
	"time"
)

// oversubscription reports whether a namespace's demand exceeds its
// allocation, and by how much. demand is the sum of the weights of the
// namespace's ENABLED projects the packer was asked to place; alloc is the
// two-phase allocation the NamespaceAllocator produced for this cycle.
//
// demand > alloc means the share-out cannot place the whole namespace this
// cycle regardless of ordering: the packer's placement arithmetic sums to
// alloc. The surplus (demand-alloc) is therefore HELD — not rescaled away
// silently — and the quantity is a statement about the namespace's
// configuration vs its budget, not about which project lost the race.
func oversubscription(demand, alloc int) int {
	if demand > alloc {
		return demand - alloc
	}
	return 0
}

// nsHold is one namespace's budget hold: the candidates the greedy pack
// queued (did not fit) plus the arithmetic that explains the hold.
//
// held carries st.queued — the candidates NOT placed this cycle. The queue is
// budget/concurrency-shaped by construction (cooldown-skipped candidates are
// never queued), and a hold only exists when demand > alloc, i.e. the share-out
// could not place the namespace in full regardless of ordering; queued
// projects there are held work by definition.
type nsHold struct {
	nsID   string
	demand int
	alloc  int
	held   []*ProjectUrgency
}

// newNsHold builds a hold; the caller gates on over() > 0 (under-budget
// namespaces never produce one).
func newNsHold(nsID string, demand, alloc int, held []*ProjectUrgency) nsHold {
	return nsHold{nsID: nsID, demand: demand, alloc: alloc, held: held}
}

// over returns the surplus that could not be placed.
func (h nsHold) over() int { return oversubscription(h.demand, h.alloc) }

// heldNames renders the held lane names for logs and event details.
func (h nsHold) heldNames() []string {
	out := make([]string, 0, len(h.held))
	for _, pu := range h.held {
		out = append(out, pu.Project.Name)
	}
	return out
}

// logBudgetHold writes the grep-stable BUDGET-HOLD line for one namespace's
// hold. Shape mirrors the packer's other aggregate lines (FLAT-FALLBACK,
// NS-UNASSIGNED): one line per namespace per cycle, names inline.
func logBudgetHold(h nsHold) {
	log.Printf("BUDGET-HOLD: namespace %s over budget (demand %d > alloc %d, over %d) — holding %d project(s) this cycle: %v",
		h.nsID, h.demand, h.alloc, h.over(), len(h.held), h.heldNames())
}

// emitBudgetHoldEvent writes the events-table row for a held namespace
// through the shared EventLogger (the same channel as gateway-health
// transitions and EVAL-ZERO-SELECT). el nil (tooling) = no-op; Emit is
// already non-blocking and best-effort.
func emitBudgetHoldEvent(el *EventLogger, h nsHold, passID int64, now time.Time) {
	if el == nil {
		return
	}
	el.Emit(context.Background(), SeverityHigh, "scheduler",
		"namespace over weight budget: work held this cycle",
		map[string]any{
			"namespace":    h.nsID,
			"demand":       h.demand,
			"allocation":   h.alloc,
			"over":         h.over(),
			"held":         len(h.held),
			"held_names":   h.heldNames(),
			"pass_id":      passID,
			"evaluated_at": now.UTC().Format(time.RFC3339),
		})
}

// recordBudgetHoldDeferrals persists the per-lane hold: one deferrals row per
// queued candidate, reason "budget" (the SCHED-GAP-155 vocabulary entry the
// admission pass already uses for the weight-budget residual), detail naming
// the namespace and the arithmetic. rec is the Loop — or a test double — so
// the packer never needs a DB handle of its own.
func recordBudgetHoldDeferrals(rec deferralRecorder, h nsHold, passID int64) {
	if rec == nil {
		return
	}
	detail := "budget_hold ns=" + h.nsID +
		" demand=" + itoa(h.demand) +
		" alloc=" + itoa(h.alloc) +
		" over=" + itoa(h.over())
	for _, pu := range h.held {
		rec.recordDeferral(pu.Project.Name, AdmissionReasonBudget, passID, detail)
	}
}

// deferralRecorder is the minimal surface of Loop the hold path needs, so a
// packer-side test can supply a recording stub instead of a full Loop.
type deferralRecorder interface {
	recordDeferral(project, reason string, passID int64, detail string)
}

// itoa is a tiny strconv.Itoa stand-in kept local so this file pulls no
// extra imports beyond what it uses elsewhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
