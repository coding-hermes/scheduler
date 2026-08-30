package scheduler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-080 (2026-08-29): the 2026-08-28 gateway
// state.db corruption burst (HTTP 500 'database disk image is malformed')
// silently dropped 7 ticks — the spawn path had NO retry for transient
// gateway failures, so a 5xx fell straight to the GATEWAY FAIL → SKIPPED
// block and the tick was gone. These tests pin the bounded same-pair retry:
// a blip that recovers completes the tick (no drop), a persistent 5xx still
// fails it (completion gate intact), and 401/403 never enter the retry path
// (GAP-035 terminal classification unchanged).

// schedGap080Spawner builds a spawner on the given test DB wired to the
// handler with the standard test client (shared daemon key, exec fallback
// disabled) and captures the process log into buf (no t.Parallel anywhere in
// this package, so global log.SetOutput is safe under -p 1).
func schedGap080Spawner(t *testing.T, db *sql.DB, buf *bytes.Buffer, handler http.HandlerFunc, clientTimeout time.Duration) *Spawner {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", clientTimeout))
	spawner.SetNoExecFallback(true)
	return spawner
}

// schedGap080CaptureLog redirects the process logger into a buffer for the
// duration of the test and restores it on cleanup.
func schedGap080CaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// schedGap080CompletedResponse is a minimal valid /v1/responses 200 payload.
func schedGap080CompletedResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"id":     "resp_gap080",
		"status": "completed",
		"output": []map[string]any{
			{
				"type": "message",
				"content": []map[string]any{
					{"type": "output_text", "text": "blip recovered"},
				},
			},
		},
		"usage": map[string]int{},
	})
}

// schedGap080ServerError writes the real 2026-08-28 corruption shape: HTTP
// 500 with the gateway's server_error envelope.
func schedGap080ServerError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "server_error",
			"message": "Internal server error: database disk image is malformed",
		},
	})
}

// TestSCHEDGAP080_BlipRecoversWithRetry — the acceptance criterion for the
// whole fix: a gateway that 500s twice and then recovers must complete the
// tick via bounded retries on the SAME pair — GATEWAY RETRY lines logged, no
// GATEWAY FAIL / SKIPPED for this tick, exactly one tick row, and the
// transient counter reflects the two failed attempts.
func TestSCHEDGAP080_BlipRecoversWithRetry(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap080-blip"
		tickID      = "gap080-blip-2026-08-28-10-01-25"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	var requests int32
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		if n <= 2 {
			schedGap080ServerError(w)
			return
		}
		schedGap080CompletedResponse(w)
	}, 5*time.Second)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a recovered blip must complete the tick, not drop it", outcome.Status, TickCompleted)
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Errorf("gateway POST count = %d, want 3 (1 initial + 2 transient retries before success)", got)
	}
	if got := spawner.GatewayErrorCount(); got != 2 {
		t.Errorf("GatewayErrorCount() = %d, want 2 (two transient failures before recovery)", got)
	}
	if n := schedGap080TickRowCount(t, db, tickID); n != 1 {
		t.Errorf("tick rows for %s = %d, want exactly 1", tickID, n)
	}
	logs := buf.String()
	if !strings.Contains(logs, "GATEWAY RETRY: "+projectName+" tick="+tickID) {
		t.Errorf("logs missing 'GATEWAY RETRY' for the tick:\n%s", logs)
	}
	if strings.Contains(logs, "GATEWAY FAIL") {
		t.Errorf("logs contain 'GATEWAY FAIL' — a recovered blip must never reach the FAIL path:\n%s", logs)
	}
	if strings.Contains(logs, "SKIPPED") {
		t.Errorf("logs contain 'SKIPPED ... dropping tick' — a recovered blip must never be dropped:\n%s", logs)
	}
}

// TestSCHEDGAP080_Persistent500FailsAfterRetries — the bounded-retry safety
// net: a gateway that 500s EVERY attempt is retried exactly
// gatewayRetryMaxAttempts times (4 POSTs total) and then FAILS the tick via
// the existing path — gateway error text persisted, consecutive_failures
// incremented, tick never completed, counter reflects all 4 failures.
func TestSCHEDGAP080_Persistent500FailsAfterRetries(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap080-persistent"
		tickID      = "gap080-persistent-2026-08-28-10-02-31"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var requests int32
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		schedGap080ServerError(w)
	}, 5*time.Second)

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — a persistently-5xx gateway must fail the tick")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to carry 'HTTP 500'", err.Error())
	}
	if !strings.Contains(err.Error(), "database disk image is malformed") {
		t.Errorf("error = %q, want the gateway detail 'database disk image is malformed'", err.Error())
	}
	if got := atomic.LoadInt32(&requests); got != 1+gatewayRetryMaxAttempts {
		t.Errorf("gateway POST count = %d, want %d (1 initial + %d retries — bounded!)", got, 1+gatewayRetryMaxAttempts, gatewayRetryMaxAttempts)
	}
	if got := spawner.GatewayErrorCount(); got != 1+gatewayRetryMaxAttempts {
		t.Errorf("GatewayErrorCount() = %d, want %d", got, 1+gatewayRetryMaxAttempts)
	}
	// SCHED-GAP-079 completion gate: the tick is NEVER recorded completed.
	if got := tickStatusOf(t, db, tickID); got == "completed" {
		t.Errorf("tick status = %q — a failed 500 tick must never be completed/committed", got)
	}
	// noteSpawnFailure fires exactly once (the SKIPPED block), incrementing
	// consecutive_failures — the error is persisted, not swallowed.
	var failures int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, projectName).Scan(&failures); err != nil {
		t.Fatalf("query consecutive_failures: %v", err)
	}
	if failures != 1 {
		t.Errorf("consecutive_failures = %d, want 1 (noteSpawnFailure fired once on the exhausted-failure path)", failures)
	}
	logs := buf.String()
	if !strings.Contains(logs, "GATEWAY FAIL") {
		t.Errorf("logs missing 'GATEWAY FAIL' for the exhausted-failure path:\n%s", logs)
	}
	if !strings.Contains(logs, "SKIPPED") {
		t.Errorf("logs missing 'SKIPPED' (exec fallback disabled, dropping tick):\n%s", logs)
	}
	if !strings.Contains(logs, "GATEWAY RETRY") {
		t.Errorf("logs missing 'GATEWAY RETRY' (retries did happen before exhaustion):\n%s", logs)
	}
}

// TestSCHEDGAP080_AuthRejectionNotRetried — a 401 stays terminal (GAP-035):
// exactly ONE POST, no transient counter, no GATEWAY RETRY. The retry path
// must never burn attempts on auth rejections or weaken the no-retry-flood
// guard.
func TestSCHEDGAP080_AuthRejectionNotRetried(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap080-auth"
		tickID      = "gap080-auth-2026-08-28-10-03-28"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var requests int32
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "auth_error",
				"message": "Invalid gateway API key",
			},
		})
	}, 5*time.Second)

	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error on HTTP 401")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("errors.Is(err, ErrGatewayKeyRejected) = false, want true (terminal classification preserved)")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("gateway POST count = %d, want 1 — auth rejection must NOT be retried", got)
	}
	if got := spawner.GatewayErrorCount(); got != 0 {
		t.Errorf("GatewayErrorCount() = %d, want 0 — auth rejections are never counted as transient", got)
	}
	if logs := buf.String(); strings.Contains(logs, "GATEWAY RETRY") {
		t.Errorf("logs contain 'GATEWAY RETRY' for a 401 — the transient retry path touched an auth rejection:\n%s", logs)
	}
}

// TestSCHEDGAP080_NetworkTimeoutRecovers — a client-side network timeout
// (*url.Error) is transient: the first POST times out, the retry lands on a
// healthy gateway, and the tick completes.
func TestSCHEDGAP080_NetworkTimeoutRecovers(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap080-timeout"
		tickID      = "gap080-timeout-2026-08-28-10-01-30"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var requests int32
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			time.Sleep(2 * time.Second) // blow the client timeout
			return
		}
		schedGap080CompletedResponse(w)
	}, 200*time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a timeout blip that recovers must complete the tick", outcome.Status, TickCompleted)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("gateway POST count = %d, want 2 (1 timed out + 1 retry)", got)
	}
	if got := spawner.GatewayErrorCount(); got != 1 {
		t.Errorf("GatewayErrorCount() = %d, want 1", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "GATEWAY RETRY") {
		t.Errorf("logs missing 'GATEWAY RETRY':\n%s", logs)
	}
}

// schedGap080TickRowCount counts tick rows for the given id.
func schedGap080TickRowCount(t *testing.T, db *sql.DB, tickID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE id = ?`, tickID).Scan(&n); err != nil {
		t.Fatalf("count tick rows: %v", err)
	}
	return n
}
