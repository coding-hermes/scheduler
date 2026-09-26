package dashboard

import (
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SCHED-GAP-1623: tickWork is the only uncached per-render subprocess in the
// dashboard enrichment path. The fleet render re-derives "what did this tick
// work on" for the last ≤20 completed ticks of every project — one `git log`
// fork/exec per sample per render, hundreds of subprocesses at fleet scale,
// every render. Completed-tick windows are immutable history, so the memo
// cache (tick_work_cache.go) must serve a repeated (workdir, window) lookup
// with ZERO additional git execs.

// initGitRepo creates a real git repo with two commits, returning the workdir.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-q", "-m", "first commit")
	run("commit", "--allow-empty", "-q", "-m", "second commit")
	return dir
}

// countingGitRunner swaps tickWorkRunner for a wrapper that counts real git
// invocations, so a test can assert the memo served a repeat without an exec.
func countingGitRunner(t *testing.T) (calls *atomic.Int64, restore func()) {
	t.Helper()
	realRunner := tickWorkRunner
	calls = &atomic.Int64{}
	tickWorkRunner = func(cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		if realRunner != nil {
			return realRunner(cmd)
		}
		return cmd.Output()
	}
	return calls, func() { tickWorkRunner = realRunner }
}

func resetTickWorkCache() {
	tickWorkCache.Lock()
	tickWorkCache.m = make(map[string]tickWorkCacheEntry)
	tickWorkCache.Unlock()
}

// TestTickWorkMemoRepeatWindowNoExec: the same (workdir, spawned, completed)
// window asked twice costs exactly ONE git exec — the second call is a memo
// hit. This is the regression test for the per-render subprocess storm: with
// the fix reverted (cache bypassed), the second call execs again and the
// test fails.
func TestTickWorkMemoRepeatWindowNoExec(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	wd := initGitRepo(t)
	calls, restore := countingGitRunner(t)
	defer restore()

	spawned := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	completed := time.Now().Format(time.RFC3339)

	first := tickWorkCached(clock.Real(), wd, spawned, completed, 4)
	if calls.Load() != 1 {
		t.Fatalf("first call: got %d git execs, want 1", calls.Load())
	}
	second := tickWorkCached(clock.Real(), wd, spawned, completed, 4)
	if calls.Load() != 1 {
		t.Fatalf("memo miss: second identical call cost %d total execs, want still 1 (window is immutable history)", calls.Load())
	}
	if first != second {
		t.Fatalf("memo changed the answer: %q vs %q", first, second)
	}
	if first == "" {
		t.Fatal("tickWork returned empty for a repo with commits in-window")
	}
}

// TestTickWorkMemoDistinctWindowsExec: distinct windows must NOT be served
// from the memo (a different until may see new commits) — each unique window
// pays its own exec, exactly like the uncached baseline.
func TestTickWorkMemoDistinctWindowsExec(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	wd := initGitRepo(t)
	calls, restore := countingGitRunner(t)
	defer restore()

	spawned := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	completedA := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	completedB := time.Now().Format(time.RFC3339)

	_ = tickWorkCached(clock.Real(), wd, spawned, completedA, 4)
	_ = tickWorkCached(clock.Real(), wd, spawned, completedB, 4)
	if got := calls.Load(); got != 2 {
		t.Fatalf("distinct windows: got %d execs, want 2", got)
	}
}

// TestTickWorkMemoLiveWindowNotMemoized: a window whose completed instant is
// clamped to the clock's now (running tick, until clamped forward) must not
// memoize — its answer can change as commits land. The memo applies only to
// fixed windows.
func TestTickWorkMemoLiveWindowNotMemoized(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	wd := initGitRepo(t)
	calls, restore := countingGitRunner(t)
	defer restore()

	spawned := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	// completed empty → until = now() at call time, i.e. a moving window.
	_ = tickWorkCached(clock.Real(), wd, spawned, "", 4)
	_ = tickWorkCached(clock.Real(), wd, spawned, "", 4)
	if got := calls.Load(); got < 2 {
		t.Fatalf("live windows: got %d execs, want >=2 (moving window must not memoize)", got)
	}
}

// TestTickWorkMemoEviction: the cache is bounded — exceeding the entry cap
// evicts the oldest entries instead of growing without bound. Manipulates the
// singleton directly so the bound is proven without an exec storm.
func TestTickWorkMemoEviction(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()

	base := clock.Real().Now().Add(-time.Hour)
	for i := 0; i < tickWorkCacheMaxEntries+tickWorkCacheEvictBatch+2; i++ {
		key := "wd" + strconv.Itoa(i%26) + "\x00" + "spawned" + strconv.Itoa(i%26) + "\x00" + time.Unix(int64(1700000000+i), 0).Format(time.RFC3339) + "\x004"
		tickWorkCache.m[key] = tickWorkCacheEntry{work: "w", fetchedAt: base.Add(time.Duration(i) * time.Second)}
	}
	tickWorkCache.Lock()
	before := len(tickWorkCache.m)
	evictTickWorkCacheLocked()
	n := len(tickWorkCache.m)
	tickWorkCache.Unlock()
	if n > tickWorkCacheMaxEntries {
		t.Fatalf("cache at %d entries after eviction, cap is %d", n, tickWorkCacheMaxEntries)
	}
	if before-n < tickWorkCacheEvictBatch {
		t.Fatalf("eviction dropped only %d entries, want >= %d", before-n, tickWorkCacheEvictBatch)
	}
}

// TestTickWorkMemoEmptyWorkdir: no workdir → no exec, no memo entry.
func TestTickWorkMemoEmptyWorkdir(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	calls, restore := countingGitRunner(t)
	defer restore()
	if got := tickWorkCached(clock.Real(), "", "2026-01-01T00:00:00Z", "", 4); got != "" {
		t.Fatalf("empty workdir: got %q, want empty", got)
	}
	tickWorkCache.Lock()
	n := len(tickWorkCache.m)
	tickWorkCache.Unlock()
	if n != 0 || calls.Load() != 0 {
		t.Fatalf("empty workdir memoized (%d entries) or exec'd (%d calls)", n, calls.Load())
	}
}

// TestTickWorkMemoBoundsSanity: guard the invariants the bound relies on.
func TestTickWorkMemoBoundsSanity(t *testing.T) {
	if tickWorkCacheMaxEntries <= 0 || tickWorkCacheEvictBatch <= 0 || tickWorkCacheEvictBatch > tickWorkCacheMaxEntries {
		t.Fatalf("cache bounds incoherent: max=%d evict=%d", tickWorkCacheMaxEntries, tickWorkCacheEvictBatch)
	}
	if tickWorkCacheTTLDefault <= 0 {
		t.Fatalf("TTL must be positive, got %v", tickWorkCacheTTLDefault)
	}
}
