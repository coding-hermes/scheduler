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
//
// INT-CI-006 (2026-09-18) — why this file now runs against a HOLDING gateway.
// The second pass below asserts a CROSS-PASS invariant: the nudge pass 1
// admitted must still count against its namespace cap when pass 2 runs. On a
// real gateway that holds for minutes — the nudge tick sits in
// queued/running for the whole turn. The instant mock gateway used elsewhere
// answered in microseconds, so the pass-1 nudge was often already `completed`
// when pass 2 read `namespaceInflight`; the namespace looked empty and a second
// sync-lane nudge was admitted (measured: the `sync-lane nudges = 2` branch,
// ~1-4% of runs, exactly the CI Unit Tests flake on 5eaf1af/05918eb/ebcacfd).
// The test's own mock gateway now HOLDS every /v1/responses open until the test
// releases it, which reproduces the production timing exactly: the pass-1 nudge
// is provably still in flight (queued/running) across the second pass. No
// assertion was weakened, deleted or skipped — the bound is the original one.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

	gw := newHeldResumeGateway(t)
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

	// Barrier (INT-CI-006): the first pass's spawn is parked on the held gateway
	// and cannot reach a terminal status until the test releases it. Verify the
	// barrier is ENGAGED (a spawn is actually parked) and then that the premise
	// the second pass depends on holds — the pass-1 nudge is still in flight, so
	// the namespace it occupies is not empty.
	waitForHeldSpawn(t, gw, 5*time.Second)
	if got := l.namespaceInflight(ctx, "sync-lanes"); got != 1 {
		t.Fatalf("barrier precondition: sync-lanes in flight after the first pass = %d, want 1 — the pass-1 nudge must still be queued/running (held gateway), otherwise the second pass below cannot test the cross-pass cap", got)
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

	// Pin the mechanism behind that bound: the second pass deferred because the
	// pass-1 nudge still counted as in flight. Without the in-flight count the
	// namespace looks empty and a sibling lane is admitted (the flake).
	if got := l.namespaceInflight(ctx, "sync-lanes"); got != 1 {
		t.Errorf("sync-lanes in flight after the second pass = %d, want 1 (still only the pass-1 nudge)", got)
	}

	// Release the parked spawns and let the pool settle, so the completion
	// writers are done before the test's in-memory DB is closed.
	gw.releaseAll()
	waitForSettled(t, db, 10*time.Second)
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

// heldResumeGateway is the resume test gateway with a hold gate on
// /v1/responses: an accepted spawn is counted, then parked until releaseAll, so
// the spawning tick stays queued/running — never terminal — for as long as the
// test needs. A real gateway holds a nudge open for the whole turn; the instant
// mock let a pass-1 nudge complete between two resumeOrphans passes, which is
// the INT-CI-006 flake this barrier removes.
type heldResumeGateway struct {
	srv     *httptest.Server
	release chan struct{}
	once    sync.Once
	spawns  atomic.Int32
}

func newHeldResumeGateway(t *testing.T) *heldResumeGateway {
	t.Helper()
	g := &heldResumeGateway{release: make(chan struct{})}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			g.spawns.Add(1)
			<-g.release // held open: the tick row cannot reach a terminal status
			schedGap080CompletedResponse(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		// Release BEFORE Close: httptest.Server.Close waits for outstanding
		// handlers, so a still-parked request would hang the suite.
		g.releaseAll()
		g.srv.Close()
	})
	return g
}

// wire attaches the held gateway to the loop's spawner (no exec fallback).
func (g *heldResumeGateway) wire(l *Loop) {
	l.SetGatewayClient(NewGatewayClient(g.srv.URL, "sk-daemon-shared", 30*time.Second))
	l.spawner.SetNoExecFallback(true)
}

// releaseAll lets every parked /v1/responses finish. Idempotent.
func (g *heldResumeGateway) releaseAll() { g.once.Do(func() { close(g.release) }) }

// waitForHeldSpawn blocks until at least one spawn is parked on the gateway —
// the barrier is verified, not merely armed — and fails loudly if no spawn
// reaches it, because then the tick is not in flight for the reason the
// cross-pass assertion assumes.
func waitForHeldSpawn(t *testing.T, g *heldResumeGateway, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g.spawns.Load() >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no spawn reached the held gateway within %v — the barrier precondition (pass-1 nudge in flight) does not hold", timeout)
}

// waitForSettled waits for every queued/running tick row to reach a terminal
// status once the gateway has been released.
func waitForSettled(t *testing.T, db *sql.DB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM ticks WHERE status IN ('queued','running')`).Scan(&n); err != nil {
			t.Fatalf("count in-flight ticks: %v", err)
		}
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tick rows did not settle within %v after the gateway was released", timeout)
}
