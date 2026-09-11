package scheduler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Regression tests for SCHED-GAP-102 (2026-09-10): a provider-auth-failed
// spawn (ollama-cloud "No usable credentials") returns a gateway 2xx with
// status=completed, a NON-empty resp.ID (session placeholder), empty output
// text and 0/0 token usage. The SCHED-GAP-079 gate only failed explicit
// failure statuses or empty-output-AND-empty-session, so these ticks were
// recorded completed/committed while doing zero work — poisoning success
// rates and adaptive cooldown streaks, and delivering error text to project
// threads as if it were a real tick report. The gate now classifies
// zero-tokens AND empty-text as a provider instant-death failure.

// TestSCHED_GAP_102_ZeroTokensEmptyOutputFails — a 200 response with
// status=completed, a non-empty session id, NO output text and 0/0 tokens is
// the provider-auth-death shape: the LLM was never invoked. The tick must be
// recorded failed (completed=false, gwFailErr names the instant-death), so
// Wait() yields TickFailed with the diagnosable error text.
func TestSCHED_GAP_102_ZeroTokensEmptyOutputFails(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_placeholder",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})

	project := PackedProject{Name: "gap102-zero-tokens", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap102-zero-tokens-2026-09-10-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick — the gate must produce a failed tick, not an error, for a zero-token 200")
	}

	if tick.completed {
		t.Error("tick.completed = true, want false — a zero-token empty-output response did no work and must not be recorded completed")
	}
	if !strings.Contains(tick.gwFailErr, "provider instant-death") {
		t.Errorf("tick.gwFailErr = %q, want it to contain 'provider instant-death'", tick.gwFailErr)
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (zero tokens + empty output must never be completed/committed)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "provider instant-death") {
		t.Errorf("outcome.Error = %q, want it to carry 'provider instant-death'", outcome.Error)
	}
	if outcome.Error == "" {
		t.Error("outcome.Error is empty — the error column must never be empty on a gated failure")
	}
}

// TestSCHED_GAP_102_ZeroTokensWithTextStaysCompleted — prohibition guard:
// 0/0 tokens WITH real output text stays completed. The LLM may have served
// the response from cached context without token billing. The gate is
// zero-tokens AND empty-text — never zero-tokens alone.
func TestSCHED_GAP_102_ZeroTokensWithTextStaysCompleted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_cached",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "real output from cached context"},
					},
				},
			},
			"usage": map[string]int{},
		})
	})

	project := PackedProject{Name: "gap102-zero-tokens-text", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap102-zero-tokens-text-2026-09-10-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — zero tokens WITH output text must stay completed", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_cached" {
		t.Errorf("SessionID = %q, want 'resp_cached'", tick.SessionID)
	}
}

// TestSCHED_GAP_102_NonZeroTokensEmptyOutputStaysCompleted — prohibition
// guard (SCHED-GAP-079 tool-only case): non-zero token usage with empty text
// and a real persisted session id is a tool-only tick (DuckBrain writes etc.)
// — it must stay completed. Never gate on output emptiness when the LLM was
// actually invoked and billed.
func TestSCHED_GAP_102_NonZeroTokensEmptyOutputStaysCompleted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_tool",
			"status": "completed",
			"output": []map[string]any{},
			"usage": map[string]int{
				"input_tokens":  1200,
				"output_tokens": 45,
				"total_tokens":  1245,
			},
		})
	})

	project := PackedProject{Name: "gap102-nonzero-tokens", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap102-nonzero-tokens-2026-09-10-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — non-zero tokens with a persisted session is a tool-only tick and must stay completed", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_tool" {
		t.Errorf("SessionID = %q, want 'resp_tool'", tick.SessionID)
	}
}
