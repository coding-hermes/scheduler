package dashboard

import (
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// Per-window memo cache for tickWork (SCHED-GAP-1623).
//
// Measured problem: tickWork runs one `git log` fork/exec per completed-tick
// sample per project on every render. The fleet render re-derives "what did
// this tick work on" for the last ≤20 completed ticks of every lane
// (tickSamplesFromCompleted + predictor.go's per-sample classify), so a
// 496-lane fleet pays hundreds of git subprocesses per render — measured
// today at HEAD: /dashboard/partial 3.37s ±0.31 and GET / 3.34s ±0.77
// (criterion <1.5s; DASH-PERF-003 had verified 0.87s). The render handler
// blocks in enrichProjects' bounded pool waiting on those execs; wall time
// far exceeds render CPU.
//
// The memo is sound because a completed tick's (spawned, completed) window is
// IMMUTABLE HISTORY: the commits reachable in [since−2s, until+2s] cannot
// change unless the repo's history itself is rewritten (an ops event, not a
// render concern — the same verdict class the GitReins cache accepts). So a
// (workdir, spawned, completed, commitCount) key is memoized for a TTL
// window, and repeated renders cost zero execs for already-classified ticks.
// Only NEW samples (new ticks completing) pay the one-time exec — which is
// the natural cost the uncached baseline paid every render.
//
// Windows whose until instant was clamped to the clock's now (running ticks:
// completed == "" or completed < since) are moving windows — their answer can
// change as commits land — so they are NOT memoized (the same distinction
// gitReinsCache makes with its mtime stamp).
//
// Scope (SCHED-GAP-1623): this is a per-window memo only. It does not batch
// multiple windows into one git invocation (a larger change touching output
// shaping); the exec count per warm render drops to ~zero for steady history
// because every sample hits the memo, which satisfies the AC's "no unbounded
// os/exec per lane per render".

// tickWorkCacheTTLDefault is how long a window's classification is reused.
// It only needs to cover a render burst; the entry is invalidated for free by
// never being asked again once the sample ages out of the ≤20-completed-ticks
// query. It mirrors gitReinsCacheTTLDefault (60s) for the same reason: it
// must exceed the cold-render cost, else every render is cold.
const tickWorkCacheTTLDefault = 60 * time.Second

// tickWorkCacheTTL is the effective reuse window. Var for test injection —
// the cache is a package-level singleton with no owner to inject into (the
// same shape as gitReinsCacheTTL). Production code never assigns it.
var tickWorkCacheTTL = tickWorkCacheTTLDefault

// tickWorkCacheMaxEntries caps the memo. Sized to the MEASURED fleet, not
// guessed: the 496-lane snapshot (~750MB tick history) touches ~10k distinct
// (workdir, spawned, completed) sample keys per fleet render (≤20 completed
// samples × 496 lanes, plus the predictor's second pass over the same
// samples), so a 1,024 cap thrashes — each render's own early entries are
// evicted by its later inserts before the next render re-reads them, and
// warm renders pay every exec again (measured 2026-09-26: warm ≈ cold at
// cap 1024). Entries are ~150 bytes (key + short work string + timestamp),
// so 16k entries is ~3MB and covers a fleet several times the current size.
// tickWorkCacheEvictBatch is how many of the oldest entries drop when the
// cap+hysteresis band is exceeded (the gitReinsCache batch-eviction shape:
// no LRU bookkeeping, bounded memory). Hysteresis: eviction runs only past
// maxEntries+batch, so the common overshoot never sorts per insert.
const (
	tickWorkCacheMaxEntries = 16384
	tickWorkCacheEvictBatch = 256
)

// tickWorkCacheEntry is one cached window classification.
type tickWorkCacheEntry struct {
	work      string
	fetchedAt time.Time
}

// tickWorkCache is the process-wide singleton memo.
var tickWorkCache = struct {
	sync.Mutex
	m map[string]tickWorkCacheEntry
}{}

func init() {
	tickWorkCache.m = make(map[string]tickWorkCacheEntry)
}

// tickWorkRunner is the subprocess seam: it runs the git command tickWork
// builds. Nil means the real cmd.Output(). A var so tests can count execs and
// prove the memo served a repeat with zero execs (the SCHED-GAP-1623 AC).
var tickWorkRunner func(cmd *exec.Cmd) ([]byte, error)

// tickWorkRunnerOutput runs the seam (or the real exec) — the single point
// generator_data.go's tickWork calls through.
func tickWorkRunnerOutput(cmd *exec.Cmd) ([]byte, error) {
	if tickWorkRunner != nil {
		return tickWorkRunner(cmd)
	}
	return cmd.Output()
}

// tickWorkKey keys the memo: workdir + window bounds + cap. The ±2s clock
// slack lives INSIDE the window derivation, so the raw strings are the key.
func tickWorkKey(workdir, spawned, completed string, commitCount int) string {
	return workdir + "\x00" + spawned + "\x00" + completed + "\x00" + strconv.Itoa(commitCount)
}

// tickWorkCached is the memo-wrapped tickWork used by render call sites. The
// original tickWork stays untouched (project pages keep their semantics);
// tickSamplesFromCompleted and the predictor re-point at the memo. A fixed
// window (completed parses and is >= since) is served from / stored in the
// memo; a moving window (clamped to now) bypasses the memo entirely.
func tickWorkCached(clk clock.Clock, workdir, spawned, completed string, commitCount int) string {
	if workdir == "" || spawned == "" {
		return tickWork(clk, workdir, spawned, completed, commitCount)
	}
	since, err1 := time.Parse(time.RFC3339, spawned)
	if err1 != nil {
		return tickWork(clk, workdir, spawned, completed, commitCount)
	}
	// Fixed window iff completed parses and is not before since (no clamp).
	fixed := false
	if completed != "" {
		if until, err := time.Parse(time.RFC3339, completed); err == nil && !until.Before(since) {
			fixed = true
		}
	}
	key := tickWorkKey(workdir, spawned, completed, commitCount)
	if fixed {
		tickWorkCache.Lock()
		e, hit := tickWorkCache.m[key]
		if hit && clock.Real().Since(e.fetchedAt) < tickWorkCacheTTL {
			tickWorkCache.Unlock()
			return e.work
		}
		tickWorkCache.Unlock()
	}
	work := tickWork(clk, workdir, spawned, completed, commitCount)
	if fixed {
		tickWorkCache.Lock()
		tickWorkCache.m[key] = tickWorkCacheEntry{work: work, fetchedAt: clock.Real().Now()}
		evictTickWorkCacheLocked()
		tickWorkCache.Unlock()
	}
	return work
}

// evictTickWorkCacheLocked drops the oldest entries until the memo is at or
// under tickWorkCacheMaxEntries. Caller holds the mutex. Hysteresis: nothing
// happens until the memo exceeds cap+batch, so the common overshoot (a render
// inserting a few hundred new windows) never pays the sort; when the band IS
// exceeded, the oldest len-cap entries drop in one pass (the gitReinsCache
// batch-eviction shape: no LRU bookkeeping, bounded memory).
func evictTickWorkCacheLocked() {
	if len(tickWorkCache.m) <= tickWorkCacheMaxEntries+tickWorkCacheEvictBatch {
		return
	}
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(tickWorkCache.m))
	for k, e := range tickWorkCache.m {
		all = append(all, kv{key: k, at: e.fetchedAt})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].at.Equal(all[j].at) {
			return all[i].key < all[j].key
		}
		return all[i].at.Before(all[j].at)
	})
	drop := len(all) - tickWorkCacheMaxEntries
	for _, e := range all[:drop] {
		delete(tickWorkCache.m, e.key)
	}
}
