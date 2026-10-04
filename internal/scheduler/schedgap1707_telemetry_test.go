package scheduler_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// stringsContains is strings.Contains under a test-local name (kept next to
// its only caller).
func stringsContains(s, sub string) bool { return strings.Contains(s, sub) }

// SCHED-GAP-1707 acceptance, partial-telemetry arm: a tick that ends in
// status=timeout/outcome=timeout WITH a partial mark persists the telemetry
// consumed so far (tokens_in/tokens_out/cost_usd) plus
// telemetry_partial=1/reason/silence — and the row is therefore
// distinguishable from an idle 0/0/0 timeout. The test fails if the
// lifecycle write drops any leg.
func TestSCHEDGAP1707_TimeoutPersistsPartialTelemetry(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)

	tickID := "alpha-1707-timeout"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-2 * time.Hour),
		Finished: now,
		Status:   scheduler.TickTimeout,
		ExitCode: -1,
		Error:    "tick-timeout wall 2h0m0s: tick deadline exceeded: session context deadline expired during gateway POST",
		// The partial telemetry a deadline kill measured: the trace's
		// SSE-folded probe + its derived cost (a productive-but-too-big
		// wave had already streamed these before the wall).
		TokensIn:               42400,
		TokensOut:              7000,
		CostUSD:                0.311,
		CostSource:             scheduler.CostSourceGateway,
		TelemetryPartial:       true,
		TelemetryPartialReason: scheduler.TelemetryPartialTickDeadline,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var status, outcome, reason string
	var tin, tout int64
	var cost float64
	var partial int
	var partialReason string
	var silence int64
	var workers int
	if err := db.QueryRow(`SELECT status, COALESCE(outcome,''), failure_reason,
	      tokens_in, tokens_out, cost_usd,
	      telemetry_partial, telemetry_partial_reason, session_silence_s, workers_terminal
	   FROM ticks WHERE id = ?`, tickID).
		Scan(&status, &outcome, &reason, &tin, &tout, &cost, &partial, &partialReason, &silence, &workers); err != nil {
		t.Fatalf("read timeout row: %v", err)
	}
	if status != "timeout" || outcome != "timeout" {
		t.Fatalf("status/outcome = %s/%s, want timeout/timeout", status, outcome)
	}
	if tin != 42400 || tout != 7000 {
		t.Errorf("partial tokens = %d/%d, want 42400/7000 — the timeout path dropped the consumed telemetry", tin, tout)
	}
	if cost != 0.311 {
		t.Errorf("partial cost = %v, want 0.311", cost)
	}
	if partial != 1 {
		t.Errorf("telemetry_partial = %d, want 1 — the timeout row is indistinguishable from an idle 0/0/0 row", partial)
	}
	if partialReason != scheduler.TelemetryPartialTickDeadline {
		t.Errorf("telemetry_partial_reason = %q, want %q", partialReason, scheduler.TelemetryPartialTickDeadline)
	}
	if reason != "gateway_transport" {
		t.Errorf("failure_reason = %q, want gateway_transport (classifier unchanged by 1707)", reason)
	}
	if workers != -1 {
		t.Errorf("workers_terminal = %d, want -1 (no manifest for this tick)", workers)
	}
}

// The control arm: a timeout WITHOUT the partial mark still reads
// telemetry_partial=0 — the mark is the producer's statement, never
// fabricated by the write site.
func TestSCHEDGAP1707_TimeoutWithoutMarkStaysHonest(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)

	tickID := "alpha-1707-idle"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  "alpha",
		Started:  now.Add(-2 * time.Hour),
		Finished: now,
		Status:   scheduler.TickTimeout,
		ExitCode: -1,
		Error:    "stale — timeout at 2h0m0s",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var partial int
	var tin int64
	if err := db.QueryRow(`SELECT telemetry_partial, tokens_in FROM ticks WHERE id = ?`, tickID).
		Scan(&partial, &tin); err != nil {
		t.Fatalf("read idle row: %v", err)
	}
	if partial != 0 {
		t.Errorf("telemetry_partial = %d, want 0 (the write site must not fabricate the mark)", partial)
	}
	if tin != 0 {
		t.Errorf("tokens_in = %d, want 0 (idle row stays zeros)", tin)
	}
}

// The watchdog arm's persistence half: a session_silent timeout row carries
// the named reason AND the quiet duration.
func TestSCHEDGAP1707_SessionSilentRowCarriesReasonAndSilence(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)

	tickID := "alpha-1707-silent"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	// The watchdog's own error text — the classifier must read it as
	// harness-side and stamp failure_reason=session_silent. Rendered with
	// the same shape sessionSilentError produces (marker + measured
	// silence); the marker is the classifier's contract.
	err := lt.Complete(scheduler.TickOutcome{
		TickID:                 tickID,
		Project:                "alpha",
		Started:                now.Add(-90 * time.Minute),
		Finished:               now,
		Status:                 scheduler.TickTimeout,
		ExitCode:               -1,
		Error:                  "session silent — no token delta and no tool activity for 1h30m0s",
		CostSource:             scheduler.CostSourceGateway,
		TelemetryPartial:       true,
		TelemetryPartialReason: scheduler.TelemetryPartialSessionSilent,
		TelemetrySilenceS:      5400,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var reason, cls string
	var silence int64
	var partial int
	if err := db.QueryRow(`SELECT COALESCE(failure_reason,''), error, session_silence_s, telemetry_partial
	   FROM ticks WHERE id = ?`, tickID).
		Scan(&cls, &reason, &silence, &partial); err != nil {
		t.Fatalf("read silent row: %v", err)
	}
	if cls != "session_silent" {
		t.Errorf("failure_reason = %q, want session_silent — the classifier must own the watchdog verdict", cls)
	}
	if silence != 5400 {
		t.Errorf("session_silence_s = %d, want 5400", silence)
	}
	if partial != 1 {
		t.Errorf("telemetry_partial = %d, want 1", partial)
	}
	if want := "session silent"; !stringsContains(reason, want) {
		t.Errorf("error text %q missing marker %q", reason, want)
	}
}

// completed ticks never carry the mark (the struct field is timeout-only).
func TestSCHEDGAP1707_CompletedRowNeverPartial(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha")
	lt := scheduler.NewLifecycleTracker(db)
	tickID := "alpha-1707-done"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID: tickID, Project: "alpha",
		Started: now.Add(-time.Minute), Finished: now,
		Status: scheduler.TickCompleted, ExitCode: 0,
		TelemetryPartial: true, // must be IGNORED on the completed path
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var partial int
	if err := db.QueryRow(`SELECT telemetry_partial FROM ticks WHERE id = ?`, tickID).Scan(&partial); err != nil {
		t.Fatalf("read completed row: %v", err)
	}
	if partial != 0 {
		t.Errorf("telemetry_partial = %d on a COMPLETED row, want 0", partial)
	}
}

// The completed path never carries the mark (asserted in
// schedgap1707_silence_test.go's capture test); the external-package half
// of this file pins the persisted rows through the public API only.
