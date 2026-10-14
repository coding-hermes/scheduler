package dashboard

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Snapshot cache for the fleet overview collect (SCHED-GAP-1730).
//
// Measured problem (copy of the live 78k-tick DB): a cold GET / costs
// 4.58s per op and a warm one 1.04s, and the SQL share is only ~0.5s of
// that. The dominant cost is the per-render enrichment pipeline collect()
// runs (batched window queries, per-lane board reads, tickWork git execs,
// caches, fleetLearned), re-paid on every render because the htmx
// autorefresh re-runs the same collect every 10s.
//
// The snapshot is sound for a short reuse window because the overview is a
// monitor surface, not a control one: it re-renders every 10s regardless,
// and every control path (SetDB, SetWeightBudget, SetClock - the
// generator's only inputs collect reads besides the DB) invalidates
// explicitly. Same 60s window as the GitReins cache; the tick-work and CI
// caches sit one level down in the same pipeline. The stamp comes from
// g.clock() so a simulated clock drives expiry in tests.
//
// Copy-on-read is load-bearing, not cosmetic: the render path sorts and
// filters data.Projects IN PLACE (fleetProjectFilters + fleetTableSlice),
// so handing callers the cached slice directly would let one render's sort
// corrupt the snapshot (and race concurrent renders reading it). Both the
// hit and the miss path therefore return a deep copy; the snapshot's
// slices are never touched after the store.

// overviewTTLDefault is how long a collect() result is reused. It must comfortably
// exceed a warm render (measured 1.04s) or every render goes cold again;
// 60s covers the 10s autorefresh loop ~6x and bounds overview staleness to
// a minute.
const overviewTTLDefault = 60 * time.Second

// overviewCacheTTL is the effective reuse window. Var rather than const for one
// reason only: the same test-injection shape as the GitReins cache TTL (a
// test that needs to observe expiry shrinks it). Production code never
// assigns it; per-generator override goes through snapTTL.
var overviewCacheTTL = overviewTTLDefault

// ovSnapshot is the generator's single cached collect() result plus its
// fetch stamp. One entry, one mutex: the overview is a singleton page, so
// a keyed map would be dead weight. The mutex is held across collect() on
// a miss, which serializes concurrent cold renders - deliberate: two
// overlapping collects (page load + the 10s autorefresh firing into the
// same process) would otherwise duplicate the whole enrichment pipeline.
type ovSnapshot struct {
	mu    sync.Mutex
	data  FleetData
	stamp time.Time // g.clock() instant the collect ran at
}

// cachedOverview returns the fleet overview dataset, serving a deep copy of the
// cached collect() result while it is younger than the reuse window and
// re-running collect() on a miss or expiry. The returned value is owned by
// the caller: renders sort and filter it in place, so it is ALWAYS a copy.
//
// CacheAge is stamped on cached serves ("30s" for a snapshot fetched 30s
// ago) and left empty on a fresh collect - the template renders the age so
// a cached page says it is cached.
func (g *Generator) cachedOverview(ctx context.Context) FleetData {
	now := g.clock().Now()
	g.snap.mu.Lock()
	defer g.snap.mu.Unlock()
	if !g.snap.stamp.IsZero() && g.snap.stamp.Add(g.overviewTTLFor()).After(now) {
		data := cloneFleetData(g.snap.data)
		data.CacheAge = ageLabel(now.Sub(g.snap.stamp))
		return data
	}
	data := g.collect(ctx)
	g.snap.data = data
	g.snap.stamp = now
	return cloneFleetData(data)
}

// expireOverview drops the cached collect() result. Called whenever one of the
// generator's inputs changes (SetDB, SetWeightBudget, SetClock) so a
// reconfigured generator never serves a snapshot collected under the old
// input. Cheap when nothing is cached.
func (g *Generator) expireOverview() {
	g.snap.mu.Lock()
	g.snap.data = FleetData{}
	g.snap.stamp = time.Time{}
	g.snap.mu.Unlock()
}

// overviewTTLFor is the effective reuse window for THIS generator: its own
// snapTTL when set, else the package default. snapTTL exists so a test can
// shrink one generator's window without touching the package var other
// tests share.
func (g *Generator) overviewTTLFor() time.Duration {
	if g.snapTTL > 0 {
		return g.snapTTL
	}
	return overviewCacheTTL
}

// ageLabel renders a snapshot's age for the Generated line. Seconds below
// two minutes (the window the 10s autorefresh lives in), minutes+seconds
// beyond that. Sub-second ages read as "<1s" - never "0s", which would
// read as fresh for a cached serve.
func ageLabel(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		return "<1s"
	}
	if secs < 120 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// cloneFleetData deep-copies the slice-bearing fields of a FleetData (Projects
// incl. each row's CostSeries, RecentTicks, Namespaces, NamespaceTicks).
// Scalar fields copy with the struct. TableState, Control and the dropdown
// vocabularies are NOT cached (they are applied per render after
// cachedOverview returns), so they copy shallowly and stay per-render.
func cloneFleetData(src FleetData) FleetData {
	out := src
	if src.Projects != nil {
		out.Projects = make([]FleetRow, len(src.Projects))
		copy(out.Projects, src.Projects)
		for i := range out.Projects {
			if src.Projects[i].CostSeries != nil {
				out.Projects[i].CostSeries = make([]float64, len(src.Projects[i].CostSeries))
				copy(out.Projects[i].CostSeries, src.Projects[i].CostSeries)
			}
		}
	}
	if src.RecentTicks != nil {
		out.RecentTicks = make([]TickRow, len(src.RecentTicks))
		copy(out.RecentTicks, src.RecentTicks)
	}
	if src.Namespaces != nil {
		out.Namespaces = make([]NamespaceRow, len(src.Namespaces))
		copy(out.Namespaces, src.Namespaces)
	}
	if src.NamespaceTicks != nil {
		out.NamespaceTicks = make([]NamespaceTickRow, len(src.NamespaceTicks))
		copy(out.NamespaceTicks, src.NamespaceTicks)
	}
	return out
}
