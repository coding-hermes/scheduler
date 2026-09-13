package sync

// ── SCHED-GAP-115 (S12 §11): WaveCostTotal in the DuckBrain snapshot ────
//
// The fleet summary gains the attributed wave-cost total next to 112's
// WaveDepthTotal. Read-replica only (W4: attribution figures, never
// additive).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSyncFleetSummary_WaveCostTotal: a running wave with attributed
// worker rows reports WaveCostTotal == their sum; a serial tick's cost
// never enters it.
func TestSyncFleetSummary_WaveCostTotal(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "wavy", "r1", "w1", 1)
	insertProject(t, db, "plain", "r2", "w2", 1)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, created_at)
		VALUES ('wavy-2026-09-13-07-00-00', 'wavy', 'running', ?, 2, ?)`, now, now); err != nil {
		t.Fatalf("insert wave tick: %v", err)
	}
	for _, w := range []database.TickWorker{
		{TickID: "wavy-2026-09-13-07-00-00", TaskID: "T-1", Branch: "wt/T-1", CostUSD: 0.15},
		{TickID: "wavy-2026-09-13-07-00-00", TaskID: "T-2", Branch: "wt/T-2", CostUSD: 0.25},
	} {
		if _, err := database.CreateTickWorker(context.Background(), db, &w); err != nil {
			t.Fatalf("CreateTickWorker: %v", err)
		}
	}
	// Serial running tick with real cost — must not leak into WaveCostTotal.
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, cost_usd, created_at)
		VALUES ('plain-2026-09-13-07-00-00', 'plain', 'running', ?, 0, 5.00, ?)`, now, now); err != nil {
		t.Fatalf("insert serial tick: %v", err)
	}

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	if err := s.syncFleetSummary(context.Background()); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.WaveDepthTotal != 2 {
		t.Errorf("WaveDepthTotal = %d, want 2", received.WaveDepthTotal)
	}
	if received.WaveCostTotal < 0.40-1e-9 || received.WaveCostTotal > 0.40+1e-9 {
		t.Errorf("WaveCostTotal = %v, want 0.40 (0.15+0.25 only — serial cost never enters)", received.WaveCostTotal)
	}
	if received.ActiveTicks != 2 {
		t.Errorf("ActiveTicks = %d, want 2", received.ActiveTicks)
	}
}

// TestSyncFleetSummary_WaveCostTotalEmpty: no waves → 0.
func TestSyncFleetSummary_WaveCostTotalEmpty(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('t1', 'p1', 'running', ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	if err := s.syncFleetSummary(context.Background()); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}
	if received.WaveCostTotal != 0 {
		t.Errorf("WaveCostTotal = %v, want 0", received.WaveCostTotal)
	}
}
