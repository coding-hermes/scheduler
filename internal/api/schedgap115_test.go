package api_test

// ── SCHED-GAP-115 (S12 §11): wave cost attribution on the status surface ──
//
// wave_cost_total (sum of tick_workers.cost_usd over running waves) rides
// next to 112's wave_depth_total; per-wave entries carry their attributed
// cost. Serial fleets read 0. Read-replica only.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestAPI_Status_WaveCostSurface: one running 2-worker wave with attributed
// rows reports wave_cost_total == their sum and waves[0].cost == the same;
// a second wave adds in. active_ticks stays 2 (cost never inflates ticks).
func TestAPI_Status_WaveCostSurface(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "wavy")
	mustCreateAPITestProject(t, a.db, "plain")
	now := time.Now().UTC().Format(time.RFC3339)

	seed := func(tickID, project string, workers []database.TickWorker) {
		t.Helper()
		if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, created_at)
			VALUES (?, ?, 'running', ?, ?, ?)`, tickID, project, now, len(workers), now); err != nil {
			t.Fatalf("seed tick %s: %v", tickID, err)
		}
		for _, w := range workers {
			w.TickID = tickID
			if _, err := database.CreateTickWorker(context.Background(), a.db, &w); err != nil {
				t.Fatalf("CreateTickWorker %s: %v", w.TaskID, err)
			}
		}
	}
	seed("wavy-2026-09-13-05-00-00", "wavy", []database.TickWorker{
		{TaskID: "T-1", Branch: "wt/T-1", CostUSD: 0.11},
		{TaskID: "T-2", Branch: "wt/T-2", CostUSD: 0.22},
	})
	seed("wavy-2026-09-13-05-30-00", "wavy", []database.TickWorker{
		{TaskID: "T-3", Branch: "wt/T-3"}, // unattributed → 0
	})
	// Serial running tick: never enters the wave-cost surface.
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, cost_usd, created_at)
		VALUES ('plain-2026-09-13-05-00-00', 'plain', 'running', ?, 0, 9.99, ?)`, now, now); err != nil {
		t.Fatalf("seed serial tick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if got := body["wave_cost_total"].(float64); got < 0.33-1e-9 || got > 0.33+1e-9 {
		t.Errorf("wave_cost_total = %v, want 0.33 (0.11+0.22+0)", got)
	}
	waves, ok := body["waves"].([]interface{})
	if !ok || len(waves) != 2 {
		t.Fatalf("waves = %v, want 2 running waves", body["waves"])
	}
	w0 := waves[0].(map[string]interface{})
	if got := w0["cost"].(float64); got < 0.33-1e-9 || got > 0.33+1e-9 {
		t.Errorf("waves[0].cost = %v, want 0.33", got)
	}
	w1 := waves[1].(map[string]interface{})
	if got := w1["cost"].(float64); got != 0 {
		t.Errorf("waves[1].cost = %v, want 0 (unattributed)", got)
	}
	// The serial tick's $9.99 must NOT appear anywhere in the wave surface.
	if got := body["wave_cost_total"].(float64); got > 1.0 {
		t.Errorf("wave_cost_total = %v — serial tick cost leaked into the wave surface", got)
	}
	if got := body["active_ticks"].(float64); got != 3 {
		t.Errorf("active_ticks = %v, want 3 (2 waves + 1 serial)", got)
	}
}

// TestAPI_Status_WaveCostSerialZero: a serial-only fleet reads
// wave_cost_total 0 — the surface is additive-optional observability.
func TestAPI_Status_WaveCostSerialZero(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "serial-only")
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, cost_usd, created_at)
		VALUES ('serial-only-2026-09-13-06-00-00', 'serial-only', 'running', ?, 0, 0.42, ?)`, now, now); err != nil {
		t.Fatalf("seed serial tick: %v", err)
	}
	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := body["wave_cost_total"].(float64); got != 0 {
		t.Errorf("wave_cost_total = %v, want 0 (serial fleet)", got)
	}
}
