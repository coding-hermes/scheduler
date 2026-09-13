package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSyncFleetSummary_WaveFields (SCHED-GAP-112, S12 §9.5): the fleet
// snapshot gains ActiveWaves / WaveDepthTotal / WaveWorkers — a running
// 3-worker tick yields ActiveWaves=1, WaveDepthTotal=3 and one compact
// entry; ActiveTicks stays 1 (workers never inflate it — W1/W3).
func TestSyncFleetSummary_WaveFields(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "wavy", "r1", "w1", 1)
	if err := database.CreateNamespace(context.Background(), db, &database.Namespace{
		ID: "ns-wave", Weight: 10, Reserved: 1, HardCap: 100, MaxConcurrent: 4,
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET namespace_id='ns-wave' WHERE name='wavy'`); err != nil {
		t.Fatalf("assign namespace: %v", err)
	}
	started := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, worker_count, wave_recovery, created_at)
		VALUES ('wavy-2026-09-13-00-00-00', 'wavy', 'running', ?, 3, 1, ?)`, started, started); err != nil {
		t.Fatalf("insert wave tick: %v", err)
	}

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.ActiveTicks != 1 {
		t.Errorf("ActiveTicks = %d, want 1", received.ActiveTicks)
	}
	if received.ActiveWaves != 1 {
		t.Errorf("ActiveWaves = %d, want 1", received.ActiveWaves)
	}
	if received.WaveDepthTotal != 3 {
		t.Errorf("WaveDepthTotal = %d, want 3", received.WaveDepthTotal)
	}
	if received.WaveWorkers == nil {
		t.Fatal("WaveWorkers is nil — must be non-nil [] (never null)")
	}
	if len(received.WaveWorkers) != 1 {
		t.Fatalf("len(WaveWorkers) = %d, want 1", len(received.WaveWorkers))
	}
	w := received.WaveWorkers[0]
	if w.Project != "wavy" || w.TickID != "wavy-2026-09-13-00-00-00" {
		t.Errorf("WaveWorkers[0] identity wrong: %+v", w)
	}
	if w.Namespace != "ns-wave" {
		t.Errorf("WaveWorkers[0].Namespace = %q, want ns-wave", w.Namespace)
	}
	if w.WorkerCount != 3 {
		t.Errorf("WaveWorkers[0].WorkerCount = %d, want 3", w.WorkerCount)
	}
}

// TestSyncFleetSummary_WaveFieldsEmpty: with no waves the snapshot carries
// zero values and a NON-NIL empty WaveWorkers slice.
func TestSyncFleetSummary_WaveFieldsEmpty(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	// Serial running tick: worker_count 0 → not a wave.
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('t1', 'p1', 'running', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.ActiveWaves != 0 || received.WaveDepthTotal != 0 {
		t.Errorf("ActiveWaves=%d WaveDepthTotal=%d, want 0/0", received.ActiveWaves, received.WaveDepthTotal)
	}
	if received.WaveWorkers == nil {
		t.Fatal("WaveWorkers is nil — must be non-nil [] (never null)")
	}
	if len(received.WaveWorkers) != 0 {
		t.Errorf("len(WaveWorkers) = %d, want 0", len(received.WaveWorkers))
	}
	// ActiveTicks still counts the serial running tick.
	if received.ActiveTicks != 1 {
		t.Errorf("ActiveTicks = %d, want 1", received.ActiveTicks)
	}
}
