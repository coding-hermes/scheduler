package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1693 acceptance: in the no-exec-fallback drop path the
// ErrTickDeadlineExceeded class alone is authoritative — the session ctx's
// DeadlineExceeded FLAG is not, because its value at classification time is
// an ordering accident.
//
// The defect (measured 2026-10-01 on the live daemon): 34 of 50
// tick-deadline kills booked status=failed + failure_reason=gateway_transport
// with the drop wrapper text ("gateway unreachable and exec fallback
// disabled: tick deadline exceeded") instead of status=timeout. Session
// readbacks showed active work (110-416 messages, activity within 0-90s of
// the kill) — the gateway was fine; OUR wall tore the POST down, but the
// SCHED-GAP-1684 guard required ctx.Err()==DeadlineExceeded at the
// classification site and lost the race: the POST closure's deferred
// cancel() runs BEFORE classification (so ctx reads context.Canceled, not
// DeadlineExceeded), and the read-side deadline that minted the sentinel
// can fire before the session ctx's own flag lands. A sentinel-carrying
// gwErr then fell through to the generic no-exec-fallback drop.
//
// Arm 1 (TestSchedGap1693TickDeadlineSentinelAloneBooksTimeout): an SSE
// stream that stays demonstrably ACTIVE until an INNER deadline (the
// gateway client's http.Client timeout — net/http implements it as a
// context deadline child of the request, the same teardown class as the
// session wall) ends the read while the tick wall stays alive. The read
// must come back ErrTickDeadlineExceeded, and the tick must land TickTimeout
// at BOTH halves of the spawn path even though ctx.Err() is context.Canceled
// — not DeadlineExceeded — when the guard runs.
//
// Arm 2 (TestSchedGap1693TickDeadlineSentinelIsSelfWallOnly): pins the
// sentinel's exhaustiveness at the source — it is produced ONLY by our own
// deadline teardown. (a) inner-deadline mid-SSE → sentinel while the session
// ctx is alive; (b) mid-run server EOF with every deadline alive →
// transient, NOT the sentinel; (c) a GENUINE transport failure under an
// already-fired session ctx (the exact "shares a deadline" shape the old
// ctx.Err() AND was guarding against) never carries the sentinel —
// criterion 3's do-not-widen contract, pinned where the errors are minted.
//
// RED proof: against the pre-fix guard (errors.Is(...) && ctx.Err() ==
// context.DeadlineExceeded) arm 1 fails — the tick drops with the wrapper
// text instead of booking timeout. Arm 2 passes both before and after (it
// pins source behavior, not the classification); the fix must not make it
// load-bearing.

// schedGap1693ActiveStreamHandler serves a well-formed SSE stream that
// never EOFs on its own: one output_text.delta every 50ms (well under the
// 400ms per-turn watch, so only a deadline can end the stream).
func schedGap1693ActiveStreamHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Hermes-Session-Id", "sess-1693-inner")
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

// TestSchedGap1693TickDeadlineSentinelAloneBooksTimeout: gwErr carries
// ErrTickDeadlineExceeded while ctx.Err() is NOT DeadlineExceeded at the
// classification site — the tick must book TickTimeout anyway
// (SCHED-GAP-1693), never the failed/gateway_transport drop row.
func TestSchedGap1693TickDeadlineSentinelAloneBooksTimeout(t *testing.T) {
	gap170BootState(t) // fresh, unarmed gate state (client nil; restores on cleanup)
	gap170ResetGate(t) // and no client left installed for the next test

	db := newTestDB(t)
	const projectName = "sgap1693-order"
	const tickID = "sgap1693-order-2026-10-01-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	srv := httptest.NewServer(schedGap1693ActiveStreamHandler())
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 4)
	// The INNER client deadline (2s) fires long before the tick wall (30s):
	// the SSE read dies with a DeadlineExceeded-Is error — the sentinel —
	// while the session ctx has NOT fired. 2s > 400ms keeps the idle watch
	// reset (50ms events), so the supervised POST is armed and only the
	// inner deadline can end it.
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 2*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second // the TICK wall stays ALIVE
	spawner.SetGatewayResponseTimeout(400 * time.Millisecond)

	beforeDeferrals := gap170Deferrals()
	started := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a gwErr carrying ErrTickDeadlineExceeded must book TickTimeout regardless of the ctx.Err() flag order (SCHED-GAP-1693); the drop wrapper is the measured 34-row misclassification", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on a deadline-torn-down POST")
	}
	// Premise check: the 30s session wall cannot have fired inside this
	// window — if it had, ctx.Err()==DeadlineExceeded would hold and this
	// test would be vacuous (the old AND satisfied for the wrong reason).
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("test premise broken: spawn took %v — the 30s session wall may have fired, which would make the ctx-flag race unreal", elapsed)
	}
	outcome := tick.Wait()
	if outcome.Status != TickTimeout {
		t.Errorf("Wait() status = %s, want %s — the sentinel alone is authoritative (SCHED-GAP-1693)", outcome.Status, TickTimeout)
	}
	if got := outcome.Status.Outcome(); got != "timeout" {
		t.Errorf("status.Outcome() = %q, want \"timeout\" (the outcome column must carry the timeout)", got)
	}
	if !strings.Contains(outcome.Error, "tick-timeout wall 30s") {
		t.Errorf("outcome.Error = %q, want the deadline-named reason (\"tick-timeout wall 30s: …\")", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "tick deadline exceeded") {
		t.Errorf("outcome.Error = %q, want the underlying sentinel text recorded for audit", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 0 {
		t.Errorf("deferrals_total delta = %d, want 0 — a sentinel-carrying error is our wall, never a transient blip", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — the timeout row stays transport-class (SCHED-GAP-143/1705 carve-out), the lane counter must not move", got)
	}

	// Full stack: the same shape through the slot pool, asserting the ROW.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "***", 2*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(400 * time.Millisecond)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state — a booked timeout must still complete the tick row", fsTickID)
	}
	if status != "timeout" {
		t.Errorf("ticks.status = %q, want \"timeout\" — booking failed/gateway_transport is the SCHED-GAP-1693 defect", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "timeout" {
		t.Errorf("ticks.outcome = %q, want \"timeout\"", got)
	}
	errText := tickErrorOf(t, db, fsTickID)
	if !strings.Contains(errText, "tick-timeout wall") {
		t.Errorf("ticks.error = %q, want the deadline-named reason in the row", errText)
	}
	if !strings.Contains(errText, "tick deadline exceeded") {
		t.Errorf("ticks.error = %q, want the sentinel text in the row (NOT the \"gateway unreachable and exec fallback disabled\" wrapper)", errText)
	}
	if strings.Contains(errText, "gateway unreachable and exec fallback disabled") {
		t.Errorf("ticks.error = %q — the drop wrapper text on a deadline kill is the misclassification under repair", errText)
	}
	// The wall fired inside a gateway POST, so the shared classifier still
	// stamps the existing transport marker on the TickTimeout row
	// (convention pinned by SCHED-GAP-1684 — unchanged here).
	if got := schedGap203BFailureReason(t, db, fsTickID); got != FailureReasonGatewayTransport {
		t.Errorf("ticks.failure_reason = %q, want %q", got, FailureReasonGatewayTransport)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 0 {
		t.Errorf("deferrals_total delta = %d after the slot-pool spawn, want 0", got)
	}
}

// TestSchedGap1693TickDeadlineSentinelIsSelfWallOnly pins the sentinel's
// production contract at the source: ErrTickDeadlineExceeded is minted ONLY
// when a deadline of OUR OWN tore the gateway call down — never for a
// genuine transport failure, no matter what the surrounding context flags
// say. This is what makes the sentinel-alone classification safe.
func TestSchedGap1693TickDeadlineSentinelIsSelfWallOnly(t *testing.T) {
	// (a) Inner client deadline mid-SSE while the session ctx is ALIVE →
	// the sentinel (the production twin of the 34 misbooked rows).
	srv := httptest.NewServer(schedGap1693ActiveStreamHandler())
	t.Cleanup(srv.Close)
	aliveCtx := context.Background() // the session ctx never fires
	_, _, err := NewGatewayClient(srv.URL, "***", 300*time.Millisecond).
		SendResponseStream(aliveCtx, "prompt", "m", "p", "***", "sess-1693-a", 0)
	if err == nil {
		t.Fatal("expected the inner client deadline to end the SSE read")
	}
	if !errors.Is(err, ErrTickDeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap ErrTickDeadlineExceeded — the client-side deadline is OUR teardown, the same class as the session wall", err)
	}

	// (b) Mid-run server EOF with every deadline alive → transient, never
	// the sentinel (the SCHED-GAP-203 blip shape must stay reachable).
	eofSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: output_text.delta\ndata: {\"type\":\"output_text.delta\",\"seq\":0}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Handler returns → the stream EOFs mid-run.
	}))
	t.Cleanup(eofSrv.Close)
	_, _, err = NewGatewayClient(eofSrv.URL, "***", 5*time.Second).
		SendResponseStream(context.Background(), "prompt", "m", "p", "***", "sess-1693-b", 0)
	if err == nil {
		t.Fatal("expected the mid-run EOF to end the SSE read")
	}
	if errors.Is(err, ErrTickDeadlineExceeded) {
		t.Fatalf("err = %v — a mid-run EOF with every deadline alive must NOT carry ErrTickDeadlineExceeded", err)
	}
	if !errors.Is(err, ErrGatewayTransient) || !strings.Contains(err.Error(), "sse stream ended without a terminal event") {
		t.Fatalf("err = %v, want the SCHED-GAP-203 transient shape unchanged", err)
	}

	// (c) A GENUINE transport failure under an already-FIRED session ctx —
	// the exact "shares a deadline" shape the old ctx.Err() AND was meant
	// to guard: the error carries context.DeadlineExceeded but NOT the
	// sentinel, so it keeps its non-timeout classification and must never
	// be reclassified as our wall by the sentinel-alone check.
	//
	// The handler must keep its connection buffers DRAINED: consuming the
	// request body (and bounding the park) arms net/http's connection
	// teardown, so httptest.Server.Close in the test cleanup cannot wait
	// forever on a connection whose client side vanished under an
	// already-canceled context.
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(hangSrv.Close)
	firedCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = NewGatewayClient(hangSrv.URL, "***", 0).
		SendResponseStream(firedCtx, "prompt", "m", "p", "***", "sess-1693-c", 0)
	if err == nil {
		t.Fatal("expected the fired session ctx to abort the hung POST")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded (the session wall fired mid-POST)", err)
	}
	if errors.Is(err, ErrTickDeadlineExceeded) {
		t.Fatalf("err = %v — a transport failure that merely shares a deadline with the ctx must NOT carry ErrTickDeadlineExceeded (the sentinel stays exclusive to our read-side teardown)", err)
	}
}
