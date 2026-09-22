package scheduler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Regression tests for SCHED-GAP-205 (2026-09-22): a gateway 2xx with
// status=completed, non-zero output-token usage and NO assistant output item
// recorded a completed/committed tick — a provider billed for tokens and
// never produced a real assistant message (73 ticks across 31 projects
// since 2026-09-21T18:00). The completion gate now classifies
// completed + !HasAssistantMessage + OutputTokens>0 as FAILED (the
// OutputTokens>0 guard keeps the new 205 branch a strict superset of 102's
// instant-death: 0/0 stays 102's domain, 205 catches the billed-but-empty
// shape). The tool-only invariant survives because a tool-only completion
// carries its tool calls INSIDE an assistant output item.

// TestSCHEDGAP205_NonZeroTokenZeroAssistantFails — the actual 205 shape:
// status=completed, usage 10/0, output=[] → classified FAILED, not a
// phantom committed tick.
func TestSCHEDGAP205_NonZeroTokenZeroAssistantFails(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_205_billed_empty",
			"status": "completed",
			"output": []map[string]any{},
			"usage": map[string]int{
				"input_tokens":  100,
				"output_tokens": 10,
				"total_tokens":  110,
			},
		})
	})

	project := PackedProject{Name: "gap205-billed-empty", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap205-billed-empty-2026-09-22-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick — the gate must produce a failed tick, not an error, for the billed-but-empty shape")
	}

	if tick.completed {
		t.Error("tick.completed = true, want false — billed tokens with zero assistant output must not be recorded completed")
	}
	if !strings.Contains(tick.gwFailErr, "zero assistant output with non-zero token usage") {
		t.Errorf("tick.gwFailErr = %q, want it to contain the 205 sentinel 'zero assistant output with non-zero token usage'", tick.gwFailErr)
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (completed + non-zero tokens + no assistant message must fail)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "zero assistant output with non-zero token usage") {
		t.Errorf("outcome.Error = %q, want it to carry the 205 sentinel", outcome.Error)
	}
	if outcome.Error == "" {
		t.Error("outcome.Error is empty — the error column must never be empty on a gated failure")
	}
}

// TestSCHEDGAP205_ToolOnlyStillCompletes — regression guard for the 079
// tool-only invariant: usage 10/10, status=completed, output=[assistant
// message with output_text] stays completed. Real successful completions
// carry role="assistant" — the new gate never displaces them.
func TestSCHEDGAP205_ToolOnlyStillCompletes(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_205_tool",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "x"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  100,
				"output_tokens": 10,
				"total_tokens":  110,
			},
		})
	})

	project := PackedProject{Name: "gap205-tool", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap205-tool-2026-09-22-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — an assistant message with output_text stays completed under the 205 gate", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_205_tool" {
		t.Errorf("SessionID = %q, want 'resp_205_tool'", tick.SessionID)
	}
}

// TestSCHEDGAP205_ZeroTokenStillInstantDeath — prohibition guard for 102:
// usage 0/0, status=completed, output=[] still fails under 102's
// instant-death rule (the 0/0 arm must not be displaced by the new 205 arm,
// which is gated on OutputTokens>0).
func TestSCHEDGAP205_ZeroTokenStillInstantDeath(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_205_zer0",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})

	project := PackedProject{Name: "gap205-zero-tokens", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap205-zero-tokens-2026-09-22-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick — 0/0 stays a failed tick")
	}

	if tick.completed {
		t.Error("tick.completed = true, want false — 0/0 remains 102's instant-death domain")
	}
	if !strings.Contains(tick.gwFailErr, "provider instant-death") {
		t.Errorf("tick.gwFailErr = %q, want the 102 sentinel 'provider instant-death' (the 0/0 arm must not be displaced by 205)", tick.gwFailErr)
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (0/0 stays instant-death)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "provider instant-death") {
		t.Errorf("outcome.Error = %q, want it to carry 'provider instant-death'", outcome.Error)
	}
}
