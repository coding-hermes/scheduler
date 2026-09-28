package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// featureUsageIsolated resets the database package's in-memory counters
// before and after a test (package-level global shared across this binary).
func featureUsageIsolated(t *testing.T) {
	t.Helper()
	database.ResetFeatureUsage()
	t.Cleanup(database.ResetFeatureUsage)
}

// featureUsageCounts flushes pending counters into db and returns the
// persisted use_count keyed by feature.
func featureUsageCounts(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	if err := database.FlushFeatureUsage(context.Background(), db); err != nil {
		t.Fatalf("flush feature usage: %v", err)
	}
	rows, err := database.LoadFeatureUsage(context.Background(), db)
	if err != nil {
		t.Fatalf("load feature usage: %v", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Feature] = r.UseCount
	}
	return out
}

// TestFeatureDedupeSuppression proves the SCHED-GAP-103 dedup fire point: a
// second spawn attempt for an already-reserved project records a suppression.
func TestFeatureDedupeSuppression(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	sp := NewSpawner(db, 4)
	p := NewSlotPool(1, sp, NewLifecycleTracker(db))
	proj := PackedProject{Name: "dedup-x"}

	// Reserve the name exactly like the first spawn attempt does, then drive
	// the shared spawn body: its tryReserve fails and the dedup is recorded.
	if !p.tryReserve(proj.Name) {
		t.Fatal("first reserve should succeed")
	}
	p.spawn(proj, "tick-dedup", clock.Real().Now(), false, db, false)

	counts := featureUsageCounts(t, db)
	if counts[database.FeatureDedupeSuppress] != 1 {
		t.Errorf("dedupe_suppressions count = %d, want 1 (%v)", counts[database.FeatureDedupeSuppress], counts)
	}
}

// TestFeatureWaveTickDispatch proves the wave-tick fire point: dispatching a
// tick into a wave-enabled namespace records a wave_ticks use.
func TestFeatureWaveTickDispatch(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, max_concurrent, wave_enabled) VALUES ('wave-ns', 10, 0, 1)`); err != nil {
		t.Fatalf("insert wave namespace: %v", err)
	}
	sp := NewSpawner(db, 4)
	sp.effectiveTickTimeout(PackedProject{NamespaceID: "wave-ns"})

	counts := featureUsageCounts(t, db)
	if counts[database.FeatureWaveTicks] != 1 {
		t.Errorf("wave_ticks count = %d, want 1 (%v)", counts[database.FeatureWaveTicks], counts)
	}
}

// TestFeatureLoadGateDeferral proves the load-gate fire point: the single
// emitLoadGateDeferred funnel (shared by all three deferral sites) records a
// load_gate_deferrals use.
func TestFeatureLoadGateDeferral(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.emitLoadGateDeferred("loadgate-p", "ns1", "")

	counts := featureUsageCounts(t, db)
	if counts[database.FeatureLoadGateDeferrals] != 1 {
		t.Errorf("load_gate_deferrals count = %d, want 1 (%v)", counts[database.FeatureLoadGateDeferrals], counts)
	}
}

// TestFeatureAdmissionModeFires proves the admission_mode fire point: a
// tasks-mode lane that owns a board holding open work is admitted via the
// SCHED-GAP-124 waiver, which records an admission_mode use.
func TestFeatureAdmissionModeFires(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)
	now := fixedEvalNow()

	wd := admitWorkdirWithBoard(t, `{"id":"FM-1","status":"pending","title":"work"}`)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "tasks-admit", CooldownS: 60,
		AdmissionMode: "tasks", Workdir: wd,
	})
	l := admitNewLoop(t, db, now)
	admitCaptureLog(t)

	l.evaluate()

	counts := featureUsageCounts(t, db)
	if counts[database.FeatureAdmissionMode] != 1 {
		t.Errorf("admission_mode count = %d, want 1 (%v)", counts[database.FeatureAdmissionMode], counts)
	}
}
