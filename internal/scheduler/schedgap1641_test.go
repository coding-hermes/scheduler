package scheduler

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-1641 regression tests (2026-09-25 evidence: project-reviews/
// artifacts/instant-tick-burst-2026-09-25.md). A tick whose gateway turn was
// a ONE-SHOT — a small handful of SSE events and a few hundred output tokens
// against a huge prompt — recorded outcome=committed with a clean deliver
// while the gateway was answering every spawn instantly with no real work.
// The burst: 241 instant ticks fleet-wide in one hour (tokens_in identical at
// 44,469, tokens_out 62-181), with NO state.db sessions to contradict them —
// healthy-tick accounting had zero signal anything was wrong.
//
// The classification keys on the persisted tick trace fingerprint:
//
//	trace usage fields present AND events <= instantTurnMaxEvents AND
//	tokens_out <= instantTurnMaxTokensOut
//
// and routes the tick through the EXISTING SCHED-GAP-079 failed-tick
// machinery: status=failed / outcome=failed / failure_reason=instant_turn.
// Fail-open (stays completed) when the trace carries NO token fields —
// legacy traces keep accounting exactly as they always have.

// sse1641Handler streams a scripted SSE turn with the given event count
// (deltas + created + completed) and usage, mirroring sseTurnHandler.
func sse1641Handler(events, usageIn, usageOut int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Hermes-Session-Id", "sess-1641")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		write := func(event, data string) {
			_, _ = w.Write([]byte("event: " + event + "\ndata: " + data + "\n\n"))
			flusher.Flush()
		}
		write("response.created", `{"type":"response.created","response":{"id":"resp_1641","status":"in_progress","output":[]}}`)
		for i := 1; i <= events-2; i++ {
			write("response.output_text.delta", `{"type":"response.output_text.delta","delta":"chunk"}`)
		}
		write("response.completed", fmt1641Envelope(usageIn, usageOut))
	}
}

func fmt1641Envelope(usageIn, usageOut int) string {
	body := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_1641",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "done"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  usageIn,
				"output_tokens": usageOut,
				"total_tokens":  usageIn + usageOut,
			},
		},
	}
	blob, _ := json.Marshal(body)
	return string(blob)
}

// schedGap1641Loop builds a Loop whose gateway serves one SSE turn with the
// given event count and usage. The slot pool is the only path that Enqueues
// the ticks row, so the full stack (spawn → Wait → lifecycle.Complete) is
// exercised and the persisted outcome / failure_reason / gateway_trace
// columns are directly assertable.
func schedGap1641Loop(t *testing.T, events int, usageIn, usageOut int) *Loop {
	t.Helper()
	db := newTestDB(t)
	srv := httptest.NewServer(sse1641Handler(events, usageIn, usageOut))
	t.Cleanup(srv.Close)
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(20 * time.Second) // knob < tick deadline → supervised SSE path
	t.Cleanup(func() { _ = db.Close() })
	return loop
}

// TestSCHEDGAP1641_InstantTurnNotCommitted — AC 1 + 2, table-driven,
// full stack through the slot pool + lifecycle.Complete. The instant arm
// constructs the burst shape (events=6 / tokens_out=114) and asserts the
// tick is accounted NON-healthy end to end: ticks.status=failed,
// outcome=failed, error carries the instant-turn sentinel, and
// failure_reason=instant_turn. The productive arm (events=322 /
// tokens_out=14581) must stay completed/committed.
func TestSCHEDGAP1641_InstantTurnNotCommitted(t *testing.T) {
	tests := []struct {
		name     string
		events   int
		usageIn  int
		usageOut int
		wantFail bool
	}{
		{
			name:     "instant one-shot burst shape (2026-09-25 evidence)",
			events:   6,
			usageIn:  44469,
			usageOut: 114,
			wantFail: true,
		},
		{
			name:     "normal productive tick stays healthy",
			events:   322,
			usageIn:  52000,
			usageOut: 14581,
			wantFail: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			loop := schedGap1641Loop(t, tc.events, tc.usageIn, tc.usageOut)
			projectName := "gap1641-" + strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(tc.name)
			mustCreateProjectINFRA012(t, loop.db, projectName)

			tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, loop.db)
			status, ok := waitForTickTerminal(t, loop.db, tickID, 10*time.Second)
			if !ok {
				t.Fatalf("tick %s never reached a terminal state within 10s", tickID)
			}

			var outcome, errText, failureReason string
			if err := loop.db.QueryRow(`SELECT outcome, COALESCE(error,''), failure_reason FROM ticks WHERE id = ?`, tickID).Scan(&outcome, &errText, &failureReason); err != nil {
				t.Fatalf("query tick row: %v", err)
			}

			if tc.wantFail {
				if status != string(TickFailed) {
					t.Errorf("ticks.status = %q, want %q — an instant one-shot turn must not account healthy (events=%d tokens_out=%d)",
						status, TickFailed, tc.events, tc.usageOut)
				}
				if outcome != "failed" {
					t.Errorf("ticks.outcome = %q, want failed (pre-1641 this row read committed)", outcome)
				}
				if !strings.Contains(errText, GatewayInstantTurnSentinel) {
					t.Errorf("ticks.error = %q, want the instant-turn sentinel %q", errText, GatewayInstantTurnSentinel)
				}
				if failureReason != FailureReasonInstantTurn {
					t.Errorf("failure_reason = %q, want %q (machine-auditable instant-turn marker)", failureReason, FailureReasonInstantTurn)
				}
			} else {
				if status != string(TickCompleted) {
					t.Fatalf("ticks.status = %q, want %q — a productive tick (events=%d tokens_out=%d) must stay completed",
						status, TickCompleted, tc.events, tc.usageOut)
				}
				// SCHED-GAP-1652: the outcome column no longer restates health —
				// it reports an OBSERVED ARTIFACT. This fixture's workdir is not a
				// git repo, so the tick can produce no commit and no changed file,
				// and asserting "committed" here would be the exact false claim
				// 1652 removed (2,533 rows read outcome='committed' with commits=0).
				// What SCHED-GAP-1641 actually guarantees is that a productive tick
				// is NOT the instant-turn FAILURE — carried by status and
				// failure_reason, asserted above and below — not by this column.
				if outcome == "failed" {
					t.Errorf("ticks.outcome = %q — a productive tick must never read as the instant-turn failure", outcome)
				}
				if failureReason == FailureReasonInstantTurn {
					t.Errorf("productive tick stamped failure_reason=%q — the classifier fired outside the band", FailureReasonInstantTurn)
				}
			}

			// AC "keep the raw trace intact": the persisted gateway_trace
			// still carries the observable fingerprint — events + usage —
			// regardless of the verdict.
			var traceStr string
			if err := loop.db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
				t.Fatalf("query gateway_trace: %v", err)
			}
			for _, fragment := range []string{`"events":`, `"tokens_in":`, `"tokens_out":`} {
				if !strings.Contains(traceStr, fragment) {
					t.Errorf("gateway_trace missing %s (trace=%s)", fragment, traceStr)
				}
			}
		})
	}
}

// TestSCHEDGAP1641_WarnEmitted — AC 5: the WARN line fires for the instant
// shape (captured via the package-standard log redirect) and does NOT fire
// for the productive shape.
func TestSCHEDGAP1641_WarnEmitted(t *testing.T) {
	capture := func(events, usageIn, usageOut int) string {
		buf := &admitLogBuf{}
		prev := log.Writer()
		log.SetOutput(buf)
		defer log.SetOutput(prev)

		loop := schedGap1641Loop(t, events, usageIn, usageOut)
		const projectName = "gap1641-warn"
		mustCreateProjectINFRA012(t, loop.db, projectName)
		tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, loop.db)
		if _, ok := waitForTickTerminal(t, loop.db, tickID, 10*time.Second); !ok {
			t.Fatalf("tick %s never reached a terminal state within 10s", tickID)
		}
		return buf.b.String()
	}

	instant := capture(6, 44469, 114)
	if !strings.Contains(instant, "WARN") || !strings.Contains(instant, "instant") {
		t.Errorf("instant-turn log capture missing the WARN instant line:\n%s", instant)
	}

	productive := capture(322, 52000, 14581)
	if strings.Contains(productive, "instant") {
		t.Errorf("productive tick must not emit the instant-turn WARN:\n%s", productive)
	}
}

// TestSCHEDGAP1641_MissingTraceFieldsStayHealthy — the fail-open clause:
// a completion whose trace carries NO token fields (the legacy JSON POST
// path leaves the minimal trace's usage unset) must keep accounting
// healthy, even at burst-shaped usage numbers.
func TestSCHEDGAP1641_MissingTraceFieldsStayHealthy(t *testing.T) {
	db := newTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1641_legacy","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":44469,"output_tokens":114,"total_tokens":44583}}`))
	}))
	t.Cleanup(srv.Close)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 30*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{Name: "gap1641-legacy-trace", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap1641-legacy-2026-09-25-14-01-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a trace without token fields must stay healthy (fail-open)",
			outcome.Status, TickCompleted)
	}
	if got := failureReasonClass(outcome.Error); got == FailureReasonInstantTurn {
		t.Errorf("legacy trace classified instant_turn — the sentinel must only fire on PRESENT fields")
	}
}

// ── SCHED-GAP-1641 review follow-up (2026-11-24): the FALLBACK hop ─────────
//
// The instant-one-shot gate was wired only around the PRIMARY attempt's
// completion. When the gateway rejected the primary pair (HTTP 401
// ErrGatewayKeyRejected, the TASK-ROUTER-002 pair-rejection message) and the
// FALLBACK hop answered, the merged trace carried the fingerprint
// (classification=completed, events=3, attempts=2) but the classifier still
// failed open: mergePostTrace folds attempts/events/classification from every
// attempt yet never folds the usage fields, so the first (failed, usage-less)
// attempt's zeros survived and the "zero means absent" fail-open clause
// swallowed the gate. A one-shot fallback turn booked a healthy COMPLETED
// tick (tokens_in=3 / tokens_out=5, no instant-turn WARN) — the exact burst
// shape 1641 was written to stop, one hop over.

// sse1641FallbackGateway serves the reviewer's probe shape: every dispatch
// for reject-model is answered with a 401 PAIR rejection ("provider rejected"
// — deliberately NOT the terminal "Invalid gateway API key" message, so the
// spawn advances to the fallback hop), and the fallback pair gets the
// instant-fingerprint SSE turn (3 events, tokens_in=3, tokens_out=5). Every
// dispatch is recorded for the census assertions.
type sse1641FallbackGateway struct {
	mu         sync.Mutex
	dispatches []dispatchRecord
}

func (g *sse1641FallbackGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		g.dispatches = append(g.dispatches, dispatchRecord{model: req.Model, provider: req.Provider})
		g.mu.Unlock()
		if req.Model == "reject-model" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"type": "auth_error", "message": "provider rejected"},
			})
			return
		}
		sse1641Handler(3, 3, 5)(w, r)
	}
}

func (g *sse1641FallbackGateway) snapshot() []dispatchRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]dispatchRecord(nil), g.dispatches...)
}

// TestSCHEDGAP1641_FallbackHopInstantTurnFailed — the fallback hop's
// completion must route through the SAME instant-one-shot gate as the
// primary path: primary 401-rejected, fallback answers the one-shot
// fingerprint, and the tick must book failed with failure_reason=instant_turn
// and the same WARN — never a healthy completed tick. Full stack through the
// slot pool + lifecycle.Complete, mirroring TestSCHEDGAP1641_InstantTurnNotCommitted.
func TestSCHEDGAP1641_FallbackHopInstantTurnFailed(t *testing.T) {
	gw := &sse1641FallbackGateway{}
	db := newTestDB(t)
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "test-key", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(20 * time.Second) // knob < tick deadline → supervised SSE path
	t.Cleanup(func() { _ = db.Close() })

	// The WARN is part of the classification contract (the operator signal
	// that caught the 2026-09-25 burst) — capture it like AC 5 does.
	buf := &admitLogBuf{}
	prev := log.Writer()
	log.SetOutput(buf)
	defer log.SetOutput(prev)

	const projectName = "gap1641-fallback-instant"
	mustCreateProjectINFRA012(t, loop.db, projectName)
	project := PackedProject{
		Name:             projectName,
		Workdir:          t.TempDir(),
		Model:            "reject-model",
		Provider:         "reject-provider",
		FallbackModel:    "fallback-model",
		FallbackProvider: "fallback-provider",
	}
	tickID := loop.slotPool.Spawn(project, time.Now(), true, loop.db)
	status, ok := waitForTickTerminal(t, loop.db, tickID, 10*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state within 10s", tickID)
	}

	// Premise: the instant fingerprint is the FALLBACK hop's, not the
	// primary's — exactly two dispatches, primary rejected then fallback 200.
	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (primary 401 + fallback 200)", len(dispatches))
	}
	if dispatches[0].model != "reject-model" || dispatches[0].provider != "reject-provider" {
		t.Errorf("dispatch 1 = (%q, %q), want the rejected primary (reject-model, reject-provider)",
			dispatches[0].model, dispatches[0].provider)
	}
	if dispatches[1].model != "fallback-model" || dispatches[1].provider != "fallback-provider" {
		t.Errorf("dispatch 2 = (%q, %q), want the fallback hop (fallback-model, fallback-provider)",
			dispatches[1].model, dispatches[1].provider)
	}

	var outcome, errText, failureReason string
	if err := loop.db.QueryRow(`SELECT outcome, COALESCE(error,''), failure_reason FROM ticks WHERE id = ?`, tickID).Scan(&outcome, &errText, &failureReason); err != nil {
		t.Fatalf("query tick row: %v", err)
	}

	if status != string(TickFailed) {
		t.Errorf("ticks.status = %q, want %q — a one-shot FALLBACK turn must not account healthy (pre-fix this row read completed)", status, TickFailed)
	}
	if outcome != "failed" {
		t.Errorf("ticks.outcome = %q, want failed (pre-fix this row read completed)", outcome)
	}
	if !strings.Contains(errText, GatewayInstantTurnSentinel) {
		t.Errorf("ticks.error = %q, want the instant-turn sentinel %q", errText, GatewayInstantTurnSentinel)
	}
	if failureReason != FailureReasonInstantTurn {
		t.Errorf("failure_reason = %q, want %q (same classification as the primary-path gate)", failureReason, FailureReasonInstantTurn)
	}
	if !strings.Contains(buf.b.String(), "WARN: GATEWAY INSTANT TURN") {
		t.Errorf("log capture missing the WARN: GATEWAY INSTANT TURN line:\n%s", buf.b.String())
	}

	// The fix's mechanism, pinned: the merged trace now carries the FINAL
	// attempt's usage (3 in / 5 out), so the classifier sees the fingerprint
	// instead of the failed first attempt's zeros.
	var traceStr string
	if err := loop.db.QueryRow(`SELECT gateway_trace FROM ticks WHERE id = ?`, tickID).Scan(&traceStr); err != nil {
		t.Fatalf("query gateway_trace: %v", err)
	}
	if !strings.Contains(traceStr, `"tokens_in":3`) || !strings.Contains(traceStr, `"tokens_out":5`) {
		t.Errorf("gateway_trace = %s, want the fallback attempt's usage folded in (tokens_in=3, tokens_out=5)", traceStr)
	}
}
