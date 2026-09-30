package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1684 acceptance: classify a tick's terminal status by CAUSE when
// the gateway POST dies under a transient-classed error.
//
// The defect (measured 2026-09-30 on the live daemon): the SSE reader folds
// EVERY stream end without a terminal event into ErrGatewayTransient —
// including the read error the TICK's own session deadline produces when its
// cancel tears the stream down. gatewayTransientBlip then booked our own wall
// as a gateway blip: 30 of 41 deferred ticks in 3 days died at EXACTLY
// 120.0/180.0 min while outcome `timeout` was used 9x in 7 days. A runaway
// foreman was invisible, and the ledger blamed the gateway for our wall.
//
// Arm 1 (TestSchedGap1684TickDeadlineBooksTimeoutNotDeferred): a stream that
// stays demonstrably ACTIVE (an event every 50ms, so the SCHED-GAP-119 idle
// watch keeps resetting) until the 1s session wall fires. The read must come
// back as ErrTickDeadlineExceeded, the ctx re-check at the deferral site must
// hold, and the tick must land status=timeout / outcome=timeout with the wall
// in the reason — at both halves of the spawn path (Spawner.Spawn/Wait and
// the full slot-pool → lifecycle.Complete row).
//
// Arm 2 (TestSchedGap1684GenuineDropStillDeferred): the exact SCHED-GAP-203
// shape — the stream EOFs mid-run while the tick deadline is still alive.
// That deferral must be byte-for-byte unchanged.
//
// RED proof: with the spawn-side hook neutralized (mutation), arm 1 fails —
// the tick is dropped/deferred instead of timed out; arm 2 keeps passing
// (the fix must not be load-bearing for the genuine shape).

// schedGap1684ActiveStreamHandler serves a well-formed SSE stream that stays
// active until the request context is torn down: one output_text.delta every
// 50ms, each a real event that resets the turn watch (well under the 400ms
// per-turn deadline the tests arm). The stream never EOFs on its own — only
// the session deadline ends it, which is exactly the arm-1 shape.
func schedGap1684ActiveStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Hermes-Session-Id", "sess-1684-wall")
		w.WriteHeader(http.StatusOK)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, "event: output_text.delta\ndata: {\"type\":\"output_text.delta\",\"seq\":%d}\n\n", i); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}
}

// TestSchedGap1684TickDeadlineBooksTimeoutNotDeferred: the tick's own wall
// tore down an active stream — book timeout, not deferred.
func TestSchedGap1684TickDeadlineBooksTimeoutNotDeferred(t *testing.T) {
	gap170BootState(t) // fresh, unarmed gate state (client nil; restores on cleanup)
	gap170ResetGate(t) // and no client left installed for the next test

	db := newTestDB(t)
	const projectName = "sgap1684-wall"
	const tickID = "sgap1684-wall-2026-09-30-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	srv := httptest.NewServer(schedGap1684ActiveStreamHandler())
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 1 * time.Second                         // the TICK wall under test
	spawner.SetGatewayResponseTimeout(400 * time.Millisecond) // arms the supervised (streaming) POST; idle watch resets on every 50ms event, so only the wall can end it

	beforeDeferrals := gap170Deferrals()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a tick whose OWN wall expired must be booked timeout, not dropped/deferred (SCHED-GAP-1684)", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on a session-deadline abort")
	}
	outcome := tick.Wait()
	if outcome.Status != TickTimeout {
		t.Errorf("Wait() status = %s, want %s — our wall is TickTimeout semantics, never a gateway blip", outcome.Status, TickTimeout)
	}
	if got := outcome.Status.Outcome(); got != "timeout" {
		t.Errorf("status.Outcome() = %q, want \"timeout\" (the outcome column must carry the timeout too)", got)
	}
	if !strings.Contains(outcome.Error, "tick-timeout wall 1s") {
		t.Errorf("outcome.Error = %q, want the wall named in the reason (\"tick-timeout wall 1s: …\")", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "tick deadline exceeded") {
		t.Errorf("outcome.Error = %q, want the underlying gateway error recorded for audit", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 0 {
		t.Errorf("deferrals_total delta = %d, want 0 — a self-timeout is NOT a transient blip", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — a timeout must not charge the lane's failure counter", got)
	}

	// Full stack: the same shape through the slot pool, asserting the ROW.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "***", 5*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(1 * time.Second)
	loop.SetGatewayResponseTimeout(400 * time.Millisecond)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state — a booked timeout must still complete the tick row", fsTickID)
	}
	if status != "timeout" {
		t.Errorf("ticks.status = %q, want \"timeout\" — booking this as \"deferred\" is the SCHED-GAP-1684 defect", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "timeout" {
		t.Errorf("ticks.outcome = %q, want \"timeout\"", got)
	}
	if errText := tickErrorOf(t, db, fsTickID); !strings.Contains(errText, "tick-timeout wall") {
		t.Errorf("ticks.error = %q, want the wall duration in the reason", errText)
	}
	// The wall fired inside a gateway POST, so the shared classifier stamps
	// the existing transport marker on the TickTimeout row.
	if got := schedGap203BFailureReason(t, db, fsTickID); got != FailureReasonGatewayTransport {
		t.Errorf("ticks.failure_reason = %q, want %q", got, FailureReasonGatewayTransport)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 0 {
		t.Errorf("deferrals_total delta = %d after the slot-pool spawn, want 0", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d after the slot-pool spawn, want 0", got)
	}
}

// TestSchedGap1684GenuineDropStillDeferred: the SCHED-GAP-203 mid-run SSE
// drop (stream EOFs while the tick deadline is still alive) must stay a
// deferral — the reclassification is for OUR wall only.
func TestSchedGap1684GenuineDropStillDeferred(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const projectName = "sgap1684-sse"
	const tickID = "sgap1684-sse-2026-09-30-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	srv := httptest.NewServer(schedGap203BSSEHandler())
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second                 // tick deadline stays ALIVE
	spawner.SetGatewayResponseTimeout(2 * time.Second) // arms the supervised POST; the stream EOFs long before any deadline

	beforeDeferrals := gap170Deferrals()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a mid-stream SSE drop with the tick deadline alive is a blip and must be DEFERRED", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on a dropped SSE stream")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s — a genuine drop must keep the SCHED-GAP-203 deferral unchanged", outcome.Status, TickDeferred)
	}
	if !strings.Contains(outcome.Error, "sse stream ended without a terminal event") {
		t.Errorf("outcome.Error = %q, want the reader's own transient text", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 1 {
		t.Errorf("deferrals_total delta = %d, want 1 — the genuine blip still lands in the deferral counter", got)
	}

	// Full stack: the row must still land deferred/deferred.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "***", 5*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(2 * time.Second)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state", fsTickID)
	}
	if status != "deferred" {
		t.Errorf("ticks.status = %q, want \"deferred\" — the genuine-drop classification must be unchanged", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "deferred" {
		t.Errorf("ticks.outcome = %q, want \"deferred\"", got)
	}
	if got := schedGap203BFailureReason(t, db, fsTickID); got != "" {
		t.Errorf("ticks.failure_reason = %q, want \"\" — the status column already names the class", got)
	}
}
