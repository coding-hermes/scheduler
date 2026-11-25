package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17044: the gateway COMPLETED path measured the registered workdir
// blind. A stand-in workdir (a plain non-git dir — the lane's real commits
// land in the repo_url local: checkout the gateway session pushes to) always
// measured (0,0), so lifecycle stamped a real commit-and-push tick
// dry_run/no_changes (pulse-pm-2026-11-25-10-13-20: 3.2M tokens counted as
// dry burn). The fix routes the completed path's measurement through the same
// resolveMeasureWorkdir order the failed/timeout half already used. These
// tests reproduce the pulse-pm shape and pin the sentinel/fallback contract.

// mustCreateProject17044 creates a projects row with an EXPLICIT workdir and
// repo_url — the stand-in shape (non-git workdir + local: repo_url) needs
// different values than mustCreateProjectINFRA012 writes.
func mustCreateProject17044(t *testing.T, db *sql.DB, name, workdir, repoURL string) {
	t.Helper()
	p := &database.Project{
		Name:      name,
		RepoURL:   repoURL,
		Workdir:   workdir,
		Weight:    10,
		Priority:  5,
		CooldownS: 0,
		DecayRate: 1.0,
		Model:     "test-model",
		Provider:  "test-provider",
		Enabled:   true,
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// brokenGitDir is a dir whose .git ENTRY exists (isGitWorkdir true) but is
// not a valid gitdir file — git itself refuses it. The fixture for the
// "resolved but errored → -1/-1 unmeasured" arm.
func brokenGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, ".git"), "garbage-not-a-gitdir\n"); err != nil {
		t.Fatalf("write broken .git: %v", err)
	}
	return dir
}

// TestSCHEDGAP17044_MeasureGatewayWindow_StandinResolvesLocalRepo pins the
// resolution order of the completed path's measurement helper: workdir-if-git
// else repo_url local: checkout, with the -1 sentinel on a resolved-but-broken
// repo and the legacy (0,0) fallback when nothing is resolvable.
func TestSCHEDGAP17044_MeasureGatewayWindow_StandinResolvesLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	repo := makeTempGitRepo(t)
	standin := t.TempDir() // non-git: the pulse-pm shape

	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	// The repo has a pre-window ROOT commit first (like a real checkout):
	// git diff-tree prints nothing for a root commit (no parent to diff
	// against, no --root flag), so a fixture whose ONLY commit is the
	// in-window one would report commits=1 but files=0.
	gitCommitAt(t, repo, "setup.go", "package setup", base)
	start := base.Add(time.Second)
	end := base.Add(2 * time.Second)
	gitCommitAt(t, repo, "pulse.go", "package pulse", start)

	// Premise: the repo itself measures the window. Without this the
	// resolution assertions below would be a tautology.
	if c, f := countGitChanges(repo, start, end); c != 1 || f != 1 {
		t.Fatalf("fixture premise: commits=%d files=%d in window, want 1/1", c, f)
	}

	const projName = "gap17044-standin"
	mustCreateProject17044(t, db, projName, standin, "local:"+repo)

	t.Run("standin workdir resolves local repo_url", func(t *testing.T) {
		commits, files := measureGatewayWindow(db, standin, projName, start, end)
		if commits != 1 || files != 1 {
			t.Errorf("commits=%d files=%d, want 1/1 (measured in the repo_url checkout)", commits, files)
		}
	})

	t.Run("git workdir still measured directly", func(t *testing.T) {
		commits, files := measureGatewayWindow(db, repo, projName, start, end)
		if commits != 1 || files != 1 {
			t.Errorf("commits=%d files=%d, want 1/1", commits, files)
		}
	})

	t.Run("resolved but broken repo stamps -1 unmeasured", func(t *testing.T) {
		const projName2 = "gap17044-broken"
		broken := brokenGitDir(t)
		mustCreateProject17044(t, db, projName2, broken, "local:"+broken)
		commits, files := measureGatewayWindow(db, broken, projName2, start, end)
		if commits != -1 || files != -1 {
			t.Errorf("commits=%d files=%d, want -1/-1 (resolved repo errored — never a fabricated 0)", commits, files)
		}
	})

	t.Run("missing projects row falls back to registered workdir", func(t *testing.T) {
		commits, files := measureGatewayWindow(db, standin, "gap17044-no-such-project", start, end)
		if commits != 0 || files != 0 {
			t.Errorf("commits=%d files=%d, want 0/0 (legacy fallback: non-git registered dir)", commits, files)
		}
	})

	t.Run("non-local repo_url falls back to registered workdir", func(t *testing.T) {
		const projName3 = "gap17044-remote"
		// A DISTINCT stand-in dir: CreateProject rejects two enabled
		// projects sharing one workdir.
		standin3 := t.TempDir()
		mustCreateProject17044(t, db, projName3, standin3, "https://example.com/"+projName3)
		commits, files := measureGatewayWindow(db, standin3, projName3, start, end)
		if commits != 0 || files != 0 {
			t.Errorf("commits=%d files=%d, want 0/0 (legacy fallback: no resolvable repo)", commits, files)
		}
	})

	t.Run("nil db falls back to registered workdir", func(t *testing.T) {
		commits, files := measureGatewayWindow(nil, standin, projName, start, end)
		if commits != 0 || files != 0 {
			t.Errorf("commits=%d files=%d, want 0/0 (nil db → legacy fallback)", commits, files)
		}
	})
}

// TestSCHEDGAP17044_StandinTickCommitsClassifiedCommitted is the pulse-pm
// reproduction end to end: a gateway tick whose session commits to the REAL
// repo (repo_url local:) inside the tick window while the registered workdir
// is a stand-in non-git dir must classify committed, not dry_run/no_changes.
// Driven through SlotPool.Spawn → Wait() → lifecycle.Complete, so the
// persisted row's status/outcome/commits columns are what's asserted.
func TestSCHEDGAP17044_StandinTickCommitsClassifiedCommitted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	repo := makeTempGitRepo(t)
	standin := t.TempDir() // non-git: .git ABSENT, the pulse-pm shape

	const projName = "gap17044-pulse-pm"
	mustCreateProject17044(t, db, projName, standin, "local:"+repo)

	// A pre-window ROOT commit, like a real checkout: git diff-tree prints
	// nothing for a root commit, so a repo whose ONLY commit is the session's
	// would report commits=1 but files_changed=0.
	past := time.Now().Add(-time.Hour)
	gitCommitAt(t, repo, "setup.go", "package setup", past)

	// The mock gateway session: it COMMITS into the real repo mid-turn
	// (exactly the evidence shape — the session committed AND pushed to
	// /home/kara/get-h3/pulse inside the tick window) before answering.
	// The commit's wall-clock timestamp falls inside [reqStart, completeAt]
	// by construction: reqStart is captured before the POST, the commit
	// runs inside the handler, completeAt after the response arrives.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := writeFile(filepath.Join(repo, "pulse.go"), "package pulse\n"); err != nil {
			t.Errorf("gateway session write pulse.go: %v", err)
		}
		cmd := exec.Command("git", "-C", repo, "add", "pulse.go")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("gateway session git add: %v\n%s", err, out)
		}
		cmd = exec.Command("git", "-C", repo, "commit", "-m", "gateway session commits mid-turn")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("gateway session git commit: %v\n%s", err, out)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-gap17044",
			"status": "completed",
			// SCHED-GAP-205: minimal assistant item — completed with
			// non-zero tokens and no assistant output is a gated failure.
			// The function_call item also keeps zeroToolSession false: a
			// session that committed has, by definition, run tools.
			"output": []map[string]any{
				{
					"type":      "function_call",
					"name":      "shell",
					"arguments": "{}",
					"call_id":   "call-gap17044",
				},
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "committed pulse.go"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  3200000,
				"output_tokens": 4000,
				"total_tokens":  3204000,
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
	tickID := pool.Spawn(PackedProject{Name: projName, Workdir: standin}, now, true, nil)

	// Wait for the row to reach a terminal status, then read it back.
	var status string
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
		if err == nil && (status == "completed" || status == "failed" || status == "timeout") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tick %s never reached a terminal status (last status=%q)", tickID, status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var outcome string
	var commits, files int
	var errText sql.NullString
	if err := db.QueryRow(`SELECT outcome, commits, files_changed, error FROM ticks WHERE id = ?`, tickID).
		Scan(&outcome, &commits, &files, &errText); err != nil {
		t.Fatalf("read tick row: %v", err)
	}

	if status != "completed" {
		t.Errorf("status = %q, want completed (error=%q)", status, errText.String)
	}
	if outcome != "committed" {
		t.Errorf("outcome = %q, want committed — the pulse-pm dry_run/no_changes misclassification", outcome)
	}
	if commits < 1 {
		t.Errorf("commits = %d, want >= 1 (the session's commit inside the window)", commits)
	}
	if files < 1 {
		t.Errorf("files_changed = %d, want >= 1", files)
	}

	t.Logf("PERSISTED: status=%s outcome=%s commits=%d files_changed=%d", status, outcome, commits, files)
}

// TestSCHEDGAP17044_FixRoutesThroughResolver is the RED-arm companion: with
// the pre-fix call shape (countGitChanges(st.workdir, ...) — measuring the
// registered stand-in blind) the tick measured (0,0); the fixed shape
// (measureGatewayWindow resolving repo_url) measures the real repo. Both
// shapes are asserted side by side so the test fails against a regression
// back to the blind measurement.
func TestSCHEDGAP17044_FixRoutesThroughResolver(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)
	repo := makeTempGitRepo(t)
	standin := t.TempDir()

	base := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	start, end := base.Add(time.Second), base.Add(2*time.Second)
	// Root commit BEFORE the window (diff-tree prints nothing for a root
	// commit), then the in-window commit.
	gitCommitAt(t, repo, "setup.go", "package setup", base)
	gitCommitAt(t, repo, "work.go", "package work", start)

	// Premise: the fixture repo holds the commit the window should find.
	if c, f := countGitChanges(repo, start, end); c != 1 || f != 1 {
		t.Fatalf("fixture premise: commits=%d files=%d in window, want 1/1", c, f)
	}

	mustCreateProject17044(t, db, "gap17044-red", standin, "local:"+repo)

	// Pre-fix shape: the registered stand-in measured blind.
	oc, of := countGitChanges(standin, start, end)
	// Post-fix shape: the completed path's resolved measurement.
	nc, nf := measureGatewayWindow(db, standin, "gap17044-red", start, end)

	if oc != 0 || of != 0 {
		t.Errorf("pre-fix shape (blind standin measurement) = (%d,%d), want (0,0)", oc, of)
	}
	if nc <= 0 || nf <= 0 {
		t.Errorf("post-fix shape (resolved repo_url measurement) = (%d,%d), want >0", nc, nf)
	}
}
