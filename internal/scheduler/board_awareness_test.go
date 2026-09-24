package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

func TestCountPending_JSONL(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := `{"id":"TASK-001","status":"pending","title":"fix bug"}` + "\n" +
		`{"id":"TASK-002","status":"pending","title":"add tests"}` + "\n" +
		`{"id":"TASK-003","status":"complete","title":"done"}` + "\n" +
		`{this is malformed JSON` + "\n"
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 2 {
		t.Errorf("CountPending(jsonl) = %d, want 2 (2 pending, 1 complete, 1 malformed)", got)
	}
}

func TestCountPending_MD_Fallback(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "## [ ] GAP-001 - fix bug\n" +
		"## [ ] GAP-002 - add tests\n" +
		"## [ ] GAP-003 - update docs\n" +
		"## [x] GAP-000 - done task\n"
	path := filepath.Join(boardDir, "tasks.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 3 {
		t.Errorf("CountPending(md) = %d, want 3 (3 unchecked, 1 checked)", got)
	}
}

func TestCountPending_NoBoardFiles(t *testing.T) {
	dir := t.TempDir()
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 0 {
		t.Errorf("CountPending(empty dir) = %d, want 0", got)
	}
}

func TestCountPending_EmptyWorkdir(t *testing.T) {
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending("")
	if got != 0 {
		t.Errorf("CountPending(empty) = %d, want 0", got)
	}
}

func TestCountPending_MtimeReread(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 1 {
		t.Fatalf("first CountPending = %d, want 1", got)
	}
	got = c.CountPending(dir)
	if got != 1 {
		t.Fatalf("second CountPending (cached) = %d, want 1", got)
	}
	// The cache claim, measured rather than inferred: a second read of an
	// unchanged board must not reach the freshness reader at all. (Counting the
	// reader is the only way to see this — a re-read of the same one-line board
	// also returns 1, so the count assertion above cannot tell the two apart.)
	reads := new(int)
	c.freshnessRead = func(workdir, boardPath string) FreshnessReport {
		*reads++
		return FreshnessReport{}
	}
	got = c.CountPending(dir)
	if got != 1 {
		t.Fatalf("second CountPending (cached) = %d, want 1", got)
	}
	if *reads != 0 {
		t.Fatalf("the cached read consulted the board %d time(s), want 0 — the second call must be served from cache", *reads)
	}
	// Move the SEAM past the cache TTL instead of sleeping for it. The cache
	// key is (fetchedAt within TTL, board mtime, registry mtime); the mtime is
	// what actually changes below, so this is the control case — stepping the
	// clock must NOT be what makes the new count visible (the next assertions
	// prove the third read is served by an mtime change, not by TTL expiry).
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC))
	c.SetClock(sim)
	sim.Advance(1100 * time.Millisecond) // was time.Sleep(1100ms): mtime granularity
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"+`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	// SCHED-GAP-1577: on filesystems with coarse mtime granularity (1s
	// HFS+/ext3-style fallback, some overlayfs) the second write can land in
	// the SAME mtime tick as the first — the cache key still matches, the
	// board is never re-read, and this test fails only on fresh-install
	// machines. Force a mtime deterministically different from the current
	// one (current +2s, anchored to the file itself so it holds on any
	// machine; os.Chtimes precedent: dashboard/git_reins_cache_test.go). The
	// assertion below now proves "a changed mtime forces a re-read" — a
	// property no FS granularity can quantize away — instead of "a quick
	// rewrite happened to move the mtime on this machine".
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat before Chtimes: %v", err)
	}
	future := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	got = c.CountPending(dir)
	if got != 2 {
		t.Errorf("third CountPending (mtime changed) = %d, want 2", got)
	}
	if *reads != 1 {
		t.Errorf("board consultations after the mtime change = %d, want 1 (the board must be re-read, not served from cache)", *reads)
	}
}

func TestCountPending_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(100 * time.Millisecond)
	// Drive the TTL through the seam: the counter's `fetchedAt` comparison
	// runs on the injected clock, so expiry is deterministic rather than a
	// race against real elapsed time on a loaded runner.
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))
	c.SetClock(sim)
	got := c.CountPending(dir)
	if got != 1 {
		t.Fatalf("first CountPending = %d, want 1", got)
	}
	// PREMISE for the TTL assertion: the 100ms TTL has NOT expired, so this
	// read was served from cache — proven by driving the freshness reader
	// (touched only on the re-read path), not inferred from a count that a
	// re-read would also produce.
	reads := new(int)
	c.freshnessRead = func(workdir, boardPath string) FreshnessReport {
		*reads++
		return FreshnessReport{}
	}
	got = c.CountPending(dir)
	if got != 1 {
		t.Errorf("cached CountPending = %d, want 1", got)
	}
	if *reads != 0 {
		t.Fatalf("premise: the cached read consulted the board %d time(s), want 0 (the second call must be cached)", *reads)
	}
	sim.Advance(150 * time.Millisecond) // was time.Sleep(150ms): past the 100ms TTL
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"+`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	got = c.CountPending(dir)
	if got != 2 {
		t.Errorf("post-TTL CountPending = %d, want 2", got)
	}
	if *reads != 1 {
		t.Errorf("board consultations after the TTL elapsed = %d, want 1 — an expired TTL must force a re-read", *reads)
	}
}

func TestCountPending_Concurrent(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := `{"status":"pending"}` + "\n" + `{"status":"complete"}` + "\n"
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(time.Second)
	done := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() {
			got := c.CountPending(dir)
			done <- got
		}()
	}
	for i := 0; i < 20; i++ {
		got := <-done
		if got != 1 {
			t.Errorf("goroutine CountPending = %d, want 1", got)
		}
	}
}

func TestPendingBoostUrgencyFor(t *testing.T) {
	cases := []struct {
		name    string
		pending int
		want    float64
	}{
		{"zero pending", 0, pendingBoostUrgency},
		{"one pending", 1, pendingBoostUrgency + 1},
		{"ten pending", 10, pendingBoostUrgency + 10},
		{"at cap (1000)", 1000, pendingBoostUrgency + 1000},
		{"above cap (1001)", 1001, pendingBoostUrgency + 1000},
		{"negative", -5, pendingBoostUrgency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pendingBoostUrgencyFor(c.pending)
			if got != c.want {
				t.Errorf("pendingBoostUrgencyFor(%d) = %v, want %v", c.pending, got, c.want)
			}
			if got >= starvationBoostUrgency {
				t.Errorf("pendingBoostUrgencyFor(%d) = %v >= starvationBoostUrgency %v", c.pending, got, starvationBoostUrgency)
			}
		})
	}
}

func TestPendingBoost_BelowStarvation(t *testing.T) {
	maxPending := pendingBoostUrgencyFor(10000)
	minStarvation := starvationBoostUrgencyFor(0)
	if maxPending >= minStarvation {
		t.Errorf("max pending boost %v >= min starvation boost %v", maxPending, minStarvation)
	}
	if maxPending < 100000 {
		t.Errorf("max pending boost %v < 100000 - too close to organic urgency range", maxPending)
	}
}
