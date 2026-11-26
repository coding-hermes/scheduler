package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-1762 regression suite — TOOL-ACTIVE turns count as progress in
// the per-turn idle gate.
//
// LIVE DEFECT this pins: 14 of 22 terminal pm-family ticks in 24h failed
// "stalled: no progress for 30m0s". PM lanes are long tool-heavy turns
// (skill loads, board reads, live probes). The idle deadline resets only
// on real SSE events; during a LONG tool execution the stream goes silent
// between the tool-call item event and its result event, so a productive
// turn with a silent tool-run gap longer than the deadline was aborted
// mid-tool.
//
// FIX: when the stream's most recent real event marks a tool call in
// flight (sseEventIsToolActivity: the GAP-1655 item-type list arriving as
// output_item.added/done, or the function_call/custom_tool_call argument
// events), the idle timer is armed to a BOUNDED tool-execution window
// (toolGraceMultiple=3 × the deadline) instead of the deadline itself.
// The tool's result event — the next real event of any other kind —
// restores normal semantics.
//
// Arms pinned here (deadline scaled to 250ms, window to 750ms — same
// arithmetic as the GAP-1755 suite's 30m/90m production shape):
//
//	A  tool event → silence > deadline but < grace (keepalives during
//	   it) → result event → completed; NOT stalled.
//	B  last real event NOT a tool call → silence > deadline → still
//	   aborts stalled (no weakening of GAP-119/1755).
//	C  tool-active silence > the tool grace bound → still aborts
//	   stalled (the bound is real, never unbounded).
//	D  a stream of ONLY keepalive comments > deadline still aborts
//	   (GAP-119 anti-liveness contract, kept byte-identical).

// sgap1762SSEWrite is the tiny SSE frame helper shared by the arms.
func sgap1762SSEWrite(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	w.(http.Flusher).Flush()
}

// sgap1762ArmCHandler emits created → tool-call item → silence (keepalives
// during it) → result → completed. The keepalive loop runs for the whole
// silent window: keepalives during a tool execution must neither clear the
// tool window nor extend it.
func sgap1762ArmAHandler(silence time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sgap1762SSEWrite(w, "response.created", `{"type":"response.created","response":{"id":"resp_1762a","status":"in_progress","output":[]}}`)
		// The tool call goes in flight.
		sgap1762SSEWrite(w, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"read_board","call_id":"call_1"}}`)
		// Silent tool execution: no real events, only gateway keepalives.
		deadline := time.Now().Add(silence)
		for time.Now().Before(deadline) {
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		// The tool's result — the next real event of another kind.
		sgap1762SSEWrite(w, "response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call_output","call_id":"call_1","output":"board read"}}`)
		// SCHED-GAP-1641: productive-scale usage (outside the instant-turn band).
		sgap1762SSEWrite(w, "response.completed", `{"type":"response.completed","response":{"id":"resp_1762a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tick done"}]}],"usage":{"input_tokens":42400,"output_tokens":7000,"total_tokens":49400}}}`)
	}
}

// TestSgap1762_ToolSilentTurnCompletes is arm A — the fix. A turn whose
// most recent real event is a tool-call item, followed by a silent tool
// run of 500ms (2× the 250ms per-turn deadline, inside the 750ms tool
// grace), then the result event and a terminal completed envelope, must
// COMPLETE — never "stalled". Verified full-stack through the slot pool
// (the only path that enqueues the ticks row) so the persisted gateway
// trace can be asserted too.
func TestSgap1762_ToolSilentTurnCompletes(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1762-tool-active"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := httptest.NewServer(sgap1762ArmAHandler(500 * time.Millisecond))
	defer srv.Close()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "«redacted:sk-…»", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(250 * time.Millisecond) // tool grace = 750ms

	start := time.Now()
	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, tickID, 10*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state within %v of real time — a tool-active silence inside the tool grace must complete, not wedge", tickID, 10*time.Second)
	}
	if status != "completed" {
		t.Fatalf("ticks.status = %q (error %q), want completed — a tool-active silence of 500ms under a 250ms deadline (inside the 750ms tool grace) must NOT be classified stalled",
			status, tickErrorOf(t, db, tickID))
	}
	if errText := tickErrorOf(t, db, tickID); strings.Contains(errText, "stalled") {
		t.Errorf("ticks.error = %q — a tool-active turn must never carry the stalled classification", errText)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("turn completed in %v, want >= 500ms — the silent tool window must actually have been sat through", elapsed)
	}
	// Trace stays truthful: Events counts real SSE events (created + tool
	// item + result + completed = 4; keepalives excluded) and the optional
	// tool-activity field is populated without repurposing anything.
	var traceStr sql.NullString
	if err := db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if !strings.Contains(traceStr.String, `"classification":"completed"`) {
		t.Errorf("gateway_trace = %q, want classification completed", traceStr.String)
	}
	if !strings.Contains(traceStr.String, `"tool_events":1`) {
		t.Errorf("gateway_trace = %q, want tool_events=1 (the tool-call item event) — trace must account tool activity honestly", traceStr.String)
	}
}

// TestSgap1762_NonToolIdleTurnStillStalls is arm B — no weakening. A turn
// whose last real event is NOT a tool call (response.created + one plain
// delta), followed by silence longer than the deadline, must still abort
// with the TurnDeadlineError / "stalled: no progress" contract text.
func TestSgap1762_NonToolIdleTurnStillStalls(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1762-plain-idle"
	mustCreateProjectINFRA012(t, db, projectName)

	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sgap1762SSEWrite(w, "response.created", `{"type":"response.created","response":{"id":"resp_1762b","status":"in_progress","output":[]}}`)
		// A plain NON-tool event — the last real event of the turn.
		sgap1762SSEWrite(w, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"thinking…"}`)
		<-release // silence forever (released at test end)
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "«redacted:sk-…»", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1762-arm-b-2026-11-26-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s, want %s — a NON-tool turn silent past the deadline must still abort",
			outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") || !strings.Contains(outcome.Error, "no progress") {
		t.Errorf("Wait() error = %q, want the 'stalled: no progress' contract text", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "250ms") {
		t.Errorf("Wait() error = %q, want the 250ms deadline that fired named in the text", outcome.Error)
	}
}

// TestSgap1762_ToolGraceBoundStillAborts is arm C — the bound is real. A
// tool-active silence of 2s (beyond the 750ms tool grace = 3 × 250ms)
// still aborts with the stalled classification: the tool-execution window
// is bounded, never unbounded. The abort must also land AFTER the bare
// deadline (the tool window actually armed) and well before the tick
// deadline (the slot was freed promptly).
func TestSgap1762_ToolGraceBoundStillAborts(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1762-tool-bound"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := httptest.NewServer(sgap1762ArmAHandler(2 * time.Second)) // silence >> 750ms grace
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "«redacted:sk-…»", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond) // tool grace = 750ms

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1762-arm-c-2026-11-26-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	elapsed := time.Since(start)

	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s (error %q), want %s — a tool-active silence beyond the tool grace bound must still abort",
			outcome.Status, outcome.Error, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") || !strings.Contains(outcome.Error, "no progress") {
		t.Errorf("Wait() error = %q, want the 'stalled: no progress' contract text", outcome.Error)
	}
	// The tool window actually armed: the abort fired at the 750ms grace,
	// not the 250ms bare deadline (elapsed < 600ms would mean the tool
	// poke never reached the watcher).
	if elapsed < 600*time.Millisecond {
		t.Errorf("turn aborted after %v, want >= 600ms — the abort must come from the 750ms tool grace, not the 250ms bare deadline", elapsed)
	}
	// The bound is real: the wedged tool did NOT hold the slot to the
	// 30s tick wall.
	if elapsed >= 5*time.Second {
		t.Errorf("turn aborted after %v, want well under the tick wall — the tool grace bound must abort, not hang", elapsed)
	}
}

// TestSgap1762_OnlyKeepalivesStillAborts is arm D — the GAP-119
// anti-liveness contract, pinned literally: a stream of ONLY keepalive
// comments for longer than the deadline (zero real events) aborts exactly
// like a byte-silent hang.
func TestSgap1762_OnlyKeepalivesStillAborts(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap1762-only-keepalive"
	mustCreateProjectINFRA012(t, db, projectName)

	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// ONLY keepalives — no response.created, no real events, forever.
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
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "«redacted:sk-…»", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(400 * time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap1762-arm-d-2026-11-26-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Fatalf("Wait() status = %s, want %s — a keepalive-only stream is no activity, deadline or not",
			outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") {
		t.Errorf("Wait() error = %q, want stalled classification", outcome.Error)
	}
}

// A result must restore the normal deadline, even when the preceding tool
// event would otherwise have armed the longer grace period.
func TestSgap1762_ResultRestoresDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sgap1762SSEWrite(w, "response.output_item.done", `{"type":"response.output_item.done","item":{"type":"function_call"}}`)
		sgap1762SSEWrite(w, "response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call_output"}}`)
		<-r.Context().Done()
	}))
	defer srv.Close()
	client := NewGatewayClient(srv.URL, "", 10*time.Second)
	start := time.Now()
	_, trace, err := client.SendResponseStream(context.Background(), "test", "", "", "", "", 300*time.Millisecond)
	var deadlineErr *TurnDeadlineError
	if !errors.As(err, &deadlineErr) {
		t.Fatalf("error = %v, want TurnDeadlineError", err)
	}
	if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
		t.Errorf("result did not restore 300ms deadline: elapsed %v", elapsed)
	}
	if trace.Events != 2 || trace.ToolEvents != 1 {
		t.Errorf("trace events=%d tool_events=%d, want 2 and 1", trace.Events, trace.ToolEvents)
	}
}

// TestSgap1762_SSEEventIsToolActivity pins the stream-level classifier
// directly: tool item types (the GAP-1655 canonical list) on
// output_item.added/done and the streamed argument events mark tool
// activity; result items, message items, deltas and unknown future shapes
// do not (under-counting is the conservative direction).
func TestSgap1762_SSEEventIsToolActivity(t *testing.T) {
	cases := []struct {
		name       string
		eventType  string
		data       string
		wantActive bool
	}{
		{"function_call_item_added", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"function_call","name":"x"}}`, true},
		{"function_call_item_done", "response.output_item.done",
			`{"type":"response.output_item.done","item":{"type":"function_call","name":"x"}}`, true},
		{"custom_tool_call_item", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"custom_tool_call"}}`, true},
		{"local_shell_call_item", "response.output_item.done",
			`{"type":"response.output_item.done","item":{"type":"local_shell_call"}}`, true},
		{"apply_patch_call_item", "response.output_item.done",
			`{"type":"response.output_item.done","item":{"type":"apply_patch_call"}}`, true},
		{"tool_use_item", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"tool_use"}}`, true},
		{"function_call_args_delta", "response.function_call_arguments.delta",
			`{"type":"response.function_call_arguments.delta","delta":"{\"cmd\""}`, true},
		{"function_call_args_done", "response.function_call_arguments.done",
			`{"type":"response.function_call_arguments.done"}`, true},
		{"custom_tool_call_input_delta", "response.custom_tool_call_input.delta",
			`{"type":"response.custom_tool_call_input.delta","delta":"..."}`, true},
		// The tool's RESULT arriving — NOT tool activity: normal semantics
		// resume (this is what clears the window in production).
		{"function_call_output_result", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"function_call_output","call_id":"c"}}`, false},
		{"message_item", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`, false},
		{"plain_delta", "response.output_text.delta",
			`{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"created", "response.created",
			`{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}`, false},
		// Unknown future shape reads as plain activity (under-count).
		{"unknown_item_type", "response.output_item.added",
			`{"type":"response.output_item.added","item":{"type":"mcp_call"}}`, false},
	}
	for _, tc := range cases {
		if got := sseEventIsToolActivity(tc.eventType, tc.data); got != tc.wantActive {
			t.Errorf("sseEventIsToolActivity(%s, %s) = %v, want %v — %s",
				tc.eventType, tc.data, got, tc.wantActive, tc.name)
		}
	}
}
