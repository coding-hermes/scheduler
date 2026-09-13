package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestAPI_Status_WaveSurface (SCHED-GAP-112, S12 §9.4): a running tick with
// worker_count=3 reports wave_depth_total==3, waves[0].worker_count==3 and
// active_ticks==1 — workers NEVER enter active_ticks (W1/W3).
func TestAPI_Status_WaveSurface(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "wavy")
	if err := database.CreateNamespace(context.Background(), a.db, &database.Namespace{
		ID: "ns-wave", Weight: 10, Reserved: 1, HardCap: 100, MaxConcurrent: 4,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE projects SET namespace_id='ns-wave' WHERE name='wavy'`); err != nil {
		t.Fatalf("assign namespace: %v", err)
	}

	started := time.Now().UTC().Add(-90 * time.Second).Format(time.RFC3339)
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, wave_recovery, created_at)
		VALUES ('wavy-2026-09-13-00-00-00', 'wavy', 'running', ?, 3, 1, ?)`, started, started); err != nil {
		t.Fatalf("seed running wave tick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if got := body["active_ticks"].(float64); got != 1 {
		t.Errorf("active_ticks = %v, want 1 (a wave is ONE tick — W1/W3)", got)
	}
	if got := body["wave_depth_total"].(float64); got != 3 {
		t.Errorf("wave_depth_total = %v, want 3", got)
	}
	if got := body["wave_workers_cap_configured"].(bool); !got {
		t.Errorf("wave_workers_cap_configured = %v, want true (ns-wave caps at 3)", got)
	}
	waves, ok := body["waves"].([]interface{})
	if !ok {
		t.Fatalf("waves is %T, want array: %v", body["waves"], body["waves"])
	}
	if len(waves) != 1 {
		t.Fatalf("len(waves) = %d, want 1", len(waves))
	}
	w0 := waves[0].(map[string]interface{})
	if w0["project"] != "wavy" || w0["tick_id"] != "wavy-2026-09-13-00-00-00" {
		t.Errorf("waves[0] identity wrong: %v", w0)
	}
	if w0["namespace"] != "ns-wave" {
		t.Errorf("waves[0].namespace = %v, want ns-wave", w0["namespace"])
	}
	if got := w0["worker_count"].(float64); got != 3 {
		t.Errorf("waves[0].worker_count = %v, want 3", got)
	}
	if _, ok := w0["started_at"].(string); !ok {
		t.Errorf("waves[0].started_at missing: %v", w0)
	}
	if age := w0["age_s"].(float64); age < 80 || age > 200 {
		t.Errorf("waves[0].age_s = %v, want ~90", age)
	}
}

// TestAPI_Status_WavesEmptyIsArrayNotNull: with no running waves the decoded
// waves value must be a NON-NIL empty slice — the bumps convention (the raw
// JSON must carry [] not null).
func TestAPI_Status_WavesEmptyIsArrayNotNull(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "serial-only")

	// Serial running tick: worker_count 0 → invisible to the wave surface.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, created_at)
		VALUES ('serial-only-2026-09-13-00-00-00', 'serial-only', 'running', ?, 0, ?)`, now, now); err != nil {
		t.Fatalf("seed serial tick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	waves, ok := body["waves"].([]interface{})
	if !ok {
		t.Fatalf("waves is %T, want [] — NEVER null (bumps convention)", body["waves"])
	}
	if len(waves) != 0 {
		t.Errorf("len(waves) = %d, want 0 (serial tick invisible)", len(waves))
	}
	if got := body["wave_depth_total"].(float64); got != 0 {
		t.Errorf("wave_depth_total = %v, want 0", got)
	}
	if got := body["wave_workers_cap_configured"].(bool); got {
		t.Errorf("wave_workers_cap_configured = %v, want false (no namespace caps)", got)
	}
	// active_ticks still counts the serial tick.
	if got := body["active_ticks"].(float64); got != 1 {
		t.Errorf("active_ticks = %v, want 1", got)
	}
}

// TestAPI_Status_WavesRawJSONArrayNotNull pins the WIRE format (not just the
// decoded shape): JSON null would decode to nil; [] must appear on the wire.
func TestAPI_Status_WavesRawJSONArrayNotNull(t *testing.T) {
	a := newAPITestServer(t)
	resp, err := http.Get(a.ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wavesRaw, ok := raw["waves"]
	if !ok {
		t.Fatal("status JSON missing waves key")
	}
	if string(wavesRaw) == "null" || string(wavesRaw) == "" {
		t.Fatalf("waves on the wire = %s, want []", string(wavesRaw))
	}
	var arr []interface{}
	if err := json.Unmarshal(wavesRaw, &arr); err != nil {
		t.Fatalf("waves is not a JSON array: %s (%v)", string(wavesRaw), err)
	}
	if arr == nil {
		t.Fatal("decoded waves slice is nil — wire carried null")
	}
}

// TestAPI_TickByID_WaveFields (S12 §9.4): /api/v1/ticks/{id} carries
// worker_count, wave_recovery and the tick_workers[] rows for a wave tick.
func TestAPI_TickByID_WaveFields(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "wavy")
	now := time.Now().UTC().Format(time.RFC3339)
	tickID := "wavy-2026-09-13-01-00-00"
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, wave_recovery, created_at)
		VALUES (?, 'wavy', 'running', ?, 3, 1, ?)`, tickID, now, now); err != nil {
		t.Fatalf("seed wave tick: %v", err)
	}
	for _, w := range []database.TickWorker{
		{TickID: tickID, TaskID: "SCHED-GAP-109", Branch: "wt/SCHED-GAP-109"},
		{TickID: tickID, TaskID: "SCHED-GAP-110", Branch: "wt/SCHED-GAP-110"},
		{TickID: tickID, TaskID: "SCHED-GAP-111", Branch: "wt/SCHED-GAP-111"},
	} {
		if _, err := database.CreateTickWorker(context.Background(), a.db, &w); err != nil {
			t.Fatalf("CreateTickWorker %s: %v", w.TaskID, err)
		}
	}

	status, body := a.do(t, "GET", "/api/v1/ticks/"+tickID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if got := body["worker_count"].(float64); got != 3 {
		t.Errorf("worker_count = %v, want 3", got)
	}
	if got := body["wave_recovery"].(float64); got != 1 {
		t.Errorf("wave_recovery = %v, want 1", got)
	}
	workers, ok := body["tick_workers"].([]interface{})
	if !ok {
		t.Fatalf("tick_workers is %T, want array: %v", body["tick_workers"], body["tick_workers"])
	}
	if len(workers) != 3 {
		t.Fatalf("len(tick_workers) = %d, want 3", len(workers))
	}
	w0 := workers[0].(map[string]interface{})
	if w0["task_id"] != "SCHED-GAP-109" || w0["branch"] != "wt/SCHED-GAP-109" {
		t.Errorf("tick_workers[0] wrong: %v", w0)
	}
	// Every pre-v27 Tick field must still ride along in the wave envelope.
	for _, key := range []string{"id", "project_name", "session_id", "status", "spawned_at",
		"completed_at", "exit_code", "commits", "files_changed", "tokens_in", "tokens_out",
		"cost_usd", "urgency", "weight_used", "error", "created_at"} {
		if _, ok := body[key]; !ok {
			t.Errorf("wave tick envelope missing pre-v27 key %q: %v", key, body)
		}
	}
}

// TestAPI_TickByID_SerialShapeUnchanged (regression): a serial tick (no
// worker rows) returns the bare Tick envelope — no tick_workers key, no
// null, byte-shape identical to pre-SCHED-GAP-112 responses.
func TestAPI_TickByID_SerialShapeUnchanged(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "plain")
	now := time.Now().UTC().Format(time.RFC3339)
	tickID := "plain-2026-09-13-02-00-00"
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES (?, 'plain', 'completed', ?)`, tickID, now); err != nil {
		t.Fatalf("seed serial tick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/ticks/"+tickID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if _, ok := body["tick_workers"]; ok {
		t.Errorf("serial tick carries tick_workers — envelope must stay pre-v27: %v", body)
	}
	// worker_count / wave_recovery land as zero-valued Tick fields (v27).
	if got := body["worker_count"].(float64); got != 0 {
		t.Errorf("worker_count = %v, want 0", got)
	}
	if got := body["wave_recovery"].(float64); got != 0 {
		t.Errorf("wave_recovery = %v, want 0", got)
	}
	// The pre-v27 shape is a bare Tick — re-encode it and confirm it is
	// exactly the Tick struct serialization.
	var tick database.Tick
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b, &tick); err != nil {
		t.Fatalf("serial response does not decode as a bare Tick: %v", err)
	}
	if tick.ID != tickID || tick.ProjectName != "plain" {
		t.Errorf("tick round-trip wrong: %+v", tick)
	}
}

// TestAPI_Status_WaveDepthSumsAcrossProjects: two running wave ticks in
// different projects sum into wave_depth_total; active_ticks counts both.
func TestAPI_Status_WaveDepthSumsAcrossProjects(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	mustCreateAPITestProject(t, a.db, "beta")
	now := time.Now().UTC().Format(time.RFC3339)
	for _, seed := range []struct {
		id, project string
		wc          int
	}{
		{"alpha-2026-09-13-03-00-00", "alpha", 3},
		{"beta-2026-09-13-03-00-01", "beta", 2},
	} {
		if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, created_at)
			VALUES (?, ?, 'running', ?, ?, ?)`, seed.id, seed.project, now, seed.wc, now); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if got := body["wave_depth_total"].(float64); got != 5 {
		t.Errorf("wave_depth_total = %v, want 5 (3+2)", got)
	}
	if got := body["active_ticks"].(float64); got != 2 {
		t.Errorf("active_ticks = %v, want 2", got)
	}
	if got := len(body["waves"].([]interface{})); got != 2 {
		t.Errorf("len(waves) = %d, want 2", got)
	}
}
