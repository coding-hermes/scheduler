package scheduler

import "strings"

// OrphanAbortMarker is the drain-abort error wording stampAbort uses when it
// reaps in-flight ticks (loop.go). It is ONE constant for the whole package:
// the harness marker list above matches the text for pre-1608 rows whose only
// persisted signature is the abort wording, and the SCHED-GAP-1608
// orphan-stamp exclusion (orphan_exclusion.go) matches the same wording for
// its legacy text-only probe — never restate the abort text anywhere else.
const OrphanAbortMarker = "aborted by graceful shutdown"

// HarnessFailure reports whether a failed tick's error came from the harness
// (gateway / scheduler infrastructure) rather than from the project itself.
// These ticks never reached the project, so they must not feed per-project
// health accounting (SCHED-GAP-134). Keep this list tight: only outage classes
// where every project on the box fails identically.
//
// It is the SINGLE authority for that classification (SCHED-GAP-173). Two
// surfaces classify the same ticks with it and must never disagree:
//
//   - the enforcer — CheckFailureRateAutoDisable (alert_escalation.go), which
//     decides whether a lane actually gets parked, and
//   - the read-only status surface — computeProjectFailureRates
//     (internal/api/server_helpers.go), which reports auto_disable_armed.
//
// Before the list was shared, the status surface counted every failed/timeout
// row, so a lane whose failures were all gateway noise read
// auto_disable_armed=true with failure_rate=0.91 while the enforcer's verdict
// on the same window was ~0.02: a healthy lane permanently advertised as
// "about to be parked". Add new markers HERE only — a second marker list in
// any other surface is a parity regression, pinned by
// TestSCHEDGAP173_ClassifierIsSingleAuthority (internal/api).
func HarnessFailure(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	for _, marker := range []string{
		"gateway unreachable",
		"gateway is draining",
		"gateway_auth_error",
		"invalid gateway api key",
		"connection refused",
		"exec fallback disabled",
		OrphanAbortMarker,
		// SCHED-GAP-203: the transport-class transient errors. The spawn path
		// classifies these as ErrGatewayTransient (gateway_client.go) and the
		// deferral path (Spawner.transientGatewayDeferral) keeps them OUT of
		// consecutive_failures — but the classifier must agree on the TEXT,
		// because it owns the decision for every surface that only sees a
		// stored error string: the auto-disable enforcer, the read-only
		// status surface, and (pre-203) the unwrapped gwErr that
		// noteSpawnFailureClassed receives. Measured consequence of the gap:
		// the mid-stream shape "gateway transient error: sse stream ended
		// without a terminal event" matched NO marker, so a blip the harness
		// absorbed still counted as the lane's failure — the exact pollution
		// SCHED-GAP-203 measures. Both spellings are listed: the %w-wrapped
		// ErrGatewayTransient text and the SSE reader's own terminal phrase.
		"gateway transient error",
		"sse stream ended without a terminal event",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// harnessFailure is the package-private spelling of HarnessFailure kept for
// the existing in-package callers and tests (failureReasonClass, the
// SCHED-GAP-134 classifier test, schedgap143_drain_class_test.go). It is a
// forwarder, not a second classifier: HarnessFailure owns the marker list
// (SCHED-GAP-173).
func harnessFailure(errText string) bool { return HarnessFailure(errText) }
