package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
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

// TestBatchStatsParityWithPerProjectQueries is the DASH-PERF-003 correctness
// contract: the batched window-query path (batchCompletedSamples /
// batchTickHealth + costSeriesFromSamples / observabilityFromSamples /
// tickSamplesFromCompleted + learnedETAFromSamples) must produce IDENTICAL
// results to the per-project queries it replaced (recentCostSeries,
// recentTickHealth, observabilityStats, learnedETA) across cap boundaries
// (>20 ticks), status mixes, and edge-case timestamps.
func TestBatchStatsParityWithPerProjectQueries(t *testing.T) {
	ctx := context.Background()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	workdirs := map[string]string{}
	mustProject := func(name string) {
		t.Helper()
		wd := t.TempDir()
		workdirs[name] = wd
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "https://example.com/" + name,
			Workdir: wd, Weight: 10, Priority: 3, CooldownS: 900,
			DecayRate: 1.0, Model: "test", Provider: "test", Enabled: true,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	comp := func(i int) string {
		return base.Add(time.Duration(i)*time.Hour + 30*time.Minute).Format(time.RFC3339)
	}
	mustTick := func(id, proj string, status database.TickStatus, spawned time.Time, completed string, cost float64) {
		t.Helper()
		if err := database.CreateTick(ctx, db, &database.Tick{
			ID: id, ProjectName: proj, Status: status,
			SpawnedAt:   spawned.Format(time.RFC3339),
			CompletedAt: completed,
			CostUSD:     cost,
		}); err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}

	mustProject("alpha")
	// 25 completed (exceeds every cap), 3 failed, 2 timeout, 1 running.
	for i := range 25 {
		c := comp(i)
		if i == 21 {
			c = "not-a-timestamp" // unparseable window: consumed by LIMIT, skipped by math
		}
		mustTick(fmt.Sprintf("a-c%02d", i), "alpha", database.StatusCompleted, base.Add(time.Duration(i)*time.Hour), c, float64(i)/10)
	}
	for i := range 3 {
		mustTick(fmt.Sprintf("a-f%d", i), "alpha", database.StatusFailed, base.Add(time.Duration(30+i)*time.Hour), "", 0)
	}
	for i := range 2 {
		mustTick(fmt.Sprintf("a-t%d", i), "alpha", database.StatusTimeout, base.Add(time.Duration(35+i)*time.Hour), "", 0)
	}
	mustTick("a-run", "alpha", database.StatusRunning, base.Add(40*time.Hour), "", 0)

	mustProject("beta")
	for i := range 5 {
		mustTick(fmt.Sprintf("b-c%02d", i), "beta", database.StatusCompleted, base.Add(time.Duration(i)*time.Hour), comp(i), float64(i+1))
	}

	mustProject("gamma") // no ticks at all

	g := &Generator{db: db}
	samples := g.batchCompletedSamples(ctx)
	health := g.batchTickHealth(ctx)

	// Batch shape: capped at 20, newest-first.
	if got := len(samples["alpha"]); got != 20 {
		t.Errorf("batchCompletedSamples(alpha) = %d samples, want 20 (capped)", got)
	}
	if got := len(samples["beta"]); got != 5 {
		t.Errorf("batchCompletedSamples(beta) = %d samples, want 5", got)
	}
	if got := len(samples["gamma"]); got != 0 {
		t.Errorf("batchCompletedSamples(gamma) = %d samples, want 0", got)
	}
	if got := samples["alpha"][0].spawnedAt; got != base.Add(24*time.Hour).Format(time.RFC3339) {
		t.Errorf("newest alpha sample = %q, want a-c24", got)
	}
	// alpha's last-10 by spawned_at: a-run(40h) + 2 timeout + 3 failed + 4 completed.
	if got := health["alpha"]; got != (tickHealth{total: 10, failed: 5}) {
		t.Errorf("batchTickHealth(alpha) = %+v, want total=10 failed=5", got)
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		// Cost sparkline parity.
		wantCost := g.recentCostSeries(ctx, name, 12)
		gotCost := costSeriesFromSamples(samples[name], 12)
		if !reflect.DeepEqual(gotCost, wantCost) {
			t.Errorf("%s: costSeriesFromSamples = %v, recentCostSeries = %v", name, gotCost, wantCost)
		}
		// Recent tick health parity.
		wantTotal, wantFailed := g.recentTickHealth(ctx, name, 10)
		got := health[name]
		if got.total != wantTotal || got.failed != wantFailed {
			t.Errorf("%s: batchTickHealth = %+v, recentTickHealth = (%d,%d)", name, got, wantTotal, wantFailed)
		}
		// Observability parity (board 3 done / 7 total → non-trivial ETA).
		wantAvg, wantAvgCost, wantPct, wantEta, wantAt, wantProj := g.observabilityStats(ctx, name, 3, 7, wantTotal, wantFailed)
		gotAvg, gotAvgCost, gotPct, gotEta, gotAt, gotProj := observabilityFromSamples(samples[name], 3, 7, got.total, got.failed)
		if gotAvg != wantAvg || gotAvgCost != wantAvgCost || gotPct != wantPct || gotEta != wantEta || gotProj != wantProj {
			t.Errorf("%s: observability mismatch: batched=(%d,%v,%d,%q,%v) per-query=(%d,%v,%d,%q,%v)",
				name, gotAvg, gotAvgCost, gotPct, gotEta, gotProj, wantAvg, wantAvgCost, wantPct, wantEta, wantProj)
		}
		if gotAt != wantAt {
			wt, err1 := time.Parse(time.RFC3339, wantAt)
			gt, err2 := time.Parse(time.RFC3339, gotAt)
			if err1 != nil || err2 != nil || wt.Sub(gt) > 2*time.Second || wt.Sub(gt) < -2*time.Second {
				t.Errorf("%s: completionAt mismatch: batched=%q per-query=%q", name, gotAt, wantAt)
			}
		}
		// Learned-ETA parity (fleet prior built once, shared by both paths).
		steps := []BoardStep{
			{ID: "T01", Title: "bootstrap", Status: "done", Commit: "abc123"},
			{ID: "T02", Title: "implement daemon core", Status: "active"},
			{ID: "T03", Title: "test the kill-shot", Status: "pending"},
			{ID: "T04", Title: "docs readme", Status: "pending"},
		}
		fleet := g.fleetLearned(ctx)
		wantLEta, wantLAt, wantLBreak, wantLProj := g.learnedETA(ctx, name, workdirs[name], steps, fleet)
		gotLEta, gotLAt, gotLBreak, gotLProj := learnedETAFromSamples(steps, tickSamplesFromCompleted(workdirs[name], samples[name]), fleet)
		if wantLEta != gotLEta || wantLProj != gotLProj {
			t.Errorf("%s: learnedETA mismatch: batched=(%v,%v) per-query=(%v,%v)",
				name, gotLEta, gotLProj, wantLEta, wantLProj)
		}
		// Breakdown parts tie on equal durations, and equal-duration parts
		// order by map iteration (cosmetic) — compare as a multiset.
		if !sameBreakdownParts(wantLBreak, gotLBreak) {
			t.Errorf("%s: learnedETA breakdown mismatch: batched=%q per-query=%q", name, gotLBreak, wantLBreak)
		}
		if gotLAt != wantLAt {
			wt, err1 := time.Parse(time.RFC3339, wantLAt)
			gt, err2 := time.Parse(time.RFC3339, gotLAt)
			if err1 != nil || err2 != nil || wt.Sub(gt) > 2*time.Second || wt.Sub(gt) < -2*time.Second {
				t.Errorf("%s: learnedETA completionAt mismatch: batched=%q per-query=%q", name, gotLAt, wantLAt)
			}
		}
	}
}

// sameBreakdownParts compares two etaBreakdown strings as multisets of
// "type dur" parts. Equal-duration parts tie-break by map iteration order in
// etaBreakdown (cosmetic), so the string itself may differ while the parts
// are identical.
func sameBreakdownParts(a, b string) bool {
	if a == b {
		return true
	}
	parts := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, " + ")
	}
	pa, pb := parts(a), parts(b)
	sort.Strings(pa)
	sort.Strings(pb)
	return reflect.DeepEqual(pa, pb)
}

// TestFleetLearnedDeterministic verifies the concurrently-accumulated fleet
// prior (DASH-PERF-003) is reproducible across runs: bucket totals and the
// overall aggregate must not depend on goroutine interleaving.
func TestFleetLearnedDeterministic(t *testing.T) {
	ctx := context.Background()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	base := time.Now().UTC().Add(-72 * time.Hour)
	for p := 0; p < 3; p++ {
		name := fmt.Sprintf("proj-%d", p)
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "https://example.com/" + name,
			Workdir: t.TempDir(), Weight: 10, Priority: 3, CooldownS: 900,
			DecayRate: 1.0, Model: "test", Provider: "test", Enabled: true,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
		for i := 0; i < 30; i++ {
			sp := base.Add(time.Duration(p*40+i) * time.Hour)
			if err := database.CreateTick(ctx, db, &database.Tick{
				ID:          fmt.Sprintf("%s-c%02d", name, i),
				ProjectName: name,
				Status:      database.StatusCompleted,
				SpawnedAt:   sp.Format(time.RFC3339),
				CompletedAt: sp.Add(45 * time.Minute).Format(time.RFC3339),
				CostUSD:     float64(i) / 7,
			}); err != nil {
				t.Fatalf("CreateTick: %v", err)
			}
		}
	}

	g := &Generator{db: db}
	m1 := g.fleetLearned(ctx)
	m2 := g.fleetLearned(ctx)
	if m1.overall.count != m2.overall.count || m1.overall.total != m2.overall.total {
		t.Errorf("fleetLearned overall nondeterministic: %+v vs %+v", m1.overall, m2.overall)
	}
	if len(m1.byType) != len(m2.byType) {
		t.Errorf("fleetLearned byType count nondeterministic: %d vs %d", len(m1.byType), len(m2.byType))
	}
	for typ, ds := range m1.byType {
		ds2, ok := m2.byType[typ]
		if !ok || ds.count != ds2.count || ds.total != ds2.total {
			t.Errorf("fleetLearned byType[%s] nondeterministic: %+v vs %+v", typ, ds, ds2)
		}
	}
}
