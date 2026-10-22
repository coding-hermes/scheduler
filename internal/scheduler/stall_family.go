package scheduler

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// SCHED-GAP-1755 — per-family per-turn stall policy + the liveness heartbeat
// channel.
//
// ── The measured problem (scheduler.db, 24h window ending 2026-10-22 05:40) ──
//
// The SCHED-GAP-117/119 per-turn deadline (`--gateway-response-timeout`,
// default 30m) was the fleet's #1 failure class: 70 of 140 failed ticks in
// the window carried `stalled: no progress for 30m0s`. The population splits
// into TWO shapes, and only one of them is the shape the deadline was built
// for:
//
//	A) ROLE-SUFFIX (SATELLITE) LANES — 50 of 70 stalls, 100% of them
//	   gateway_trace deadline_mode="wall", events=0, session_id="": the POST
//	   produced NOT EVEN RESPONSE HEADERS for the whole 30m. Observed
//	   families: sync 29 (eduos-sync 3, chimera-v2-sync 2, crier-sync 2,
//	   duckbrain-sync 2, monitoring-sync 2, pulse-sync 2, + 16 one-offs),
//	   pm 15, qa 3, dogfood 2, perf 1. Their turns are long un-streamed
//	   writes (DuckBrain HTTP syncs, board/PM passes, QA batteries) and the
//	   gateway's own model-fallback chain can cycle for the whole window
//	   before its first chunk (agent.log 2026-10-22 00:24-00:47 pulse-sync:
//	   gpt-6-luna → longcat-2.5-preview → mimo-v2.6-pro, one attempt per
//	   ~10m, history=0 each). The 30m deadline was measuring GATEWAY QUEUE /
//	   FALLBACK LATENCY, not agent liveness. The split is EXACT, not
//	   approximate: a re-read of the rolling window 40m later (55 satellite
//	   stalls) found 55/55 wall+zero-event and 20/20 primary stalls
//	   idle+events>0 — no exceptions in either direction.
//
//	B) PRIMARY/FOREMAN LANES — 20 of 70 stalls, ALL deadline_mode="idle"
//	   with events>0 (173/173/149/145/…): the stream was established and the
//	   turn then went quiet for 30m. That is exactly the GAP-119 shape (a
//	   wedged turn inside a productive stream) and the deadline stays armed
//	   for those families.
//
// ── The mechanism (AC 1) ──
//
// (a) FAMILY EXEMPTION, chosen as the primary mechanism. The per-turn
// deadline is a PER-FAMILY policy: every role-suffix family (the canonical
// satellite table, laneRoleSuffixes — sync, qa, pm, dogfood, perf, docs,
// readme, releng, review) is EXEMPT by default, i.e. its POST runs under the
// tick wall (`--tick-timeout`, 3h) alone and can never be failed as
// "stalled: no progress". Primary/role-less (foreman) lanes keep the
// deadline exactly as shipped. The exemption is a policy, not a removal: an
// operator (or a test) can ARM any family with SetStallFamilyDeadline, and
// the census exposes the value actually in force per family (AC 2).
//
// (b) A LIVENESS HEARTBEAT CHANNEL backs the deadline for every family that
// still has one armed (AC 4's second prong). The channel is the gateway
// session's activity in the Hermes agent state store — `sessions.
// tool_call_count` plus `session_model_usage` token totals, keyed by
// session_key = tick id — which the gateway writes as the agent works, on a
// completely different path from the SSE stream. The tick's POST can
// therefore be alive while emitting zero SSE events, and the deadline now
// reads that. A timer fire is a STALL only when the heartbeat channel is
// EMPTY (no readable channel, or a signature that has not moved since the
// previous fire). This is the same probe the SCHED-GAP-1707 silence
// watchdog already trusts (probeSessionActivity); reusing it keeps ONE
// definition of "this session is doing something".
//
// ── What is deliberately NOT here ──
//
//   - No max_concurrent / admission change: this is not an admission
//     problem (the row says so, and the satellite stalls are queue+fallback
//     latency, not slot starvation).
//   - No weakening for foreman lanes: their 20 stalls are the GAP-119 shape
//     and keep firing; the heartbeat only spares them when the session is
//     demonstrably writing.
//   - No new failure enum: an exempt-family turn still fails honestly on the
//     tick wall (tick_timeout) if it really is wedged.

// StallFamilyPrimary is the census family of a role-less lane (foremans and
// solo primaries). Every other family is named by its role suffix without the
// leading dash ("sync", "qa", "pm", …).
const StallFamilyPrimary = "primary"

// StallFamilyOf maps a lane name to its stall-census family. It reads the ONE
// role-suffix table (laneRoleSuffixes, SCHED-GAP-1696) so the stall policy can
// never drift from the admission law's satellite classification: a lane the
// fleet calls a satellite is a lane whose deadline this policy governs.
func StallFamilyOf(lane string) string {
	for _, s := range laneRoleSuffixes {
		if strings.HasSuffix(lane, s) {
			return strings.TrimPrefix(s, "-")
		}
	}
	return StallFamilyPrimary
}

// StallFamilyExempt reports whether a family's per-turn deadline is DISARMED
// by default. True for every role-suffix family (the satellite class), false
// for StallFamilyPrimary — see the header evidence (50/50 zero-event stalls
// were role-suffix lanes; all 20 event-bearing stalls were primary lanes).
func StallFamilyExempt(family string) bool {
	if family == "" || family == StallFamilyPrimary {
		return false
	}
	for _, s := range laneRoleSuffixes {
		if strings.TrimPrefix(s, "-") == family {
			return true
		}
	}
	return false
}

var (
	stallFamilyMu        sync.RWMutex
	stallFamilyDeadlines = map[string]time.Duration{}
)

// SetStallFamilyDeadline pins one family's per-turn deadline explicitly
// (SCHED-GAP-1755). d > 0 ARMS the deadline for that family (overriding its
// default exemption — the AC 4 arm, and the operator escape hatch when a
// family starts wedging); d == 0 keeps/disarms the exemption. Negative values
// are ignored as nonsensical (the same posture as SetGatewayResponseTimeout).
// Process-wide: the daemon wires nothing here — the defaults ARE the fix — but
// tests and an operator script can re-arm a family.
func SetStallFamilyDeadline(family string, d time.Duration) {
	if family == "" || d < 0 {
		return
	}
	stallFamilyMu.Lock()
	defer stallFamilyMu.Unlock()
	stallFamilyDeadlines[family] = d
}

// SetStallFamilyDeadlineForLane is the lane-name form of
// SetStallFamilyDeadline (resolves the family first).
func SetStallFamilyDeadlineForLane(lane string, d time.Duration) {
	SetStallFamilyDeadline(StallFamilyOf(lane), d)
}

// ResetStallFamilyDeadlines drops every explicit pin, restoring the shipped
// defaults (role-suffix families exempt, primary armed). Test hook.
func ResetStallFamilyDeadlines() {
	stallFamilyMu.Lock()
	defer stallFamilyMu.Unlock()
	stallFamilyDeadlines = map[string]time.Duration{}
}

// stallFamilyDeadline resolves the per-turn deadline that APPLIES to one
// family: an explicit pin wins; otherwise the family default (0 = exempt for
// role-suffix families) or the daemon-wide knob. 0 means "no per-turn
// deadline — the tick wall is the only bound".
func stallFamilyDeadline(family string, global time.Duration) time.Duration {
	stallFamilyMu.RLock()
	pinned, ok := stallFamilyDeadlines[family]
	stallFamilyMu.RUnlock()
	if ok {
		return pinned
	}
	if StallFamilyExempt(family) {
		return 0
	}
	if global < 0 {
		return 0
	}
	return global
}

// StallDeadlineForLane is the exported resolution used by the spawn path and
// the census surfaces: the per-turn deadline (0 = none) that applies to this
// lane given the daemon-wide knob.
func StallDeadlineForLane(lane string, global time.Duration) time.Duration {
	return stallFamilyDeadline(StallFamilyOf(lane), global)
}

// StallFamilyPolicy is one row of the stall census (AC 2): the family, the
// per-turn deadline actually in force for it (0 = exempt), and whether the
// liveness heartbeat channel is armed (always true — the channel is
// unconditional; the field exists so an operator reading the census can see
// the deadline is not the only supervisor).
type StallFamilyPolicy struct {
	Family     string        `json:"family"`
	Deadline   time.Duration `json:"-"`
	DeadlineMS int64         `json:"deadline_ms"`
	// DeadlineLabel is the operator-readable value: "exempt" or the duration.
	DeadlineLabel string `json:"deadline"`
	Exempt        bool   `json:"exempt"`
	Heartbeat     bool   `json:"heartbeat"`
}

// StallFamilyPolicies returns every family's resolved policy, sorted by family
// name (deterministic output for the census endpoint and its tests).
func StallFamilyPolicies(global time.Duration) []StallFamilyPolicy {
	families := make([]string, 0, 1+len(laneRoleSuffixes))
	families = append(families, StallFamilyPrimary)
	for _, s := range laneRoleSuffixes {
		families = append(families, strings.TrimPrefix(s, "-"))
	}
	sort.Strings(families)
	out := make([]StallFamilyPolicy, 0, len(families))
	for _, f := range families {
		d := stallFamilyDeadline(f, global)
		label := "exempt"
		if d > 0 {
			label = d.String()
		}
		out = append(out, StallFamilyPolicy{
			Family:        f,
			Deadline:      d,
			DeadlineMS:    d.Milliseconds(),
			DeadlineLabel: label,
			Exempt:        d == 0,
			Heartbeat:     true,
		})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// The liveness heartbeat channel (AC 1 prong b / AC 4).
// ─────────────────────────────────────────────────────────────────────────────

// stallHeartbeatProbe is the package seam the turn watch reads. It returns a
// MONOTONIC activity signature for one tick's gateway session — 0 means the
// channel is empty (no session row yet: a POST still queued at the gateway, or
// an unreadable store). Tests swap it to drive the channel without a live
// Hermes state store; production keeps defaultStallHeartbeatSignal.
var stallHeartbeatProbe = defaultStallHeartbeatSignal

// defaultStallHeartbeatSignal reads the gateway session's progress from the
// Hermes agent state store: token totals (session_model_usage, keyed by
// session id) + the session's tool-call counter, resolved through
// session_key = tick id. Both terms are monotonic, so any NEW work moves the
// sum. An unreadable store or a session the gateway never registered reads as
// 0 = empty channel, which never spares a tick (fail-closed: an unmeasurable
// channel is an empty one, exactly like the silence watchdog's contract).
func defaultStallHeartbeatSignal(tickID string) int64 {
	p := probeSessionActivity(tickID)
	if !p.found {
		return 0
	}
	return p.total + p.toolCalls
}

// stallHeartbeat is one tick's heartbeat channel handle. The probe is captured
// at construction so a test swap after the fact cannot move a live watch.
type stallHeartbeat struct {
	tickID string
	probe  func(string) int64
}

// newStallHeartbeat builds the channel handle for one tick, or nil when the
// tick has no id (nothing to key the store lookup on).
func newStallHeartbeat(tickID string) *stallHeartbeat {
	if tickID == "" {
		return nil
	}
	return &stallHeartbeat{tickID: tickID, probe: stallHeartbeatProbe}
}

// signature reads the channel once. nil receiver = empty channel (0).
func (h *stallHeartbeat) signature() int64 {
	if h == nil || h.probe == nil {
		return 0
	}
	return h.probe(h.tickID)
}
