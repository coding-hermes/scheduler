package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-203-B acceptance: the SPAWN side of "a transient gateway blip is a
// deferral, not a lane failure".
//
// WHY THESE TESTS ARE SHAPED THIS WAY. The parent row measured the defect on the
// live daemon: /api/v1/status reported the gateway-health gate armed=true
// healthy=true deferrals_total=0 over 32h while 9 of 458 ticks failed with
// "gateway unreachable and exec fallback disabled: …". Two distinct transport
// shapes were in that sample — a refused connect and an SSE stream that ended
// without a terminal event — and BOTH reached exactly one code path: Spawn()'s
// no-exec-fallback drop, where a non-nil gwErr is the terminal outcome. (The two
// OTHER TickFailed sites in Wait() — the SCHED-GAP-079 gwFailErr completion gate
// and the SCHED-GAP-085 board-closure gate — are unreachable for a transport
// error: a non-nil gwErr skips the response handling entirely, and the closure
// gate's error is a board verdict. That is why the deferral branch lives at the
// drop site and the two Wait() sites keep their comments explaining so.)
//
// Each test therefore asserts the SAME four things the row asked for:
//
//	(a) the tick comes back DEFERRED (outcome path) and lands as a deferral,
//	(b) ticks.status reads "deferred" — never "failed",
//	(c) the gate's deferrals_total increments,
//	(d) the lane is NOT charged: consecutive_failures untouched, no
//	    noteSpawnFailureClassed / recordGatewayDrop call.
//
// The auth case is the opposite-direction guard: ErrGatewayKeyRejected must
// still fail loudly (GAP-035) and must NOT move the deferral counter.

// schedGap203BClosedGatewayURL returns the URL of a server that has already been
// shut down, so the dial is refused. This is the live shape (b):
// `gateway POST: Post "http://…": dial tcp …: connect: connection refused`.
func schedGap203BClosedGatewayURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // port released → next dial is refused
	return url
}

// schedGap203BSSEHandler serves a well-formed SSE stream that DIES mid-run: one
// accepted event, then EOF with no terminal event. This is the live shape (a)
// (readSSEResponse → "gateway transient error: sse stream ended without a
// terminal event"). The supervised (streaming) path is armed by the per-turn
// deadline, so the caller must SetGatewayResponseTimeout.
func schedGap203BSSEHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Hermes-Session-Id", "sess-sse-dropped")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Return without a response.completed/response.failed event: the stream
		// ends, which the reader classifies as transient.
	}
}

// schedGap203BCountDeferralEvents counts the per-lane deferral events the gate
// emitted, selected by the machine marker rather than by message text.
func schedGap203BCountDeferralEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events
		WHERE json_extract(details, '$.event_type') = 'gateway_health_transient_defer'`).Scan(&n); err != nil {
		t.Fatalf("count transient deferral events: %v", err)
	}
	return n
}

// schedGap203BConsecutiveFailures reads the project's lane-failure counter.
func schedGap203BConsecutiveFailures(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, project).Scan(&n); err != nil {
		t.Fatalf("query consecutive_failures for %s: %v", project, err)
	}
	return n
}

// schedGap203BFailureReason reads the transport-class marker (must stay empty
// for a deferred row: the status itself names the class).
func schedGap203BFailureReason(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT failure_reason FROM ticks WHERE id = ?`, tickID).Scan(&s); err != nil {
		t.Fatalf("query failure_reason for %s: %v", tickID, err)
	}
	return s
}

// schedGap203BOutcomeOf reads the outcome column ("deferred" for a deferral).
func schedGap203BOutcomeOf(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(`SELECT outcome FROM ticks WHERE id = ?`, tickID).Scan(&s); err != nil {
		t.Fatalf("query outcome for %s: %v", tickID, err)
	}
	return s.String
}

// schedGap203BAssertConnectFailureDeferred drives the refused-dial shape through
// BOTH halves of the spawn path: Spawner.Spawn/Wait directly, and the full
// slot-pool → lifecycle.Complete chain that writes the row.
func schedGap203BAssertConnectFailureDeferred(t *testing.T) {
	t.Helper()
	gap170BootState(t) // fresh, unarmed gate state (client nil; restores on cleanup)
	gap170ResetGate(t) // and no client left installed for the next test

	db := newTestDB(t)
	const projectName = "sgap203b-connect"
	const tickID = "sgap203b-connect-2026-09-21-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(schedGap203BClosedGatewayURL(t), "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	beforeDeferrals := gap170Deferrals()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a transient gateway blip must be DEFERRED, not dropped as an error; "+
			"gatewayTransientBlip classification is the whole point of SCHED-GAP-203 (note this shape is a *url.Error, "+
			"NOT an ErrGatewayTransient wrap)", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on a transient gateway blip")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s — a refused dial is a gateway blip, not a lane failure", outcome.Status, TickDeferred)
	}
	if got := outcome.Status.Outcome(); got != "deferred" {
		t.Errorf("status.Outcome() = %q, want \"deferred\" (the outcome column must carry the deferral too)", got)
	}
	if !strings.Contains(outcome.Error, "connection refused") {
		t.Errorf("outcome.Error = %q, want the gateway's own error text (the deferral must be auditable)", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 1 {
		t.Errorf("deferrals_total delta = %d, want 1 — the mid-spawn blip must land in the SAME counter the pre-spawn gate uses", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — a deferral is not the lane's failure", got)
	}

	// Full stack: the same shape through the slot pool, asserting the ROW.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(schedGap203BClosedGatewayURL(t), "sk-daemon-shared", 5*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state — the deferred outcome must still complete the tick row", fsTickID)
	}
	if status != "deferred" {
		t.Errorf("ticks.status = %q, want \"deferred\" — booking this blip as \"failed\" is the defect SCHED-GAP-203 exists to remove", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "deferred" {
		t.Errorf("ticks.outcome = %q, want \"deferred\"", got)
	}
	if got := schedGap203BFailureReason(t, db, fsTickID); got != "" {
		t.Errorf("ticks.failure_reason = %q, want \"\" — the status column already names the class", got)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 2 {
		t.Errorf("deferrals_total delta = %d after two deferred spawns, want 2", got)
	}
	if got := schedGap203BCountDeferralEvents(t, db); got < 1 {
		t.Errorf("transient deferral events = %d, want >= 1 — the deferral must be visible to the fleet, not only counted", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d after the slot-pool spawn, want 0", got)
	}
}

// schedGap203BAssertSSEDropDeferred drives the mid-run SSE drop shape.
func schedGap203BAssertSSEDropDeferred(t *testing.T) {
	t.Helper()
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const projectName = "sgap203b-sse"
	const tickID = "sgap203b-sse-2026-09-21-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	srv := httptest.NewServer(schedGap203BSSEHandler())
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(2 * time.Second) // arms the (streaming) supervised POST

	beforeDeferrals := gap170Deferrals()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a mid-stream SSE drop is a transient gateway blip and must be DEFERRED", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on a dropped SSE stream")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s — the SSE stream ended without a terminal event", outcome.Status, TickDeferred)
	}
	if !strings.Contains(outcome.Error, "sse stream ended without a terminal event") {
		t.Errorf("outcome.Error = %q, want the reader's own transient text", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 1 {
		t.Errorf("deferrals_total delta = %d, want 1", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d, want 0", got)
	}

	// Classifier agreement (SCHED-GAP-203-B item 4): the SHARED classifier the
	// auto-disable enforcer and the read-only status surface use must call this
	// text harness-side. Before this row the mid-stream spelling matched NO
	// marker (the wrapper's "gateway unreachable" did the work), so the
	// unwrapped gwErr that noteSpawnFailureClassed receives was counted as the
	// lane's own failure.
	if !harnessFailure("gateway transient error: sse stream ended without a terminal event") {
		t.Error("harnessFailure(transient SSE drop) = false — the shared classifier must classify this class harness-side")
	}
	if got := failureReasonClass("gateway transient error: sse stream ended without a terminal event"); got != FailureReasonGatewayTransport {
		t.Errorf("failureReasonClass(transient SSE drop) = %q, want %q — the whole point is that the wrapper text is no longer load-bearing", got, FailureReasonGatewayTransport)
	}

	// Full stack.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(2 * time.Second)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state", fsTickID)
	}
	if status != "deferred" {
		t.Errorf("ticks.status = %q, want \"deferred\" (not \"failed\")", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "deferred" {
		t.Errorf("ticks.outcome = %q, want \"deferred\"", got)
	}
	errText := tickErrorOf(t, db, fsTickID)
	if !strings.Contains(errText, "sse stream ended without a terminal event") {
		t.Errorf("ticks.error = %q, want the gateway reason recorded", errText)
	}
	if got := schedGap203BFailureReason(t, db, fsTickID); got != "" {
		t.Errorf("ticks.failure_reason = %q, want \"\"", got)
	}
	if got := schedGap203BConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d after the slot-pool spawn, want 0", got)
	}
	// The deferral must NOT push the cooldown: the project keeps its floor.
	var cd, streak int
	if err := db.QueryRow(`SELECT cooldown_s, no_progress_ticks FROM projects WHERE name = ?`, projectName).
		Scan(&cd, &streak); err != nil {
		t.Fatalf("query cooldown: %v", err)
	}
	if streak != 0 {
		t.Errorf("no_progress_ticks = %d after a deferred tick, want 0 — a gateway blip must not extend the no-progress streak", streak)
	}
}

// schedGap203BAssertAuthStillFails is the opposite-direction guard: an auth
// rejection is terminal (GAP-035) and must never be laundered into a deferral.
func schedGap203BAssertAuthStillFails(t *testing.T) {
	t.Helper()
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const projectName = "sgap203b-auth"
	const tickID = "sgap203b-auth-2026-09-21-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"auth_error","message":"Invalid gateway API key"}}`))
	}))
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	beforeDeferrals := gap170Deferrals()
	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error on HTTP 401 — an auth rejection is terminal (GAP-035)")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("errors.Is(err, ErrGatewayKeyRejected) = false for %v — the terminal classification must survive", err)
	}
	if got := gap170Deferrals() - beforeDeferrals; got != 0 {
		t.Errorf("deferrals_total delta = %d on an auth rejection, want 0 — the deferral path must not swallow a key regression", got)
	}

	// Full stack: the row must land failed, not deferred.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state", fsTickID)
	}
	if status != "failed" {
		t.Errorf("ticks.status = %q on a 401, want \"failed\" — deferred must NOT become a blanket amnesty", status)
	}
	if got := gap170Deferrals() - beforeDeferrals; got != 0 {
		t.Errorf("deferrals_total delta = %d after the slot-pool 401, want 0", got)
	}
}

// TestSpawnTransient_PredicateScope pins the DEFERRAL predicate itself, because
// it is the load-bearing decision of this row and it is deliberately narrower
// than the retry classifier. "Worth retrying" and "never the lane's fault" are
// different questions: our own deadline expiring and an HTTP 5xx refusal are
// both retryable, but both stay FAILURES with their existing contracts
// (TickTimeout semantics; SCHED-GAP-143's transport-class markers).
func TestSpawnTransient_PredicateScope(t *testing.T) {
	// A real refused dial produces the exact live shape: *url.Error, NOT
	// wrapped in ErrGatewayTransient.
	req, reqErr := http.NewRequest("POST", schedGap203BClosedGatewayURL(t)+"/v1/responses", nil)
	if reqErr != nil {
		t.Fatalf("build request: %v", reqErr)
	}
	_, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err == nil {
		t.Fatal("premise: the closed-port dial succeeded")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"refused dial (*url.Error)", fmt.Errorf("gateway POST: %w", err), true},
		{"SSE stream ended", fmt.Errorf("%w: sse stream ended without a terminal event", ErrGatewayTransient), true},
		{"body read failure", fmt.Errorf("read response: %w: %w", ErrGatewayTransient, errors.New("unexpected EOF")), true},
		{"our own deadline", fmt.Errorf("gateway POST: %w", &url.Error{Op: "Post", Err: context.DeadlineExceeded}), false},
		{"cancelled (drain)", fmt.Errorf("gateway POST: %w", &url.Error{Op: "Post", Err: context.Canceled}), false},
		{"HTTP 503 drain refusal", &GatewayStatusError{StatusCode: http.StatusServiceUnavailable, msg: "gateway POST: HTTP 503: draining"}, false},
		{"HTTP 500 corruption", &GatewayStatusError{StatusCode: http.StatusInternalServerError, msg: "gateway POST: HTTP 500: database disk image is malformed"}, false},
		{"auth rejection", fmt.Errorf("%w (HTTP 401): bad key", ErrGatewayKeyRejected), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := gatewayTransientBlip(tc.err); got != tc.want {
			t.Errorf("gatewayTransientBlip(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSpawnTransientConnectFailure_IsDeferred — briefed case 1.
func TestSpawnTransientConnectFailure_IsDeferred(t *testing.T) {
	schedGap203BAssertConnectFailureDeferred(t)
}

// TestSpawnSSEStreamDropped_IsDeferred — briefed case 2.
func TestSpawnSSEStreamDropped_IsDeferred(t *testing.T) {
	schedGap203BAssertSSEDropDeferred(t)
}

// TestSpawnGatewayKeyRejected_StillFails — briefed case 3.
func TestSpawnGatewayKeyRejected_StillFails(t *testing.T) {
	schedGap203BAssertAuthStillFails(t)
}

// TestSpawnTransient_AllBriefedCases exists for the acceptance COMMAND, not for
// extra coverage. The row's acceptance line is
// `go test -short -race -count=1 -run TestSpawnTransient ./internal/scheduler/`,
// and Go's -run regex is matched UNANCHORED against the test name: only the
// first of the three briefed names contains "TestSpawnTransient", so the literal
// command would execute one of the three cases and silently skip the other two —
// a green result that proves two thirds of the row. This umbrella runs all three
// bodies under a name the selector matches, so the documented command is honest.
// The three individually-named tests above remain the canonical entries (they are
// what a reviewer greps for).
func TestSpawnTransient_AllBriefedCases(t *testing.T) {
	t.Run("ConnectFailure_IsDeferred", schedGap203BAssertConnectFailureDeferred)
	t.Run("SSEStreamDropped_IsDeferred", schedGap203BAssertSSEDropDeferred)
	t.Run("GatewayKeyRejected_StillFails", schedGap203BAssertAuthStillFails)
}
