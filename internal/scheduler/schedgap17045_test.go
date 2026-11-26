package scheduler

// SCHED-GAP-17045: the gateway completed path's PUSH (spawn.go Wait()) and
// the code/board commit SPLIT (adaptive_cooldown's persistGitCommitSignals)
// both still aimed at the REGISTERED stand-in workdir after SCHED-GAP-17044
// fixed only the measurement. A stand-in workdir is deliberately non-git
// (~/.hermes/stand-in/<lane>/<project>); the product lands in the repo_url
// local: checkout. So a lane that committed but did not push logged
// "push STRANDED" even though its commits exist in the resolved repo, and a
// resolved-repo tick stamped -1/-1 (unmeasured) instead of a real
// code/board anatomy. These tests reproduce the stand-in shape and pin both
// sites: resolution via the projects row (repo_url local:) with the exact
// legacy fallback when nothing is resolvable.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mustGitRepoWithBareOrigin creates a working clone with a bare origin and
// its main branch tracking origin/main (the pushTickWork precondition),
// plus one already-pushed root commit.
func mustGitRepoWithBareOrigin(t *testing.T) (work, remote string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")
	gitRun(t, base, "init", "--bare", "-b", "main", remote)
	gitRun(t, base, "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "-m", "init")
	gitRun(t, work, "remote", "add", "origin", remote)
	gitRun(t, work, "push", "-u", "origin", "main") // sets upstream
	return work, remote
}

// mustStrandCommit adds one un-pushed commit to work — the exact strand the
// push site exists to recover.
func mustStrandCommit(t *testing.T, work, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", name)
	gitRun(t, work, "commit", "-m", "strand "+name)
}

func mustRemoteCount(t *testing.T, remote string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "rev-list", "--count", "main").Output()
	if err != nil {
		t.Fatalf("remote rev-list: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("remote count %q: %v", out, err)
	}
	return n
}

// TestSCHEDGAP17045_PushResolvesLocalRepo (AC1a): the push site resolves the
// repo_url checkout for a stand-in project and pushes the strand from THERE.
// The registered workdir is non-git; the only way the strand reaches the
// remote is a push run against the resolved repo.
func TestSCHEDGAP17045_PushResolvesLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	work, remote := mustGitRepoWithBareOrigin(t)
	mustStrandCommit(t, work, "b.txt") // 2 commits total, 1 stranded
	standin := t.TempDir()             // non-git: the pulse-pm shape

	const projName = "gap17045-push-standin"
	mustCreateProject17044(t, db, projName, standin, "local:"+work)

	// Premise: the registered workdir cannot push (non-git), so a push from
	// it — the pre-fix shape — strands by construction.
	if ok, detail := pushTickWork(standin); ok {
		t.Fatalf("fixture premise broken: pushTickWork(%q) from a non-git dir reported ok (%s)", standin, detail)
	}

	resolved := resolveWorkdirOrDefault(db, projName, standin)
	if resolved != work {
		t.Fatalf("resolveWorkdirOrDefault = %q, want the repo_url checkout %q", resolved, work)
	}
	if ok, detail := pushTickWork(resolved); !ok {
		t.Fatalf("pushTickWork(resolved): want ok=true, got false (%s)", detail)
	}
	if n := mustRemoteCount(t, remote); n != 2 {
		t.Fatalf("remote main has %d commit(s), want 2 — the strand was not pushed from the resolved repo", n)
	}
}

// TestSCHEDGAP17045_PushLegacyFallback (AC2a): an unresolvable project (no
// row / nil db) keeps the registered workdir — pre-fix behavior byte for
// byte, including the non-git strand shape.
func TestSCHEDGAP17045_PushLegacyFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)
	work, _ := mustGitRepoWithBareOrigin(t)
	standin := t.TempDir()

	t.Run("missing projects row falls back to registered workdir", func(t *testing.T) {
		if got := resolveWorkdirOrDefault(db, "gap17045-no-such-project", standin); got != standin {
			t.Fatalf("resolveWorkdirOrDefault = %q, want the registered dir %q", got, standin)
		}
		if ok, detail := pushTickWork(standin); ok {
			t.Fatalf("pushTickWork(standin): want ok=false for a non-git registered dir (%s)", detail)
		}
	})

	t.Run("nil db falls back to registered workdir", func(t *testing.T) {
		if got := resolveWorkdirOrDefault(nil, "gap17045-anything", work); got != work {
			t.Fatalf("resolveWorkdirOrDefault(nil db) = %q, want the registered dir %q", got, work)
		}
	})
}

// TestSCHEDGAP17045_SplitResolvesLocalRepo (AC1b): the code/board split for a
// stand-in project measures the resolved repo — real code_commits/
// board_commits, never -1/-1 and never (0,0).
func TestSCHEDGAP17045_SplitResolvesLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	repo := makeTempGitRepo(t)
	standin := t.TempDir() // non-git: the pulse-pm shape

	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	// Pre-window root commit (a real checkout's history), then one code
	// commit and one board-bookkeeping commit inside the tick window.
	gitCommitAt(t, repo, "setup.go", "package setup", base)
	start := base.Add(time.Second)
	gitCommitAt(t, repo, "feature.go", "package feature", start)
	// gitCommitAt writes files without creating parent dirs — make the board
	// subdir first.
	if err := os.MkdirAll(filepath.Join(repo, ".coding-hermes", "board"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCommitAt(t, repo, filepath.Join(".coding-hermes", "board", "tasks.md"), "# board", start)

	// Premise: the repo itself classifies 1 code + 1 board.
	if c, b, ok := classifyGitCommits(repo, start, 2); !ok || c != 1 || b != 1 {
		t.Fatalf("fixture premise: classifyGitCommits(repo) = (%d, %d, %v), want (1, 1, true)", c, b, ok)
	}

	const projName = "gap17045-split-standin"
	mustCreateProject17044(t, db, projName, standin, "local:"+repo)

	outcome := TickOutcome{Project: projName, Started: start, Commits: 2}
	code, board, measured := persistGitCommitSignals(db, outcome, standin)
	if !measured {
		t.Fatalf("persistGitCommitSignals: measured=false — the split was NOT taken in the resolved repo (got %d/%d unmeasured)", code, board)
	}
	if code != 1 || board != 1 {
		t.Fatalf("persistGitCommitSignals = (%d, %d), want (1, 1) measured in the repo_url checkout", code, board)
	}
}

// TestSCHEDGAP17045_SplitLegacyFallback (AC2b): unresolvable projects keep
// the exact pre-fix semantics — measured against the registered workdir,
// with the -1/-1 unmeasured marker for a non-git dir and a real measurement
// when the registered dir IS a git checkout.
func TestSCHEDGAP17045_SplitLegacyFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)

	repo := makeTempGitRepo(t)
	standin := t.TempDir()

	base := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	gitCommitAt(t, repo, "setup.go", "package setup", base)
	start := base.Add(time.Second)
	gitCommitAt(t, repo, "feature.go", "package feature", start)

	t.Run("nil db: registered non-git dir stays -1/-1 unmeasured", func(t *testing.T) {
		outcome := TickOutcome{Project: "gap17045-nil-db", Started: start, Commits: 1}
		code, board, measured := persistGitCommitSignals(nil, outcome, standin)
		if measured || code != -1 || board != -1 {
			t.Fatalf("persistGitCommitSignals(nil db) = (%d, %d, %v), want (-1, -1, false)", code, board, measured)
		}
	})

	t.Run("missing projects row: registered non-git dir stays -1/-1 unmeasured", func(t *testing.T) {
		outcome := TickOutcome{Project: "gap17045-no-row", Started: start, Commits: 1}
		code, board, measured := persistGitCommitSignals(db, outcome, standin)
		if measured || code != -1 || board != -1 {
			t.Fatalf("persistGitCommitSignals(no row) = (%d, %d, %v), want (-1, -1, false)", code, board, measured)
		}
	})

	t.Run("registered dir IS a git repo: measured there directly", func(t *testing.T) {
		const projName = "gap17045-git-workdir"
		// repo_url points elsewhere; the workdir itself is a git checkout,
		// so resolution must return the workdir (first arm of the order).
		mustCreateProject17044(t, db, projName, repo, "local:"+standin)
		outcome := TickOutcome{Project: projName, Started: start, Commits: 1}
		code, board, measured := persistGitCommitSignals(db, outcome, repo)
		if !measured || code != 1 || board != 0 {
			t.Fatalf("persistGitCommitSignals(git workdir) = (%d, %d, %v), want (1, 0, true)", code, board, measured)
		}
	})
}
