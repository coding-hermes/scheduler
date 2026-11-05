package dashboard

import (
	"context"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ── PERF-001-R3-R3: boot warm-up for the fleet-overview snapshot ──────────
//
// Measured on the live-sized 16.7 GB boot probe (ev2/rework1): with DB reads
// off the serialized connection, the FIRST overview collect still could not
// finish inside the 2.5s render budget, because collect() is a pipeline
// whose tail is SUBPROCESS work, not DB work —
//
//	stages=db-projects[820ms],db-lanes[26ms],fleet-learned[451ms],
//	       ci-warm[1.2s,in-flight]  → served=partial (budget 2.5s)
//
// ci-warm dispatches at most ciWarmMaxPerPass `gh` probes per pass and the
// enrich stage runs the per-lane git classification under its own work
// deadline, so on an 880-lane fleet the first pass needs ~5-10s while every
// boot-window render waits at most 2.5s. No request-side change can fix
// that; the pipeline has to be warm BEFORE the first render. This file adds
// the boot warm-up: the daemon runs a few full collects back-to-back in the
// background right after wiring (detached from any request), storing each
// result through the SAME snapshot protocol a request-driven collect uses,
// until the subprocess caches stop growing.
//
// After warm-up, a boot-window render finds a fresh snapshot (CacheAge
// stamped honestly) or joins the in-flight warm collect and is served full
// when it lands — the boot.log `served=partial` lines disappear because the
// data is already there, not because the budget moved (it did not; the
// budget constants are untouched).

// warmOverviewBootMaxPasses is the pass cap: 880-lane fleets converge the
// ciCache at ciWarmMaxPerPass (256) per pass, so four passes cover the live
// fleet with margin; smaller fleets converge in one or two and exit early.
const warmOverviewBootMaxPasses = 4

// warmOverviewBootGap is the wall-clock breather between passes, so a busy
// host (and the boot probe's own traffic) interleaves with the warm pass's
// subprocess work instead of being starved by it.
const warmOverviewBootGap = 200 * time.Millisecond

// WarmOverviewBoot primes the fleet-overview snapshot at boot. It runs up to
// maxPasses full collects back-to-back (<= 0 selects the default), stops
// early once a pass adds no new CI-cache entries (the subprocess caches have
// converged — later passes would re-render the same data), and returns when
// ctx is cancelled. Call it on its own goroutine: this blocks until the
// passes finish.
//
// Concurrency: each pass takes the snapshot's in-flight marker, so a
// boot-window render JOINS the warm collect (wait bounded by its own render
// budget) instead of starting a second collect over the same DB — the same
// one-collect-at-a-time invariant cachedOverview enforces.
//
// The inter-pass gap reads the WALL clock (clock.Real), for the same reason
// StartHealthRefresher does: the daemon's clock seam can be sim-scaled, and
// a sim-scaled 200ms gap would either never fire (driven clock) or fire a
// thousand times too fast (auto scale) — both wrong for an operational
// breather that exists to pace real subprocess work.
func (g *Generator) WarmOverviewBoot(ctx context.Context, maxPasses int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxPasses <= 0 {
		maxPasses = warmOverviewBootMaxPasses
	}
	for i := 0; i < maxPasses; i++ {
		if ctx.Err() != nil {
			return
		}
		before := g.ciCacheLen()
		passStart := clock.Real().Now()
		if i == 0 {
			// Pass 1 is the boot fast pass: DB core only, so the first
			// snapshot lands inside the render budget and the boot-window
			// renders serve a full lane table instead of degrading. Passes
			// 2+ run the complete pipeline (ci-warm + enrichment) and
			// converge the subprocess caches.
			g.warmOverviewPass(ctx, collectOpts{fastPass: true})
		} else {
			g.warmOverviewPass(ctx)
		}
		after := g.ciCacheLen()
		log.Printf("OVERVIEW-WARM: pass %d/%d took %s (ciCache %d→%d)", i+1, maxPasses, clock.Real().Since(passStart).Round(time.Millisecond), before, after)
		if after == before && i > 0 {
			// This pass grew the CI cache by zero entries: the subprocess
			// caches have converged and further passes re-render the same
			// snapshot for nobody.
			return
		}
		if i == maxPasses-1 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.Real().After(warmOverviewBootGap):
		}
	}
}

// warmOverviewPass runs one collect under the snapshot's in-flight protocol:
// take the marker (or join whoever holds it), run collect, store the result
// through the generation check exactly like cachedOverview's goroutine, and
// release the marker. A cancelled ctx (daemon shutting down) never stores —
// a torn snapshot must not outlive the pass that built it.
func (g *Generator) warmOverviewPass(ctx context.Context, opts ...collectOpts) {
	g.snap.mu.Lock()
	if g.snap.inflight {
		// A request-driven collect is running: let it finish; this pass
		// would only duplicate the work.
		done := g.snap.done
		g.snap.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	gen := g.snap.gen
	done := make(chan struct{})
	g.snap.inflight = true
	g.snap.done = done
	g.snap.mu.Unlock()

	data := g.collect(ctx, opts...)

	g.snap.mu.Lock()
	if g.snap.gen == gen && ctx.Err() == nil {
		g.snap.data = data
		g.snap.stamp = g.clock().Now()
	}
	g.snap.inflight = false
	g.snap.done = nil
	g.snap.mu.Unlock()
	close(done)
}

// ciCacheLen reports the CI-conclusion cache size. It is the warm-up loop's
// convergence signal: a pass that added no entries has nothing left to
// probe.
func (g *Generator) ciCacheLen() int {
	g.ciMu.Lock()
	defer g.ciMu.Unlock()
	return len(g.ciCache)
}
