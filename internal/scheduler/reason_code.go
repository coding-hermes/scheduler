package scheduler

import (
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-16947 — the derivation half of the tick reason code.
//
// The VOCABULARY lives in internal/database/reason_code.go (the migration v91
// CHECK enforces it); THIS file decides which member a concluded tick earns.
// The derivation runs at tick finalization — lifecycle.Complete is the single
// authority (the coverage convention: every row that method writes carries the
// code, no second UPDATE that can silently miss) — over signals the finalizer
// already holds: the terminal outcome, the transport-class failure reason, the
// row's no-op verdict, the tokens-out measurement mark, and the deadline the
// spawn paths carry onto TickOutcome.
//
// THE FIVE MECHANISMS (row SCHED-GAP-16947, measured 2026-10-29) and where each
// lands:
//
//	(a) model declared no-op             → model_noop   (noop_flag=1 read from the row)
//	(b) real work, no artifact           → no_artifact  (dry_run with a token figure)
//	(c) session produced no output       → malformed_tool_call (no usage figure at all:
//	     at all                                           22.5% of dry traces carry no tokens_out;
//	                                                      a MEASURED zero output lands here too)
//	    session overran its own wall     → deadline_exceeded (the liveness rule below)
//	(d) never opened a session by design → not_spawned  (deferred ticks, simulated ticks)
//	(e) execution surface unavailable    → lane_unavailable (transport-class failures,
//	                                                      owner-gone reaps, the repaired blackout)
//
// HOW THIS DIFFERS FROM ticks.failure_reason. The SCHED-430 vocabulary
// (dryreason.go: EMPTY_BOARD / OWNERSHIP_BARRIER / GATE_WITHHELD / …) answers
// "which gate stood this lane down" and rides failure_reason; the transport
// classifier (gateway_drain / session_silent / …) answers "did the harness end
// this tick". This column answers the third question — "what MECHANISM made the
// tick produce nothing" — and is the only one that can name a silent session, a
// wall overrun, or a blackout. The three are complementary: a row can read
// failure_reason=GATE_WITHHELD and reason_code=no_artifact, and that pair says
// both which gate and which mechanism. Neither column is derived from the other.
//
// PRIORITY is evidence strength, strongest first, and it is deliberate:
//
//  1. A deadline overrun outranks everything — a tick past its wall is a
//     liveness verdict (recorded timeout/deadline_exceeded, NEVER completed),
//     no matter what artifacts it happened to land mid-window.
//  2. The no-op guard's declared verdict (model_noop) outranks the artifact
//     shape: pre==post on a lane that may not no-op is a NAMED declaration of
//     no-op, not a silent dry run. It requires a session (the guard reads git),
//     so it cannot mask a session that never spoke.
//  3. A session that produced no usage figure at all is mechanism (c) —
//     malformed_tool_call — BEFORE the no_work bucket: "nothing was measured"
//     is a stronger and more alarming statement than "measured zero tool
//     calls", and no_work is reserved for a MEASURED zero-tool transcript.
//  4. no_work: the outcome column's measured zero-tool-call verdict.
//  5. Artifact shape: no_artifact for a dry tick whose tokens were measured (a
//     session ran, spent, and landed nothing), malformed_tool_call for a
//     MEASURED zero output.
//  6. '' on a committed tick: the column explains ticks that produced nothing;
//     a productive tick owes no reason (the same convention failure_reason uses
//     on successful rows).

// tickOverranDeadline is the liveness rule's predicate: did this tick run
// at-or-past its own session wall? A tick with no known deadline (Deadline<=0 —
// slot-pool reaps, the drain path, simulated ticks) can never have overrun one,
// and zero timestamps are the honest "unmeasured", so the rule stays inert on
// hand-built outcomes. The wall is inclusive (>=): a context deadline is done AT
// its instant, and the measured offender (2,380s against 1,800s) is far past
// either way.
func tickOverranDeadline(o TickOutcome) bool {
	if o.Deadline <= 0 || o.Started.IsZero() || o.Finished.IsZero() {
		return false
	}
	return o.Finished.Sub(o.Started) >= o.Deadline
}

// resolveTickReasonCode derives the closed-vocabulary reason code for one
// concluded tick. Pure function — every input is a signal the finalizer already
// holds — so the mapping is unit-testable without a database.
//
//	o              the finalization outcome (Status, GuardAbort, telemetry mark, tokens)
//	terminal       terminalOutcome(o)'s verdict (committed/dry_run/no_work/failed/timeout/deferred)
//	failureReason  failureReasonClass(o.Error): "" unless the error is harness-classified
//	noopFlag       the tick row's ticks.noop_flag (1 = the no-op guard re-entered this lane)
func resolveTickReasonCode(o TickOutcome, terminal string, failureReason string, noopFlag int) string {
	// The liveness rule outranks every status mapping: an overrun is
	// deadline_exceeded whether the caller already knew (status=timeout) or the
	// finalizer is forcing it (completed → timeout, in Complete).
	if tickOverranDeadline(o) {
		return database.TickReasonCodeDeadlineExceeded
	}

	switch o.Status {
	case TickTimeout:
		switch o.TelemetryPartialReason {
		case TelemetryPartialTickDeadline, TelemetryPartialDispatchDeadline:
			// A wall fired (the tick's own session deadline, or the
			// dispatch-reply deadline): the overrun class.
			return database.TickReasonCodeDeadlineExceeded
		case TelemetryPartialStaleReap:
			// The owner vanished (stale heartbeat) — the lane's execution
			// surface died mid-tick, a wall did not fire.
			return database.TickReasonCodeLaneUnavailable
		}
		if failureReason != "" {
			return harnessFailureReasonCode(failureReason, o.Error)
		}
		// A timeout with no classified cause: the raw signal is the timeout
		// itself plus whatever error text rode the row.
		return database.TickReasonCodeUnknown("timeout: " + firstErrorLine(o.Error))

	case TickFailed:
		if o.GuardAbort {
			// The builder no-artifact guard aborted a read-only session: the
			// verdict is literally "no artifact" (outcome aborted:no_artifact),
			// so the reason code says the same thing.
			return database.TickReasonCodeNoArtifact
		}
		if failureReason != "" {
			return harnessFailureReasonCode(failureReason, o.Error)
		}
		return database.TickReasonCodeUnknown("failure: " + firstErrorLine(o.Error))

	case TickDeferred:
		// The harness declined to charge the lane: no session opened.
		return database.TickReasonCodeNotSpawned

	case TickAborted:
		// An operator restart aborted an in-flight tick: the lane's execution
		// surface was taken away from it (OrphanOperatorRestart /
		// OrphanDrainTimeout), never a wall and never a lane fault.
		return database.TickReasonCodeLaneUnavailable

	case TickCompleted:
		switch terminal {
		case "committed":
			// Productive tick — nothing to explain.
			return ""
		case string(database.OutcomeDryRun), string(database.OutcomeNoWork):
			if noopFlag == 1 {
				// The no-op guard measured pre==post on a lane that may not
				// no-op: the model ran and declared no work.
				return database.TickReasonCodeModelNoop
			}
			if tickSessionSilent(o) {
				// Mechanism (c): the session produced NO usage figure at all
				// (the 22.5% shape measured 2026-10-29). Nothing was measured,
				// so "zero tool calls" cannot be claimed either.
				return database.TickReasonCodeMalformedToolCall
			}
			if terminal == string(database.OutcomeNoWork) {
				// A MEASURED zero-tool transcript: the strongest
				// nothing-to-do proof there is — UNLESS the worktree probe
				// (SCHED-GAP-16936 piece 2) found uncommitted content.
				// "the lane had nothing to do" is a claim about the lane's
				// state, and a dirty worktree refutes it: there IS work
				// sitting there, it just was not landed by this tick. The
				// vocabulary is closed (v91 CHECK) and holds no member for
				// the I-did-nothing-but-content-exists case, so the honest
				// code is no_artifact (the artifact question is open, not
				// answered by "nothing to do") while recovery_state carries
				// the precise evidence.
				if o.RecoveryState == RecoveryStateAvailable {
					return database.TickReasonCodeNoArtifact
				}
				return database.TickReasonCodeNoWork
			}
			if o.TokensOut <= 0 {
				// The tokens figure was measured and read zero: output that
				// never became a tool call.
				return database.TickReasonCodeMalformedToolCall
			}
			// The model produced output, ran, and landed nothing.
			return database.TickReasonCodeNoArtifact
		}
	}
	// Unreachable from the code paths (every status/terminal pair is mapped
	// above); the fallback carries the raw pair so a future shape can never land
	// as a bare unknown.
	return database.TickReasonCodeUnknown("status=" + string(o.Status) + " terminal=" + terminal)
}

// harnessFailureReasonCode maps a harness-classified failure_reason (the classes
// failureReasonClass returns) into the reason vocabulary.
//
//	gateway_drain / gateway_5xx / gateway_transport / operator_restart — the
//	  lane's execution surface was unavailable (gateway down, draining,
//	  failing upstream, or the scheduler itself restarted over the tick):
//	  lane_unavailable.
//	instant_turn — the gateway answered with a near-zero-output one-shot turn
//	  (SCHED-GAP-1641): the model produced no usable output,
//	  malformed_tool_call.
//	session_silent / sleep_poll — scheduler-side kills (SCHED-GAP-1707/1698):
//	  the session was STOPPED on suspicion, its output state is unknown, so the
//	  honest code is the unknown escape carrying the class as the raw signal —
//	  not a fabricated output verdict.
func harnessFailureReasonCode(class string, errText string) string {
	switch class {
	case FailureReasonGatewayDrain, FailureReasonGateway5xx,
		FailureReasonGatewayTransport, FailureReasonOperatorRestart:
		return database.TickReasonCodeLaneUnavailable
	case FailureReasonInstantTurn:
		return database.TickReasonCodeMalformedToolCall
	default:
		// session_silent, sleep_poll, and any future class: unknown with the
		// class as the raw signal — the vocabulary's escape hatch exists
		// exactly for named-but-unmapped causes.
		return database.TickReasonCodeUnknown("failure " + class + ": " + firstErrorLine(errText))
	}
}

// tickSessionSilent reports whether this tick's session produced NO usage
// measurement at all — mechanism (c). The signal is the explicit
// tokens_out_unavailable mark the spawn paths stamp (a gateway terminal envelope
// with no usage object is database.TokensOutUnavailableGatewayNoUsage, a
// deferred/aborted tick is TokensOutUnavailableNoSession, a simulated tick is
// TokensOutUnavailableSimulated, a remote hand-out is
// TokensOutUnavailableRemoteDispatch). A tick whose figure came from a real
// observation — including a genuine 0 — is NOT silent: it measured.
//
// Deliberately narrow: an estimate (the exec path's fleet-average fallback)
// means the session ran but was not metered, which is not the same claim as "no
// output", so only the no-usage marks count here.
func tickSessionSilent(o TickOutcome) bool {
	switch resolveTokensOutUnavailable(o) {
	case database.TokensOutUnavailableGatewayNoUsage,
		database.TokensOutUnavailableNoSession:
		return true
	}
	return false
}

// resolveTokensOutUnavailable decides whether a tick's tokens_out figure is a
// real measurement (returns "") or not (returns the raw reason it is not — the
// explicit-unavailable mark for ticks.tokens_out_unavailable). The derivation
// reads what the spawn paths stamped, in this order of authority:
//
//	explicit mark on the outcome       — the path that knows best (remote dispatch
//	                                     legs, deferred/no-session rows, a gateway
//	                                     envelope with no usage object)
//	cost source simulated              — the sim spawner fabricated the figures
//	cost source estimated(_subincluded)— no telemetry, a fleet-average estimate
//	measured / gateway / ""            — a real observation (a genuine 0 included);
//	                                     "" is the legacy/unset source, left
//	                                     honest-empty
func resolveTokensOutUnavailable(o TickOutcome) string {
	if raw := strings.TrimSpace(o.TokensOutUnavailable); raw != "" {
		return raw
	}
	switch o.CostSource {
	case CostSourceSimulated:
		return database.TokensOutUnavailableSimulated
	case CostSourceEstimated, CostSourceEstimatedSubincluded:
		return database.TokensOutUnavailableEstimatedNoTelemetry
	}
	return ""
}

// usageUnavailableMark renders the explicit tokens-out-unavailable mark for a
// terminal gateway envelope (SCHED-GAP-16947 AC3): "" when the envelope carried
// a `usage` object (a real observation — a genuine 0 included), the named mark
// when it carried none. The distinction only exists because Response records
// usage-key PRESENCE (Response.UsagePresent); the plain Usage struct decodes an
// absent key and a measured zero to the same 0.
func usageUnavailableMark(resp *Response) string {
	if resp == nil || resp.UsagePresent {
		return ""
	}
	return database.TokensOutUnavailableGatewayNoUsage
}

// firstErrorLine renders the first non-empty line of an error text (the
// raw-signal feed for the unknown: escape hatch — one line, no stack noise).
func firstErrorLine(errText string) string {
	for _, line := range strings.Split(errText, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// tickNoopFlag reads the tick row's no-op verdict flag best-effort (0 on any
// failure — a read error can only degrade the reason code toward the
// artifact-shape buckets, never fail the completion). The gateway spawn path's
// closeNoopVerdict stamps the flag BEFORE the slot pool finalizes the row, so
// Complete's read observes the verdict; exec/sim/dispatch rows read the column
// default 0.
func (lt *LifecycleTracker) tickNoopFlag(tickID string) int {
	if lt.db == nil || tickID == "" {
		return 0
	}
	var flag int
	_ = lt.db.QueryRow(`SELECT COALESCE(noop_flag, 0) FROM ticks WHERE id = ?`, tickID).Scan(&flag)
	return flag
}
