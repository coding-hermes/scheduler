package scheduler

// SCHED-GAP-1707 — partial telemetry on timeout + silence-watchdog visibility.
//
// A wave tick killed by its deadline (or by the session-silence watchdog)
// never reached lifecycle.Complete's telemetry write: the row landed
// status=timeout with tokens_in/tokens_out/cost_usd all 0 and the fleet could
// not tell a productive-but-too-big wave from a dead session. The fix has
// three parts, all riding ONE capture site per kill path:
//
//  1. TELEMETRY PARTIAL MARK — every kill path calls timeoutTelemetry
//     (below) before finalizing, so the row carries whatever telemetry the
//     path had already measured PLUS telemetry_partial=1 and a named reason
//     (migration v64). A timeout row that reads 0/0/0 with
//     telemetry_partial=0 is now provably an idle session, never a dead one.
//  2. SILENCE WATCHDOG (session_silence.go) — a gateway tick whose
//     Hermes-state telemetry shows no token delta and no tool activity for
//     sessionSilenceGrace is cancelled with failure_reason=session_silent
//     and the quiet duration on ticks.session_silence_s. OFF by default —
//     the daemon arms it, embedding tests stay byte-identical.
//  3. WAVE CONVERGENCE (v64 workers_terminal) — wave manifest ingest
//     records how many workers were already 'done' when the tick ended, so
//     "too much work for the window" is countable in SQL.
//
// NO BACKOFF CHANGE: the "No timeout backoff" decision (docs/design-decisions.md)
// is untouched — the timeout/failed cooldown chain, consecutive_failures
// semantics and the GAP-133 failure-backoff gate all behave exactly as
// before. This row only adds visibility.

// Telemetry partial reason vocabulary (ticks.telemetry_partial_reason,
// migration v64). A closed set: every producer below is a code site in this
// package, and a row carrying any other value was written by something that
// ignored the vocabulary. ” = not partial (the v64 column default).
const (
	// TelemetryPartialTickDeadline: the tick's OWN session deadline tore
	// the gateway POST (or the exec process) down (SCHED-GAP-1684 path and
	// the exec kill timer). The most common partial row.
	TelemetryPartialTickDeadline = "tick_deadline"
	// TelemetryPartialSessionSilent: the session-silence watchdog cancelled
	// a session that showed no token delta and no tool activity for the
	// configured grace. The row also carries session_silence_s.
	TelemetryPartialSessionSilent = "session_silent"
	// TelemetryPartialStaleReap: CleanupStaleProjects flipped a running row
	// past the stale window — no Wait() path ever ran for it, so the
	// telemetry columns are whatever the reap could measure (nothing).
	TelemetryPartialStaleReap = "stale_reap"
	// TelemetryPartialDispatchDeadline: a remote-dispatched tick (SCHED-GAP-1710)
	// whose reply never arrived before the session deadline.
	TelemetryPartialDispatchDeadline = "dispatch_deadline"
)

// telemetryPartialReasonIsValid reports whether reason belongs to the
// vocabulary above. The empty string is deliberately NOT valid — a partial
// row without a reason is a bug this check makes loud at the write site.
func telemetryPartialReasonIsValid(reason string) bool {
	switch reason {
	case TelemetryPartialTickDeadline, TelemetryPartialSessionSilent,
		TelemetryPartialStaleReap, TelemetryPartialDispatchDeadline:
		return true
	}
	return false
}

// timeoutTelemetry is the ONE telemetry capture for tick timeout paths
// (SCHED-GAP-1707 deliverable 1). It returns the partial-telemetry tuple to
// set on the TickOutcome: whatever usage the kill site already measured (the
// trace's SSE-folded token probe for gateway kills; zero for paths that
// never observed the session), PLUS the partial mark and its reason from the
// closed vocabulary.
//
// A gateway kill has no terminal envelope — that is WHY the row is partial —
// so the honest figure is the partial probe, never a fabricated estimate.
// 0/0/$0 with telemetry_partial=1 means "no observable usage before the
// wall", which the mark now says out loud instead of leaving the row
// indistinguishable from an idle session.
//
// Pure function over inputs the CALLER already holds: no I/O, so the hot
// timeout route stays allocation-free.
func timeoutTelemetry(reason string, tokensIn, tokensOut int, cost float64) (tin, tout int, usd float64, partial bool, namedReason string) {
	if !telemetryPartialReasonIsValid(reason) {
		// A caller bug (empty or unknown reason) must never write a partial
		// row with an unnamed cause: fall back to the most common class.
		// The vocabulary test pins every call site's reason, so this branch
		// is unreachable in a green tree.
		reason = TelemetryPartialTickDeadline
	}
	return tokensIn, tokensOut, cost, true, reason
}

// partialTelemetry is the kill-site capture: the measured-or-probed usage
// plus the partial mark, ready for Wait() to spread onto the TickOutcome.
type partialTelemetry struct {
	tokensIn  int
	tokensOut int
	costUSD   float64
	// silenceS carries the watchdog's measured quiet duration (whole
	// seconds) for the session_silent reason; 0 on every other path.
	silenceS int64
	partial  bool
	reason   string
}

// markPartialTelemetry wraps a measured (tokens, cost) triple into the
// partial tuple named reason. The one constructor every kill path calls.
func markPartialTelemetry(reason string, tokensIn, tokensOut int, cost float64) partialTelemetry {
	tin, tout, usd, partial, named := timeoutTelemetry(reason, tokensIn, tokensOut, cost)
	return partialTelemetry{tokensIn: tin, tokensOut: tout, costUSD: usd, partial: partial, reason: named}
}

// markSilentTelemetry is markPartialTelemetry for the watchdog: the same
// tuple plus the measured quiet duration.
func markSilentTelemetry(tokensIn, tokensOut int, cost float64, silenceS int64) partialTelemetry {
	pt := markPartialTelemetry(TelemetryPartialSessionSilent, tokensIn, tokensOut, cost)
	pt.silenceS = silenceS
	return pt
}

// sessionSilentMarker is the watchdog kill's error-text marker. HarnessFailure
// (failureclass.go) carries it, so a watchdog-killed tick stamps
// failure_reason=session_silent through the SAME classifier every other
// harness-side verdict uses, and never feeds per-project health accounting
// (SCHED-GAP-134) as the lane's own fault.
const sessionSilentMarker = "session silent"

// sessionSilentError renders the watchdog's ticks.error text. quiet names
// the measured silence (already formatted, e.g. "90s"). The text is stable:
// the classifier (HarnessFailure → failureReasonClass) and operator greps
// depend on the leading "session silent" marker.
func sessionSilentError(quiet string) string {
	return sessionSilentMarker + " — no token delta and no tool activity for " + quiet
}
