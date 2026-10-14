package dashboard_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1730: the overview snapshot cache. collect() costs 4.58s cold /
// 1.04s warm on a copy of the live DB and the 10s htmx autorefresh re-runs
// it on every render, so the two overview entry points serve a 60s-TTL
// deep-copied snapshot (overview_cache.go). These tests pin the four
// behaviours the cache must keep: hits within the window, expiry at the
// window edge, invalidation on SetDB, and copy isolation (a render's sort
// must never corrupt the snapshot).

// newOverviewClockGen builds a generator on a sim clock, cache armed.
func newOverviewClockGen(t *testing.T, db *sql.DB) (*dashboard.Generator, *clock.SimClock) {
	t.Helper()
	sc := clock.NewSimClockAt(1, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	g := dashboard.NewGenerator(db, nil)
	g.SetClock(sc)
	return g, sc
}

func overviewRender(t *testing.T, g *dashboard.Generator) string {
	t.Helper()
	var buf strings.Builder
	if err := g.GenerateParams(&buf, nil); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	return buf.String()
}

// TestOverviewCache_HitServesSnapshotWithinTTL: a render inside the reuse
// window serves the snapshot — data added after the collect does NOT
// appear, and the page says it is cached with the snapshot's age.
// mustCostedTick seeds a completed tick carrying cost + a completion stamp
// (the cost-series window queries key on completed_at and cost_usd, both of
// which the shared mustCreateTick helper leaves zero).
func mustCostedTick(t *testing.T, db *sql.DB, id, projectName string, at time.Time, cost float64) {
	t.Helper()
	stamp := at.UTC().Format(time.RFC3339)
	if err := database.CreateTick(context.Background(), db, &database.Tick{
		ID:          id,
		ProjectName: projectName,
		Status:      database.StatusCompleted,
		Outcome:     database.OutcomeCommitted,
		SpawnedAt:   stamp,
		CompletedAt: stamp,
		CostUSD:     cost,
	}); err != nil {
		t.Fatalf("CreateTick %s: %v", id, err)
	}
}

func TestOverviewCache_HitServesSnapshotWithinTTL(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	g, sc := newOverviewClockGen(t, db)

	first := overviewRender(t, g)
	if !strings.Contains(first, `href="/projects/alpha"`) {
		t.Fatalf("first render missing alpha (pre-condition): %s", snippet(first, "projects"))
	}
	if strings.Contains(first, `id="cacheAge"`) {
		t.Errorf("fresh collect must not claim a snapshot age")
	}

	// New lane lands + the clock advances 30s: still inside the 60s window.
	mustCreateProject(t, db, "beta", 10, 5)
	sc.Advance(30 * time.Second)
	cached := overviewRender(t, g)
	if strings.Contains(cached, `href="/projects/beta"`) {
		t.Errorf("cached render served post-collect lane beta — snapshot reused data changed under it")
	}
	if !strings.Contains(cached, `id="cacheAge"`) || !strings.Contains(cached, "snapshot cached 30s") {
		t.Errorf("cached render must stamp the snapshot age; got: %s", snippet(cached, "Generated "))
	}
}

// TestOverviewCache_TTLExpiryRerunsCollect: past the window the next render
// re-runs collect() — the post-collect lane appears and the age marker is
// gone (a fresh serve is not a cached one).
func TestOverviewCache_TTLExpiryRerunsCollect(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	g, sc := newOverviewClockGen(t, db)

	if _, err := overviewRender(t, g), error(nil); err != nil {
		t.Fatalf("first render: %v", err)
	}
	mustCreateProject(t, db, "beta", 10, 5)
	sc.Advance(30 * time.Second)
	if out := overviewRender(t, g); !strings.Contains(out, `id="cacheAge"`) {
		t.Fatalf("pre-condition: render at +30s must be a cached serve")
	}
	sc.Advance(40 * time.Second) // 70s since the collect — window expired
	fresh := overviewRender(t, g)
	if !strings.Contains(fresh, `href="/projects/beta"`) {
		t.Errorf("expired window must re-run collect; beta lane missing")
	}
	if strings.Contains(fresh, `id="cacheAge"`) {
		t.Errorf("fresh collect after expiry must not claim a snapshot age")
	}
}

// TestOverviewCache_InvalidateOnSetDB: rewiring the generator to a new DB
// must not serve the old DB's snapshot.
func TestOverviewCache_InvalidateOnSetDB(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	g, sc := newOverviewClockGen(t, db)
	overviewRender(t, g) // fills the snapshot from db

	db2 := newTestDB(t)
	mustCreateProject(t, db2, "omega", 10, 5)
	g.SetDB(db2)

	out := overviewRender(t, g)
	if !strings.Contains(out, `href="/projects/omega"`) {
		t.Errorf("SetDB must invalidate the snapshot; omega (new DB) missing")
	}
	if strings.Contains(out, `href="/projects/alpha"`) {
		t.Errorf("SetDB must invalidate the snapshot; old-DB lane alpha still served")
	}
	if strings.Contains(out, `id="cacheAge"`) {
		t.Errorf("post-invalidation render is a fresh collect, not a cached serve")
	}
	_ = sc
}

// TestOverviewCache_CopyIsolation: mutating a served copy — including the
// per-row cost series — must leave the generator's snapshot untouched.
func TestOverviewCache_CopyIsolation(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	mustCostedTick(t, db, "alpha-tick", "alpha", time.Now().UTC(), 12.5)
	g, _ := newOverviewClockGen(t, db)

	snap := g.OverviewDataForTest(context.Background()) // fills the cache (fresh serve)
	if len(snap.Projects) != 1 || len(snap.Projects[0].CostSeries) == 0 {
		t.Fatalf("pre-condition: expected one project with a cost series; got %+v", snap.Projects)
	}
	want := snap.Projects[0].CostSeries[0]

	out1 := g.OverviewDataForTest(context.Background()) // cached serve
	if out1.CacheAge == "" {
		t.Errorf("cached serve must carry the snapshot age label")
	}
	out1.Projects[0].CostToday = 999.0
	out1.Projects[0].CostSeries[0] = 777.0
	out1.Projects[0].Completed = 4242

	out2 := g.OverviewDataForTest(context.Background()) // cached serve again
	if out2.Projects[0].CostToday == 999.0 {
		t.Errorf("row-level mutation of a served copy leaked into the snapshot")
	}
	if out2.Projects[0].Completed == 4242 {
		t.Errorf("scalar mutation of a served copy leaked into the snapshot")
	}
	if out2.Projects[0].CostSeries[0] == 777.0 || out2.Projects[0].CostSeries[0] != want {
		t.Errorf("cost-series mutation leaked into the snapshot: got %v, want %v",
			out2.Projects[0].CostSeries[0], want)
	}
	// The snapshot itself (pre-clone value) must also still hold the data.
	if snap.Projects[0].CostSeries[0] != want {
		t.Errorf("first serve was an alias, not a copy: snapshot cost now %v", snap.Projects[0].CostSeries[0])
	}
}

// TestOverviewCache_SecondRenderRunsNoQueries: the regression this cache
// exists for — a render inside the window costs ZERO collect queries. The
// full page still runs exactly ONE query after the cache (the
// distinct-lane dropdown query in GenerateParams, outside collect), so the
// assertion is == 1 with that named exception.
func TestOverviewCache_SecondRenderRunsNoQueries(t *testing.T) {
	db, queryCount := newQueryCountingTestDB(t)
	for i := 0; i < 3; i++ {
		mustCreateProject(t, db, "proj-"+string(rune('a'+i)), 1, 1)
	}
	sc := clock.NewSimClockAt(1, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	g := dashboard.NewGenerator(db, nil)
	g.SetClock(sc)

	overviewRender(t, g) // cold: fills the snapshot
	cold := queryCount.Load()
	if cold == 0 {
		t.Fatalf("cold render executed 0 queries — the counting DB is not counting")
	}

	queryCount.Store(0)
	sc.Advance(10 * time.Second) // the htmx autorefresh re-render
	overviewRender(t, g)
	if got := queryCount.Load(); got != 1 {
		t.Errorf("cached render executed %d queries, want 1 (collect 0 + the distinct-lane dropdown query)", got)
	}
}
