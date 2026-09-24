package dashboard

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// writeGitReinsVerdict materializes a verdict at
// <workdir>/.gitreins/history/<date>/<sha>/verdict.json and returns the verdict
// path plus the date directory (whose mtime is the cache's freshness stamp).
func writeGitReinsVerdict(t *testing.T, workdir, date, sha string, passed bool) (verdictPath, dateDir string) {
	t.Helper()
	dateDir = filepath.Join(workdir, ".gitreins", "history", date)
	dir := filepath.Join(dateDir, sha)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := `{"task_id":"` + sha + `","task_title":"fixture","passed":` +
		strconv.FormatBool(passed) + `,"evaluated_at":"` + date + `T00:00:00Z"}`
	verdictPath = filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(verdictPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", verdictPath, err)
	}
	return verdictPath, dateDir
}

// pushDirMtime moves a directory's mtime forward, the explicit form of "a new
// verdict landed here" that does not depend on the filesystem's mtime
// granularity.
func pushDirMtime(t *testing.T, dir string) {
	t.Helper()
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
}

// resetGitReinsCache empties the process-wide cache so one test's fixtures are
// never served to the next.
func resetGitReinsCache() {
	gitReinsCache.Lock()
	gitReinsCache.m = make(map[string]gitReinsCacheEntry)
	gitReinsCache.Unlock()
}

// countGitReinsFileReads swaps the verdict-file reader for a counting wrapper
// and returns the counter. It proves a warm hit re-walks nothing: the counter
// only moves when a verdict.json is actually read off disk.
func countGitReinsFileReads(t *testing.T) *int64 {
	t.Helper()
	orig := gitReinsReadFile
	var n int64
	gitReinsReadFile = func(path string) ([]byte, error) {
		atomic.AddInt64(&n, 1)
		return orig(path)
	}
	t.Cleanup(func() { gitReinsReadFile = orig })
	return &n
}

// TestCachedReadGitReins_WarmHit is the warm-path proof: the second call for an
// unchanged history tree is served from the cache and reads no verdict file.
func TestCachedReadGitReins_WarmHit(t *testing.T) {
	dir := t.TempDir()
	verdictPath, _ := writeGitReinsVerdict(t, dir, "2026-09-01", "aaa", true)
	resetGitReinsCache()
	reads := countGitReinsFileReads(t)

	first := cachedReadGitReins(dir, 3)
	if first.Total != 1 || first.Passed != 1 || first.Failed != 0 {
		t.Fatalf("first call: total=%d passed=%d failed=%d, want 1/1/0 (verdict at %s)",
			first.Total, first.Passed, first.Failed, verdictPath)
	}
	afterFirst := atomic.LoadInt64(reads)
	if afterFirst == 0 {
		t.Fatalf("first call read no verdict.json (fixture at %s)", verdictPath)
	}
	if len(first.Latest) != 1 {
		t.Fatalf("first call: %d latest verdicts, want 1", len(first.Latest))
	}

	second := cachedReadGitReins(dir, 3)
	if second.Total != first.Total || second.Passed != first.Passed || second.Failed != first.Failed {
		t.Errorf("warm hit changed the summary: %+v, want %+v", second, first)
	}
	if len(second.Latest) != len(first.Latest) {
		t.Errorf("warm hit lost the latest-verdict list: %d entries, want %d", len(second.Latest), len(first.Latest))
	}
	if got := atomic.LoadInt64(reads); got != afterFirst {
		t.Errorf("warm hit re-walked the history tree: verdict.json reads %d → %d", afterFirst, got)
	}
}

// TestCachedReadGitReins_MtimeInvalidates proves an mtime change on the history
// tree (a newly written verdict) beats the TTL window: the very next call
// re-walks and returns the fresh aggregate.
func TestCachedReadGitReins_MtimeInvalidates(t *testing.T) {
	dir := t.TempDir()
	_, dateDir := writeGitReinsVerdict(t, dir, "2026-09-01", "aaa", false)
	resetGitReinsCache()
	reads := countGitReinsFileReads(t)

	first := cachedReadGitReins(dir, 3)
	if first.Total != 1 || first.Passed != 0 {
		t.Fatalf("first call: total=%d passed=%d, want 1/0", first.Total, first.Passed)
	}
	afterFirst := atomic.LoadInt64(reads)

	// A second verdict lands in the SAME date directory (the common case: many
	// verdicts per day), then the stamp moves forward.
	writeGitReinsVerdict(t, dir, "2026-09-01", "bbb", true)
	pushDirMtime(t, dateDir)

	second := cachedReadGitReins(dir, 3)
	if second.Total != 2 || second.Passed != 1 || second.Failed != 1 {
		t.Errorf("after mtime change: total=%d passed=%d failed=%d, want 2/1/1",
			second.Total, second.Passed, second.Failed)
	}
	if got := atomic.LoadInt64(reads); got <= afterFirst {
		t.Errorf("mtime change did not invalidate the cache: verdict.json reads stayed at %d", got)
	}
}

// TestCachedReadGitReins_TTLExpiry proves the TTL is honoured even when the
// history tree never changes: with the tree untouched, only an expired entry
// can explain a re-walk. The TTL is shrunk rather than the clock advanced —
// the cache is a package-level singleton with no clock to inject, so the TTL
// itself is the seam (the production default is gitReinsCacheTTLDefault).
func TestCachedReadGitReins_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	writeGitReinsVerdict(t, dir, "2026-09-01", "aaa", true)
	resetGitReinsCache()
	reads := countGitReinsFileReads(t)

	origTTL := gitReinsCacheTTL
	gitReinsCacheTTL = 20 * time.Millisecond
	t.Cleanup(func() { gitReinsCacheTTL = origTTL })

	first := cachedReadGitReins(dir, 3)
	if first.Total != 1 || first.Passed != 1 {
		t.Fatalf("first call: total=%d passed=%d, want 1/1", first.Total, first.Passed)
	}
	afterFirst := atomic.LoadInt64(reads)

	time.Sleep(60 * time.Millisecond)
	second := cachedReadGitReins(dir, 3)
	if second.Total != first.Total || second.Passed != first.Passed {
		t.Errorf("expired refetch changed the summary: %+v, want %+v", second, first)
	}
	if got := atomic.LoadInt64(reads); got <= afterFirst {
		t.Errorf("an entry older than the TTL was served from cache: verdict.json reads stayed at %d", got)
	}
}

// TestGitReinsHistoryMtime_PrefersNewestDateDir pins the stamp contract: the
// stamp is the newest date directory's mtime, so the first verdict of a new day
// invalidates even though history/ itself is untouched by that write.
func TestGitReinsHistoryMtime_PrefersNewestDateDir(t *testing.T) {
	dir := t.TempDir()
	writeGitReinsVerdict(t, dir, "2020-01-01", "old", true)
	writeGitReinsVerdict(t, dir, "2026-09-01", "new", true)
	dateDir := filepath.Join(dir, ".gitreins", "history", "2026-09-01")
	pushDirMtime(t, dateDir) // +5s

	got, ok := gitReinsHistoryMtime(dir)
	if !ok {
		t.Fatalf("no stamp for an existing history tree at %s", dir)
	}
	if !got.After(time.Now()) {
		t.Errorf("stamp = %v, want the pushed-forward newest date dir mtime (future)", got)
	}
}

// TestDashboardRenderCacheTTLsAtLeast60s pins Change 3 (SCHED-GAP-1576): a
// render-cache window shorter than a cold render makes every render cold. The
// dashboard's render caches are the CI conclusion cache (the only render cache
// with a TTL; defaulted at 300s by DASH-PERF-003) and the new GitReins walk
// cache.
func TestDashboardRenderCacheTTLsAtLeast60s(t *testing.T) {
	const floor = 60 * time.Second
	if ciCacheDefaultTTL < floor {
		t.Errorf("ciCacheDefaultTTL = %v, want >= %v", ciCacheDefaultTTL, floor)
	}
	g := &Generator{}
	if got := g.ciTTLValue(); got < floor {
		t.Errorf("resolved CI cache TTL = %v, want >= %v", got, floor)
	}
	if gitReinsCacheTTLDefault < floor {
		t.Errorf("gitReinsCacheTTLDefault = %v, want >= %v", gitReinsCacheTTLDefault, floor)
	}
}
