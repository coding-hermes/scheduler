package scheduler

// ADV-R13 — regression tests for the host load/memory telemetry (the
// host_samples measurement foundation).
//
// Every test drives the REAL evaluate() with a pinned clock seam
// (fixedEvalNow, ADV-R04/G6) in simulation mode, so there are no sleeps and
// no wall-clock reads: the sampler is injected for determinism and the
// persisted timestamps are compared against the constructed instant.
//
// The scope these tests pin: exactly ONE sample per evaluation pass, taken
// at entry (so early-return paths still leave their measurement), a
// missing reading persisted as an honest GAP (no row, no fabricated zero,
// evaluation unaffected), and no coupling to the admission path.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// telemetrySwapSampler installs an injected sampler and restores the
// package default on cleanup. There is no t.Parallel anywhere in this file
// (and none in the package): the sampler is a package global.
func telemetrySwapSampler(t *testing.T, fn func() (HostLoadSample, bool)) {
	t.Helper()
	orig := sampleHostLoad
	sampleHostLoad = fn
	t.Cleanup(func() { sampleHostLoad = orig })
}

// fakeSample returns a sampler closure returning a fixed reading.
func fakeSample(s HostLoadSample, ok bool) func() (HostLoadSample, bool) {
	return func() (HostLoadSample, bool) { return s, ok }
}

// telemetrySample returns the deterministic reading the tests assert on.
func telemetrySample() HostLoadSample {
	return HostLoadSample{
		Load1:             1.25,
		Load5:             0.75,
		Load15:            0.5,
		MemTotalBytes:     16 << 30,
		MemAvailableBytes: 8 << 30,
		Source:            "proc",
	}
}

// telemetryNewLoop builds the loop every test drives: simulation mode (the
// real spawner is never touched) + the pinned clock seam.
func telemetryNewLoop(t *testing.T, db *sql.DB, now time.Time) *Loop {
	t.Helper()
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(now))
	return l
}

func telemetryCountRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM host_samples`).Scan(&n); err != nil {
		t.Fatalf("count host_samples: %v", err)
	}
	return n
}

// TestADVR13_DeterministicSamplePersisted proves the injectable-sampler
// contract: one evaluate() with a fake sampler persists exactly one row
// carrying EXACTLY the injected numbers and a parseable RFC3339 timestamp
// taken from the loop's clock seam (AC1, AC2).
func TestADVR13_DeterministicSamplePersisted(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "qa", 0, "cooldown")
	admitInsertProject(t, db, admitProjectSpec{
		Name: "tele-ok", NS: "qa", CooldownS: 60, Last: now.Add(-90 * time.Second),
	})
	l := telemetryNewLoop(t, db, now)
	telemetrySwapSampler(t, fakeSample(telemetrySample(), true))

	l.evaluate()

	if n := telemetryCountRows(t, db); n != 1 {
		t.Fatalf("host_samples rows after one evaluate = %d, want exactly 1", n)
	}
	got, err := database.LatestHostSample(context.Background(), db)
	if err != nil {
		t.Fatalf("LatestHostSample: %v", err)
	}
	want := telemetrySample()
	if got.Load1 != want.Load1 || got.Load5 != want.Load5 || got.Load15 != want.Load15 {
		t.Errorf("persisted loads = (%v, %v, %v), want (%v, %v, %v)",
			got.Load1, got.Load5, got.Load15, want.Load1, want.Load5, want.Load15)
	}
	if got.MemTotalBytes != want.MemTotalBytes || got.MemAvailableBytes != want.MemAvailableBytes {
		t.Errorf("persisted memory = (%d, %d), want (%d, %d)",
			got.MemTotalBytes, got.MemAvailableBytes, want.MemTotalBytes, want.MemAvailableBytes)
	}
	if got.Source != want.Source {
		t.Errorf("source = %q, want %q", got.Source, want.Source)
	}
	ts, err := time.Parse(time.RFC3339, got.SampledAt)
	if err != nil {
		t.Fatalf("sampled_at %q is not RFC3339: %v", got.SampledAt, err)
	}
	if !ts.Equal(now.UTC()) {
		t.Errorf("sampled_at = %v, want the pinned loop instant %v", ts, now.UTC())
	}
}

// TestADVR13_ConsecutiveCyclesTwoDistinctSamples proves one sample per
// evaluation pass across passes: two evaluate() calls persist TWO DISTINCT
// samples (different injected load values) with non-decreasing timestamps,
// and the sampler was consulted exactly twice — once per pass, never per
// project or per spawned tick (AC2).
func TestADVR13_ConsecutiveCyclesTwoDistinctSamples(t *testing.T) {
	start := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "qa", 0, "cooldown")
	admitInsertProject(t, db, admitProjectSpec{
		Name: "tele-cycle", NS: "qa", CooldownS: 60, Last: start.Add(-90 * time.Second),
	})
	clk := clock.NewSimClockAt(1.0, start)
	t.Cleanup(func() { clk.Close() })
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(clk)

	calls := 0
	telemetrySwapSampler(t, func() (HostLoadSample, bool) {
		calls++
		s := telemetrySample()
		s.Load1 = float64(calls) // distinct value per call
		return s, true
	})

	l.evaluate()
	clk.Advance(90 * time.Second)
	l.evaluate()

	if calls != 2 {
		t.Fatalf("sampler consulted %d times across 2 passes, want exactly 2 (one per pass)", calls)
	}
	type row struct {
		id    int64
		at    string
		load1 float64
	}
	rows, err := db.Query(`SELECT id, sampled_at, load1 FROM host_samples ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("query host_samples: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.at, &r.load1); err != nil {
			t.Fatalf("scan host_samples row: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate host_samples rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("host_samples rows after 2 passes = %d, want 2", len(got))
	}
	if got[0].load1 == got[1].load1 {
		t.Errorf("both samples carry load1=%v — samples are not distinct", got[0].load1)
	}
	t1, err := time.Parse(time.RFC3339, got[0].at)
	if err != nil {
		t.Fatalf("first sampled_at %q is not RFC3339: %v", got[0].at, err)
	}
	t2, err := time.Parse(time.RFC3339, got[1].at)
	if err != nil {
		t.Fatalf("second sampled_at %q is not RFC3339: %v", got[1].at, err)
	}
	if t2.Before(t1) {
		t.Errorf("timestamps decreased: %v → %v, want non-decreasing", t1, t2)
	}
}

// TestADVR13_UnavailableSamplerRecordsGapWithoutAborting proves the
// unavailable arm: a sampler reporting ok=false persists NO row (a missing
// reading is a gap, never a fabricated zero) and the evaluation pass still
// completes its selection work — the project is still selected and its sim
// tick row is still written (AC2's non-interference half).
func TestADVR13_UnavailableSamplerRecordsGapWithoutAborting(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "qa", 0, "cooldown")
	admitInsertProject(t, db, admitProjectSpec{
		Name: "tele-gap", NS: "qa", CooldownS: 60, Last: now.Add(-90 * time.Second),
	})
	l := telemetryNewLoop(t, db, now)
	telemetrySwapSampler(t, fakeSample(HostLoadSample{}, false))

	l.evaluate() // must not panic, must not abort the pass

	if n := telemetryCountRows(t, db); n != 0 {
		t.Fatalf("host_samples rows = %d, want 0 (a missing reading is a gap, never a fabricated row)", n)
	}
	var ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'tele-gap'`).Scan(&ticks); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticks < 1 {
		t.Fatalf("evaluation did not complete its selection work: %d tick rows for tele-gap, want >= 1", ticks)
	}
	if _, err := database.LatestHostSample(context.Background(), db); err != sql.ErrNoRows {
		t.Fatalf("LatestHostSample on empty table = %v, want sql.ErrNoRows (no synthesized zero row)", err)
	}
}
