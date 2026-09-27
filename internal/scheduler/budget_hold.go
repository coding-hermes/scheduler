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
//
// SCHED-GAP-1614 — fairness rotation and hold-event dedupe.
//
// The 1582 enforcement worked, but the live fleet ran satellite namespaces
// at demand 60-125x against allocations of 1-3, and the greedy pack —
// deterministic, urgency-ordered — queued the IDENTICAL held_names set
// cycle over cycle (measured 22:15/22:24/22:47 identical): enabled lanes
// with pending board work sat multi-day while their "project starved"
// escalations fired. Three changes, all in this file or wired to it:
//
//  1. FAIRNESS ROTATION (NamespaceHoldState + the packer wiring): the lanes
//     the previous pass HELD lead this pass's candidate order, advancing
//     one ring step per held pass, so each pass holds a DIFFERENT subset
//     and every starved lane gets its turn at the front. Deterministic —
//     the cursor is an integer, no time input — and pack-stable: placement
//     still respects the allocation, only the ORDER of equal candidates
//     changes.
//  2. HOLD-EVENT DEDUPE (signature + ShouldEmitHold): the HIGH event fires
//     on STATE CHANGE — enter hold, or the namespace's held-eligible
//     roster/demand/alloc changing — not on every pass. The signature is
//     deliberately over the namespace ROSTER, not the held subset: rotation
//     (item 1) intentionally varies the subset every pass while describing
//     the same situation, so a subset signature would re-emit forever and
//     defeat the dedupe. The measured noise was 11 identical HIGH lines
//     per cycle.
//  3. Budget-hold escalation reconciliation lives on the escalator
//     (alert_escalation.go, BudgetHoldView) and reads the packer's
//     heldByProject view built here after each pack.

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
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
	// members (SCHED-GAP-1614) is the namespace's held-eligible roster this
	// pass — every enabled, budget-gate-passed member that entered the
	// scoring loop. It is the dedupe signature's basis: rotation varies the
	// held SUBSET by design; the roster is what actually changed when the
	// situation changed.
	members []string
	// rotated (SCHED-GAP-1614) is true when rotation promoted lanes to the
	// front of this pass's candidate order and at least one of them is in
	// this hold; rotatedNames names them (canonical order).
	rotated      bool
	rotatedNames []string
	// emit (SCHED-GAP-1614) is the dedupe verdict from
	// NamespaceHoldState.NoteHold: true when the caller should emit the
	// HIGH hold event this pass (state change: first hold, roster/arithmetic
	// change, or re-enter after a not-held pass). The packer's hold state
	// records the hold either way; this only gates the event.
	emit bool
}

// newNsHold builds a hold; the caller gates on over() > 0 (under-budget
// namespaces never produce one). members is optional (variadic) so existing
// call sites keep compiling; the packer passes the roster for the 1614
// dedupe signature.
func newNsHold(nsID string, demand, alloc int, held []*ProjectUrgency, members ...string) nsHold {
	return nsHold{nsID: nsID, demand: demand, alloc: alloc, held: held, members: members}
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

// signature (SCHED-GAP-1614) is the namespace-level identity of this hold
// for event dedupe: namespace, the demand/alloc arithmetic, and the
// held-eligible ROSTER in canonical order — deliberately NOT which subset
// this pass held, because rotation intentionally varies that every pass
// while describing the same situation. Two passes with equal signatures are
// the same hold; any difference is a state change worth re-emitting.
func (h nsHold) signature() string {
	names := append([]string(nil), h.members...)
	sort.Strings(names)
	return h.nsID + "|" + itoa(h.demand) + "/" + itoa(h.alloc) + "|" + strings.Join(names, ",")
}

// logBudgetHold writes the grep-stable BUDGET-HOLD line for one namespace's
// hold. Shape mirrors the packer's other aggregate lines (FLAT-FALLBACK,
// NS-UNASSIGNED): one line per namespace per cycle, names inline.
// SCHED-GAP-1614: when rotation carried held-from-last-pass lanes into this
// hold, they are named after the set so the rotation is visible per cycle.
func logBudgetHold(h nsHold) {
	rotation := ""
	if h.rotated {
		rotation = " — rotated to front: " + strings.Join(h.rotatedNames, ",")
	}
	log.Printf("BUDGET-HOLD: namespace %s over budget (demand %d > alloc %d, over %d) — holding %d project(s) this cycle: %v%s",
		h.nsID, h.demand, h.alloc, h.over(), len(h.held), h.heldNames(), rotation)
}

// emitBudgetHoldEvent writes the events-table row for a held namespace
// through the shared EventLogger (the same channel as gateway-health
// transitions and EVAL-ZERO-SELECT). el nil (tooling) = no-op; Emit is
// already non-blocking and best-effort. SCHED-GAP-1614: the caller (Loop
// evaluate) dedupes this to state changes via nsHold.signature, and the
// payload reports the rotation provenance when rotation ran.
func emitBudgetHoldEvent(el *EventLogger, h nsHold, passID int64, now time.Time) {
	if el == nil {
		return
	}
	details := map[string]any{
		"namespace":    h.nsID,
		"demand":       h.demand,
		"allocation":   h.alloc,
		"over":         h.over(),
		"held":         len(h.held),
		"held_names":   h.heldNames(),
		"pass_id":      passID,
		"evaluated_at": now.UTC().Format(time.RFC3339),
	}
	if h.rotated {
		details["rotated_to_front"] = h.rotatedNames
	}
	el.Emit(context.Background(), SeverityHigh, "scheduler",
		"namespace over weight budget: work held this cycle", details)
}

// recordBudgetHoldDeferrals persists the per-lane hold: one deferrals row per
// queued candidate, reason "budget" (the SCHED-GAP-155 vocabulary entry the
// admission pass already uses for the weight-budget residual), detail naming
// the namespace and the arithmetic. rec is the Loop — or a test double — so
// the packer never needs a DB handle of its own. SCHED-GAP-1614: the detail
// also names the lanes rotation carried to the front, so a deferral row for a
// repeatedly-held lane shows whether it was promoted and held anyway.
func recordBudgetHoldDeferrals(rec deferralRecorder, h nsHold, passID int64) {
	if rec == nil {
		return
	}
	detail := "budget_hold ns=" + h.nsID +
		" demand=" + itoa(h.demand) +
		" alloc=" + itoa(h.alloc) +
		" over=" + itoa(h.over())
	if h.rotated {
		detail += " rotated_to_front=" + strings.Join(h.rotatedNames, ",")
	}
	for _, pu := range h.held {
		rec.recordDeferral(pu.Project.Name, AdmissionReasonBudget, passID, detail)
	}
}

// deferralRecorder is the minimal surface of Loop the hold path needs, so a
// packer-side test can supply a recording stub instead of a full Loop.
type deferralRecorder interface {
	recordDeferral(project, reason string, passID int64, detail string)
}

// ---------------------------------------------------------------------------
// SCHED-GAP-1614: per-namespace hold state — the fairness-rotation cursor
// and the hold-event dedupe memory.
// ---------------------------------------------------------------------------

// NamespaceHoldState carries ONE namespace's budget-hold memory across
// evaluation passes:
//
//   - the ROTATION CURSOR: which previously-held lanes lead the next pass's
//     candidate order (one ring step per held pass, so the promotion ring
//     visits every lane);
//   - the DEDUPE memory: the signature of the last emitted hold, so the HIGH
//     event fires on state change only;
//   - the IN-HOLD flag, so re-entering a hold after leaving it re-emits even
//     when the signature is unchanged.
//
// The live state is owned by the MultiPoolPacker (created by
// NewMultiPoolPacker, keyed by namespace ID, guarded by the packer's
// holdMu), so rotation and dedupe survive for the life of the daemon. Tests
// construct it standalone with NewNamespaceHoldState and drive deterministic
// sequences — the state takes NO time input; the ring is an integer cursor.
//
// A nil *NamespaceHoldState is legal everywhere and degrades to pass-through
// (no rotation, always emit): legacy call sites and tooling are unaffected.
type NamespaceHoldState struct {
	mu sync.Mutex
	// cursor is the ring position: the previously-held set, sorted, rotates
	// so the lane at (cursor mod len) is promoted first. Advanced once per
	// held pass by EndPass.
	cursor int
	// lastHeld is the previous held pass's member names (canonical sorted
	// order). hasLast distinguishes "never held" (a real empty hold cannot
	// occur — a hold always has members) from "no previous pass".
	lastHeld []string
	hasLast  bool
	// lastSig is the canonical signature of the last hold SEEN by NoteHold
	// (emitted or deduped); hasSig distinguishes the first hold from an
	// empty-signature hold. lastDemand/lastAlloc/lastMembers keep the
	// structured view the escalator surface (BudgetHoldView) renders.
	lastSig     string
	hasSig      bool
	lastDemand  int
	lastAlloc   int
	lastMembers []string
	// inHold is true from NoteHold until NoteNoHold: it makes the NEXT
	// NoteHold after a gap a fresh enter (re-emit) even at the same
	// signature.
	inHold bool
}

// NewNamespaceHoldState builds ready-to-use hold state.
func NewNamespaceHoldState() *NamespaceHoldState { return &NamespaceHoldState{} }

// BeginPass returns the lanes to PROMOTE to the front of this pass's
// candidate order: the members of the previous held set that are still
// eligible, in ring order starting at (cursor mod len). Empty result = no
// rotation this pass (first pass, no prior hold, or none of the previously
// held lanes is eligible today) — the caller leaves its order untouched.
// The cursor itself advances in EndPass, once per held pass.
func (s *NamespaceHoldState) BeginPass(members []string) (rotated []string) {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasLast || len(s.lastHeld) == 0 {
		return nil
	}
	eligible := make(map[string]bool, len(members))
	for _, n := range members {
		eligible[n] = true
	}
	// The previously-held ring, rotated to the cursor position.
	k := s.cursor % len(s.lastHeld)
	ring := append(append([]string(nil), s.lastHeld[k:]...), s.lastHeld[:k]...)
	for _, n := range ring {
		if eligible[n] {
			rotated = append(rotated, n)
		}
	}
	return rotated
}

// EndPass records the held member set produced by the pass BeginPass
// ordered, and advances the rotation ring one step. next may be empty in
// principle (an empty hold never reaches this state in the packer, but the
// state stays total). Canonical order is stored so the ring is independent
// of the pack's queue ordering.
func (s *NamespaceHoldState) EndPass(next []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHeld = append([]string(nil), next...)
	sort.Strings(s.lastHeld)
	s.hasLast = true
	s.cursor++
}

// NoteHold records that the namespace is in hold and reports whether the
// HIGH event should EMIT: the first hold always emits, an unchanged
// signature while continuously in hold does not, any signature change
// re-emits, and a hold after a not-held gap re-emits (a fresh enter). The
// recording happens whether or not the event is emitted — the dedupe window
// is continuous-hold, not since-last-emit.
func (s *NamespaceHoldState) NoteHold(h nsHold) bool {
	sig := h.signature()
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := !s.inHold
	s.inHold = true
	s.lastDemand = h.demand
	s.lastAlloc = h.alloc
	s.lastMembers = append([]string(nil), h.members...)
	if s.hasSig && sig == s.lastSig && !fresh {
		return false
	}
	s.lastSig = sig
	s.hasSig = true
	return true
}

// NoteNoHold records that the namespace was NOT held this pass, so the next
// NoteHold is a fresh enter and re-emits (enter/leave/enter is three state
// changes, not one long hold).
func (s *NamespaceHoldState) NoteNoHold() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inHold = false
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
