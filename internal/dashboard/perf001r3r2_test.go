package dashboard

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// r3r2DB is this file's own fixture: the shared newTestDB/mustCreateProject
// helpers live in the EXTERNAL dashboard_test package (generator_test.go),
// and these tests need the generator's unexported state, so they run inside
// the package and build their fixture here.
func r3r2DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func r3r2CreateProject(t *testing.T, db *sql.DB, name, workdir string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   workdir,
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

// r3r2Snippet returns a small window of s around needle for failure messages
// (the external test package's snippet() helper is not visible here).
func r3r2Snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		if len(s) > 120 {
			return s[:120] + "..."
		}
		return s
	}
	end := i + 120
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}

// PERF-001-R3-R2 — the cold fleet render is BOUNDED, and a slow render names
// its wait.
//
// The measured live failure (8ec55a6f, 845 lanes / 809 distinct workdirs):
// the first GET / after the snapshot and CI TTL lapsed blocked 79.685s with
// ~0 request CPU. The goroutine dump taken mid-flight (this row's AC1) named
// the chain exactly:
//
//	goroutine N [sync.Mutex.Lock]  dashboard.cachedOverview  overview_cache.go:68
//	  ← GET / waiting on the snapshot mutex
//	goroutine M [chan send]        dashboard.warmCIConclusions generator_data.go:1685 (sem)
//	  ← /dashboard/partial: the concurrent render HOLDING the mutex, dispatching
//	    CI fetches for a 845-element workdir slice
//	8 × [wait4/IO wait]            os/exec.(*Cmd).Output ← warmCIConclusions.func1
//
// So the wait was NOT the scheduler Loop mutex PERF-001-R3 fixed: the dashboard
// root never takes Loop.mu (its only scheduler dependency is the immutable
// UrgencyCalculator and the atomic paused flag). The holders were the CI
// fan-out (`gh run list` per workdir, 8-wide, up to ciFetchTimeout each) and
// the snapshot mutex held ACROSS that collect, which serialized every other
// render behind it.
//
// These tests pin the three properties that make that shape impossible:
//  1. the snapshot mutex is never held across collect() — a render that gives
//     up does not queue behind it (TestPerfR3R2_RenderDoesNotQueueBehindCollect);
//  2. a first render with no snapshot degrades to an explicit partial page
//     inside the budget instead of blocking (TestPerfR3R2_FirstRenderIsBounded);
//  3. an expired snapshot is served STALE inside the budget, marked with its
//     age + refresh-pending note (TestPerfR3R2_StaleServedWithinBudget);
// plus the bounded CI warm pass (TestPerfR3R2_CIRWarmIsBounded) and the trace
// that names the wait (TestPerfR3R2_TraceNamesInflightStage).

// slowProjectGen builds a generator over a fresh test DB with ONE project that
// has a workdir, so the CI warm stage has real work to do, plus a runner that
// blocks for the given duration (the network-bound `gh run list` stand-in).
func slowProjectGen(t *testing.T, runnerDelay time.Duration) (*Generator, *sql.DB, string) {
	t.Helper()
	db := r3r2DB(t)
	wd := t.TempDir()
	r3r2CreateProject(t, db, "alpha", wd)
	g := NewGenerator(db, nil)
	g.ciTTL = 60 * time.Second
	g.ciRunner = func(string) string {
		time.Sleep(runnerDelay)
		return "success"
	}
	return g, db, wd
}

// waitForSnapshot blocks until the generator has a snapshot stamp (a
// background collect landed) or the deadline passes. Keeps a test from
// returning while a detached collect is still writing to a DB the test is
// about to close.
func waitForSnapshot(t *testing.T, g *Generator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.snap.mu.Lock()
		fresh := !g.snap.stamp.IsZero()
		g.snap.mu.Unlock()
		if fresh {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background collect never stored a snapshot")
}

// TestPerfR3R2_FirstRenderIsBounded is AC2 for the no-snapshot case: the very
// first render after boot must NOT wait for the whole pipeline. With a collect
// that takes far longer than the budget, the page comes back inside the budget
// and says it is still collecting (and names what it was doing).
func TestPerfR3R2_FirstRenderIsBounded(t *testing.T) {
	g, _, _ := slowProjectGen(t, 400*time.Millisecond)
	g.SetRenderBudget(60 * time.Millisecond)

	start := time.Now()
	var buf strings.Builder
	if err := g.GenerateParams(&buf, nil); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("first render took %s with a 60ms budget — the bound did not hold", elapsed)
	}
	page := buf.String()
	if !strings.Contains(page, `id="partialRender"`) {
		t.Fatalf("degraded first render must carry the partial notice; got %s", r3r2Snippet(page, "Generated "))
	}
	if !strings.Contains(page, "overview still collecting") {
		t.Errorf("partial note must say the overview is still collecting; got %s", r3r2Snippet(page, "overview still"))
	}

	// The collect was NOT abandoned: it finishes and fills the snapshot, so the
	// next render is a normal fresh page.
	waitForSnapshot(t, g)
	var buf2 strings.Builder
	if err := g.GenerateParams(&buf2, nil); err != nil {
		t.Fatalf("second GenerateParams: %v", err)
	}
	page2 := buf2.String()
	if strings.Contains(page2, `id="partialRender"`) {
		t.Errorf("second render served after the collect landed must not be partial")
	}
	if !strings.Contains(page2, `href="/projects/alpha"`) {
		t.Errorf("second render must carry the lane row; got %s", r3r2Snippet(page2, "projects"))
	}
}

// TestPerfR3R2_StaleServedWithinBudget is AC2 for the expired-snapshot case:
// a refresh that overruns the budget serves the previous snapshot (with its
// age and an explicit refresh-pending note) rather than blocking the request.
func TestPerfR3R2_StaleServedWithinBudget(t *testing.T) {
	g, _, _ := slowProjectGen(t, 0)
	g.snapTTL = 40 * time.Millisecond
	g.SetRenderBudget(60 * time.Millisecond)

	// First render: fresh (collect is instant with a zero-delay runner).
	var first strings.Builder
	if err := g.GenerateParams(&first, nil); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if strings.Contains(first.String(), `id="refreshPending"`) {
		t.Fatalf("a fresh render must not claim a pending refresh")
	}

	// Make the next collect slow and let the snapshot expire.
	g.ciRunner = func(string) string { time.Sleep(400 * time.Millisecond); return "success" }
	g.ciMu.Lock()
	g.ciCache = map[string]ciCacheEntry{}
	g.ciMu.Unlock()
	time.Sleep(60 * time.Millisecond)

	start := time.Now()
	var second strings.Builder
	if err := g.GenerateParams(&second, nil); err != nil {
		t.Fatalf("second render: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("stale serve took %s with a 60ms budget", elapsed)
	}
	page := second.String()
	if !strings.Contains(page, `id="refreshPending"`) {
		t.Fatalf("stale serve must carry the refresh-pending notice; got %s", r3r2Snippet(page, "Generated "))
	}
	if !strings.Contains(page, `id="cacheAge"`) {
		t.Errorf("stale serve must stamp the snapshot age")
	}
	if !strings.Contains(page, `href="/projects/alpha"`) {
		t.Errorf("stale serve must still carry the previous snapshot's rows")
	}
	waitForSnapshot(t, g)
}

// TestPerfR3R2_RenderDoesNotQueueBehindCollect is the lock-scope half of AC1:
// the snapshot mutex is not held across collect(), so a second render arriving
// mid-collect returns (degraded) instead of parking on sync.Mutex.Lock for the
// whole pipeline — the exact wait the live goroutine dump showed at
// overview_cache.go:68.
func TestPerfR3R2_RenderDoesNotQueueBehindCollect(t *testing.T) {
	g, _, _ := slowProjectGen(t, 400*time.Millisecond)
	g.SetRenderBudget(60 * time.Millisecond)

	// Render 1 starts the collect and degrades.
	var inFlight strings.Builder
	go func() { _ = g.GenerateParams(&inFlight, nil) }()
	time.Sleep(20 * time.Millisecond) // let the collect get going

	start := time.Now()
	var second strings.Builder
	if err := g.GenerateParams(&second, nil); err != nil {
		t.Fatalf("second render: %v", err)
	}
	elapsed := time.Since(start)
	// Joining an in-flight collect is bounded by the budget too.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("a render arriving mid-collect waited %s (budget 60ms) — it queued behind the collect", elapsed)
	}
	waitForSnapshot(t, g)
}

// TestPerfR3R2_CIRWarmIsBounded pins the fan-out bound that the live dump
// caught holding the render: the dispatch phase stops at the time budget /
// count cap instead of running one `gh run list` per workdir.
func TestPerfR3R2_CIRWarmIsBounded(t *testing.T) {
	var started atomic.Int64
	g := &Generator{
		ciCache: make(map[string]ciCacheEntry),
		ciTTL:   60 * time.Second,
		ciRunner: func(string) string {
			started.Add(1)
			time.Sleep(40 * time.Millisecond)
			return "success"
		},
	}
	workdirs := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		workdirs = append(workdirs, "/repo/"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	start := time.Now()
	n := g.warmCIConclusionsWithin(context.Background(), workdirs)
	elapsed := time.Since(start)

	if n >= len(workdirs) {
		t.Fatalf("warm pass fetched all %d workdirs — the fan-out is unbounded again", n)
	}
	if n > ciWarmMaxPerPass {
		t.Errorf("warm pass started %d fetches, cap is %d", n, ciWarmMaxPerPass)
	}
	if int(started.Load()) > ciWarmMaxPerPass {
		t.Errorf("runner was invoked %d times, cap is %d", started.Load(), ciWarmMaxPerPass)
	}
	// Budget + at most one in-flight fetch per slot (concurrency 8 × 40ms).
	if elapsed > ciWarmDispatchBudget+2*time.Second {
		t.Errorf("warm pass took %s, budget %s + in-flight slack", elapsed, ciWarmDispatchBudget)
	}
}

// TestPerfR3R2_CIRWarmCoverageConverges proves the bounded pass still makes
// progress across passes: the entries it skipped are picked up next time
// (staleness-ordered), so coverage converges instead of re-fetching the head.
func TestPerfR3R2_CIRWarmCoverageConverges(t *testing.T) {
	var mu sync.Mutex
	served := map[string]int{}
	g := &Generator{
		ciCache: make(map[string]ciCacheEntry),
		ciTTL:   60 * time.Second,
		ciRunner: func(wd string) string {
			mu.Lock()
			served[wd]++
			mu.Unlock()
			return "success"
		},
	}
	workdirs := make([]string, 0, 512)
	for i := 0; i < 512; i++ {
		workdirs = append(workdirs, "/repo/wd-"+string(rune('0'+i%10))+"-"+string(rune('a'+i/10%26))+"-"+string(rune('a'+i/260)))
	}
	// Warm in successive passes (each pass sees the cache the previous filled).
	for pass := 0; pass < 6; pass++ {
		g.warmCIConclusionsWithin(context.Background(), workdirs)
	}
	g.ciMu.Lock()
	cached := len(g.ciCache)
	g.ciMu.Unlock()
	if cached < len(workdirs) {
		t.Errorf("6 bounded passes cached %d/%d workdirs — coverage does not converge", cached, len(workdirs))
	}
}

// TestPerfR3R2_EnrichRotationAdvancesByCovered pins the starvation guard's
// arithmetic: the next pass starts where this one STOPPED COVERING, so a
// budget-truncated pass hands its tail to the next pass instead of sliding one
// lane at a time.
func TestPerfR3R2_EnrichRotationAdvancesByCovered(t *testing.T) {
	g := NewGenerator(nil, nil)
	const n = 100

	// Pass 1: start 0, covered 30 (70 skipped).
	if got := g.enrichWindowStart(n); got != 0 {
		t.Fatalf("first window start = %d, want 0", got)
	}
	g.advanceEnrichRotation(0, n, 30)
	if got := g.enrichWindowStart(n); got != 30 {
		t.Fatalf("second window start = %d, want 30 (advance by covered, not by 1)", got)
	}
	// Pass 2: covers 30..59 (already-classified lanes 0..29 are re-covered
	// last), advance again.
	g.advanceEnrichRotation(30, n, 30)
	if got := g.enrichWindowStart(n); got != 60 {
		t.Fatalf("third window start = %d, want 60", got)
	}
	// Wraps.
	g.advanceEnrichRotation(90, n, 30)
	if got := g.enrichWindowStart(n); got != 20 {
		t.Fatalf("wrapped window start = %d, want 20 ((90+30)%%100)", got)
	}
	// A pass that covered everything leaves the offset unchanged (no drift).
	g.advanceEnrichRotation(0, n, n)
	if got := g.enrichWindowStart(n); got != 0 {
		t.Fatalf("full-coverage window start = %d, want 0", got)
	}
	// Degenerate sizes never index out of range.
	if got := g.enrichWindowStart(0); got != 0 {
		t.Errorf("n=0 start = %d, want 0", got)
	}
	if got := g.enrichWindowStart(1); got != 0 {
		t.Errorf("n=1 start = %d, want 0", got)
	}
}

// TestPerfR3R2_EnrichDeadlineSkipsAndConverges is the integration half: with
// the work deadline already spent, every lane skips the expensive work (and
// says so), and successive passes walk a different window.
func TestPerfR3R2_EnrichDeadlineSkipsAndConverges(t *testing.T) {
	g, _, _ := slowProjectGen(t, 0)
	// Three lanes so the rotation is observable.
	for i := 1; i < 3; i++ {
		r3r2CreateProject(t, g.db, "lane"+string(rune('0'+i)), t.TempDir())
	}
	data := g.collect(context.Background())
	if len(data.Projects) != 3 {
		t.Fatalf("expected 3 lanes, got %d", len(data.Projects))
	}
	// A deadline in the past: every lane must take the skip branch.
	past := g.clock().Now().Add(-time.Second)
	skip := g.enrichProjectsWithin(data.Projects, nil, nil, nil, past)
	if skip != len(data.Projects) {
		t.Fatalf("skipped %d lanes with a spent deadline, want %d", skip, len(data.Projects))
	}
	// The rotation advanced by what the pass would have covered (1 lane, the
	// minimum progress guarantee), so the next pass starts elsewhere.
	if got := g.enrichWindowStart(len(data.Projects)); got == 0 {
		t.Errorf("rotation did not advance after a fully-skipped pass")
	}
}

// TestPerfR3R2_TraceNamesInflightStage is AC4's data half: the trace the HTTP
// middleware logs must name the stage that was STILL RUNNING when the render
// gave up (the field that turns "79s" into "ci-warm").
func TestPerfR3R2_TraceNamesInflightStage(t *testing.T) {
	tr := NewRenderTrace(nil)
	ctx := WithRenderTrace(context.Background(), tr)

	done := stageTimer(ctx, "db-projects")
	done()
	_ = stageTimer(ctx, "ci-warm") // started, never finished (the render gave up)

	sum := tr.Summary()
	if !strings.Contains(sum, "wait=ci-warm") || !strings.Contains(sum, "in-flight") {
		t.Fatalf("summary must name the in-flight stage; got %q", sum)
	}
	if !strings.Contains(sum, "stages=db-projects") {
		t.Errorf("summary must list completed stages; got %q", sum)
	}

	tr.MarkServedStale(84*time.Second, "collect exceeded the 2.5s render budget")
	sum = tr.Summary()
	if !strings.Contains(sum, "served=stale") || !strings.Contains(sum, "1m24s") {
		t.Errorf("summary must record the stale serve and its age; got %q", sum)
	}
}

// TestPerfR3R2_CollectRecordsStages proves collect() actually marks the
// pipeline stages the slow line prints (without them the summary would be
// empty for the surface that matters).
func TestPerfR3R2_CollectRecordsStages(t *testing.T) {
	g, _, _ := slowProjectGen(t, 0)
	tr := NewRenderTrace(nil)
	ctx := WithRenderTrace(context.Background(), tr)

	data := g.collect(ctx)
	if data.TotalProjects != 1 {
		t.Fatalf("collect returned %d projects, want 1", data.TotalProjects)
	}
	sum := tr.Summary()
	for _, want := range []string{"db-projects", "db-samples", "ci-warm", "enrich", "db-tail"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing stage %q; got %q", want, sum)
		}
	}
	if strings.Contains(sum, "in-flight") {
		t.Errorf("a completed collect must not report an in-flight stage; got %q", sum)
	}
}

// TestPerfR3R2_CollectUsesCacheOnlyCI proves the enrichment loop no longer
// launches a subprocess per lane: an unwarmed workdir renders unknown instead
// of fetching inside the render.
func TestPerfR3R2_CollectUsesCacheOnlyCI(t *testing.T) {
	var calls atomic.Int64
	g, _, _ := slowProjectGen(t, 0)
	g.ciRunner = func(string) string {
		calls.Add(1)
		return "failure"
	}
	// ciTTL 0 disables the warm pass's usefulness: every entry is stale, so the
	// warm pass still runs — the point is the ENRICHMENT read, so shut the warm
	// pass out by pre-filling a cache the read can use, then counting calls.
	g.ciMu.Lock()
	g.ciCache = map[string]ciCacheEntry{}
	g.ciMu.Unlock()
	g.ciTTL = time.Nanosecond
	_ = g.collect(context.Background())
	before := calls.Load()
	if before == 0 {
		t.Skip("warm pass made no fetches in this configuration")
	}
	// Now the enrichment read must not add fetches on top of the warm pass:
	// run collect with a cache known to hold nothing warm and a bucket that
	// the warm pass cannot reach.
	g.ciMu.Lock()
	g.ciCache = map[string]ciCacheEntry{"/nonexistent": {conclusion: "success", fetchedAt: time.Now()}}
	g.ciMu.Unlock()
	calls.Store(0)
	data := g.collect(context.Background())
	var unknown int
	for _, row := range data.Projects {
		if row.CIConclusion == "" {
			unknown++
		}
	}
	if unknown != len(data.Projects) {
		t.Errorf("want every lane unknown from the cache-only read, got %d/%d unknown", unknown, len(data.Projects))
	}
	// The bounded warm pass may fetch for the one real workdir; what must not
	// happen is a fetch per enrichment row.
	if got := calls.Load(); got > 1 {
		t.Errorf("enrichment launched %d fetches for %d lanes — the per-lane fallback is back", got, len(data.Projects))
	}
}
