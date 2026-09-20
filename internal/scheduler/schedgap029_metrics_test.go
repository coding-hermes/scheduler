package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-029: Tick outcome metrics (tokens, cost, commits, files) must be
// populated from real gateway usage + git, not universally zero. These tests
// cover: (a) gateway branch Wait() returns real tokens from a crafted Response;
// (b) cost computation for known + unknown models; (c) git commit/file counting
// against a real temp git repo; (d) full end-to-end persistence through
// LifecycleTracker.Complete.

// ── Test: gateway branch Wait() returns real tokens ──

// TestSCHEDGAP029_GatewayWaitReturnsRealTokens proves the gateway-completed
// branch of SpawnedTick.Wait() populates TokensIn/TokensOut/CostUSD/Commits/
// FilesChanged from the gateway response usage, not zeros.
func TestSCHEDGAP029_GatewayWaitReturnsRealTokens(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-gap029",
			"status": "completed",
			"output": []map[string]any{},
			"usage": map[string]int{
				"input_tokens":  12000,
				"output_tokens": 3500,
				"total_tokens":  15500,
			},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-test", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:    "gap029-metrics",
		Workdir: t.TempDir(), // no .git → commits=0, files=0
	}
	tick, err := spawner.Spawn(project, "gap029-metrics-2026-08-11-12-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}

	outcome := tick.Wait()

	if outcome.Status != TickCompleted {
		t.Fatalf("status = %s, want %s", outcome.Status, TickCompleted)
	}
	if outcome.TokensIn != 12000 {
		t.Errorf("TokensIn = %d, want 12000", outcome.TokensIn)
	}
	if outcome.TokensOut != 3500 {
		t.Errorf("TokensOut = %d, want 3500", outcome.TokensOut)
	}
	if outcome.CostUSD <= 0 {
		t.Errorf("CostUSD = %f, want > 0", outcome.CostUSD)
	}
	// No .git in temp dir → commits and files should be 0 (graceful).
	if outcome.Commits != 0 {
		t.Errorf("Commits = %d, want 0 (no .git)", outcome.Commits)
	}
	if outcome.FilesChanged != 0 {
		t.Errorf("FilesChanged = %d, want 0 (no .git)", outcome.FilesChanged)
	}
}

// ── Test: cost computation for known + unknown models ──

func TestSCHEDGAP029_ComputeCost_KnownModel(t *testing.T) {
	// deepseek-v4-flash: $0.14/1M in, $0.28/1M out (static map fallback)
	cost := computeCostUSD("deepseek", "deepseek-v4-flash", routerRate{}, 1000000, 1000000)
	wantIn := 0.14
	wantOut := 0.28
	want := wantIn + wantOut
	if cost < want*0.99 || cost > want*1.01 {
		t.Errorf("cost for 1M/1M deepseek-v4-flash = %.6f, want ~%.6f", cost, want)
	}
}

func TestSCHEDGAP029_ComputeCost_UnknownModel(t *testing.T) {
	// Unknown model falls back to fixed per-token rates.
	cost := computeCostUSD("", "totally-unknown-model", routerRate{}, 1000, 500)
	want := float64(1000)*estCostPerIn + float64(500)*estCostPerOut
	if cost < want*0.99 || cost > want*1.01 {
		t.Errorf("cost for unknown model = %.6f, want ~%.6f (fallback)", cost, want)
	}
	if cost <= 0 {
		t.Errorf("cost for unknown model must be > 0, got %f", cost)
	}
}

func TestSCHEDGAP029_ComputeCost_ZeroTokens(t *testing.T) {
	cost := computeCostUSD("deepseek", "deepseek-v4-flash", routerRate{}, 0, 0)
	if cost != 0 {
		t.Errorf("cost for 0 tokens = %f, want 0", cost)
	}
}

// ── Test: git commit/file counting against a real temp repo ──

// makeTempGitRepo creates a real git repo in a temp dir, makes N commits
// each touching a file, and returns the repo path. Commits are timestamped
// at commit time (real wall clock). Returns the repo dir.
func makeTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	return dir
}

// gitCommitAt commits filename into dir with an EXPLICIT committer/author date,
// so the repo's commit ordinals are chosen by the test instead of by the wall
// clock. Pre-seam these tests slept 1.1s between commits to step the real clock
// into the next second (git stores whole-second ordinals); pinning the date
// removes the sleep entirely and makes the window boundaries exact — git's
// --since/--until are inclusive at whole seconds, verified against a scratch
// repo: a commit at 12:00:05 is IN a window starting 12:00:05 and OUT of one
// starting 12:00:06.
func gitCommitAt(t *testing.T, dir, filename, content string, when time.Time) {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := writeFile(path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+when.UTC().Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+when.UTC().Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("add", filename)
	run("commit", "-m", "add "+filename)
}

// gitCommitDate returns a commit's committer date by its (1-based, newest-first)
// position, and fails loudly when the position does not exist — the premise
// every window assertion below rests on.
func gitCommitDate(t *testing.T, dir string, skip int) time.Time {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "log", fmt.Sprintf("--skip=%d", skip), "-1", "--format=%cI")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log --skip=%d: %v", skip, err)
	}
	raw := strings.TrimSpace(string(out))
	d, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("commit date %q is not RFC3339: %v", raw, err)
	}
	return d
}

func writeFile(path, content string) error {
	return exec.Command("bash", "-c", fmt.Sprintf("cat > %s <<'EOF'\n%s\nEOF", path, content)).Run()
}

func TestSCHEDGAP029_GitCommitCount_RealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := makeTempGitRepo(t)

	// Three commits at PINNED instants — no sleep anywhere. a/b/c are one
	// second apart so each window boundary below can be placed strictly
	// between two of them, which is exactly the discrimination the old
	// "commit, sleep 1.1s, commit" choreography was approximating.
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	gitCommitAt(t, dir, "a.txt", "alpha", base)
	gitCommitAt(t, dir, "b.txt", "beta", base.Add(1*time.Second))
	gitCommitAt(t, dir, "c.txt", "gamma", base.Add(2*time.Second))

	// Premise, read back from git itself: the ordinals are the ones the test
	// asked for. Without this a silent date-pin failure would turn the
	// window assertions below into a tautology.
	gotA, gotB, gotC := gitCommitDate(t, dir, 2), gitCommitDate(t, dir, 1), gitCommitDate(t, dir, 0)
	if !gotA.Equal(base) || !gotB.Equal(base.Add(time.Second)) || !gotC.Equal(base.Add(2*time.Second)) {
		t.Fatalf("premise: pinned commit dates = (%s, %s, %s), want (%s, %s, %s) — git ignored the date pin",
			gotA, gotB, gotC, base, base.Add(time.Second), base.Add(2*time.Second))
	}

	// The window opens STRICTLY after a.txt (base+1s, where b.txt sits — git's
	// --since is inclusive, so the boundary itself is in-window) and closes at
	// c.txt inclusive. a.txt is therefore excluded and b.txt + c.txt counted.
	startWindow := base.Add(1 * time.Second)
	endWindow := base.Add(2 * time.Second)

	commits, files := countGitChanges(dir, startWindow, endWindow)
	if commits != 2 {
		t.Errorf("commits in window = %d, want 2 (b.txt + c.txt)", commits)
	}
	if files != 2 {
		t.Errorf("files in window = %d, want 2 (b.txt + c.txt)", files)
	}
	// The other side of the boundary: opening one second later must drop b.txt
	// too. This is what makes the count above a measurement rather than an
	// accident of git's inclusivity.
	commitsLater, _ := countGitChanges(dir, base.Add(2*time.Second), base.Add(2*time.Second))
	if commitsLater != 1 {
		t.Errorf("commits in the c.txt-only window = %d, want 1 — the window boundary must exclude a.txt AND b.txt", commitsLater)
	}
}

func TestSCHEDGAP029_GitCommitCount_ZeroCommitsOutsideWindow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := makeTempGitRepo(t)

	base := time.Date(2026, 9, 20, 12, 30, 0, 0, time.UTC)
	gitCommitAt(t, dir, "a.txt", "alpha", base)

	// Window strictly AFTER the commit — the pinned date removes the sleep the
	// pre-seam test used to push the real clock past the commit.
	start := base.Add(time.Second)
	end := start.Add(10 * time.Second)
	if got := gitCommitDate(t, dir, 0); !got.Before(start) {
		t.Fatalf("premise: commit date %s is not before the window start %s — the window would have something to exclude", got, start)
	}

	commits, files := countGitChanges(dir, start, end)
	if commits != 0 {
		t.Errorf("commits outside window = %d, want 0", commits)
	}
	if files != 0 {
		t.Errorf("files outside window = %d, want 0", files)
	}
}

func TestSCHEDGAP029_GitCommitCount_NonGitDir(t *testing.T) {
	dir := t.TempDir() // no .git
	commits, files := countGitChanges(dir, time.Now().Add(-time.Hour), time.Now())
	if commits != 0 {
		t.Errorf("commits in non-git dir = %d, want 0", commits)
	}
	if files != 0 {
		t.Errorf("files in non-git dir = %d, want 0", files)
	}
}

// ── Test: full end-to-end persistence ──

// TestSCHEDGAP029_FullFlow_PersistsMetrics simulates the full lifecycle:
// gateway response with usage → SpawnedTick.Wait() → LifecycleTracker.Complete
// → read the row back from SQLite and verify tokens/cost/commits/files are
// persisted.
func TestSCHEDGAP029_FullFlow_PersistsMetrics(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap029-e2e")
	lt := NewLifecycleTracker(db)

	// Create a temp git repo with one commit inside the window.
	hasGit := false
	if _, err := exec.LookPath("git"); err == nil {
		gdir := makeTempGitRepo(t)
		// real.go and feature.go at PINNED instants (was: two 200ms sleeps
		// around a wall-clock window). The window opens strictly after
		// real.go and includes feature.go.
		gbase := time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC)
		gitCommitAt(t, gdir, "real.go", "package main", gbase)
		startWin := gbase.Add(time.Second)
		gitCommitAt(t, gdir, "feature.go", "package feature", startWin)
		endWin := startWin.Add(time.Second)

		// Verify the repo itself counts correctly. This test's own fixture is
		// asserted to hold ONE commit in the window; the earlier shape logged
		// the pre-check and carried on, so a fixture that landed zero commits
		// would still hand hasGit=true and 1/1 below to the persistence
		// assertions — a green test measuring nothing.
		c, f := countGitChanges(gdir, startWin, endWin)
		if c != 1 || f != 1 {
			t.Fatalf("fixture premise: commits=%d files=%d in the window, want 1/1 (window %s..%s)",
				c, f, startWin.Format(time.RFC3339), endWin.Format(time.RFC3339))
		}
		hasGit = true
	}

	// Simulate the Wait() outcome from a gateway tick with real usage.
	tokensIn := 25000
	tokensOut := 5000
	model := "deepseek-v4-flash"
	cost := computeCostUSD("deepseek", model, routerRate{}, tokensIn, tokensOut)
	commits, files := 0, 0
	if hasGit {
		commits, files = 1, 1
	}

	outcome := TickOutcome{
		TickID:       "gap029-e2e-2026-08-11-12-00-00",
		Project:      "gap029-e2e",
		SessionID:    "resp-gap029-e2e",
		Started:      time.Now().Add(-5 * time.Minute),
		Finished:     time.Now(),
		Status:       TickCompleted,
		TokensIn:     tokensIn,
		TokensOut:    tokensOut,
		CostUSD:      cost,
		Commits:      commits,
		FilesChanged: files,
	}

	// Enqueue + start + complete (mimics slot_pool.go flow).
	tickID := outcome.TickID
	if err := lt.Enqueue("gap029-e2e", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	if err := lt.Complete(outcome); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Read the row back and verify all metrics persisted.
	var dbTokensIn, dbTokensOut, dbCommits, dbFiles int
	var dbCost float64
	var dbStatus string
	err := db.QueryRow(`
		SELECT tokens_in, tokens_out, cost_usd, commits, files_changed, status
		FROM ticks WHERE id = ?
	`, tickID).Scan(&dbTokensIn, &dbTokensOut, &dbCost, &dbCommits, &dbFiles, &dbStatus)
	if err != nil {
		t.Fatalf("query tick: %v", err)
	}

	if dbStatus != "completed" {
		t.Errorf("status = %q, want 'completed'", dbStatus)
	}
	if dbTokensIn != tokensIn {
		t.Errorf("tokens_in = %d, want %d", dbTokensIn, tokensIn)
	}
	if dbTokensOut != tokensOut {
		t.Errorf("tokens_out = %d, want %d", dbTokensOut, tokensOut)
	}
	if dbCost <= 0 {
		t.Errorf("cost_usd = %f, want > 0", dbCost)
	}
	if dbCommits != commits {
		t.Errorf("commits = %d, want %d", dbCommits, commits)
	}
	if dbFiles != files {
		t.Errorf("files_changed = %d, want %d", dbFiles, files)
	}

	t.Logf("PERSISTED: tokens_in=%d tokens_out=%d cost_usd=%.6f commits=%d files_changed=%d status=%s",
		dbTokensIn, dbTokensOut, dbCost, dbCommits, dbFiles, dbStatus)
}

// ── Test: Complete() persists commits/files_changed ──

// TestSCHEDGAP029_LifecycleComplete_PersistsCommitsAndFiles proves the
// updated Complete() writes commits and files_changed columns.
func TestSCHEDGAP029_LifecycleComplete_PersistsCommitsAndFiles(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap029-cf")
	lt := NewLifecycleTracker(db)
	tickID := "gap029-cf-1"

	if err := lt.Enqueue("gap029-cf", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}

	now := time.Now().UTC()
	if err := lt.Complete(TickOutcome{
		TickID:       tickID,
		Project:      "gap029-cf",
		Started:      now.Add(-time.Minute),
		Finished:     now,
		Status:       TickCompleted,
		TokensIn:     5000,
		TokensOut:    1000,
		CostUSD:      0.035,
		Commits:      3,
		FilesChanged: 7,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var commits, files, tin, tout int
	var cost float64
	err := db.QueryRow(`
		SELECT commits, files_changed, tokens_in, tokens_out, cost_usd
		FROM ticks WHERE id = ?
	`, tickID).Scan(&commits, &files, &tin, &tout, &cost)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if commits != 3 {
		t.Errorf("commits = %d, want 3", commits)
	}
	if files != 7 {
		t.Errorf("files_changed = %d, want 7", files)
	}
	if tin != 5000 {
		t.Errorf("tokens_in = %d, want 5000", tin)
	}
	if tout != 1000 {
		t.Errorf("tokens_out = %d, want 1000", tout)
	}
	if cost < 0.034 || cost > 0.036 {
		t.Errorf("cost_usd = %f, want ~0.035", cost)
	}
}

// ── Test: gateway spawn path carries real usage through to SlotPool ──

// TestSCHEDGAP029_SlotPoolGatewayMetricsPersist drives the real SlotPool.Spawn
// → Wait() → lifecycle.Complete path through a gateway mock and verifies the
// persisted row has non-zero tokens.
func TestSCHEDGAP029_SlotPoolGatewayMetricsPersist(t *testing.T) {
	db := newTestDB(t)
	const projName = "gap029-slotpool"
	mustCreateProjectINFRA012(t, db, projName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-slotpool",
			"status": "completed",
			"output": []map[string]any{},
			"usage": map[string]int{
				"input_tokens":  8000,
				"output_tokens": 2000,
				"total_tokens":  10000,
			},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 2)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-test", 10*time.Second))
	spawner.SetNoExecFallback(true)

	lc := NewLifecycleTracker(db)
	pool := NewSlotPool(2, spawner, lc)

	now := time.Now()
	tickID := pool.Spawn(PackedProject{Name: projName, Workdir: t.TempDir()}, now, true, nil)

	// Wait for the tick to complete.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
		if err == nil && status == string(TickCompleted) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var tokensIn, tokensOut int
	var cost float64
	err := db.QueryRow(`SELECT tokens_in, tokens_out, cost_usd FROM ticks WHERE id = ?`, tickID).
		Scan(&tokensIn, &tokensOut, &cost)
	if err != nil {
		t.Fatalf("query tick %s: %v", tickID, err)
	}
	if tokensIn != 8000 {
		t.Errorf("tokens_in = %d, want 8000", tokensIn)
	}
	if tokensOut != 2000 {
		t.Errorf("tokens_out = %d, want 2000", tokensOut)
	}
	if cost <= 0 {
		t.Errorf("cost_usd = %f, want > 0", cost)
	}

	t.Logf("PERSISTED via SlotPool: tokens_in=%d tokens_out=%d cost_usd=%.6f", tokensIn, tokensOut, cost)
}
