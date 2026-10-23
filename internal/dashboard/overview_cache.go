package dashboard

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot cache for the fleet overview (SCHED-GAP-1730).
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
//
// ── PERF-001-R3-R2: the cold render is BOUNDED ────────────────────────────
//
// SCHED-GAP-1730 sized the cache for the STEADY state (the 10s autorefresh
// loop), which left the COLD state unbounded: the first render after the
// snapshot and/or the CI TTL lapsed ran the whole pipeline inline, on the
// request goroutine, with no deadline and no cap. Measured live (8ec55a6f,
// 845 lanes / 809 distinct workdirs): the first GET / after an idle window
// blocked 79.685s with ~0 request CPU while the CI warm pass ran ~809 `gh
// run list` subprocesses 8-wide (each up to the 2s ciFetchTimeout) and the
// batched tick queries built temp b-trees over the 3.9GB DB. Warm hits were
// 5-6ms. That is a wait, not compute — and the daemon logged nothing.
//
// Two changes, both in this file:
//
//  1. BUDGET. collect() runs on its own goroutine and the render waits at
//     most overviewBudget() for it (default 2.5s; SCHEDULER_DASHBOARD_
//     RENDER_BUDGET, 0/negative = unbounded escape hatch). A first paint
//     therefore cannot block longer than the budget.
//
//  2. DEGRADE, EXPLICITLY. When the budget trips the render serves the
//     PREVIOUS snapshot (stamped with its own age, marked refresh-pending)
//     or, when no snapshot exists yet (the very first render after boot),
//     a partial page that says so and names the stage that was still
//     running. The collect is NOT abandoned: it keeps running detached
//     from the request and stores its result for the next render, so
//     serving stale converges instead of thrashing.
//
// The collect goroutine is detached with context.WithoutCancel on purpose: a
// client that disconnects (or a budget that trips) must not leave a
// half-built snapshot populated from context-cancelled queries, which would
// cache garbage for the whole TTL window.

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

// PERF-001-R3-R2 render budget.
//
// overviewBudgetDefault is how long a request may wait for a cold collect
// before the render degrades. It is a WAIT bound, not a work bound: the
// collect keeps running in the background and the next render serves the
// result. 2.5s sits above the measured warm render (5-6ms) and above a
// healthy cold collect on the test fleets, and below any operator's idea of
// "the dashboard is stuck" - the pre-fix live shape was 79.7s.
const (
	overviewBudgetEnv     = "SCHEDULER_DASHBOARD_RENDER_BUDGET"
	overviewBudgetDefault = 2500 * time.Millisecond
)

// overviewBudgetResolved caches the env resolution: a per-request getenv would
// be a syscall on every render. sync.Once because the value is process
// configuration, not per-generator state (per-generator override is
// renderBudget, set by tests through SetRenderBudget).
var (
	overviewBudgetOnce sync.Once
	overviewBudgetVal  time.Duration
)

// resolveOverviewBudget maps the env spelling to a budget. Unset/unparsable
// falls back to the default (an operator typo must not silently disable the
// bound); an explicit 0 or negative means "unbounded" - the documented escape
// hatch for an operator who wants the old inline behavior back.
func resolveOverviewBudget(raw string, testBinary bool) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Test binaries keep the historical inline behavior unless a test
		// opts in: the suite's DB fixtures are tiny and complete in
		// microseconds, and a loaded host must never turn a passing render
		// assertion into a flaky partial page. Tests that exercise the bound
		// set it explicitly (SetRenderBudget) - the same test-binary carve-out
		// blockprofile.go makes for the profiling rate.
		if testBinary {
			return 0
		}
		return overviewBudgetDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return overviewBudgetDefault
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// ovSnapshot is the generator's single cached collect() result plus its
// fetch stamp and the in-flight marker that keeps a cold fleet to ONE collect
// at a time.
//
// One entry, one mutex: the overview is a singleton page. The mutex is NEVER
// held across collect() (PERF-001-R3-R2): it is held only for the snapshot
// read/copy and the in-flight bookkeeping, so a render that degrades on the
// budget does not queue behind the collect it just gave up waiting for.
type ovSnapshot struct {
	mu    sync.Mutex
	data  FleetData
	stamp time.Time // g.clock() instant the collect ran at
	// inflight marks a collect running for generation gen. Joiners wait on
	// done (bounded by their own budget) instead of starting a second
	// collect over the same DB.
	inflight bool
	done     chan struct{}
	// gen is bumped by expireOverview(): a collect that started under an old
	// generation must not store its result over a snapshot collected from
	// the new inputs (SetDB wiring a different DB is the live case).
	gen uint64
}

// cachedOverview returns the fleet overview dataset, serving a deep copy of the
// cached collect() result while it is younger than the reuse window and
// running collect() (bounded, see the file header) on a miss or expiry. The
// returned value is owned by the caller: renders sort and filter it in
// place, so it is ALWAYS a copy.
//
// CacheAge is stamped on cached serves ("30s" for a snapshot fetched 30s
// ago) and left empty on a fresh collect - the template renders the age so
// a cached page says it is cached. A serve that degraded on the render
// budget also sets RefreshPending (stale snapshot) or Partial (no snapshot
// yet), and the trace in ctx records which stage was still running.
func (g *Generator) cachedOverview(ctx context.Context) FleetData {
	now := g.clock().Now()
	budget := g.overviewBudget()
	if rt := RenderTraceFrom(ctx); rt != nil {
		rt.SetBudget(budget)
	}

	g.snap.mu.Lock()
	// Fresh snapshot: the steady state (the 10s autorefresh) never collects.
	if !g.snap.stamp.IsZero() && g.snap.stamp.Add(g.overviewTTLFor()).After(now) {
		data := cloneFleetData(g.snap.data)
		data.CacheAge = ageLabel(now.Sub(g.snap.stamp))
		g.snap.mu.Unlock()
		return data
	}
	// Expired. Join an in-flight collect rather than starting a second one.
	if g.snap.inflight {
		done := g.snap.done
		g.snap.mu.Unlock()
		// Name the wait: without this span a joining render's slow line would
		// carry no `wait=` field at all, and "which request was waiting on
		// whose collect" is exactly what the row's dump had to answer by hand.
		endAwait := stageTimer(ctx, "await-collect")
		defer endAwait()
		select {
		case <-done:
			// The other collect finished; its result is in the snapshot.
			g.snap.mu.Lock()
			if !g.snap.stamp.IsZero() {
				data := cloneFleetData(g.snap.data)
				if age := g.clock().Since(g.snap.stamp); age > time.Duration(0) {
					data.CacheAge = ageLabel(age)
				}
				g.snap.mu.Unlock()
				return data
			}
			g.snap.mu.Unlock()
		case <-g.budgetTimer(budget):
		case <-ctx.Done():
		}
		return g.degradeOverview(ctx, budget, "waiting for the overview collect already in flight")
	}

	// Start the collect on its own goroutine so the wait is boundable.
	gen := g.snap.gen
	done := make(chan struct{})
	g.snap.inflight = true
	g.snap.done = done
	g.snap.mu.Unlock()

	resCh := make(chan FleetData, 1)
	go func() {
		// Detached from the request: a client disconnect or a tripped budget
		// must not populate the snapshot with context-cancelled (zeroed)
		// results. Values still flow, so the render trace keeps recording.
		data := g.collect(context.WithoutCancel(ctx))
		g.snap.mu.Lock()
		if g.snap.gen == gen {
			g.snap.data = data
			g.snap.stamp = g.clock().Now()
		}
		g.snap.inflight = false
		g.snap.done = nil
		g.snap.mu.Unlock()
		close(done)
		resCh <- data
	}()

	select {
	case data := <-resCh:
		return cloneFleetData(data)
	case <-g.budgetTimer(budget):
		return g.degradeOverview(ctx, budget, fmt.Sprintf("collect exceeded the %s render budget", roundDur(budget)))
	case <-ctx.Done():
		// Distinguish an operator/client disconnect from the budget: the
		// handlers derive a context deadline from the same budget, so a plain
		// ctx.Done() at budget time is NOT a cancellation and must not be
		// reported as one.
		why := "client disconnected before the collect finished"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && budget > 0 {
			why = fmt.Sprintf("collect exceeded the %s render budget", roundDur(budget))
		}
		return g.degradeOverview(ctx, budget, why)
	}
}

// budgetTimer returns a channel that fires after budget, or a nil channel
// (never fires) when the budget is disabled. It rides the generator's CLOCK
// SEAM so a sim clock drives it exactly like every other wait in the daemon.
func (g *Generator) budgetTimer(budget time.Duration) <-chan time.Time {
	if budget <= 0 {
		return nil
	}
	return g.clock().After(budget)
}

// degradeOverview builds the page served when the render budget tripped: the
// previous snapshot (explicitly aged + refresh-pending) when one exists, else
// a partial page that says the first collect is still running and names what
// it was doing. Both cases mark the trace so the slow-request log line carries
// the reason instead of a bare duration.
func (g *Generator) degradeOverview(ctx context.Context, budget time.Duration, why string) FleetData {
	note := why
	g.snap.mu.Lock()
	stale := !g.snap.stamp.IsZero()
	var data FleetData
	var age time.Duration
	if stale {
		data = cloneFleetData(g.snap.data)
		age = g.clock().Since(g.snap.stamp)
	}
	g.snap.mu.Unlock()

	rt := RenderTraceFrom(ctx)
	if stale {
		data.CacheAge = ageLabel(age)
		data.RefreshPending = true
		data.RefreshNote = fmt.Sprintf("refresh in progress: %s; showing the %s-old snapshot and refreshing in the background.", note, ageLabel(age))
		if rt != nil {
			rt.MarkServedStale(age, note)
		}
		return data
	}

	// No snapshot at all (first render after boot, or right after an
	// invalidation): render the shell honestly instead of blocking.
	data = FleetData{
		Title:       "Fleet Overview",
		GeneratedAt: g.clock().Now().Format(time.RFC3339),
		Partial:     true,
		PartialNote: fmt.Sprintf("overview still collecting: %s. This page is incomplete - cards and tables fill in on the next refresh (the 10s autorefresh, or reload now).", note),
		BudgetTotal: 100,
	}
	if b := g.weightBudget; b > 0 {
		data.BudgetTotal = b
	}
	if rt != nil {
		rt.MarkPartial(note)
	}
	return data
}

// overviewBudget is the effective render budget for THIS generator: its own
// renderBudget when set (tests), else the process resolution. The field is
// atomic because the render path reads it without a lock and tests set it
// after construction.
func (g *Generator) overviewBudget() time.Duration {
	if v := g.renderBudget.Load(); v != 0 {
		if v < 0 {
			return 0 // negative = explicitly unbounded
		}
		return time.Duration(v)
	}
	overviewBudgetOnce.Do(func() {
		overviewBudgetVal = resolveOverviewBudget(os.Getenv(overviewBudgetEnv), inTestBinary())
	})
	return overviewBudgetVal
}

// SetRenderBudget overrides the render budget for this generator (tests, and
// an operator-facing knob if it ever needs one). 0 leaves the process
// resolution in place; a negative value means "unbounded" (the old inline
// behavior) and exists so a test can prove the pre-fix shape.
func (g *Generator) SetRenderBudget(d time.Duration) {
	if d < 0 {
		g.renderBudget.Store(-1)
		return
	}
	g.renderBudget.Store(int64(d))
}

// expireOverview drops the cached collect() result. Called whenever one of the
// generator's inputs changes (SetDB, SetWeightBudget, SetClock) so a
// reconfigured generator never serves a snapshot collected under the old
// input. Cheap when nothing is cached.
//
// PERF-001-R3-R2: it also bumps the generation and clears the in-flight
// marker. Bumping makes a collect that started under the old inputs discard
// its result instead of storing it over the new configuration's snapshot;
// clearing the marker lets the very next render start a collect immediately
// (waiting on the old one would serve data from the inputs that were just
// replaced).
func (g *Generator) expireOverview() {
	g.snap.mu.Lock()
	g.snap.data = FleetData{}
	g.snap.stamp = time.Time{}
	g.snap.gen++
	g.snap.inflight = false
	g.snap.done = nil
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

// inTestBinary reports whether this process is a `go test` binary. The flag
// package registers "test.v" in every test binary and nowhere else. It gates
// only the DEFAULT render budget (resolveOverviewBudget): the suite's fixtures
// are tiny and complete in microseconds, and a co-loaded host must never turn
// a passing render assertion into a flaky partial page. Tests that exercise
// the bound opt in with SetRenderBudget. Same carve-out shape as
// internal/scheduler's blockprofile.go.
func inTestBinary() bool { return flag.Lookup("test.v") != nil }
