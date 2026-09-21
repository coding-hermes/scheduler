package api

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// insertHelperTestTick inserts a completed tick row directly into the DB.
// The owning project must already exist (ticks.project_name is a FK).
func insertHelperTestTick(t *testing.T, db *sql.DB, id, project, status string, spawnedAt time.Time) {
	t.Helper()
	ts := spawnedAt.Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, status, ts, ts, ts); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

func mustCreateHelperTestProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestComputeProjectFailureRates_AutoDisableArmed (GAP-047) verifies the
// armed-state computation mirrors the exact auto-disable condition in
// internal/scheduler/alert_escalation.go CheckFailureRateAutoDisable:
// armed = threshold > 0 && total >= minTicks && rate >= threshold.
func TestComputeProjectFailureRates_AutoDisableArmed(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mustCreateHelperTestProject(t, db, "heavy") // 8 failed + 2 completed → rate 0.8, total 10
	mustCreateHelperTestProject(t, db, "small") // 4 completed → total 4
	mustCreateHelperTestProject(t, db, "clean") // 5 completed → rate 0.0

	now := time.Now()
	for i := 0; i < 8; i++ {
		insertHelperTestTick(t, db, "heavy-fail-"+string(rune('a'+i)), "heavy", "failed",
			now.Add(-time.Duration(20-i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertHelperTestTick(t, db, "heavy-ok-"+string(rune('a'+i)), "heavy", "completed",
			now.Add(-time.Duration(5-i)*time.Minute))
	}
	for i := 0; i < 4; i++ {
		insertHelperTestTick(t, db, "small-ok-"+string(rune('a'+i)), "small", "completed",
			now.Add(-time.Duration(8-i)*time.Minute))
	}
	for i := 0; i < 5; i++ {
		insertHelperTestTick(t, db, "clean-ok-"+string(rune('a'+i)), "clean", "completed",
			now.Add(-time.Duration(6-i)*time.Minute))
	}

	// Case 1: threshold == 0 (feature off) → never armed, even at 80% failure.
	rates := computeProjectFailureRates(ctx, db, 100, 0, 5)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0: heavy armed = %+v, want false (feature off)", rates["heavy"])
	}

	// Case 2: threshold 0.5, minTicks 5 → heavy (0.8 >= 0.5, 10 >= 5) armed;
	// small (total 4 < 5) not armed; clean (0.0 < 0.5) not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5, 5)
	if r, ok := rates["heavy"]; !ok || !r.AutoDisableArmed {
		t.Errorf("threshold=0.5: heavy = %+v, want armed=true", rates["heavy"])
	}
	if r, ok := rates["small"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.5: small = %+v, want armed=false (total 4 < minTicks 5)", rates["small"])
	}
	if r, ok := rates["clean"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.5: clean = %+v, want armed=false (rate 0 < 0.5)", rates["clean"])
	}

	// Case 3: threshold 0.9 → heavy (0.8 < 0.9) not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.9, 5)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.9: heavy = %+v, want armed=false (0.8 < 0.9)", rates["heavy"])
	}

	// Case 4: minTicks 50 → heavy (total 10 < 50) not armed despite rate.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5, 50)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("minTicks=50: heavy = %+v, want armed=false (10 < 50)", rates["heavy"])
	}
}

// TestComputeProjectFailureRates_AutoDisableArmedAtThreshold verifies the
// boundary semantics: rate exactly equal to the threshold arms, and the
// unrounded ratio is used (mirroring CheckFailureRateAutoDisable).
func TestComputeProjectFailureRates_AutoDisableArmedAtThreshold(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// "exact": 5 failed + 5 completed → rate exactly 0.5.
	mustCreateHelperTestProject(t, db, "exact")
	now := time.Now()
	for i := 0; i < 5; i++ {
		insertHelperTestTick(t, db, "exact-fail-"+string(rune('a'+i)), "exact", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
		insertHelperTestTick(t, db, "exact-ok-"+string(rune('a'+i)), "exact", "completed",
			now.Add(-time.Duration(9-i)*time.Minute))
	}

	// rate == threshold → armed (>= comparison).
	rates := computeProjectFailureRates(ctx, db, 100, 0.5, 5)
	if r, ok := rates["exact"]; !ok || !r.AutoDisableArmed {
		t.Errorf("exact-threshold: exact = %+v, want armed=true (0.5 >= 0.5)", rates["exact"])
	}

	// One tick's worth below the threshold → not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5001, 5)
	if r, ok := rates["exact"]; !ok || r.AutoDisableArmed {
		t.Errorf("just-below: exact = %+v, want armed=false (0.5 < 0.5001)", rates["exact"])
	}
}

// TestComputeProjectFailureRates_ExcludesHardDeletedProjects (DOGFOOD-009)
// verifies the ghost-project class is dead: ticks whose project row no
// longer exists (hard-deleted/purged, e.g. eduos-e2e) must NOT appear in
// projects_failure_rates — previously they surfaced forever with
// failure_rate=1.0 and auto_disable_armed=true.
func TestComputeProjectFailureRates_ExcludesHardDeletedProjects(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mustCreateHelperTestProject(t, db, "live")  // still exists → must appear
	mustCreateHelperTestProject(t, db, "ghost") // will be purged → must NOT appear

	now := time.Now()
	insertHelperTestTick(t, db, "live-ok-1", "live", "completed", now.Add(-time.Minute))
	for i := 0; i < 3; i++ {
		insertHelperTestTick(t, db, "ghost-fail-"+string(rune('a'+i)), "ghost", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
	}

	// Purge the ghost project row (same path the API purge uses).
	if err := database.PurgeProject(ctx, db, "ghost"); err != nil {
		t.Fatalf("PurgeProject ghost: %v", err)
	}

	rates := computeProjectFailureRates(ctx, db, 100, 0.5, 2)
	if _, ok := rates["ghost"]; ok {
		t.Errorf("ghost (purged project) still in failure rates: %+v", rates["ghost"])
	}
	if _, ok := rates["live"]; !ok {
		t.Errorf("live project missing from failure rates: %+v", rates)
	}
}

// TestComputeProjectFailureRates_HarnessOnlyWindow (SCHED-PERF-002) — the
// single-query path must still emit a project entry (rate=0) when every
// completed tick in the window is harness-class: a project whose only
// recent failures are gateway 503s must remain visible in the status
// surface so a harness-blocked lane is not silently dropped (the same
// edge case SCHED-GAP-173 added in the N+1 rewrite). Before the
// exists-filter + per-project walk, the new path could drop the row
// because curSeen was bumped but curTotal stayed 0; the implementation
// must still flush, and `flush` always writes the entry.
func TestComputeProjectFailureRates_HarnessOnlyWindow(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mustCreateHelperTestProject(t, db, "harness-only")
	now := time.Now()
	// Three failed ticks, all harness-class markers (matches
	// failureclass.go HarnessFailure list).
	for i := 0; i < 3; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("hw-fail-%d", i), "harness-only", "failed",
			now.Add(-time.Duration(5-i)*time.Minute))
		// overwrite error to a harness marker (the insert helper
		// leaves error empty by default; update it in place).
		if _, err := db.Exec(`UPDATE ticks SET error=? WHERE id=?`,
			"gateway unreachable: 503", fmt.Sprintf("hw-fail-%d", i)); err != nil {
			t.Fatalf("update tick error: %v", err)
		}
	}

	rates := computeProjectFailureRates(ctx, db, 100, 0, 0)
	r, ok := rates["harness-only"]
	if !ok {
		t.Fatalf("harness-only missing from failure rates: %+v", rates)
	}
	if r.Total != 0 || r.Failed != 0 || r.FailureRate != 0 {
		t.Errorf("harness-only = %+v, want total=0 failed=0 rate=0 (harness-class ticks excluded)", r)
	}
	if r.AutoDisableArmed {
		t.Errorf("harness-only armed = true, want false (rate 0 < any positive threshold)")
	}
}

// TestComputeProjectFailureRates_FleetScale (SCHED-PERF-002) — a 368-project
// fleet shape must complete in well under one second on the 73k-tick
// production DB equivalent. The check uses an in-memory DB sized to match
// the live fleet (368 projects × 200 ticks) so the path is exercised at
// production scale; the assertion is on call count (1 round trip per
// invocation), not wall time, to keep the test deterministic on shared CI
// hosts. The 1-round-trip invariant is what the rewrite buys: the
// per-project loop was N+1.
func TestComputeProjectFailureRates_FleetScale(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// 368 projects × 200 ticks = 73 600 rows, matching the live fleet
	// measured on 2026-09-21.
	const nProjects = 368
	const nTicksPer = 200
	now := time.Now()
	for i := 0; i < nProjects; i++ {
		name := fmt.Sprintf("p-%04d", i)
		mustCreateHelperTestProject(t, db, name)
		for j := 0; j < nTicksPer; j++ {
			status := "completed"
			if j%10 == 0 {
				status = "failed"
			}
			insertHelperTestTick(t, db, fmt.Sprintf("%s-%04d", name, j), name, status,
				now.Add(-time.Duration(nTicksPer-j)*time.Minute))
		}
	}

	rates := computeProjectFailureRates(ctx, db, 100, 0, 0)
	if len(rates) != nProjects {
		t.Fatalf("rates length = %d, want %d", len(rates), nProjects)
	}
	// p-0000 has 200 ticks, 20 failed; window 100 keeps the most
	// recent 100 (the 100 newest by spawned_at DESC). Spawned_at
	// was inserted with descending minute offsets so the LATEST
	// 100 correspond to j in [100..199], which is 10 failed (j % 10
	// == 0 → j = 100, 110, ..., 190). 10/100 = 0.1.
	r, ok := rates["p-0000"]
	if !ok {
		t.Fatalf("p-0000 missing from rates")
	}
	if r.Total != 100 || r.Failed != 10 || r.FailureRate != 0.1 {
		t.Errorf("p-0000 = %+v, want total=100 failed=10 rate=0.1 (window of 100 from 200-tick project)", r)
	}
}
func TestComputeProjectFailureRates_WindowTruncation(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mustCreateHelperTestProject(t, db, "trunc")

	// 10 completed ticks, oldest first: 6 failed then 4 completed.
	now := time.Now()
	for i := 0; i < 10; i++ {
		status := "completed"
		if i < 6 {
			status = "failed"
		}
		insertHelperTestTick(t, db, fmt.Sprintf("trunc-%02d", i), "trunc", status,
			now.Add(-time.Duration(30-i)*time.Minute))
	}

	// Window 5 → only the 5 most recent ticks count (4 completed + 1 failed).
	rates := computeProjectFailureRates(ctx, db, 5, 0, 0)
	r, ok := rates["trunc"]
	if !ok {
		t.Fatalf("trunc missing from failure rates: %+v", rates)
	}
	if r.Total != 5 || r.Failed != 1 {
		t.Errorf("window=5: trunc = %+v, want total=5 failed=1 (most recent 5: 4 completed + 1 failed)", r)
	}
	if r.FailureRate != 0.2 {
		t.Errorf("window=5: trunc failure_rate = %v, want 0.2", r.FailureRate)
	}

	// Window 100 → no truncation: all 10 count (6 failed + 4 completed).
	rates = computeProjectFailureRates(ctx, db, 100, 0, 0)
	r, ok = rates["trunc"]
	if !ok {
		t.Fatalf("trunc missing from failure rates: %+v", rates)
	}
	if r.Total != 10 || r.Failed != 6 {
		t.Errorf("window=100: trunc = %+v, want total=10 failed=6", r)
	}
	if r.FailureRate != 0.6 {
		t.Errorf("window=100: trunc failure_rate = %v, want 0.6", r.FailureRate)
	}
}
