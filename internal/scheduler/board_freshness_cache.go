package scheduler

import (
	"os"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// Board-freshness verdict cache (PERF-002).
//
// Measured problem: at fleet scale (332 enabled projects) a cold
// /api/v1/status cost 47–130s. A 15s CPU profile of the live daemon put
// ReadBoardFreshness at 31% cumulative (gitRevVerifyCommit 22%,
// os/exec.Cmd.Start 25%): every evaluate() pass re-ran the whole git
// subprocess battery for every project whose pending-count entry had aged
// past its 60s TTL. The battery per project is one
// `git rev-parse --verify HEAD^{commit}` (gitHeadReadable) + one
// `git log -n 200` (gitRecentCommits) + a per-completed-row
// `git merge-base --is-ancestor` / `git rev-parse <sha>^{commit}` /
// `git log -1` round (board_freshness.go). At 332 projects that is
// thousands of fork/execs per pass — and the pass holds Loop.mu
// write-locked, so every API surface queues behind it.
//
// The verdict for a board whose FILE and whose HEAD are both unchanged is,
// by construction, the same verdict: the reader classifies rows against
// exactly two surfaces — the board's rows and the commit graph reachable
// from HEAD. So a read whose (repo, board, board mtime, HEAD sha) key
// matches a previous read can be served from memory.
//
// The hit path costs one os.Stat and ONE subprocess (the same
// `git rev-parse --verify HEAD^{commit}` gitHeadReadable already runs) — not
// the 5+ per completed row the full read needs.
//
// Scope (PERF-002): this is a verdict cache only. It deliberately does NOT
// restructure evaluate()'s locking — the write-lock-across-the-pass half of
// the row is a separate change.
type FreshnessVerdictCache struct {
	// clk is this component's time seam (SCHED-GAP-169): the zero value
	// reads as the WALL CLOCK, so a cache is correct before SetClock is ever
	// called and a test can drive its TTL with a simulator instead of
	// sleeping through it.
	clk clockSeam
	// ttl bounds how long an entry may be served. A non-positive TTL means
	// no entry is ever fresh (every read is a full read) — the same
	// semantics PendingTaskCounter gives a zero TTL.
	ttl time.Duration
	// max is the hard entry bound; the oldest entry is evicted first. The
	// fleet is ~332 boards, so production never reaches it — it exists so a
	// long-lived daemon on a growing fleet cannot grow the map without
	// bound.
	max int

	// verdictRead is the full-read seam: nil means the real
	// ReadBoardFreshness on this cache's clock. A field so tests can count
	// full reads (the expensive path this cache exists to avoid).
	verdictRead func(repoDir, boardPath string) FreshnessReport
	// headProbe is the key's HEAD probe: nil means gitHeadSHA, the single
	// `git rev-parse --verify HEAD^{commit}` a hit is allowed to run.
	headProbe func(repoDir string) string

	mu      sync.Mutex
	entries map[freshnessVerdictKey]freshnessVerdictEntry
	seq     uint64
	hits    int
	misses  int
	evicted int
}

const (
	// DefaultFreshnessVerdictTTL is the production verdict TTL.
	//
	// It is deliberately LONGER than the pending counter's 60s count-cache TTL
	// (board_awareness.go NewPendingTaskCounter(60 * time.Second)). The two
	// caches expire on the same clock, so an equal TTL would make the verdict
	// expire at the same instant as the count entry that holds it — every
	// count re-read would then be a cache miss and the battery would run
	// exactly as often as before. Live evaluate() passes are 3–6.5 min apart
	// (PERF-002 measurements), so the verdict must outlive one pass gap to
	// carry its weight: 10 min bridges consecutive passes.
	//
	// The TTL is only a safety net for the reader's TIME-DEPENDENT
	// classifications, which have no key component of their own: a
	// flip-window row becomes flip-overdue (and a recent-scan flip-window row
	// reverts to open) at committed_at + FlipWindow. 10 min is comfortably
	// inside DefaultFlipWindow (55 min), so a cached verdict can never be
	// stale by a whole window — and the two changes that actually move work
	// (a board edit, a landing commit) are NOT subject to this TTL at all:
	// they change the key and invalidate immediately.
	DefaultFreshnessVerdictTTL = 10 * time.Minute

	// MaxFreshnessVerdictEntries is the default entry bound. The fleet has
	// ~332 enabled projects today; 2048 leaves headroom for the per-board
	// multiplicities of a growing fleet while keeping the cache small
	// (a FreshnessReport is a row slice plus a small counts map).
	MaxFreshnessVerdictEntries = 2048
)

// freshnessVerdictKey is the complete input surface of a git-verified board
// read: the repo, the board file, the board's mtime and HEAD's sha. Two reads
// with the same key cannot produce different verdicts, so the second is served
// from the cache. Board CONTENT that changes moves the mtime; work that lands
// moves HEAD. The one classification that can move while the key stands still
// is the time-dependent flip-window one (a row ages past FlipWindow) — the TTL
// is the bound on that, and it is set well inside the window
// (DefaultFreshnessVerdictTTL).
type freshnessVerdictKey struct {
	repoDir    string
	boardPath  string
	boardMtime time.Time
	headSHA    string
}

// freshnessVerdictEntry is one cached report plus the instant it was read
// (the TTL's anchor) and its insertion order (the eviction order).
type freshnessVerdictEntry struct {
	report    FreshnessReport
	fetchedAt time.Time
	inserted  uint64
}

// NewFreshnessVerdictCache creates a verdict cache with the given TTL and
// entry bound. A non-positive maxEntries falls back to
// MaxFreshnessVerdictEntries; ttl <= 0 disables serving entirely (every read
// is a full read), matching PendingTaskCounter's zero-TTL semantics.
func NewFreshnessVerdictCache(ttl time.Duration, maxEntries int) *FreshnessVerdictCache {
	if maxEntries <= 0 {
		maxEntries = MaxFreshnessVerdictEntries
	}
	return &FreshnessVerdictCache{
		ttl:     ttl,
		max:     maxEntries,
		entries: make(map[freshnessVerdictKey]freshnessVerdictEntry),
	}
}

// Read returns the git-verified freshness report for (repoDir, boardPath),
// serving the cached verdict while the key is unchanged and the entry is
// inside its TTL.
//
//	rep := cache.Read(workdir, boardPath) // 1 exec on a hit, full read on a miss
//
// A miss (board mtime changed, HEAD moved, or TTL elapsed) runs the full
// ReadBoardFreshness and stores the verdict for the next caller. The stored
// report carries the instant it was actually read (ReadAt), not the instant it
// is served: a cached verdict is the same verdict, not a re-dated one.
func (c *FreshnessVerdictCache) Read(repoDir, boardPath string) FreshnessReport {
	now := c.clock().Now()
	key := c.key(repoDir, boardPath)

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && c.clock().Since(e.fetchedAt) <= c.ttl {
		c.hits++
		return cloneFreshnessReport(e.report)
	}
	c.misses++
	// The full read runs under the lock (single-flight): a second caller for
	// the same board would otherwise re-run the whole subprocess battery this
	// cache exists to avoid. The reader never re-enters the cache, so the
	// region cannot deadlock.
	rep := c.readFull(repoDir, boardPath)
	c.store(key, rep, now)
	return cloneFreshnessReport(rep)
}

// key computes the cache key for one read: the reader's whole input surface.
// A missing board is its own key component (zero mtime) — the reader's
// fail-safe zero report for it is cacheable like any other.
func (c *FreshnessVerdictCache) key(repoDir, boardPath string) freshnessVerdictKey {
	var mtime time.Time
	if fi, err := os.Stat(boardPath); err == nil {
		mtime = fi.ModTime()
	}
	return freshnessVerdictKey{
		repoDir:    repoDir,
		boardPath:  boardPath,
		boardMtime: mtime,
		headSHA:    c.head(repoDir),
	}
}

// head runs the key's HEAD probe — the only subprocess a cache hit performs.
// An unreadable HEAD (not a repo, no commits yet) is the empty string: a key
// component in its own right, so the repo-unreadable verdict is cached like
// any other and re-verified when the TTL elapses.
func (c *FreshnessVerdictCache) head(repoDir string) string {
	probe := c.headProbe
	if probe == nil {
		probe = gitHeadSHA
	}
	return probe(repoDir)
}

// readFull runs the full reader for one miss: the injected seam when a test
// installed one, else the real ReadBoardFreshness on this cache's clock, so a
// report's ReadAt and flip-window arithmetic live on the same timeline as the
// entry's TTL.
func (c *FreshnessVerdictCache) readFull(repoDir, boardPath string) FreshnessReport {
	if c.verdictRead != nil {
		return c.verdictRead(repoDir, boardPath)
	}
	return ReadBoardFreshness(repoDir, boardPath, FreshnessOptions{Clock: c.clock()})
}

// store records a freshly read report under key, evicting the oldest entry
// first when the bound is reached. The map is trimmed back below max BEFORE
// the insert, so len(entries) <= max always holds.
func (c *FreshnessVerdictCache) store(key freshnessVerdictKey, rep FreshnessReport, now time.Time) {
	if _, exists := c.entries[key]; !exists {
		for len(c.entries) >= c.max {
			c.evictOldest()
		}
	}
	c.seq++
	c.entries[key] = freshnessVerdictEntry{
		report:    cloneFreshnessReport(rep),
		fetchedAt: now,
		inserted:  c.seq,
	}
}

// evictOldest drops the least recently INSERTED entry. The scan is O(entries)
// and runs only when the bound is reached — negligible beside the full read
// that triggered it, and it needs no second index to stay correct.
func (c *FreshnessVerdictCache) evictOldest() {
	var (
		oldestKey freshnessVerdictKey
		oldestSeq uint64
		found     bool
	)
	for k, e := range c.entries {
		if !found || e.inserted < oldestSeq {
			oldestKey, oldestSeq, found = k, e.inserted, true
		}
	}
	if !found {
		return
	}
	delete(c.entries, oldestKey)
	c.evicted++
}

// SetClock installs the clock this cache reads for its TTL (SCHED-GAP-169).
// nil keeps the current seam.
func (c *FreshnessVerdictCache) SetClock(clk clock.Clock) { c.clk.Set(clk) }

// clock returns the cache's clock, never nil (the zero value of the seam reads
// as the wall clock).
func (c *FreshnessVerdictCache) clock() clock.Clock { return c.clk.Get() }

// Len returns the number of cached verdicts. Observability for the entry bound
// (and for tests) — never a decision input.
func (c *FreshnessVerdictCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Stats returns the cache's counters since construction: hits served from
// memory, misses that ran the full read, and evictions. Observability only.
func (c *FreshnessVerdictCache) Stats() (hits, misses, evictions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.evicted
}

// cloneFreshnessReport deep-copies a report so a cached value and every served
// value are independent: a caller that mutated a shared Counts map or Rows
// slice would otherwise make two reads of the same key disagree — which is
// exactly the "a cached verdict is indistinguishable from a fresh one"
// guarantee. Nil stays nil (a report's nil slice and an empty slice are
// different values to a caller comparing them).
func cloneFreshnessReport(rep FreshnessReport) FreshnessReport {
	out := rep
	if rep.Rows != nil {
		out.Rows = make([]RowVerdict, len(rep.Rows))
		copy(out.Rows, rep.Rows)
	}
	if rep.Counts != nil {
		out.Counts = make(map[RowStatus]int, len(rep.Counts))
		for k, v := range rep.Counts {
			out.Counts[k] = v
		}
	}
	return out
}
