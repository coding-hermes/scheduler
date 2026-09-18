package scheduler

// SCHED-GAP-142 — regression test for the boot-burst cap breach.
//
// Measured defect (2026-09-17, live): a scheduler restart re-nudged 5 orphaned
// ticks in ONE pass — 2 foremen + 3 <project>-sync lanes — so duckbrain-sync ran
// 3 ticks against max_concurrent=1, and the burst took global slots before any
// qa/pm/dogfood lane could be admitted. `resumeOrphans` spawns through the slot
// pool directly, so it never consulted the packer's namespace cap.
//
// This test replays that exact shape end-to-end through resumeOrphans.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// capTestProject creates an enabled project inside a namespace.
func capTestProject(t *testing.T, db *sql.DB, name, nsID string) {
	t.Helper()
	ns := nsID
	p := &database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: t.TempDir(),
		Weight: 3, Priority: 5, CooldownS: 21600, DecayRate: 1,
		Model: "test-model", Provider: "test-provider", Enabled: true, NamespaceID: &ns,
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
}

func TestGAP142_StartupNudgeRespectsNamespaceCap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The live shape: one satellite namespace capped at 1, the foremen namespace
	// at 8 (Foreman cap), five orphans waiting.
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "sync-lanes", Weight: 5, Reserved: 1, HardCap: 100, MaxConcurrent: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create namespace sync-lanes: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "foremen", Weight: 100, Reserved: 70, HardCap: 100, MaxConcurrent: 8, Enabled: true,
	}); err != nil {
		t.Fatalf("create namespace foremen: %v", err)
	}

	syncLanes := []string{"alpha-sync", "beta-sync", "gamma-sync"}
	foremen := []string{"foreman-one", "foreman-two"}
	for _, n := range syncLanes {
		capTestProject(t, db, n, "sync-lanes")
		orphanTickRow(t, db, n+"-tick", n, "failed", OrphanReasonDrainTimeout, 0)
	}
	for _, n := range foremen {
		capTestProject(t, db, n, "foremen")
		orphanTickRow(t, db, n+"-tick", n, "timeout", OrphanReasonZombieReap, 0)
	}

	gw := newResumeGateway(t)
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	l.noDeliver = true
	gw.wire(l)

	l.resumeOrphans("startup")

	// Exactly ONE satellite nudge may exist: the namespace cap is 1.
	syncNudges := 0
	for _, n := range syncLanes {
		syncNudges += countNudgeRows(t, db, n)
	}
	if syncNudges != 1 {
		t.Errorf("sync-lane nudges = %d, want exactly 1 (namespace max_concurrent=1) — this is the boot-burst defect", syncNudges)
	}

	// The foremen namespace has room: both must be nudged.
	foremenNudges := 0
	for _, n := range foremen {
		foremenNudges += countNudgeRows(t, db, n)
	}
	if foremenNudges != 2 {
		t.Errorf("foremen nudges = %d, want 2 (namespace max_concurrent=8)", foremenNudges)
	}

	// A deferred orphan must be left completely untouched: no nudge row, and
	// crucially no nudge CONSUMED (otherwise the retry budget burns down while
	// the lane sits at its cap).
	for _, n := range syncLanes {
		if countNudgeRows(t, db, n) > 0 {
			continue
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT nudge_count FROM ticks WHERE id = ?`, n+"-tick").Scan(&count); err != nil {
			t.Fatalf("nudge_count for %s: %v", n, err)
		}
		if count != 0 {
			t.Errorf("deferred orphan %s consumed a nudge (nudge_count=%d) — deferral must not spend the retry budget", n, count)
		}
	}

	// The cap must never be exceeded even if resumeOrphans is called again while
	// the first nudge is still in flight (queued counts toward the cap).
	l.resumeOrphans("startup-again")
	total := 0
	for _, n := range syncLanes {
		total += countNudgeRows(t, db, n)
	}
	if total > 1 {
		t.Errorf("after a second pass sync-lane nudges = %d, want <= 1 — a queued nudge must count toward the cap", total)
	}
}

// countNudgeRows counts nudge rows created for a project.
func countNudgeRows(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND id LIKE '%-nudge%'`, project).Scan(&n); err != nil {
		t.Fatalf("count nudge rows for %s: %v", project, err)
	}
	return n
}
