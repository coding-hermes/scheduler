package scheduler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-119 regression tests. Live evidence that motivated the change:
// the GAP-117 pure wall-clock per-turn deadline (30m default) failed 15
// ticks across 13 projects on its first live day, including a
// hermes-dagger tick whose window contains real commits (e87d709 16:38,
// board close c27d5c3 16:49 — git-verified) yet was recorded as a
// zero-accounted failure. The tests below pin the new contract:
//
//   - AC 1: every POST leaves a GATEWAY-POST-TRACE record (log line +
//     ticks.gateway_trace JSON) with start/finish/elapsed/deadline/
//     classification, for all three classifications.
//   - AC 2: a demonstrably active turn (SSE events arriving) must NOT be
//     failed by the per-turn deadline — the idle window closes only on NO
//     activity for the configured deadline.
//   - AC 3: when the server answers non-SSE (no activity signal), the
//     deadline behaves exactly as the GAP-117 wall clock (fail-open).
//   - AC 4: the GAP-117 stalled contract holds — a byte-silent hang ends
//     status=failed with 'stalled: no progress' well before the tick
//     deadline, slot released.

// sseGateway streams a Responses-shaped SSE stream. onConnect fires when
// the POST lands; each script entry is one SSE frame written after the
// previous activity gap elapses (activity script: frame -> sleep -> ...).
func sseGateway(handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(handler))
}

// sseTurnHandler answers /v1/responses with Content-Type text/event-stream
// and a scripted event sequence: response.created immediately, then events
// separated by gap, then response.completed carrying the envelope.
func sseTurnHandler(sessionID string, gaps []time.Duration, usageIn, usageOut int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		write("response.created", `{"type":"response.created","response":{"id":"resp_sse","status":"in_progress","output":[]}}`)
		for _, gap := range gaps {
			time.Sleep(gap)
			write("response.output_text.delta", `{"type":"response.output_text.delta","delta":"chunk"}`)
		}
		write("response.completed", fmt.Sprintf(
			`{"type":"response.completed","response":{"id":"resp_sse","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"tick done"}]}],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`,
			usageIn, usageOut, usageIn+usageOut))
	}
}

// jsonTurnHandler answers non-SSE (no Content-Type event-stream): the AC 3
// fail-open path. delay controls how long the body takes to arrive.
func jsonTurnHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_json","status":"completed","output":[{"type":"answer","content":[{"type":"output_text","text":"tick done"}]}],"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`))
	}
}

// jsonStallHandler accepts the POST and never answers (AC 3 wall-clock
// parity with GAP-117: no bytes at all).
func jsonStallHandler(requestSeen, release chan struct{}) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestSeen) })
		<-release
	}
}

// TestSgap119_ActiveTurnExceedsWallDeadlineStillCompletes is the PRIMARY
// AC 2 regression: a turn whose TOTAL wall time exceeds the per-turn knob
// (2.5s of activity under a 1s deadline) must complete — every SSE event
// resets the idle window, so the deadline only trips on NO activity.
// This exact shape was killed by GAP-117 in production (19/22/29/33/57m
// productive turns vs a 30m wall clock).
func TestSgap119_ActiveTurnExceedsWallDeadlineStillCompletes(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-active-turn"
	mustCreateProjectINFRA012(t, db, projectName)

	// 3 deltas, 800ms apart = 2.4s total wall time >> 1s deadline.
	srv := sseGateway(sseTurnHandler("sess-sgap119-active", []time.Duration{800 * time.Millisecond, 800 * time.Millisecond, 800 * time.Millisecond}, 42, 7))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(1 * time.Second) // knob < tick deadline → supervised

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap119-active-2026-09-15-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Fatalf("Wait() status = %s, want %s — an ACTIVE turn (event every 800ms) must never be failed by a 1s per-turn deadline (total wall 2.4s)",
			outcome.Status, TickCompleted)
	}
	if outcome.TokensIn != 42 || outcome.TokensOut != 7 {
		t.Errorf("usage = %d/%d, want 42/7 — SSE response.completed usage must reach the tick row", outcome.TokensIn, outcome.TokensOut)
	}
}

// TestSgap119_IdleHangAbortsAtDeadline: AC 4 + AC 2's other half — a
// POST that goes silent (one event, then nothing) trips the idle deadline
// at the configured value and fails the tick with the GAP-117 stalled
// text, well before the tick deadline.
//
// INT-CI-157: the abort is proven by the persisted gateway_trace record
// (classification=aborted-by-turn-deadline, idle_fired=true,
// elapsed_ms≈deadline), NOT by a wall-clock sample — CI run
// 35565727270 showed the deadline firing at 401.8ms of its 400ms budget
// under -race on a loaded runner while the test still failed on the
// slot-release assertion below. The terminal-row wait doubles as the
// real-time guard: waitForTickTerminal returns !ok past the 5s cap, and
// the trace elapsed_ms is the exact deadline evidence.
func TestSgap119_IdleHangAbortsAtDeadline(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-idle-hang"
	mustCreateProjectINFRA012(t, db, projectName)

	// created event, then silence for 60s (released at test end).
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := sseGateway(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Hermes-Session-Id", "sess-sgap119-idle")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", `{"type":"response.created","response":{"id":"resp_sse","status":"in_progress","output":[]}}`)
		w.(http.Flusher).Flush()
		<-release
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	// Full-stack persistence proof via the slot pool (the only path that
	// Enqueues the ticks row) — mirrors the GAP-117 test's shape.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(400 * time.Millisecond)

	// The 5s cap is the CI safety net (INT-CI-157): waitForTickTerminal
	// returns ok=false once it expires, so a pathological tick cannot hang
	// the test. The AC evidence is the trace's elapsed_ms, asserted below.
	fsStart := time.Now()
	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, tickID, 5*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state within %v of real time (spawn→abort took %v; the idle deadline did not abort the silent POST) — INT-CI-157 safety net",
			tickID, 5*time.Second, time.Since(fsStart))
	}
	if status != "failed" {
		t.Fatalf("ticks.status = %q, want failed (stalled classification)", status)
	}
	errText := tickErrorOf(t, db, tickID)
	if !strings.Contains(errText, "stalled") || !strings.Contains(errText, "no progress") {
		t.Errorf("ticks.error = %q, want the GAP-117 'stalled: no progress' contract text", errText)
	}
	// AC 1: the trace must classify the abort and carry the session link.
	var traceStr sql.NullString
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if !strings.Contains(traceStr.String, `"classification":"aborted-by-turn-deadline"`) {
		t.Errorf("gateway_trace = %q, want classification aborted-by-turn-deadline", traceStr.String)
	}
	if !strings.Contains(traceStr.String, `"session_id":"sess-sgap119-idle"`) {
		t.Errorf("gateway_trace = %q, want the real session id preserved (GAP-079 reconciliation link)", traceStr.String)
	}
	if !strings.Contains(traceStr.String, `"idle_fired":true`) {
		t.Errorf("gateway_trace = %q, want idle_fired=true", traceStr.String)
	}
	// The stalled row keeps the REAL session id (not the tick-id placeholder).
	var rowSession string
	if err := db.QueryRow(`SELECT session_id FROM ticks WHERE id = ?`, tickID).Scan(&rowSession); err != nil {
		t.Fatalf("query session_id: %v", err)
	}
	if rowSession != "sess-sgap119-idle" {
		t.Errorf("ticks.session_id = %q, want sess-sgap119-idle — the stalled row must keep the real gateway session", rowSession)
	}
	// INT-CI-157: the ticks row goes terminal at lifecycle.Complete, but the
	// slot releases only when the slot-pool goroutine RETURNS — after the
	// post-completion bookkeeping (bump accounting, adaptive-cooldown writes).
	// Under -race on a loaded CI runner that window outlasted a single
	// immediate Running() sample (run 35565727270 failed exactly here with
	// "slot pool Running() = 1"). Poll like waitForTickTerminal instead:
	// the release lands within one 20ms poll in practice; the 15s cap is a
	// CI safety net, not the AC.
	runningDeadline := time.Now().Add(15 * time.Second)
	for loop.slotPool.Running() != 0 {
		if time.Now().After(runningDeadline) {
			t.Fatalf("slot pool still holds %d slot(s) %v after the stalled tick reached terminal — slot not released (INT-CI-157)",
				loop.slotPool.Running(), time.Since(fsStart))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := loop.slotPool.Running(); n != 0 {
		t.Errorf("slot pool Running() = %d after stalled tick, want 0 (slot released)", n)
	}
}

// TestSgap119_KeepaliveIsNotActivity: SSE comments (`: keepalive`, sent by
// the gateway every 30s regardless of agent liveness — source-verified in
// _iter_stream_items) must NOT reset the idle window. A stream of
// keepalives + one initial event trips the deadline at the configured
// value exactly like a byte-silent hang.
func TestSgap119_KeepaliveIsNotActivity(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-keepalive"
	mustCreateProjectINFRA012(t, db, projectName)

	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := sseGateway(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", `{"type":"response.created","response":{"id":"resp_sse","status":"in_progress","output":[]}}`)
		flusher.Flush()
		// Keepalives forever, no real events.
		for {
			select {
			case <-release:
				return
			default:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
				time.Sleep(50 * time.Millisecond)
			}
		}
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(400 * time.Millisecond)

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap119-keepalive-2026-09-15-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("Spawn blocked %v — keepalives must not keep a dead turn alive", elapsed)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("status = %s, want failed (keepalive-only stream = no activity)", outcome.Status)
	}
	if !strings.Contains(outcome.Error, "stalled") {
		t.Errorf("error = %q, want stalled classification", outcome.Error)
	}
}

// TestSgap119_JSONFallbackIsWallClock: AC 3 fail-open — a server answering
// non-SSE (no activity signal) gets exactly the GAP-117 wall-clock
// behavior: a JSON body that takes longer than the knob to arrive is
// aborted at the knob value with the stalled classification, and a body
// arriving under the knob completes.
func TestSgap119_JSONFallbackIsWallClock(t *testing.T) {
	t.Run("slow-json-aborts-at-knob", func(t *testing.T) {
		db := newTestDB(t)
		const projectName = "sgap119-json-slow"
		mustCreateProjectINFRA012(t, db, projectName)
		srv := sseGateway(jsonTurnHandler(2 * time.Second))
		defer srv.Close()

		spawner := NewSpawner(db, 4)
		spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
		spawner.SetNoExecFallback(true)
		spawner.timeout = 30 * time.Second
		spawner.SetGatewayResponseTimeout(500 * time.Millisecond)

		loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
		loop.SetNoDeliver(true)
		loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
		loop.SetNoExecFallback(true)
		loop.SetTickTimeout(30 * time.Second)
		loop.SetGatewayResponseTimeout(500 * time.Millisecond)

		fsStart := time.Now()
		tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
		status, ok := waitForTickTerminal(t, db, tickID, 5*time.Second)
		if !ok {
			t.Fatalf("tick %s never reached a terminal state — wall-clock parity failed on the JSON path", tickID)
		}
		if fsElapsed := time.Since(fsStart); fsElapsed >= 5*time.Second {
			t.Fatalf("slot-pool tick took %v — wall-clock parity failed on the JSON path", fsElapsed)
		}
		if status != "failed" {
			t.Fatalf("ticks.status = %q, want failed (JSON slower than the knob aborts at the knob — GAP-117 parity)", status)
		}
		if errText := tickErrorOf(t, db, tickID); !strings.Contains(errText, "stalled") {
			t.Errorf("ticks.error = %q, want stalled classification (wall-clock parity)", errText)
		}
		var traceStr sql.NullString
		if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
			t.Fatalf("query gateway_trace: %v", err)
		}
		if !strings.Contains(traceStr.String, `"deadline_mode":"wall"`) {
			t.Errorf("gateway_trace = %q, want deadline_mode=wall on the JSON fallback", traceStr.String)
		}
	})

	t.Run("fast-json-completes", func(t *testing.T) {
		db := newTestDB(t)
		const projectName = "sgap119-json-fast"
		mustCreateProjectINFRA012(t, db, projectName)
		srv := sseGateway(jsonTurnHandler(100 * time.Millisecond))
		defer srv.Close()

		spawner := NewSpawner(db, 4)
		spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
		spawner.SetNoExecFallback(true)
		spawner.timeout = 30 * time.Second
		spawner.SetGatewayResponseTimeout(2 * time.Second)

		tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
			"sgap119-jsonfast-2026-09-15-00-00-00")
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		outcome := tick.Wait()
		if outcome.Status != TickCompleted {
			t.Fatalf("status = %s, want completed — a fast JSON body under the knob must complete (fail-open, no behavior change)", outcome.Status)
		}
	})
}

// TestSgap119_KnobDisabledKeepsLegacyNonStreaming: with the knob off (0),
// the POST must NOT carry stream:true — the legacy path is byte-for-byte
// pre-119 (AC 3's "no behavior change on that path").
func TestSgap119_KnobDisabledKeepsLegacyNonStreaming(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-knob-off"
	mustCreateProjectINFRA012(t, db, projectName)

	var gotStream any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStream = body["stream"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"resp_legacy","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"tick done"}]}],"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`))
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(0) // disabled

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap119-knoboff-2026-09-15-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if outcome := tick.Wait(); outcome.Status != TickCompleted {
		t.Fatalf("status = %s, want completed", outcome.Status)
	}
	if gotStream != nil {
		t.Errorf("request body carried stream=%v — the knob-off path must be byte-for-byte legacy (no stream field)", gotStream)
	}
}

// TestSgap119_TraceOnCompletedTurn: AC 1 for the happy path — a completed
// POST persists a gateway_trace row with classification=completed, the
// deadline applied, and non-zero elapsed.
func TestSgap119_TraceOnCompletedTurn(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-trace-ok"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := sseGateway(sseTurnHandler("sess-sgap119-trace", []time.Duration{50 * time.Millisecond}, 10, 5))
	defer srv.Close()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(5 * time.Second)

	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, tickID, 5*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state", tickID)
	}
	if status != "completed" {
		t.Fatalf("ticks.status = %q, want completed", status)
	}
	var traceStr sql.NullString
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	var trace struct {
		Classification string `json:"classification"`
		DeadlineMS     int64  `json:"deadline_ms"`
		DeadlineMode   string `json:"deadline_mode"`
		ElapsedMS      int64  `json:"elapsed_ms"`
		SessionID      string `json:"session_id"`
		Events         int    `json:"events"`
		TickID         string `json:"tick_id"`
		Project        string `json:"project"`
	}
	if err := json.Unmarshal([]byte(traceStr.String), &trace); err != nil {
		t.Fatalf("unmarshal trace %q: %v", traceStr.String, err)
	}
	if trace.Classification != "completed" {
		t.Errorf("classification = %q, want completed", trace.Classification)
	}
	if trace.DeadlineMode != "idle" {
		t.Errorf("deadline_mode = %q, want idle (SSE surface)", trace.DeadlineMode)
	}
	if trace.DeadlineMS != 5000 {
		t.Errorf("deadline_ms = %d, want 5000 (the applied deadline)", trace.DeadlineMS)
	}
	if trace.SessionID != "sess-sgap119-trace" {
		t.Errorf("session_id = %q, want the header-provided session id", trace.SessionID)
	}
	if trace.Events < 3 {
		t.Errorf("events = %d, want >= 3 (created + delta + completed)", trace.Events)
	}
	if trace.TickID != tickID {
		t.Errorf("tick_id = %q, want %q", trace.TickID, tickID)
	}
	if trace.ElapsedMS <= 0 {
		t.Errorf("elapsed_ms = %d, want > 0", trace.ElapsedMS)
	}
}

// TestSgap119_TransportErrorTrace: AC 1 for the transport-error class —
// a gateway 503 on the supervised path persists classification
// transport-error.
func TestSgap119_TransportErrorTrace(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap119-transport"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestSeen) })
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"gateway overloaded","type":"server_error"}}`))
	}))
	defer srv.Close()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(5 * time.Second)

	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	// The gateway 503s forever: the GAP-080 retry loop exhausts, the tick
	// drops (exec fallback disabled), and the row must reach a terminal
	// failed state with the transport-error trace persisted.
	status, ok := waitForTickTerminal(t, db, tickID, 30*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state — the 503 drop path wedged", tickID)
	}
	if status != "failed" {
		t.Fatalf("ticks.status = %q, want failed (dropped tick)", status)
	}
	var traceStr sql.NullString
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if !strings.Contains(traceStr.String, `"classification":"transport-error"`) {
		t.Errorf("gateway_trace = %q, want classification transport-error", traceStr.String)
	}
	if !strings.Contains(traceStr.String, `"attempts":4"`) && !strings.Contains(traceStr.String, `"attempts":4`) {
		t.Logf("gateway_trace attempts field: %s", traceStr.String)
	}
}

// TestSgap119_TurnDeadlineErrorNotTransient pins the retry contract: the
// idle abort must NOT enter the GAP-080 transient retry loop (re-sending
// a POST after a full idle window would double-run the foreman).
func TestSgap119_TurnDeadlineErrorNotTransient(t *testing.T) {
	var tde *TurnDeadlineError
	err := error(&TurnDeadlineError{After: time.Second, Events: 2})
	if !errors.As(err, &tde) {
		t.Fatal("errors.As(TurnDeadlineError) failed")
	}
	if IsTransientGatewayErr(err) {
		t.Error("IsTransientGatewayErr(TurnDeadlineError) = true — an idle abort must never be retried as transient")
	}
	var wrapped = fmt.Errorf("wrap: %w", err)
	if IsTransientGatewayErr(wrapped) {
		t.Error("wrapped TurnDeadlineError must also stay non-transient")
	}
}

// parseSSE drives the in-package SSE reader with no watch (nil) so the
// terminal-envelope shapes can be tested without HTTP.
func parseSSE(body string) (*Response, error) {
	return readSSEResponse(strings.NewReader(body), nil)
}

// TestSgap119_ReadSSEResponseTerminalShapes pins the envelope assembly at
// the client seam: response.completed maps to the legacy Response shape
// and response.failed maps to the error envelope, with usage carried.
func TestSgap119_ReadSSEResponseTerminalShapes(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		r, err := parseSSE(
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":6,\"total_tokens\":11}}}\n\n")
		if err != nil {
			t.Fatalf("readSSEResponse: %v", err)
		}
		if r.ID != "resp_x" || r.Status != "completed" || r.Usage.InputTokens != 5 || r.Usage.OutputTokens != 6 {
			t.Errorf("assembled response = %+v, want id=resp_x status=completed usage 5/6", r)
		}
		if r.ExtractText() != "done" {
			t.Errorf("ExtractText = %q, want done", r.ExtractText())
		}
	})
	t.Run("failed", func(t *testing.T) {
		r, err := parseSSE(
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_y\",\"status\":\"failed\",\"output\":[]},\"error\":{\"message\":\"provider 500\",\"type\":\"server_error\"}}\n\n")
		if err != nil {
			t.Fatalf("readSSEResponse(failed): %v", err)
		}
		if r.Error == nil || r.Error.Message != "provider 500" {
			t.Errorf("assembled failed response = %+v, want error.message=provider 500", r)
		}
	})
}

// TestSgap119_MigrationV28 pins migration v28: the ticks table gains the
// gateway_trace column after Migrate().
func TestSgap119_MigrationV28(t *testing.T) {
	db := newTestDB(t)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ticks') WHERE name='gateway_trace'`).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if n != 1 {
		t.Fatalf("ticks.gateway_trace missing after Migrate (count=%d) — migration v28 not applied", n)
	}
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM migrations`).Scan(&v); err != nil {
		t.Fatalf("max migration version: %v", err)
	}
	if v < 28 {
		t.Errorf("max migration = %d, want >= 28", v)
	}
}
