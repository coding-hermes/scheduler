package scheduler

// SCHED-GAP-1681 regression tests: gateway transient retry with session
// continuation. The row's live evidence (SCHED-GAP-203 follow-up, 2026-09-30):
// a gateway SSE stream that ends without a terminal event books the tick
// `deferred` and the lane waits for its next wake. The GAP-080 bounded retry
// existed but (a) its count was hardcoded, (b) the retry contract — same
// model/provider pair, SAME session key (X-Hermes-Session-Key: tickID), so
// the gateway re-attaches the session instead of minting a new one — was
// undocumented and untested, and (c) the per-POST attempt count lived only
// inside gateway_trace JSON on some paths (the legacy JSON path recorded no
// trace at all for failed attempts), never on the ticks row.
//
// These tests pin:
//   - AC 3 (session continuation): every retry POST carries the SAME session
//     key (the tick id) — the primary AC-5 scenario exercises it end-to-end.
//   - AC 1 (configurable retry count): the knob arm/disarm behavior —
//     0 = single attempt, N = up to N retries, negative ignored.
//   - AC 3 (attempt counting): every POST lands on the merged trace's
//     Attempts (failed legacy-path attempts included — the pre-fix hole),
//     and Wait() stamps the count onto TickOutcome.Attempts.
//   - Honesty: a retry that still fails books the tick deferred/failed with
//     the reason — never a silent success.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sgap1681SessionCapture records the session keys and POST order the mock
// gateway observed. requests counts every POST; sessionKeys lists the
// X-Hermes-Session-Key header per request in arrival order.
type sgap1681SessionCapture struct {
	requests    int32
	sessionKeys []string
}

// sgap1681SSEHandler builds the mock gateway for the AC-5 scenario: POST #1
// answers text/event-stream and ENDS EARLY (response.created, then the
// connection closes — the exact "sse stream ended without a terminal event"
// drop), every later POST answers a full completed SSE turn. The session key
// of each POST is recorded in arrival order. When commitDir is non-empty the
// completed attempt also lands a git commit in it, so the tick's artifact
// measurement derives outcome=committed (the row's AC-5 wording).
func sgap1681SSEHandler(cap *sgap1681SessionCapture, sessionID string, usageIn, usageOut int, commitDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&cap.requests, 1)
		cap.sessionKeys = append(cap.sessionKeys, r.Header.Get("X-Hermes-Session-Key"))
		w.Header().Set("Content-Type", "text/event-stream")
		if sessionID != "" {
			w.Header().Set("X-Hermes-Session-Id", sessionID)
		}
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		write("response.created", `{"type":"response.created","response":{"id":"resp_sgap1681","status":"in_progress","output":[]}}`)
		if n <= 1 {
			// Attempt 1: the stream DIES here — no terminal event. The
			// client's read hits EOF and must classify
			// ErrGatewayTransient ("sse stream ended without a terminal
			// event"), not a completed turn.
			return
		}
		// The retried turn does REAL work: land a commit mid-window so the
		// artifact measurement (countGitChanges) sees it and terminalOutcome
		// derives 'committed' (SCHED-GAP-1652 doctrine: artifacts decide).
		if commitDir != "" {
			if err := os.WriteFile(filepath.Join(commitDir, fmt.Sprintf("tick-work-%d.txt", time.Now().UnixNano())), []byte("work\n"), 0o644); err == nil {
				_ = exec.Command("git", "-C", commitDir, "add", "-A").Run()
				_ = exec.Command("git", "-C", commitDir, "commit", "-q", "-m", "sgap1681: retried turn lands work").Run()
			}
		}
		// Attempts 2+: a full turn — deltas (real activity) then the
		// terminal envelope. Productive-scale usage stays outside the
		// SCHED-GAP-1641 instant-turn band.
		write("response.output_text.delta", `{"type":"response.output_text.delta","delta":"resumed"}`)
		write("response.completed", fmt.Sprintf(
			`{"type":"response.completed","response":{"id":"resp_sgap1681","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"resumed and done"}]}],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`,
			usageIn, usageOut, usageIn+usageOut))
	}
}

// sgap1681TickRow holds the persisted accounting the AC-5 assertions read.
type sgap1681TickRow struct {
	status    string
	outcome   string
	attempts  int
	sessionID string
}

// TestSCHEDGAP1681_StreamDropRetriedSameSessionCommits is the PRIMARY AC-5
// regression: attempt 1's SSE stream ends without a terminal event (the
// measured blip), the GAP-080 retry fires attempt 2 with the SAME session
// key, attempt 2 completes, and the tick lands status=completed /
// outcome=committed with attempts=2 on the tick row, the real gateway
// session id preserved.
func TestSCHEDGAP1681_StreamDropRetriedSameSessionCommits(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap1681-blip"
		sessionID   = "sess-sgap1681"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	// A REAL git repo + one pre-existing commit: the tick's turn lands one
	// more commit mid-window, so terminalOutcome derives outcome=committed
	// (SCHED-GAP-1652: artifacts, not process exit). Seed at now-1h keeps
	// the whole-second --since granularity from pulling it into the window.
	workdir := initEmptyGitRepo(t)
	seedTime := time.Now().Add(-1 * time.Hour)
	gitCommitFileAt(t, workdir, "seed.txt", "seed\n", seedTime)

	cap := &sgap1681SessionCapture{}
	srv := httptest.NewServer(sgap1681SSEHandler(cap, sessionID, 42400, 7000, workdir))
	defer srv.Close()

	// FULL-STACK path via the slot pool (the only path that enqueues the
	// ticks row and runs lifecycle.Complete) — the same shape the
	// SCHED-GAP-119 tests use.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(5 * time.Second)
	loop.SetGatewayTransientRetries(3)

	packed := PackedProject{Name: projectName, Workdir: workdir}
	tickID := loop.slotPool.Spawn(packed, time.Now(), true, db)
	// The turn lands its commit ~1s in; the slot-pool goroutine then runs
	// Wait() + lifecycle.Complete. The 10s cap is the CI safety net.
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, err := sgap1681ReadTickRowFull(db, tickID)
		if err == nil && row.status != "running" && row.status != "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tick %s never reached a terminal state within 10s", tickID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// AC 5: attempts=2 — one dropped POST + one completed retry.
	if got := atomic.LoadInt32(&cap.requests); got != 2 {
		t.Errorf("gateway POST count = %d, want 2 (attempt 1 dropped, attempt 2 completed)", got)
	}

	// AC 2/3 (session continuation): BOTH attempts carried the SAME session
	// key — the tick id — so the gateway re-attaches the session instead of
	// minting a new one. This is the assertion that pins the continuation
	// contract; a fresh-POST retry would show an empty or different key.
	if len(cap.sessionKeys) != 2 {
		t.Fatalf("session keys captured = %d, want 2", len(cap.sessionKeys))
	}
	for i, key := range cap.sessionKeys {
		if key != tickID {
			t.Errorf("attempt %d session key = %q, want the SAME tick id %q (session continuation)", i+1, key, tickID)
		}
	}

	// Persisted row: status/outcome/attempts/session_id as the AC states.
	row, err := sgap1681ReadTickRowFull(db, tickID)
	if err != nil {
		t.Fatalf("query tick row: %v", err)
	}
	if row.status != "completed" {
		t.Errorf("ticks.status = %q, want completed", row.status)
	}
	if row.outcome != "committed" {
		t.Errorf("ticks.outcome = %q, want committed (SCHED-GAP-1681 AC 5: the recovered tick is committed, not dry_run)", row.outcome)
	}
	if row.attempts != 2 {
		t.Errorf("ticks.attempts = %d, want 2 (the v67 column stamped by lifecycle.Complete)", row.attempts)
	}
	// Session preserved: the row keeps the real gateway session id from the
	// terminal envelope (the header id lives on the trace), never the
	// tick-id placeholder.
	if row.sessionID != "resp_sgap1681" {
		t.Errorf("ticks.session_id = %q, want resp_sgap1681 (the gateway session preserved across the retry)", row.sessionID)
	}
	if row.sessionID == tickID {
		t.Errorf("ticks.session_id = the tick-id placeholder — the real session id was lost across the retry")
	}
	_ = sessionID // the header id is asserted on the trace below

	// The merged trace carried both attempts too (the same count the row
	// shows) and preserved the header session id for GAP-079 reconciliation.
	var rawTrace string
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&rawTrace); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if rawTrace == "" {
		t.Fatal("gateway_trace is empty — the per-POST record was not persisted")
	}
	var tr struct {
		Attempts  int    `json:"attempts"`
		SessionID string `json:"session_id"`
		Events    int    `json:"events"`
	}
	if err := json.Unmarshal([]byte(rawTrace), &tr); err != nil {
		t.Fatalf("unmarshal gateway_trace %q: %v", rawTrace, err)
	}
	if tr.Attempts != 2 {
		t.Errorf("gateway_trace.Attempts = %d, want 2 (mergePostTrace folded both POSTs)", tr.Attempts)
	}
	if tr.SessionID != sessionID {
		t.Errorf("gateway_trace.session_id = %q, want %q (the trace keeps the header id)", tr.SessionID, sessionID)
	}
}

// sgap1681ReadTickRowFull reads the four AC columns from the persisted tick
// row.
func sgap1681ReadTickRowFull(db *sql.DB, tickID string) (sgap1681TickRow, error) {
	var row sgap1681TickRow
	err := db.QueryRow(`SELECT status, outcome, attempts, COALESCE(session_id,'') FROM ticks WHERE id = ?`,
		tickID).Scan(&row.status, &row.outcome, &row.attempts, &row.sessionID)
	return row, err
}

// TestSCHEDGAP1681_ZeroRetriesMeansSingleAttempt pins the knob's OFF arm:
// --gateway-transient-retries 0 must restore the pre-GAP-080 single-attempt
// behavior — one POST, and the dropped stream books the tick honestly
// (deferred via the SCHED-GAP-203 blip path) with attempts=1.
func TestSCHEDGAP1681_ZeroRetriesMeansSingleAttempt(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap1681-zero"
		tickID      = "sgap1681-zero-2026-09-30-10-02-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	cap := &sgap1681SessionCapture{}
	srv := httptest.NewServer(sgap1681SSEHandler(cap, "", 42400, 7000, ""))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(5 * time.Second)
	spawner.SetGatewayTransientRetries(0) // OFF

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()

	if got := atomic.LoadInt32(&cap.requests); got != 1 {
		t.Errorf("gateway POST count = %d, want 1 (retries=0 must not re-send)", got)
	}
	// Honesty (AC 3): the failed attempt still books the tick with a reason
	// — the SCHED-GAP-203 blip deferral — never a silent success.
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s (the dropped stream with retries off books the blip as a deferral)",
			outcome.Status, TickDeferred)
	}
	if !strings.Contains(outcome.Error, "sse stream ended without a terminal event") {
		t.Errorf("deferred reason = %q, want the gateway drop text (auditable, not silent)", outcome.Error)
	}
	if outcome.Attempts != 1 {
		t.Errorf("TickOutcome.Attempts = %d, want 1 (the single failed attempt is counted)", outcome.Attempts)
	}
}

// TestSCHEDGAP1681_ExhaustedRetriesDeferWithAttemptCount pins the bounded
// honesty arm: every retry fails, the tick defers with the reason, and the
// row records 1 + N attempts — never fabricated success.
func TestSCHEDGAP1681_ExhaustedRetriesDeferWithAttemptCount(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap1681-exhausted"
		tickID      = "sgap1681-exhausted-2026-09-30-10-03-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	cap := &sgap1681SessionCapture{}
	// Always-drop gateway: EVERY attempt's stream ends without a terminal
	// event, so the retry loop must exhaust and the tick must defer honestly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cap.requests, 1)
		cap.sessionKeys = append(cap.sessionKeys, r.Header.Get("X-Hermes-Session-Key"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", `{"type":"response.created","response":{"id":"resp_sgap1681","status":"in_progress","output":[]}}`)
		flusher.Flush()
		// Connection ends here — no terminal event, every attempt.
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(5 * time.Second)
	spawner.SetGatewayTransientRetries(2)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()

	if got := atomic.LoadInt32(&cap.requests); got != 3 {
		t.Errorf("gateway POST count = %d, want 3 (1 initial + 2 retries, bounded)", got)
	}
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s (exhausted blip defers, honestly)", outcome.Status, TickDeferred)
	}
	if outcome.Attempts != 3 {
		t.Errorf("TickOutcome.Attempts = %d, want 3 — the deferral records every attempt", outcome.Attempts)
	}
	// Every attempt re-used the SAME session key (continuation holds even
	// when the blip repeats).
	for i, key := range cap.sessionKeys {
		if key != tickID {
			t.Errorf("attempt %d session key = %q, want %q (same session on every retry)", i+1, key, tickID)
		}
	}
}

// TestSCHEDGAP1681_LegacyPathFailedAttemptsCounted pins the pre-fix hole this
// row closed: on the LEGACY (non-supervised) path a FAILED attempt used to
// record no trace at all, so the merged trace's Attempts read 1 even when
// retries fired. Every attempt — failed ones included — must merge a trace,
// and the getter/setter contract must hold (negative ignored).
func TestSCHEDGAP1681_LegacyPathFailedAttemptsCounted(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap1681-legacy"
		tickID      = "sgap1681-legacy-2026-09-30-10-04-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	// Always 500 — the legacy JSON path (no supervision: gatewayResponseTimeout
	// >= tick deadline keeps sendTurn off the SSE path only when the gateway
	// does not answer SSE; a 5xx IS the legacy attempt trace we want).
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"blip"}}`))
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	// Knob OFF (deadline >= tick deadline → legacy non-streaming POST path).
	spawner.SetGatewayResponseTimeout(0)
	spawner.SetGatewayTransientRetries(2)

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — a persistently-5xx gateway must fail the tick")
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Errorf("gateway POST count = %d, want 3 (1 initial + 2 retries on the legacy path)", got)
	}
	// AC 4: the trace persisted by logPOSTTrace on the drop path must count
	// EVERY attempt — pre-fix the legacy path recorded no trace for failed
	// attempts, so this read attempts=1 (or nothing) while 3 POSTs were
	// actually made.
	var rawTrace string
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&rawTrace); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if rawTrace == "" {
		t.Fatal("gateway_trace is empty — the legacy path must record failed attempts too (the pre-fix hole)")
	}
	var tr struct {
		Attempts       int    `json:"attempts"`
		Classification string `json:"classification"`
	}
	if err := json.Unmarshal([]byte(rawTrace), &tr); err != nil {
		t.Fatalf("unmarshal gateway_trace %q: %v", rawTrace, err)
	}
	if tr.Attempts != 3 {
		t.Errorf("gateway_trace.Attempts = %d, want 3 — failed legacy attempts must merge into the tick trace", tr.Attempts)
	}
	if tr.Classification != "transport-error" {
		t.Errorf("gateway_trace.classification = %q, want transport-error (the last attempt's verdict)", tr.Classification)
	}
	// The setter contract: negative values are ignored (the count stays 2).
	spawner.SetGatewayTransientRetries(-5)
	if got := spawner.GatewayTransientRetries(); got != 2 {
		t.Errorf("GatewayTransientRetries() = %d after Set(-5), want 2 (negative ignored)", got)
	}
}

// TestSCHEDGAP1681_EnvResolverDefaults pins the library-level env resolution:
// unset → the pre-1681 hardcoded default (3); negative → rejected with the
// default standing; 0 → an explicit off is honored.
func TestSCHEDGAP1681_EnvResolverDefaults(t *testing.T) {
	t.Setenv("SCHEDULER_GATEWAY_TRANSIENT_RETRIES", "")
	if got := gatewayTransientRetriesFromEnv(); got != gatewayRetryDefaultMax {
		t.Errorf("unset env: gatewayTransientRetriesFromEnv() = %d, want %d (the pre-1681 default)", got, gatewayRetryDefaultMax)
	}
	t.Setenv("SCHEDULER_GATEWAY_TRANSIENT_RETRIES", "0")
	if got := gatewayTransientRetriesFromEnv(); got != 0 {
		t.Errorf("env=0: gatewayTransientRetriesFromEnv() = %d, want 0 (explicit off honored)", got)
	}
	t.Setenv("SCHEDULER_GATEWAY_TRANSIENT_RETRIES", "5")
	if got := gatewayTransientRetriesFromEnv(); got != 5 {
		t.Errorf("env=5: gatewayTransientRetriesFromEnv() = %d, want 5", got)
	}
	t.Setenv("SCHEDULER_GATEWAY_TRANSIENT_RETRIES", "-2")
	if got := gatewayTransientRetriesFromEnv(); got != gatewayRetryDefaultMax {
		t.Errorf("env=-2: gatewayTransientRetriesFromEnv() = %d, want %d (negative rejected, default stands)", got, gatewayRetryDefaultMax)
	}
	t.Setenv("SCHEDULER_GATEWAY_TRANSIENT_RETRIES", "garbage")
	if got := gatewayTransientRetriesFromEnv(); got != gatewayRetryDefaultMax {
		t.Errorf("env=garbage: gatewayTransientRetriesFromEnv() = %d, want %d (unparseable rejected)", got, gatewayRetryDefaultMax)
	}
}
