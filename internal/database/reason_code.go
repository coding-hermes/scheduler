package database

import (
	"strings"
)

// SCHED-GAP-16947 — the closed-vocabulary reason code carried on every
// concluded tick (ticks.reason_code, migration v91).
//
// THE PROBLEM. The 2026-10-29 measurement over 7 days of ticks found
// `failure_reason` empty on 100% of dry ticks while the classification read
// 'completed' for every instrumented row: the schema could not say WHY a tick
// produced nothing. Five different mechanisms were collapsed into one
// `dry_run` verdict —
//
//	(a) the model ran and declared no-op            (noop_flag=1 on 93% of foreman dry ticks)
//	(b) the model did real work, landed no artifact (pre_commit == post_commit on 1,154/1,155 dry ticks;
//	                                                 verdicts like "OK" after 32 tool calls)
//	(c) the session never produced output at all    (22.5% of dry traces carry NO tokens_out field;
//	                                                 96/473 instrumented dry ticks overran their own deadline,
//	                                                 one at 2,380s against an 1,800s wall, 133,713,830 input tokens)
//	(d) the tick never opened a session by design   (-qa/-releng-qa/-perf-qa lanes, 60 rows in 7d)
//	(e) a historical lane blackout booked as idle   (2,701 zero-token dry ticks on 2026-10-10..12 —
//	                                                 1,043/1,034/624 per day — while other lanes committed)
//
// The reason code is a NEW dimension on top of ticks.outcome: the outcome says
// what the tick produced, the reason code says why it produced nothing. It is
// stamped by every conclusion path in ONE write per row (lifecycle.Complete and
// the three side writers: the stale reap, the dead-owner reap, the usage-pool
// lease expiry, plus the sim finalizer) — no second UPDATE that can silently
// miss a row.
//
// SEMANTICS
//
//	''                 — productive tick (an artifact landed) or legacy row:
//	                     no reason is owed. The empty string is the honest
//	                     "nothing to explain", the same convention
//	                     failure_reason uses on successful rows.
//	no_work            — the session was MEASURED running zero tool calls
//	                     (outcome no_work): the lane had nothing to do.
//	model_noop         — the no-op guard measured pre==post on a lane that may
//	                     not no-op (ticks.noop_flag=1): the model ran and
//	                     declared nothing.
//	no_artifact        — the model produced output (a measured token figure) and
//	                     landed no measurable artifact.
//	malformed_tool_call— the session produced no usable model output: no usage
//	                     figure at all (mechanism c), an instant-turn kill, or a
//	                     MEASURED zero output on a dry tick.
//	deadline_exceeded  — the tick ran into its own wall: its session/dispatch
//	                     deadline fired, INCLUDING the liveness catch (a
//	                     completed verdict that overran its deadline is forced
//	                     to timeout/deadline_exceeded, never left completed).
//	not_spawned        — the tick never opened a real session by design (a
//	                     deferred tick, or a simulated tick).
//	lane_unavailable   — the lane's execution surface was unavailable: gateway
//	                     drain/transport failure, operator restart, an
//	                     owner-gone reap (stale heartbeat, dead pid, expired
//	                     usage-pool lease), or the repaired 2026-10-10..12
//	                     blackout.
//	unknown:<raw>      — none of the above. The prefix form is LOAD-BEARING: a
//	                     bare `unknown` is invalid (the CHECK rejects it,
//	                     TickReasonCodeIsValid rejects it) because an unknown
//	                     that cannot say what made it unknown is the exact
//	                     silent void this row closes. The raw signal is
//	                     whitespace-collapsed and bounded
//	                     (tickReasonCodeUnknownMaxRaw), never empty.
const (
	TickReasonCodeNoWork            = "no_work"
	TickReasonCodeModelNoop         = "model_noop"
	TickReasonCodeNoArtifact        = "no_artifact"
	TickReasonCodeMalformedToolCall = "malformed_tool_call"
	TickReasonCodeDeadlineExceeded  = "deadline_exceeded"
	TickReasonCodeNotSpawned        = "not_spawned"
	TickReasonCodeLaneUnavailable   = "lane_unavailable"

	// TickReasonCodeUnknownPrefix opens the escape-hatch form. Everything
	// after the prefix is the raw signal (see TickReasonCodeUnknown).
	TickReasonCodeUnknownPrefix = "unknown:"
)

// TokensOutUnavailable* are the raw marks carried by
// ticks.tokens_out_unavailable (AC3): the explicit statement that a row's
// tokens_out is NOT a measurement. The value column stays an INTEGER (readers
// scan it into int64 everywhere), so "unavailable" is carried by this
// companion reason — never by an absent key and never by a lying 0.
const (
	// TokensOutUnavailableSimulated: the sim spawner fabricated the figures.
	TokensOutUnavailableSimulated = "simulated"
	// TokensOutUnavailableEstimatedNoTelemetry: the exec path had no session
	// telemetry and fell back to a fleet-average estimate.
	TokensOutUnavailableEstimatedNoTelemetry = "estimated_no_telemetry"
	// TokensOutUnavailableGatewayNoUsage: the gateway's terminal envelope
	// carried NO usage object at all — the 22.5% shape behind mechanism (c).
	TokensOutUnavailableGatewayNoUsage = "gateway_envelope_no_usage"
	// TokensOutUnavailableRemoteDispatch: the hand-out ran on a remote node
	// and its usage never reached this process.
	TokensOutUnavailableRemoteDispatch = "remote_dispatch_no_usage"
	// TokensOutUnavailableNoSession: the tick concluded without ever opening a
	// session (deferred / aborted before spawn): there is nothing to measure.
	TokensOutUnavailableNoSession = "no_session_opened"
)

// TickReasonCodes is the frozen closed vocabulary in reporting order — the
// five dry-tick mechanisms first, then the two non-productive lifecycle
// verdicts. Consumers that must render every bucket range over this; the
// ticks.reason_code CHECK (migration v91) enforces exactly this set plus the
// unknown:<raw> escape hatch.
var TickReasonCodes = []string{
	TickReasonCodeNoWork,
	TickReasonCodeModelNoop,
	TickReasonCodeNoArtifact,
	TickReasonCodeMalformedToolCall,
	TickReasonCodeDeadlineExceeded,
	TickReasonCodeNotSpawned,
	TickReasonCodeLaneUnavailable,
}

// TickReasonCodeIsValid reports whether code belongs to the closed vocabulary
// or is a well-formed unknown:<raw> escape hatch. The bare forms "unknown" and
// "unknown:" are deliberately INVALID — an unknown code that cannot say what
// made it unknown is a bare unknown wearing a hat. The empty string is also
// NOT valid here: it is the "productive / legacy" value, checked with == ""
// like every other honest-empty column.
func TickReasonCodeIsValid(code string) bool {
	if strings.HasPrefix(code, TickReasonCodeUnknownPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(code, TickReasonCodeUnknownPrefix)) != ""
	}
	for _, c := range TickReasonCodes {
		if c == code {
			return true
		}
	}
	return false
}

// tickReasonCodeUnknownMaxRaw bounds the raw signal after the unknown: prefix.
// The signal is an operator-facing hint (often an error text), not a log —
// long enough to name the cause, short enough to stay one column.
const tickReasonCodeUnknownMaxRaw = 120

// TickReasonCodeUnknown renders the escape-hatch code for a raw signal:
// "unknown:" + the signal, whitespace-collapsed and rune-bounded. An empty
// signal becomes "unknown:unspecified" — the prefix contract (no bare unknown)
// holds even when the caller had nothing to say.
func TickReasonCodeUnknown(raw string) string {
	raw = strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if raw == "" {
		raw = "unspecified"
	}
	if r := []rune(raw); len(r) > tickReasonCodeUnknownMaxRaw {
		raw = string(r[:tickReasonCodeUnknownMaxRaw])
	}
	// A raw signal that is only whitespace after collapsing cannot happen
	// (Fields drops it), and an embedded colon is harmless: everything after
	// the FIRST prefix is the signal.
	return TickReasonCodeUnknownPrefix + raw
}

// CoerceTickReasonCode returns code when it satisfies the closed vocabulary and
// otherwise degrades it to a well-formed unknown carrying the offending value
// as the raw signal. This is the write-site guard: the ticks.reason_code CHECK
// (migration v91) would reject a bad value and fail the whole finalization
// UPDATE — a derivation bug must never be able to strand a tick in 'running' by
// breaking its own completion write. Valid codes (including every unknown:<raw>
// form) pass through byte-identically; the empty string (productive / legacy)
// also passes — it is not a vocabulary member, it is the honest-empty
// convention.
func CoerceTickReasonCode(code string) string {
	if code == "" || TickReasonCodeIsValid(code) {
		return code
	}
	return TickReasonCodeUnknown("unresolved:" + code)
}
