package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nowRFC3339Offset returns time.Now() shifted by the given nanosecond offset,
// formatted as RFC3339 — used to build last-tick-completed values relative to
// the present for nextTickIn assertions.
func nowRFC3339Offset(ns int64) string {
	return time.Now().Add(time.Duration(ns)).UTC().Format(time.RFC3339)
}

func TestReadBoardProgress(t *testing.T) {
	// Board mirroring the ozzgraph model-router matrix format:
	// Active table rows are pending, Completed rows are done, and the
	// perpetual NEVER-DONE audit is a heading (not counted).
	board := `# Test Project — Task Board

## Active

| ID | Task | Pri |
|----|------|-----|
| T03 | PR3: heartbeat | High |
| T04 | PR4: logging | High |

## Completed

| ID | Task | Pri | Commit |
|----|------|-----|--------|
| T00 | Bootstrap | Trivial | — |
| T01 | PR1: CI | Critical | abc123 |
| T02 | PR2: runtime | Critical | def456 |

## [ ] NEVER-DONE — Run 12-point audit

Never counted.
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	done, total := readBoardProgress(path)
	if done != 3 {
		t.Errorf("expected 3 done, got %d", done)
	}
	if total != 5 {
		t.Errorf("expected 5 total (3 done + 2 active, NEVER-DONE excluded), got %d", total)
	}
}

func TestReadBoardProgress_MissingFile(t *testing.T) {
	done, total := readBoardProgress(filepath.Join(t.TempDir(), "nope", "tasks.md"))
	if done != 0 || total != 0 {
		t.Errorf("expected (0,0) for missing board, got (%d,%d)", done, total)
	}
}

func TestReadBoardProgress_VPrefix(t *testing.T) {
	// The v2 milestone uses "| V01 | ..." task rows (not "| T##"). The parser
	// must count them as tasks so the dashboard shows progress, not 0/0.
	board := `# Project

## v2 Active — milestone

| ID | Task | Pri |
|----|------|-----|
| V01 | generic-runtime | Critical |
| V02 | autonomous-vertical-slice | Critical |

## Completed

| ID | Task | Pri | Commit |
|----|------|-----|--------|
| V01 | bootstrap | Trivial | abc123 |

## [ ] NEVER-DONE — audit
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	done, total := readBoardProgress(path)
	if done != 1 || total != 3 {
		t.Errorf("V-prefix board: got done=%d total=%d, want done=1 total=3", done, total)
	}
}

func TestIsTaskRow(t *testing.T) {
	ok := []string{
		"| T05 | task |",
		"| V01 | task |",
		"| DOCS-000 | task |",
		"| E2E-001 | task |",
		"| GAP-003 | task |",
		"| TEST-001 | task |",
	}
	for _, s := range ok {
		if !isTaskRow(s) {
			t.Errorf("isTaskRow(%q) = false, want true", s)
		}
	}
	bad := []string{
		"|----|------|",         // table separator
		"| ID | Task |",         // header row
		"## [ ] NEVER-DONE",     // heading, not a row
		"| t05 | lowercase |",   // lowercase id
		"| 123 | digits only |", // starts with digit
		"plain text",
	}
	for _, s := range bad {
		if isTaskRow(s) {
			t.Errorf("isTaskRow(%q) = true, want false", s)
		}
	}
}

func TestNextTickIn(t *testing.T) {
	if got := nextTickIn(true, "2026-08-06T00:00:00Z", 900); got != "running" {
		t.Errorf("running -> expected 'running', got %q", got)
	}
	if got := nextTickIn(false, "", 900); got != "—" {
		t.Errorf("no last tick -> expected '—', got %q", got)
	}
	// Last tick completed 10s ago, 900s cooldown → ~14m49-50s to next.
	// (Tolerant: a second may elapse between building the timestamp and the
	// call, so assert the minute prefix rather than an exact second.)
	if got := nextTickIn(false, nowRFC3339Offset(-10*1e9), 900); got != "in 14m 50s" && got != "in 14m 49s" {
		t.Errorf("countdown mismatch, got %q", got)
	}
	// Last tick completed 20 minutes ago, 900s cooldown → past due.
	if got := nextTickIn(false, nowRFC3339Offset(-20*60*1e9), 900); got != "due now" {
		t.Errorf("overdue -> expected 'due now', got %q", got)
	}
}

func TestReadGitReins(t *testing.T) {
	dir := t.TempDir()
	// .gitreins/history/<date>/<sha>/verdict.json
	gr := filepath.Join(dir, ".gitreins", "history", "2026-08-07", "abc1234")
	if err := os.MkdirAll(gr, 0o755); err != nil {
		t.Fatal(err)
	}
	pass := `{"task_id":"T10","task_title":"PR10","passed":true,"stages":{"tier1":{"passed":true},"tier2":{"passed":true}},"evaluated_at":"2026-08-07T10:00:00Z"}`
	fail := `{"task_id":"T09","task_title":"PR9","passed":false,"stages":{"tier1":{"passed":true},"tier2":{"passed":false}},"evaluated_at":"2026-08-07T09:00:00Z"}`
	if err := os.WriteFile(filepath.Join(gr, "verdict.json"), []byte(pass), 0o644); err != nil {
		t.Fatal(err)
	}
	gr2 := filepath.Join(dir, ".gitreins", "history", "2026-08-07", "def5678")
	if err := os.MkdirAll(gr2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gr2, "verdict.json"), []byte(fail), 0o644); err != nil {
		t.Fatal(err)
	}

	sum := readGitReins(dir, 10)
	if sum.Total != 2 || sum.Passed != 1 || sum.Failed != 1 {
		t.Errorf("expected total=2 pass=1 fail=1, got total=%d pass=%d fail=%d", sum.Total, sum.Passed, sum.Failed)
	}
	if sum.RatePct != 50 {
		t.Errorf("expected 50%% rate, got %d", sum.RatePct)
	}
	if len(sum.Latest) != 2 {
		t.Fatalf("expected 2 latest, got %d", len(sum.Latest))
	}
	// Newest first.
	if sum.Latest[0].TaskID != "T10" {
		t.Errorf("expected newest first T10, got %s", sum.Latest[0].TaskID)
	}
	if !sum.Latest[0].Tier2Passed || sum.Latest[0].HasTier2 != true {
		t.Errorf("expected T10 tier2 passed, got %+v", sum.Latest[0])
	}

	// Missing history → zero summary.
	empty := readGitReins(filepath.Join(t.TempDir(), "none"), 5)
	if empty.Total != 0 {
		t.Errorf("expected 0 for missing history, got %d", empty.Total)
	}
}

func TestFormatETA(t *testing.T) {
	if got := formatETA(0); got != "—" {
		t.Errorf("zero -> expected '—', got %q", got)
	}
	if got := formatETA(90 * time.Second); got != "1m" {
		t.Errorf("90s -> expected '1m', got %q", got)
	}
	if got := formatETA(2 * time.Hour); got != "2h 0m" {
		t.Errorf("2h -> expected '2h 0m', got %q", got)
	}
	if got := formatETA(26 * time.Hour); got != "1d 2h" {
		t.Errorf("26h -> expected '1d 2h', got %q", got)
	}
	if got := formatETA(10 * 24 * time.Hour); got != "1w 3d" {
		t.Errorf("10d -> expected '1w 3d', got %q", got)
	}
	// Regression: avgSecs (seconds) × steps must convert correctly — the old
	// code treated seconds as nanoseconds and produced "0m" for any real ETA.
	eta := formatETA(time.Duration(1142) * time.Second * time.Duration(24))
	if eta != "7h 36m" {
		t.Errorf("1142s x 24 steps -> expected '7h 36m', got %q", eta)
	}
}

func TestTickDuration(t *testing.T) {
	if got := tickDuration("", ""); got != "" {
		t.Errorf("empty -> expected '', got %q", got)
	}
	if got := tickDuration("2026-08-06T10:00:00Z", "2026-08-06T10:05:30Z"); got != "5m30s" {
		t.Errorf("expected 5m30s, got %q", got)
	}
	if got := tickDuration("2026-08-06T10:00:00Z", "2026-08-06T10:00:45Z"); got != "45s" {
		t.Errorf("expected 45s, got %q", got)
	}
	// Not finished (no completed_at) → empty.
	if got := tickDuration("2026-08-06T10:00:00Z", ""); got != "" {
		t.Errorf("unfinished -> expected '', got %q", got)
	}
}

func TestSparklineFunc(t *testing.T) {
	tmpl := loadTemplates()
	// Access the registered func map. The template's FuncMap is private; the
	// reliable check is that a template using {{sparkline .}} executes without
	// "function not defined" — which happens at parse time, so simply
	// re-executing the registered template is enough. Instead, verify the func
	// map contains the key by rendering a tiny template that calls it.
	mini, err := tmpl.Clone()
	if err != nil {
		t.Fatal(err)
	}
	mini, err = mini.New("sparkcheck").Parse(`{{sparkline .}}`)
	if err != nil {
		t.Fatalf("template referencing sparkline should parse: %v", err)
	}
	var buf strings.Builder
	if err := mini.ExecuteTemplate(&buf, "sparkcheck", []float64{0.03, 0.03, 0.03}); err != nil {
		t.Fatalf("sparkline exec failed: %v", err)
	}
	if !strings.Contains(buf.String(), "<svg") || !strings.Contains(buf.String(), "polyline") {
		t.Errorf("expected svg polyline, got %q", buf.String())
	}
	// nil series → em-dash.
	buf.Reset()
	if err := mini.ExecuteTemplate(&buf, "sparkcheck", []float64(nil)); err != nil {
		t.Fatalf("sparkline nil exec failed: %v", err)
	}
	if buf.String() != "—" {
		t.Errorf("nil series -> expected em-dash, got %q", buf.String())
	}
}

// TestReadBoardProgress_MarkdownChecklist verifies the markdown task-list
// format ("- [x] R2-1 ..." / "- [ ] R2-2 ...") is counted correctly. Some
// boards (e.g. gitreins2) track tasks as checklists under an "## ... Active"
// section rather than "| ID |" table rows.
func TestReadBoardProgress_MarkdownChecklist(t *testing.T) {
	board := `# Board

## v2 Active

- [x] R2-1 ModelRouter — done
- [x] R2-2 AgentRunner — done
- [ ] R2-3 CriteriaEvaluator — pending
- [ ] R2-4 Evidence store — pending

## [ ] NEVER-DONE — audit

Not counted.
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	done, total := readBoardProgress(path)
	if done != 2 {
		t.Errorf("expected 2 done ([x]), got %d", done)
	}
	if total != 4 {
		t.Errorf("expected 4 total, got %d", total)
	}
}

// TestBoardStepsMarkdownChecklist verifies readBoardSteps parses the markdown
// checklist format ("- [x] R2-1 Title (commit)") used by gitreins2-style boards,
// alongside the table-row format. Done steps get their commit from the trailing
// "(<hash>)" reference; the first pending row is marked "active" (next up).
func TestBoardStepsMarkdownChecklist(t *testing.T) {
	board := `# b
## v2 Active
- [x] R2-1 ModelRouter — done (b0420b9)
- [ ] R2-2 AgentRunner — pending
- [ ] R2-3 Evidence — pending
## Completed
| T00 | Bootstrap | — |
`
	dir := t.TempDir()
	path := filepath.Join(dir, "t.md")
	if err := os.WriteFile(path, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	steps := readBoardSteps(path)
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d: %+v", len(steps), steps)
	}
	byID := map[string]BoardStep{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	if s := byID["R2-1"]; s.Status != "done" || s.Commit != "b0420b9" {
		t.Errorf("R2-1 wrong: %+v", s)
	}
	if s := byID["R2-2"]; s.Status != "active" { // first pending = next up
		t.Errorf("R2-2 should be active (first pending): %+v", s)
	}
	if s := byID["R2-3"]; s.Status != "pending" {
		t.Errorf("R2-3 wrong: %+v", s)
	}
	if s := byID["T00"]; s.Status != "done" {
		t.Errorf("T00 wrong: %+v", s)
	}
}

// TestCIConclusionCache is the DASH-PERF-001 regression: a cache hit must
// return without re-running the gh subprocess, and a TTL-expired entry must
// refetch. A counting runner stands in for the real `gh run list` subprocess.
func TestCIConclusionCache(t *testing.T) {
	calls := 0
	g := &Generator{
		ciCache: make(map[string]ciCacheEntry),
		ciTTL:   60 * time.Second,
		ciRunner: func(workdir string) string {
			calls++
			return "success"
		},
	}
	wd := t.TempDir()

	// Cold cache → exactly one fetch.
	if got := g.ciConclusion(wd); got != "success" {
		t.Fatalf("cold cache: got %q, want success", got)
	}
	if calls != 1 {
		t.Fatalf("cold cache: expected 1 fetch, got %d", calls)
	}

	// Cache hit within TTL → no re-exec.
	if got := g.ciConclusion(wd); got != "success" {
		t.Fatalf("cache hit: got %q, want success", got)
	}
	if calls != 1 {
		t.Errorf("cache hit: expected 0 additional fetches, got %d total", calls)
	}

	// Empty workdir → never touches the runner or the cache.
	if got := g.ciConclusion(""); got != "" {
		t.Errorf("empty workdir: got %q, want empty", got)
	}
	if calls != 1 {
		t.Errorf("empty workdir: expected no fetch, got %d total", calls)
	}

	// TTL expiry → refetches exactly once.
	g.ciMu.Lock()
	g.ciCache[wd] = ciCacheEntry{conclusion: "success", fetchedAt: time.Now().Add(-61 * time.Second)}
	g.ciMu.Unlock()
	if got := g.ciConclusion(wd); got != "success" {
		t.Fatalf("after TTL expiry: got %q, want success", got)
	}
	if calls != 2 {
		t.Errorf("after TTL expiry: expected 1 refetch, got %d total", calls)
	}
}

// TestWarmCIConclusions verifies the collect() warm pass fetches each
// workdir exactly once, skips empty workdirs, and that a second warm within
// the TTL window runs zero subprocesses (per-render gh count drops to 0
// after warm-up).
func TestWarmCIConclusions(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	g := &Generator{
		ciCache: make(map[string]ciCacheEntry),
		ciTTL:   60 * time.Second,
		ciRunner: func(workdir string) string {
			mu.Lock()
			calls[workdir]++
			mu.Unlock()
			return "failure"
		},
	}
	workdirs := []string{"/repo/one", "/repo/two", "/repo/three", ""}

	g.warmCIConclusions(workdirs)

	mu.Lock()
	if calls["/repo/one"] != 1 || calls["/repo/two"] != 1 || calls["/repo/three"] != 1 {
		mu.Unlock()
		t.Fatalf("expected exactly 1 fetch per workdir, got %v", calls)
	}
	_, emptyFetched := calls[""]
	mu.Unlock()
	if emptyFetched {
		t.Errorf("empty workdir must not be fetched")
	}

	// Warm again within TTL → cache hits, zero subprocesses.
	g.warmCIConclusions(workdirs)
	mu.Lock()
	defer mu.Unlock()
	if calls["/repo/one"] != 1 {
		t.Errorf("warm re-run: expected no refetch, got %v", calls)
	}
}

// TestWarmCIConclusionsBoundedConcurrency verifies the cold-cache warm pass
// never exceeds ciMaxConcurrent gh subprocesses in flight.
func TestWarmCIConclusionsBoundedConcurrency(t *testing.T) {
	var inFlight, maxInFlight int32
	g := &Generator{
		ciCache: make(map[string]ciCacheEntry),
		ciTTL:   60 * time.Second,
		ciRunner: func(workdir string) string {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return "success"
		},
	}
	workdirs := make([]string, 0, 24)
	for i := 0; i < 24; i++ {
		workdirs = append(workdirs, fmt.Sprintf("/repo/%02d", i))
	}
	g.warmCIConclusions(workdirs)
	if m := atomic.LoadInt32(&maxInFlight); m > ciMaxConcurrent {
		t.Errorf("max in-flight %d exceeds bound %d", m, ciMaxConcurrent)
	} else if m < 2 {
		t.Errorf("expected concurrent fetches, max in-flight was %d", m)
	}
}

// TestNewGeneratorInitializesCICache verifies NewGenerator wires the cache,
// the default TTL, and the real gh runner so the cache survives across
// renders (the DASH-PERF-001 contract).
func TestNewGeneratorInitializesCICache(t *testing.T) {
	g := NewGenerator(nil)
	if g.ciCache == nil {
		t.Fatal("NewGenerator must initialize the CI conclusion cache")
	}
	if g.ciRunner == nil {
		t.Fatal("NewGenerator must wire the default gh runner")
	}
	if got := g.ciTTLValue(); got != ciCacheDefaultTTL {
		t.Errorf("default TTL = %v, want %v", got, ciCacheDefaultTTL)
	}
}
