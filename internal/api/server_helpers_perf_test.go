package api

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// perfTestTickInserter is the test-tick inserter used by
// TestComputeProjectFailureRates_OneRoundTrip and
// BenchmarkComputeProjectFailureRates. The package-private helpers
// mustCreateHelperTestProject / insertHelperTestTick in
// server_helpers_test.go take *testing.T; this file needs to drive the
// same inserts from a *testing.B (benchmark) too. Both *testing.T and
// *testing.B satisfy the perfT interface, so a single inserter serves
// both.
type perfT interface {
	Helper()
	Fatalf(format string, args ...interface{})
}

func perfInsertTick(t perfT, db *sql.DB, id, project, status string, spawnedAt time.Time) {
	t.Helper()
	ts := spawnedAt.Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, status, ts, ts, ts); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

func perfCreateProject(t perfT, db *sql.DB, name string) {
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

// TestComputeProjectFailureRates_OneRoundTrip (SCHED-PERF-002) — the
// load-bearing proof that the rewrite is a single round trip, not the
// pre-fix N+1. The test seeds a 368-project × 200-tick fixture
// (matching the live fleet as measured 2026-09-21), resets the
// failureRateQueryCount test seam, runs computeProjectFailureRates once,
// and asserts the counter incremented by exactly 1 — independent of the
// number of projects. The pre-fix N+1 shape would have issued
// 368 + 1 = 369 queries; the rewrite is a single SELECT.
//
// The counter is package-private and lives in server_helpers.go; this
// test is the only consumer. It runs fast (~2s on the 73.6k-row
// fixture) and is deterministic — it counts operations, not wall time,
// so it does not flake on shared CI hosts.
func TestComputeProjectFailureRates_OneRoundTrip(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	const nProjects = 368
	const nTicksPer = 200
	now := time.Now()
	for i := 0; i < nProjects; i++ {
		name := fmt.Sprintf("p-%04d", i)
		perfCreateProject(t, db, name)
		for j := 0; j < nTicksPer; j++ {
			status := "completed"
			if j%10 == 0 {
				status = "failed"
			}
			perfInsertTick(t, db, fmt.Sprintf("%s-%04d", name, j), name, status,
				now.Add(-time.Duration(nTicksPer-j)*time.Minute))
		}
	}

	resetFailureRateQueryCountForTest()
	before := failureRateQueryCountForTest()
	if before != 0 {
		t.Fatalf("failureRateQueryCount = %d before call; reset helper broken", before)
	}

	rates := computeProjectFailureRates(ctx, db, 100, 0, 0)
	if len(rates) != nProjects {
		t.Fatalf("rates length = %d, want %d (fixture produced %d projects)",
			len(rates), nProjects, nProjects)
	}

	after := failureRateQueryCountForTest()
	if after != 1 {
		t.Fatalf("computeProjectFailureRates issued %d SELECT queries for %d projects; "+
			"want exactly 1 (single aggregated query). Pre-fix N+1 shape would have "+
			"been %d queries (1 DISTINCT + %d per-project).",
			after, nProjects, nProjects+1, nProjects)
	}
}

// BenchmarkComputeProjectFailureRates measures ns/op for the rewritten
// single-query path on a 368-project × 200-tick in-memory fixture. Run
// with `go test -bench=BenchmarkComputeProjectFailureRates -benchtime=3x
// ./internal/api/`. A budgeted CI assertion lives in
// TestComputeProjectFailureRates_OneRoundTrip; the benchmark is the
// wall-time measurement that backs the commit acca3843 message
// ("TestComputeProjectFailureRates_FleetScale: 1.97s") and the live
// /api/v1/status p99 < 1s claim.
func BenchmarkComputeProjectFailureRates(b *testing.B) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		b.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	const nProjects = 368
	const nTicksPer = 200
	now := time.Now()
	for i := 0; i < nProjects; i++ {
		name := fmt.Sprintf("p-%04d", i)
		perfCreateProject(b, db, name)
		for j := 0; j < nTicksPer; j++ {
			status := "completed"
			if j%10 == 0 {
				status = "failed"
			}
			perfInsertTick(b, db, fmt.Sprintf("%s-%04d", name, j), name, status,
				now.Add(-time.Duration(nTicksPer-j)*time.Minute))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = computeProjectFailureRates(ctx, db, 100, 0, 0)
	}
}
