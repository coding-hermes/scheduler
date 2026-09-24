package dashboard

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// TTL cache for the GitReins history walk (SCHED-GAP-1576). readGitReins
// re-reads .gitreins/history/<date>/<sha>/verdict.json for every project on
// every cache-cold render, which dominated the dashboard render profile at
// fleet scale (~21.7% of CPU under load). The walk is idempotent for a given
// tree state, so it is cached per (workdir, maxLatest) and invalidated by the
// history tree's mtime.

// gitReinsCacheTTLDefault is how long a walked summary is reused before the
// history tree is walked again. It must exceed the cold-render cost, otherwise
// every render is cold (the warm path becomes the new cold path).
const gitReinsCacheTTLDefault = 60 * time.Second

// gitReinsCacheTTL is the effective reuse window. It is a var rather than a
// const for exactly one reason: the cache is a package-level singleton with no
// owner to inject a duration (or a clock) into, so a test that needs to observe
// expiry shrinks it here. Production code never assigns it — the same
// test-injection shape as Generator.ciTTL.
var gitReinsCacheTTL = gitReinsCacheTTLDefault

// gitReinsCacheMaxEntries caps the cache; gitReinsCacheEvictBatch is how many
// of the oldest entries are dropped when the cap is exceeded. Deliberately
// simple: no LRU bookkeeping, a batch eviction, cheapest thing that bounds
// memory across ~480 lanes without thrashing on every insert.
const (
	gitReinsCacheMaxEntries = 256
	gitReinsCacheEvictBatch = 32
)

// gitReinsCacheEntry is one cached walk result.
type gitReinsCacheEntry struct {
	summary   GitReinsSummary
	mtime     time.Time // history-tree stamp the summary was walked from
	fetchedAt time.Time // when the walk happened (wall clock)
}

// gitReinsCache is the process-wide singleton cache.
var gitReinsCache = struct {
	sync.Mutex
	m map[string]gitReinsCacheEntry
}{}

func init() {
	gitReinsCache.m = make(map[string]gitReinsCacheEntry)
}

// cachedReadGitReins returns readGitReins(workdir, maxLatest), serving a
// previously walked summary while the history tree is unchanged and the entry
// is younger than gitReinsCacheTTL. On a miss, an mtime change, or an expired
// entry it falls through to the real walker (which stays the only code that
// parses verdicts) and refreshes the cache.
//
// The cache is best-effort: a workdir with no .gitreins/history is not cached
// at all (the walk is one failed stat, and caching the absence would hide a
// history tree that appears a moment later).
func cachedReadGitReins(workdir string, maxLatest int) GitReinsSummary {
	if workdir == "" {
		return readGitReins(workdir, maxLatest)
	}
	mtime, ok := gitReinsHistoryMtime(workdir)
	if !ok {
		return readGitReins(workdir, maxLatest)
	}
	key := gitReinsCacheKey(workdir, maxLatest)

	gitReinsCache.Lock()
	if e, hit := gitReinsCache.m[key]; hit && e.mtime.Equal(mtime) && clock.Real().Since(e.fetchedAt) < gitReinsCacheTTL {
		gitReinsCache.Unlock()
		return e.summary
	}
	gitReinsCache.Unlock()

	summary := readGitReins(workdir, maxLatest)

	gitReinsCache.Lock()
	gitReinsCache.m[key] = gitReinsCacheEntry{
		summary:   summary,
		mtime:     mtime,
		fetchedAt: clock.Real().Now(),
	}
	evictGitReinsCacheLocked()
	gitReinsCache.Unlock()

	return summary
}

// gitReinsCacheKey keys on the workdir AND maxLatest: the fleet enrichment pass
// asks for 0 (no latest-verdict list) while the project page asks for 12, so a
// workdir-only key would serve the 0-call's empty Latest list to the project
// page for a full TTL window.
func gitReinsCacheKey(workdir string, maxLatest int) string {
	return workdir + "\x00" + strconv.Itoa(maxLatest)
}

// gitReinsHistoryMtime returns the freshness stamp of a project's GitReins
// history tree: the later of the history directory's own mtime and the mtime of
// its newest date directory. New verdicts land as
// history/<date>/<sha>/verdict.json, so a same-day verdict adds an entry to the
// date directory and never touches history/ itself — the newest date directory
// is what makes that case invalidate. Names are YYYY-MM-DD, so the
// lexicographically largest name is the newest. ok is false when there is no
// history tree to stamp.
func gitReinsHistoryMtime(workdir string) (time.Time, bool) {
	root := filepath.Join(workdir, ".gitreins", "history")
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return time.Time{}, false
	}
	stamp := fi.ModTime()
	entries, err := os.ReadDir(root)
	if err != nil {
		return stamp, true
	}
	newest := ""
	for _, e := range entries {
		if e.IsDir() && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		return stamp, true
	}
	if dfi, err := os.Stat(filepath.Join(root, newest)); err == nil && dfi.ModTime().After(stamp) {
		stamp = dfi.ModTime()
	}
	return stamp, true
}

// evictGitReinsCacheLocked drops the oldest gitReinsCacheEvictBatch entries once
// the cache exceeds gitReinsCacheMaxEntries. The caller holds gitReinsCache's
// mutex. Batch (not per-insert) eviction keeps a fleet-wide cold render from
// paying a sort on every single insert.
func evictGitReinsCacheLocked() {
	over := len(gitReinsCache.m) - gitReinsCacheMaxEntries
	if over <= 0 {
		return
	}
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(gitReinsCache.m))
	for k, e := range gitReinsCache.m {
		all = append(all, kv{key: k, at: e.fetchedAt})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].at.Equal(all[j].at) {
			return all[i].key < all[j].key
		}
		return all[i].at.Before(all[j].at)
	})
	n := gitReinsCacheEvictBatch
	if n > len(all) {
		n = len(all)
	}
	for _, e := range all[:n] {
		delete(gitReinsCache.m, e.key)
	}
}
