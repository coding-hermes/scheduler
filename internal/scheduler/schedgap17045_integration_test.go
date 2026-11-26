package scheduler

// SCHED-GAP-17045 (r2): completed-tick INTEGRATION coverage. The r1 tests
// drove pushTickWork and persistGitCommitSignals as helpers; the tier-2 judge
// finding asked for one COMPLETED TICK through the real tick-exit path —
// SlotPool.Spawn → Spawner.Spawn (gateway) → Wait() completed branch (spawn.go
// measureGatewayWindow + the push at spawn.go:3625) → SlotPool completeTick →
// lifecycle.Complete + adaptiveCooldown/persistGitCommitSignals — with the
// registered workdir a NON-git stand-in and repo_url a local checkout the
// test owns (bare origin included). The strand is created by the mock gateway
// session MID-TURN (inside [reqStart, completeAt] by construction, the same
// evidence shape the SCHED-GAP-17044 e2e test pins), so the only way the work
// reaches the remote or the row gets a real code/board split is the resolved
// repo path.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// strandCommitIntoRepo performs one real commit in dir as the gateway session
// would, with the identity env the bare fixture (gitRun-based) carries, and a
// wall-clock timestamp inside the tick window by construction (it runs inside
// the HTTP handler, between reqStart and completeAt).
func strandCommitIntoRepo(t *testing.T, dir, file, content, msg string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := writeFile(filepath.Join(dir, file), content); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("add", file)
	run("commit", "-m", msg)
}

// mustBareOriginRepo is the integration fixture: a work clone whose main
// tracks a test-owned bare origin (upstream set only when withUpstream — the
// no-upstream arm makes the tick-exit push fail by design), plus one
// date-PINNED root commit. The root commit's date is one minute in the past
// on purpose: git's --since/--until windows are whole-second inclusive, and
// the strand commits land within the same wall-clock second as the fixture
// build (measured: all three commits at one second), so a real-now root
// commit leaks into the tick window and inflates the count by one.
func mustBareOriginRepo(t *testing.T, withUpstream bool) (work, remote string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")
	gitRun(t, base, "init", "--bare", "-b", "main", remote)
	gitRun(t, base, "init", "-b", "main", work)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := writeFile(filepath.Join(work, "a.txt"), "x"); err != nil {
		t.Fatal(err)
	}
	pin := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", work}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+past, "GIT_COMMITTER_DATE="+past)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	pin("add", "a.txt")
	pin("commit", "-m", "init (pinned pre-window)")
	gitRun(t, work, "remote", "add", "origin", remote)
	if withUpstream {
		gitRun(t, work, "push", "-u", "origin", "main")
	} else {
		// Deliberately NO -u: main has no upstream, so `git push` at tick
		// exit fails — the arm this fixture feeds.
		gitRun(t, work, "push", "origin", "main")
	}
	return work, remote
}

// completedTickGatewayHandler answers a valid completed /v1/responses envelope
// (SCHED-GAP-205 wire shape: assistant message + usage + a function_call item
// so zeroToolSession stays false) after committing the tick's work into repo.
func completedTickGatewayHandler(t *testing.T, repo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The gateway session's work: one CODE commit and one BOARD commit,
		// neither pushed — exactly the strand the tick-exit push exists to
		// recover. Timestamps land inside [reqStart, completeAt] because the
		// handler runs between the two clock captures.
		strandCommitIntoRepo(t, repo, "feature.go", "package feature\n", "tick strand: code", ".coding-hermes", filepath.Join(".coding-hermes", "board"))
		strandCommitIntoRepo(t, repo, filepath.Join(".coding-hermes", "board", "tasks.md"), "# board\n", "tick strand: board")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-gap17045-int",
			"status": "completed",
			"output": []map[string]any{
				{
					"type":      "function_call",
					"name":      "shell",
					"arguments": "{}",
					"call_id":   "call-gap17045-int",
				},
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "committed"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  100000,
				"output_tokens": 2000,
				"total_tokens":  102000,
			},
		})
	}
}

// waitForTerminalTick polls the ticks row until status is terminal, then waits
// for the code/board split stamp (written by the completion goroutine AFTER
// lifecycle.Complete marks the row terminal — adaptiveCooldown runs later in
// the same goroutine) and returns the row's persisted fields.
func waitForTerminalSplitRow(t *testing.T, db *sql.DB, tickID string, wantSplit bool) (status, outcome string, commits, files, codeCommits, boardCommits int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var statusErr error
	for {
		statusErr = db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
		if statusErr == nil && (status == "completed" || status == "failed" || status == "timeout" || status == "deferred") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tick %s never reached a terminal status (last status=%q err=%v)", tickID, status, statusErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if wantSplit {
		// The split stamp trails the terminal write; poll until the code
		// count the fixture guarantees (>=1) shows up, then the rest of the
		// row is stable for the assertions below.
		for {
			if err := db.QueryRow(`SELECT code_commits FROM ticks WHERE id = ?`, tickID).Scan(&codeCommits); err == nil && codeCommits > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("tick %s terminal but code_commits never stamped (persistGitCommitSignals did not run through the tick path)", tickID)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := db.QueryRow(`SELECT outcome, commits, files_changed, COALESCE(code_commits, -99), COALESCE(board_commits, -99)
		FROM ticks WHERE id = ?`, tickID).
		Scan(&outcome, &commits, &files, &codeCommits, &boardCommits); err != nil {
		t.Fatalf("read tick row: %v", err)
	}
	return
}

// poolTick runs one full tick through the pool: SlotPool.Spawn (enqueues the
// row) → gateway stub → Wait() completed branch → completeTick (lifecycle
// + adaptive cooldown). Returns the tick id.
func poolTick(t *testing.T, db *sql.DB, projName, workdir string) string {
	t.Helper()
	srv := httptest.NewServer(completedTickGatewayHandler(t, resolvedRepoFor(t, db, projName)))
	t.Cleanup(srv.Close)

	spawner := NewSpawner(db, 2)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-test", 10*time.Second))
	spawner.SetNoExecFallback(true)

	pool := NewSlotPool(2, spawner, NewLifecycleTracker(db))
	tickID := pool.Spawn(PackedProject{Name: projName, Workdir: workdir}, time.Now(), true, db)
	return tickID
}

// resolvedRepoFor reads the projects row's repo_url back (the "local:<dir>"
// checkout the tick path must resolve to) so the test's gateway stub commits
// into the SAME repo the resolution will find — no duplicated fixture state.
func resolvedRepoFor(t *testing.T, db *sql.DB, projName string) string {
	t.Helper()
	var repoURL string
	if err := db.QueryRow(`SELECT repo_url FROM projects WHERE name = ?`, projName).Scan(&repoURL); err != nil {
		t.Fatalf("read projects.repo_url for %s: %v", projName, err)
	}
	if !strings.HasPrefix(repoURL, "local:") {
		t.Fatalf("repo_url = %q, want local: checkout", repoURL)
	}
	return strings.TrimPrefix(repoURL, "local:")
}

// TestSCHEDGAP17045_CompletedTickPushesResolvedRepo drives ONE completed tick
// end to end (SlotPool.Spawn → Wait() → completeTick) for a stand-in project:
//   - the completed branch measures the RESOLVED repo (commits/files real),
//   - the tick-exit push runs against the RESOLVED repo (the strand reaches
//     the test-owned bare origin; the registered stand-in is non-git, so no
//     push from it could ever do this),
//   - the code/board split stamped through the tick path (adaptiveCooldown →
//     persistGitCommitSignals) is the REAL anatomy (1 code / 1 board), never
//     -1/-1.
func TestSCHEDGAP17045_CompletedTickPushesResolvedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	work, remote := mustBareOriginRepo(t, true)
	standin := t.TempDir() // non-git: the pulse-pm shape

	// Premise: the strand does NOT exist yet — the fixture owns exactly one
	// pushed commit (init); everything on the remote after the tick came from
	// the tick path's push.
	if n := mustRemoteCount(t, remote); n != 1 {
		t.Fatalf("fixture premise: remote main has %d commits, want 1 (fixture init)", n)
	}

	const projName = "gap17045-int-push"
	mustCreateProject17044(t, db, projName, standin, "local:"+work)

	// Fixture premise: a push from the REGISTERED stand-in is impossible
	// (non-git) — the pre-fix behavior strands by construction. The
	// resolution seam (resolveWorkdirOrDefault) is what makes the push below
	// possible at all.
	if ok, detail := pushTickWork(standin); ok {
		t.Fatalf("fixture premise broken: pushTickWork(standin) reported ok (%s)", detail)
	}

	tickID := poolTick(t, db, projName, standin)
	status, outcome, commits, files, codeCommits, boardCommits := waitForTerminalSplitRow(t, db, tickID, true)

	if status != "completed" {
		t.Fatalf("status = %q, want completed — the tick path itself must complete", status)
	}
	if outcome != "committed" {
		t.Errorf("outcome = %q, want committed (the strand is real work)", outcome)
	}
	if commits != 2 || files != 2 {
		t.Errorf("commits=%d files_changed=%d, want 2/2 — measured in the RESOLVED repo (code + board strand)", commits, files)
	}
	if codeCommits != 1 || boardCommits != 1 {
		t.Errorf("code_commits=%d board_commits=%d, want 1/1 — the tick-path split must measure the resolved repo, never -1/-1", codeCommits, boardCommits)
	}
	if n := mustRemoteCount(t, remote); n != 3 {
		t.Errorf("remote main has %d commits, want 3 (fixture init + the tick's code + board strand) — the tick-exit push ran against the RESOLVED repo", n)
	}
}

// TestSCHEDGAP17045_CompletedTickPushFailureNeverFailsTick drives the same
// completed tick with a resolved repo whose branch has NO upstream: the
// tick-exit push fails (STRANDED), and the tick still completes normally —
// a push failure never fails the tick (SCHED-GAP-1694 best-effort contract).
func TestSCHEDGAP17045_CompletedTickPushFailureNeverFailsTick(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	// No upstream by design: `git push` at tick exit fails, the tick must not.
	work, remote := mustBareOriginRepo(t, false)
	standin := t.TempDir()

	const projName = "gap17045-int-noupstream"
	mustCreateProject17044(t, db, projName, standin, "local:"+work)

	tickID := poolTick(t, db, projName, standin)
	status, outcome, commits, _, codeCommits, boardCommits := waitForTerminalSplitRow(t, db, tickID, true)

	if status != "completed" {
		t.Fatalf("status = %q, want completed — a failed push (STRANDED) must never fail the tick", status)
	}
	if outcome != "committed" {
		t.Errorf("outcome = %q, want committed", outcome)
	}
	if commits != 2 {
		t.Errorf("commits = %d, want 2 — the measurement ran in the resolved repo despite the push failure", commits)
	}
	if codeCommits != 1 || boardCommits != 1 {
		t.Errorf("code_commits=%d board_commits=%d, want 1/1 — the split is stamped independently of the push outcome", codeCommits, boardCommits)
	}
	// The strand stayed local: the push failed by design, the remote keeps
	// exactly the fixture init commit.
	if n := mustRemoteCount(t, remote); n != 1 {
		t.Errorf("remote main has %d commits, want 1 — a failed push must not strand-duplicate onto the remote", n)
	}
}
