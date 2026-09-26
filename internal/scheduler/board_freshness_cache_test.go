package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ── PERF-002: board-freshness verdict cache ──────────────────────────────
//
// The cache is the difference between "every evaluate() pass forks 5+ git
// subprocesses per completed board row per project" and "one HEAD probe per
// board per pass". These tests run against REAL throwaway git repos
// (newFreshFixture — the board_freshness_test.go style) behind counting seams,
// so each assertion is about MEASURED behaviour — how many full reads and how
// many HEAD probes actually ran — rather than about a returned value two code
// paths could both produce.
//
// The clock seam (SCHED-GAP-169) drives every TTL assertion: no real sleeps.

// cacheFixture is one throwaway repo wired to a verdict cache with counting
// seams on both expensive paths.
type cacheFixture struct {
	*freshFixture
	cache *FreshnessVerdictCache
	sim   *clock.SimClock
	// reads counts FULL verdict reads (the subprocess battery), heads counts
	// HEAD probes (the one subprocess a hit is allowed).
	reads int
	heads int
}

// newCacheFixture builds the fixture: real git, a manual (never-advancing
// unless the test says so) sim clock, and seams that delegate to the real
// reader/HEAD probe while counting.
func newCacheFixture(t *testing.T, ttl time.Duration, maxEntries int) *cacheFixture {
	t.Helper()
	fx := newFreshFixture(t)
	sim := clock.NewManualSimClock(fx.now)
	cf := &cacheFixture{freshFixture: fx, sim: sim}
	cf.cache = NewFreshnessVerdictCache(ttl, maxEntries)
	cf.cache.SetClock(sim)
	cf.cache.verdictRead = func(repoDir, boardPath string) FreshnessReport {
		cf.reads++
		return ReadBoardFreshness(repoDir, boardPath, FreshnessOptions{Clock: sim})
	}
	cf.cache.headProbe = func(repoDir string) string {
		cf.heads++
		return gitHeadSHA(repoDir)
	}
	return cf
}

// TestFreshnessVerdictCache_HitServesCachedVerdictOnUnchangedKey is the core
// hit case: a second read with the board and HEAD untouched must be served
// from memory — no full read, exactly one HEAD probe.
func TestFreshnessVerdictCache_HitServesCachedVerdictOnUnchangedKey(t *testing.T) {
	cf := newCacheFixture(t, DefaultFreshnessVerdictTTL, MaxFreshnessVerdictEntries)
	cf.writeBoard(freshRow("PERF-002", "pending", nil))

	first := cf.cache.Read(cf.dir, cf.board)
	if cf.reads != 1 || cf.heads != 1 {
		t.Fatalf("premise: first read ran the full reader %d time(s) and %d HEAD probe(s), want 1/1", cf.reads, cf.heads)
	}
	if !first.WorkToSpawn {
		t.Fatalf("premise: a board with one pending row must raise WorkToSpawn (got %+v)", first.Counts)
	}

	second := cf.cache.Read(cf.dir, cf.board)
	if cf.reads != 1 {
		t.Errorf("second read ran the full reader %d time(s), want 0 — an unchanged key must be served from cache", cf.reads-1)
	}
	if cf.heads != 2 {
		t.Errorf("HEAD probes after two reads = %d, want 2 (one per read: the key's single exec, not the full battery)", cf.heads)
	}
	if !reflect.DeepEqual(second, first) {
		t.Errorf("cached verdict differs from the fresh one:\ncached: %+v\nfirst:  %+v", second, first)
	}
	if hits, misses, _ := cf.cache.Stats(); hits != 1 || misses != 1 {
		t.Errorf("stats after one miss + one hit = hits %d misses %d, want 1/1", hits, misses)
	}
}

// TestFreshnessVerdictCache_BoardMtimeChangeReverifies: an edit to the board
// moves its mtime, so the key changes and the verdict must be re-derived — and
// the re-derived verdict must reflect the edited board.
func TestFreshnessVerdictCache_BoardMtimeChangeReverifies(t *testing.T) {
	cf := newCacheFixture(t, DefaultFreshnessVerdictTTL, MaxFreshnessVerdictEntries)
	cf.writeBoard(freshRow("PERF-002", "pending", nil))
	first := cf.cache.Read(cf.dir, cf.board)
	if first.TotalRows != 1 {
		t.Fatalf("premise: first read saw %d row(s), want 1", first.TotalRows)
	}

	cf.writeBoard(
		freshRow("PERF-002", "pending", nil),
		freshRow("PERF-003", "pending", nil),
	)
	// Force an mtime the key cannot miss on ANY filesystem granularity — a
	// same-tick rewrite would leave the key identical and prove nothing
	// (os.Chtimes precedent: board_awareness_test.go, SCHED-GAP-1577).
	fi, err := os.Stat(cf.board)
	if err != nil {
		t.Fatalf("Stat board: %v", err)
	}
	future := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(cf.board, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	headBefore := gitHeadSHA(cf.dir)

	second := cf.cache.Read(cf.dir, cf.board)
	if cf.reads != 2 {
		t.Errorf("full reads after the board edit = %d, want 2 — a changed board mtime must force a full re-verify", cf.reads)
	}
	if second.TotalRows != 2 {
		t.Errorf("post-edit verdict saw %d row(s), want 2 — the re-derived verdict, not the cached one", second.TotalRows)
	}
	if headAfter := gitHeadSHA(cf.dir); headAfter != headBefore {
		t.Fatalf("test premise: the board edit moved HEAD (%s -> %s); only the mtime component is under test here", headBefore, headAfter)
	}
}

// TestFreshnessVerdictCache_HeadChangeReverifies: work landing moves HEAD
// while the board file is untouched, so the key changes and the verdict must
// be re-derived — the mid-flip case the reader's recent-commit scan exists for.
func TestFreshnessVerdictCache_HeadChangeReverifies(t *testing.T) {
	cf := newCacheFixture(t, DefaultFreshnessVerdictTTL, MaxFreshnessVerdictEntries)
	// One pending row with no pointer: a commit that NAMES it inside the flip
	// window is the only way this row stops being work-to-spawn.
	cf.writeBoard(freshRow("PERF-002", "pending", nil))
	headBefore := gitHeadSHA(cf.dir)

	first := cf.cache.Read(cf.dir, cf.board)
	if v := verdictOf(t, first, "PERF-002"); v.Status != RowOpen {
		t.Fatalf("premise: the row must start open (got %q/%q)", v.Status, v.Reason)
	}

	cf.commitAt("work", cf.workLanded(), "fix: verdict cache. Addresses PERF-002.")
	headAfter := gitHeadSHA(cf.dir)
	if headAfter == headBefore || headAfter == "" {
		t.Fatalf("test premise: the commit did not move HEAD (%q -> %q)", headBefore, headAfter)
	}

	second := cf.cache.Read(cf.dir, cf.board)
	if cf.reads != 2 {
		t.Errorf("full reads after the commit = %d, want 2 — a moved HEAD must force a full re-verify", cf.reads)
	}
	if v := verdictOf(t, second, "PERF-002"); v.Status != RowFlipWindow {
		t.Errorf("post-commit verdict = %q (reason %q), want %q — the fresh verdict, not the cached one",
			v.Status, v.Reason, RowFlipWindow)
	}
	if v := verdictOf(t, first, "PERF-002"); v.Status != RowOpen {
		t.Fatalf("the cached first verdict was mutated: status %q", v.Status)
	}
}

// TestFreshnessVerdictCache_TTLExpiryReverifies: with the key UNCHANGED, an
// elapsed TTL alone must force the full read — driven through the clock seam,
// never a real sleep.
func TestFreshnessVerdictCache_TTLExpiryReverifies(t *testing.T) {
	cf := newCacheFixture(t, 60*time.Second, MaxFreshnessVerdictEntries)
	// A marker reader, so a served fresh read is distinguishable from a served
	// cache entry: the returned report's row count IS the read number.
	marker := 0
	cf.cache.verdictRead = func(repoDir, boardPath string) FreshnessReport {
		cf.reads++
		marker++
		return FreshnessReport{TotalRows: marker, CanVerify: true}
	}
	cf.writeBoard(freshRow("PERF-002", "pending", nil))

	if got := cf.cache.Read(cf.dir, cf.board); got.TotalRows != 1 {
		t.Fatalf("first read returned %d row(s), want the marker 1", got.TotalRows)
	}
	cf.sim.Advance(30 * time.Second) // inside the 60s TTL
	if got := cf.cache.Read(cf.dir, cf.board); got.TotalRows != 1 {
		t.Errorf("read inside the TTL returned %d row(s), want the cached 1 (a fresh read would return 2)", got.TotalRows)
	}
	if cf.reads != 1 {
		t.Errorf("full reads inside the TTL = %d, want 1 — an unexpired entry must be served from cache", cf.reads)
	}
	// The board is NOT touched: nothing about the key changed. Only the TTL
	// elapsed, so only the TTL can be what forces the re-verify.
	cf.sim.Advance(31 * time.Second)
	if got := cf.cache.Read(cf.dir, cf.board); got.TotalRows != 2 {
		t.Errorf("post-TTL read returned %d row(s), want the FRESH marker 2 (1 means the expired entry was served)", got.TotalRows)
	}
	if cf.reads != 2 {
		t.Errorf("full reads after the TTL elapsed = %d, want 2 — an expired TTL must force a full re-verify", cf.reads)
	}
	if _, _, evicted := cf.cache.Stats(); evicted != 0 {
		t.Errorf("evictions = %d, want 0 at one key", evicted)
	}
}

// TestFreshnessVerdictCache_CachedVerdictMatchesFreshRead pins the contract
// that makes the cache legitimate at all: serving a verdict must be
// indistinguishable from re-deriving it for the same key.
func TestFreshnessVerdictCache_CachedVerdictMatchesFreshRead(t *testing.T) {
	cf := newCacheFixture(t, DefaultFreshnessVerdictTTL, MaxFreshnessVerdictEntries)
	closed := cf.commitAt("work", cf.workLanded(), "fix: thing. Addresses PERF-001.")
	cf.writeBoard(
		freshRow("PERF-001", "complete", map[string]string{"commit_hash": closed}),
		freshRow("PERF-002", "pending", nil),
	)

	first := cf.cache.Read(cf.dir, cf.board)
	cached := cf.cache.Read(cf.dir, cf.board)
	if cf.reads != 1 {
		t.Fatalf("premise: the second read must have been a hit (full reads = %d)", cf.reads)
	}

	fresh := ReadBoardFreshness(cf.dir, cf.board, FreshnessOptions{Clock: cf.sim})
	if !reflect.DeepEqual(cached, fresh) {
		t.Errorf("a cached verdict is distinguishable from a fresh one:\ncached: %+v\nfresh:  %+v", cached, fresh)
	}
	if !reflect.DeepEqual(first, fresh) {
		t.Errorf("the miss-path verdict differs from a fresh read:\nmiss:  %+v\nfresh: %+v", first, fresh)
	}

	// And a served report is a copy: mutating it must not poison the cache.
	cached.Counts[RowStatus("invented")] = 99
	cached.Rows = append(cached.Rows, RowVerdict{ID: "invented"})
	again := cf.cache.Read(cf.dir, cf.board)
	if !reflect.DeepEqual(again, fresh) {
		t.Errorf("a served report aliased the cached value: mutating it changed later reads:\nagain: %+v\nfresh: %+v", again, fresh)
	}
}

// TestFreshnessVerdictCache_BoundEvictsOldest proves the entry bound is real:
// the cache never exceeds max, the OLDEST entry is the one dropped, the newest
// survives, and churn above the bound cannot grow the map.
func TestFreshnessVerdictCache_BoundEvictsOldest(t *testing.T) {
	const maxEntries = 4
	cf := newCacheFixture(t, DefaultFreshnessVerdictTTL, maxEntries)
	// Git is irrelevant to the bound; stub the expensive seams so the churn
	// loop measures the cache and nothing else.
	cf.cache.verdictRead = func(repoDir, boardPath string) FreshnessReport {
		cf.reads++
		return FreshnessReport{TotalRows: 1, CanVerify: true}
	}
	cf.cache.headProbe = func(repoDir string) string {
		cf.heads++
		return "0123456789abcdef0123456789abcdef01234567"
	}

	boards := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		p := filepath.Join(cf.dir, ".coding-hermes", "board", fmt.Sprintf("bound-%d.jsonl", i))
		if err := os.WriteFile(p, []byte(freshRow("PERF-002", "pending", nil)+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
		boards = append(boards, p)
		cf.cache.Read(cf.dir, p)
	}
	if got := cf.cache.Len(); got != maxEntries {
		t.Errorf("cache holds %d entries after 6 distinct reads, want %d", got, maxEntries)
	}

	// The oldest key (bound-0) was the one dropped: re-reading it is a miss...
	before := cf.reads
	cf.cache.Read(cf.dir, boards[0])
	if cf.reads != before+1 {
		t.Errorf("re-reading the oldest key ran %d full read(s), want 1 — it should have been evicted", cf.reads-before)
	}
	// ...while the newest (bound-5) is still cached: a hit.
	before = cf.reads
	cf.cache.Read(cf.dir, boards[len(boards)-1])
	if cf.reads != before {
		t.Errorf("re-reading the newest key ran %d full read(s), want 0 — it must have survived eviction", cf.reads-before)
	}

	// Churn well above the bound: the map must stay capped.
	for i := 0; i < 200; i++ {
		p := filepath.Join(cf.dir, ".coding-hermes", "board", fmt.Sprintf("churn-%d.jsonl", i))
		if err := os.WriteFile(p, []byte(freshRow("PERF-002", "pending", nil)+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
		cf.cache.Read(cf.dir, p)
		if got := cf.cache.Len(); got > maxEntries {
			t.Fatalf("cache grew to %d entries (bound %d) after %d churned keys", got, maxEntries, i+1)
		}
	}
	if _, _, evicted := cf.cache.Stats(); evicted < 6 {
		t.Errorf("evictions = %d after 206 distinct keys with a bound of %d, want at least 6", evicted, maxEntries)
	}
}

// TestCountPending_ServesVerdictCacheAcrossCountTTL is the wiring assertion:
// the PENDING-COUNT cache expiring must not re-run the git battery, because the
// verdict key (board mtime + HEAD) is unchanged. This is the exact sequence
// that dominated the live CPU profile — a count TTL expiry per project per
// evaluate() pass.
func TestCountPending_ServesVerdictCacheAcrossCountTTL(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(
		freshRow("PERF-002", "pending", nil),
		freshRow("PERF-003", "pending", nil),
	)
	sim := clock.NewManualSimClock(fx.now)
	c := NewPendingTaskCounter(time.Second)
	c.SetClock(sim)
	if c.verdictCache == nil {
		t.Fatal("NewPendingTaskCounter built no verdict cache — the production freshness read is uncached")
	}
	reads, heads := 0, 0
	c.verdictCache.verdictRead = func(repoDir, boardPath string) FreshnessReport {
		reads++
		return ReadBoardFreshness(repoDir, boardPath, FreshnessOptions{Clock: sim})
	}
	c.verdictCache.headProbe = func(repoDir string) string {
		heads++
		return gitHeadSHA(repoDir)
	}

	if got := c.CountPending(fx.dir); got != 2 {
		t.Fatalf("first CountPending = %d, want 2", got)
	}
	if reads != 1 {
		t.Fatalf("premise: the first count must run the full verdict read (got %d)", reads)
	}

	// Expire ONLY the pending-count entry (board mtime and HEAD untouched).
	sim.Advance(2 * time.Second)
	if got := c.CountPending(fx.dir); got != 2 {
		t.Errorf("post-TTL CountPending = %d, want 2", got)
	}
	if reads != 1 {
		t.Errorf("the count TTL expiry ran the full git-verified read %d extra time(s), want the cached verdict served", reads-1)
	}
	if heads != 2 {
		t.Errorf("HEAD probes = %d, want 2 — one per count call, the single exec the hit path is allowed", heads)
	}
	if _, misses, _ := c.verdictCache.Stats(); misses != 1 {
		t.Errorf("verdict-cache misses = %d, want 1 (the count re-read must have been a hit)", misses)
	}
}

// TestPendingTaskCounter_VerdictCacheBridgesEvalPasses pins the property that
// makes the cache pay off at all: live evaluate() passes are 3–6.5 min apart,
// so a verdict TTL equal to the count TTL would expire in the same instant as
// the count entry and re-run the battery on every pass — the cache would be
// dead weight. The verdict must outlive a pass gap (and the count TTL), while
// still re-verifying once its own TTL elapses.
func TestPendingTaskCounter_VerdictCacheBridgesEvalPasses(t *testing.T) {
	fx := newFreshFixture(t)
	fx.writeBoard(freshRow("PERF-002", "pending", nil))

	sim := clock.NewManualSimClock(fx.now)
	c := NewPendingTaskCounter(60 * time.Second) // the production count TTL
	c.SetClock(sim)
	reads, heads := 0, 0
	c.verdictCache.verdictRead = func(repoDir, boardPath string) FreshnessReport {
		reads++
		return ReadBoardFreshness(repoDir, boardPath, FreshnessOptions{Clock: sim})
	}
	c.verdictCache.headProbe = func(repoDir string) string {
		heads++
		return gitHeadSHA(repoDir)
	}

	if got := c.CountPending(fx.dir); got != 1 {
		t.Fatalf("first CountPending = %d, want 1", got)
	}
	if reads != 1 {
		t.Fatalf("premise: the first count must run the full verdict read (got %d)", reads)
	}

	// One eval pass later: the count entry (60s) has long expired, the verdict
	// (10 min) has not. This is the fleet-scale hot path the row measured.
	sim.Advance(5 * time.Minute)
	if got := c.CountPending(fx.dir); got != 1 {
		t.Errorf("count after a pass gap = %d, want 1", got)
	}
	if reads != 1 {
		t.Errorf("a pass 5 min after the first re-ran the full git-verified read (%d total), want the cached verdict served", reads)
	}
	if heads != 2 {
		t.Errorf("HEAD probes = %d, want 2 — one per pass, the only subprocess a hit runs", heads)
	}

	// The verdict TTL is still a bound: past it, the battery runs again.
	sim.Advance(6 * time.Minute)
	if got := c.CountPending(fx.dir); got != 1 {
		t.Errorf("count past the verdict TTL = %d, want 1", got)
	}
	if reads != 2 {
		t.Errorf("full reads past the verdict TTL = %d, want 2 — the TTL must re-verify", reads)
	}
}
