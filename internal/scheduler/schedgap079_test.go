package scheduler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-079 (2026-08-28): a gateway tick whose
// response is an explicit failure (status=failed / session_persistence_failed)
// or carries neither output text nor a persisted session id
// (zero-output-with-no-session) was recorded completed/committed — a false
// success (heading-sync field test 10:06Z). The gate lives in Spawn()'s
// gateway success branch: the response must show a non-failure status AND
// (output text OR a persisted session id) to be completed. Failed responses
// return a NON-completed SpawnedTick carrying gwFailErr, so Wait() yields
// TickFailed and slot_pool's existing lifecycle.Complete path records
// status=failed / outcome=failed / error=<gateway text>.

// schedGap079Spawner builds a spawner wired to the given handler with the
// standard test client (shared daemon key, exec fallback disabled).
func schedGap079Spawner(t *testing.T, db *sql.DB, handler http.HandlerFunc) *Spawner {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)
	return spawner
}

// schedGap079HighEventCount counts HIGH spawn events whose details carry the
// given project + tick id (query pattern from gateway_key_guard_test.go:112).
func schedGap079HighEventCount(t *testing.T, db *sql.DB, project, tickID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='spawn' AND json_extract(details, '$.project') = ? AND json_extract(details, '$.tick_id') = ?`, project, tickID).Scan(&n); err != nil {
		t.Fatalf("count HIGH spawn events: %v", err)
	}
	return n
}

// schedGap079InfoEventCount counts INFO spawn events with the given message
// for the given project + tick id (tool-only completion visibility).
func schedGap079InfoEventCount(t *testing.T, db *sql.DB, project, tickID, message string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='INFO' AND component='spawn' AND message = ? AND json_extract(details, '$.project') = ? AND json_extract(details, '$.tick_id') = ?`, message, project, tickID).Scan(&n); err != nil {
		t.Fatalf("count INFO spawn events: %v", err)
	}
	return n
}

// TestSCHEDGAP079_StatusSessionPersistenceFailedNotCompleted — a 200 response
// with status=session_persistence_failed (no id, no error field) must NOT be
// recorded completed: Wait() yields TickFailed and the error column carries
// the gateway status text. The gate's HIGH event (tick id + project) fires so
// the failure is visible in the events table.
func TestSCHEDGAP079_StatusSessionPersistenceFailedNotCompleted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "session_persistence_failed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap079-session-failed", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap079-session-failed-2026-08-28-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick — the gate must produce a failed tick, not an error, for a 200 failure status")
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (session_persistence_failed must never be completed/committed)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "session_persistence_failed") {
		t.Errorf("outcome.Error = %q, want it to carry the gateway status 'session_persistence_failed'", outcome.Error)
	}
	if outcome.Error == "" {
		t.Error("outcome.Error is empty — the error column must never be empty on a gated failure")
	}
	if tick.SessionID != "gap079-session-failed-2026-08-28-10-00-00" {
		t.Errorf("SessionID = %q, want placeholder tick id (no real session persisted)", tick.SessionID)
	}
	if n := schedGap079HighEventCount(t, db, project.Name, "gap079-session-failed-2026-08-28-10-00-00"); n != 1 {
		t.Errorf("HIGH spawn events = %d, want 1 — the failure must be immediately visible with tick id + project", n)
	}
}

// TestSCHEDGAP079_StatusFailedNotCompleted — a 200 response with
// status=failed records TickFailed with the status in the error column.
func TestSCHEDGAP079_StatusFailedNotCompleted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "failed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})

	project := PackedProject{Name: "gap079-status-failed", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap079-status-failed-2026-08-28-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "gateway response failed") {
		t.Errorf("outcome.Error = %q, want it to carry 'gateway response failed'", outcome.Error)
	}
}

// TestSCHEDGAP079_EmptyOutputNoSessionFails — a 200 response with
// status=completed but NO output text AND NO session id is a
// zero-output-with-no-session failure (the false-success field-test shape):
// the gateway accepted the request but no session persisted. The error text
// must name the missing session so the row is diagnosable.
func TestSCHEDGAP079_EmptyOutputNoSessionFails(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})

	project := PackedProject{Name: "gap079-empty-no-session", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap079-empty-no-session-2026-08-28-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (empty output AND empty session must fail — the false-success shape)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "session") {
		t.Errorf("outcome.Error = %q, want it to mention the missing session", outcome.Error)
	}
}

// TestSCHEDGAP079_Normal200StillCommitted — a normal 200 with real output
// and a session id stays completed/committed. No regression.
func TestSCHEDGAP079_Normal200StillCommitted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_ok",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "real output"},
					},
				},
			},
			"usage": map[string]int{},
		})
	})

	project := PackedProject{Name: "gap079-normal", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap079-normal-2026-08-28-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a normal 200 with real output must stay committed", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_ok" {
		t.Errorf("SessionID = %q, want real gateway response id 'resp_ok'", tick.SessionID)
	}
}

// TestSCHEDGAP079_ToolOnlyEmptyOutputKeptCompleted — prohibition guard: a
// successful tick that only made tool calls (DuckBrain writes etc.) has empty
// text but a REAL persisted session id — it must stay completed. Never gate
// on output length alone. The INFO event makes the tool-only case visible
// without failing it.
func TestSCHEDGAP079_ToolOnlyEmptyOutputKeptCompleted(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_tool",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap079-tool-only", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap079-tool-only-2026-08-28-10-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a tool-only tick with a persisted session must stay completed", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_tool" {
		t.Errorf("SessionID = %q, want 'resp_tool'", tick.SessionID)
	}
	if n := schedGap079InfoEventCount(t, db, project.Name, "gap079-tool-only-2026-08-28-10-00-00", "gateway tick completed with no text output (tool-only)"); n != 1 {
		t.Errorf("INFO tool-only events = %d, want 1", n)
	}
}

// TestSCHEDGAP079_CorruptionSurfacedHighEvent — state.db corruption
// ('database disk image is malformed') on the gateway surfaces BOTH in the
// tick error (the gateway wraps it in the response error envelope, which the
// client classifies as a gateway error — slot_pool persists that text into
// ticks.error via lifecycle.Complete) AND as a HIGH event carrying tick id +
// project. The event must fire even when the error arrives through the
// transport path (gateway_client.go is read-only — the top-level error
// envelope is intercepted before Spawn()'s completion gate).
func TestSCHEDGAP079_CorruptionSurfacedHighEvent(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "session_persistence_failed",
			"error": map[string]string{
				"type":    "session_error",
				"message": "database disk image is malformed",
			},
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	})
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap079-corruption", Workdir: t.TempDir()}
	const tickID = "gap079-corruption-2026-08-28-10-00-00"
	_, err := spawner.Spawn(project, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error for a corrupted-session response")
	}
	// This exact text is what slot_pool persists into ticks.error via
	// lifecycle.Complete (status=failed, outcome=failed).
	if !strings.Contains(err.Error(), "database disk image is malformed") {
		t.Errorf("error = %q, want it to carry 'database disk image is malformed' verbatim", err.Error())
	}
	if n := schedGap079HighEventCount(t, db, project.Name, tickID); n != 1 {
		t.Errorf("HIGH spawn events = %d, want 1 — corruption must be visible in the events table with tick id + project", n)
	}
}

// TestSCHEDGAP079_RejectedIdleTickShows400Text — an idle-chain tick rejected
// with HTTP 400 (reasoning_effort=max not supported by Harmony for
// hf:openai/gpt-oss-120b) fails the spawn with the 400 text intact. slot_pool
// persists this exact text into ticks.error via lifecycle.Complete — a
// rejected idle tick must never be recorded completed.
func TestSCHEDGAP079_RejectedIdleTickShows400Text(t *testing.T) {
	db := newTestDB(t)

	spawner := schedGap079Spawner(t, db, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "invalid_request",
				"message": "reasoning_effort=max is not supported by Harmony",
			},
		})
	})

	project := PackedProject{Name: "gap079-rejected-idle", Workdir: t.TempDir()}
	_, err := spawner.Spawn(project, "gap079-rejected-idle-2026-08-28-10-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error on HTTP 400 — the rejection was silent")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %q, want it to carry the HTTP 400 status", err.Error())
	}
	if !strings.Contains(err.Error(), "reasoning_effort=max is not supported by Harmony") {
		t.Errorf("error = %q, want the gateway's rejection text 'reasoning_effort=max is not supported by Harmony'", err.Error())
	}
}
