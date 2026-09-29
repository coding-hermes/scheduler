package scheduler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1659: the commit counter must see commits that landed on ANY ref
// of the lane's repo, not only the ancestry of the branch the workdir happens
// to have checked out. The normal lane shape is a stable main checkout whose
// tick spawns worker worktrees (wt/<task>), so the tick's commits live on
// branches HEAD cannot see and the old `rev-list --count HEAD` delta read
// zero — 27% of "zero-commit" ticks had in fact committed.
//
// All tests pin seed commits explicitly in the past (1h before the window
// anchor) so the whole-second granularity of git's --since can never pull the
// seed into the window: the anchor is "now", seeds are at now-1h, margins are
// hours, not milliseconds.

// initEmptyGitRepo creates a throwaway git repo with NO commits and identity
// configured, ready for pinned-date commits.
func initEmptyGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, cfg := range [][2]string{{"user.email", "test@example.com"}, {"user.name", "Test"}} {
		out, err := exec.Command("git", "-C", dir, "config", cfg[0], cfg[1]).CombinedOutput()
		if err != nil {
			t.Fatalf("git config %s: %v\n%s", cfg[0], err, out)
		}
	}
	return dir
}

// gitWorktreeAt creates a linked worktree on a NEW branch off the current HEAD
// of dir and returns the worktree path. The main worktree's HEAD does not move.
func gitWorktreeAt(t *testing.T, dir, branch, wtPath string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree add %s: %v\n%s", branch, err, out)
	}
	t.Cleanup(func() {
		exec.Command("git", "-C", dir, "worktree", "remove", "--force", wtPath).Run()
		exec.Command("git", "-C", dir, "branch", "-D", branch).Run()
	})
	return wtPath
}

// gitCommitFileAt writes filename into dir and commits it with an explicit
// author/committer date, so window boundaries are exact regardless of how fast
// the test runs.
func gitCommitFileAt(t *testing.T, dir, filename, content string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+when.UTC().Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+when.UTC().Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", filename)
	run("commit", "-q", "-m", "add "+filename)
}

// mustHEAD returns the current HEAD sha of dir.
func mustHEAD(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// seedRepo builds the standard fixture: one seed commit dated an hour before
// the window anchor, so it sits unambiguously OUTSIDE every window below.
// Returns (dir, preHead) with preTotal always 1.
func seedRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := initEmptyGitRepo(t)
	gitCommitFileAt(t, dir, "seed.txt", "seed", time.Now().Add(-time.Hour))
	return dir, mustHEAD(t, dir)
}

// TestSCHEDGAP1659_CommitOnWorktreeBranchIsCounted: a commit on a worktree
// branch — invisible to the main worktree's HEAD ancestry — IS counted by the
// all-refs window sweep. This is the exact fleet shape that produced the
// 539 verified false zero-commit ticks. Until the window is wired (zero
// windowStart), this test pins the RED.
func TestSCHEDGAP1659_GitMetricsCommitOnWorktreeBranchIsCounted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()

	wt := gitWorktreeAt(t, dir, "wt/gap1659a", dir+"-wt1")
	// Lands on wt/gap1659a, NOT the main worktree's HEAD: the old HEAD-only
	// delta returned 0 here. That zero is exactly the bug.
	gitCommitFileAt(t, wt, "worker.txt", "on worktree branch", spawn)

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits < 1 {
		t.Fatalf("commits = %d, want >= 1 — the worktree-branch commit must be counted (SCHED-GAP-1659)", commits)
	}
}

// TestSCHEDGAP1659_ZeroWindowStartPreservesAncestryOnly pins the compatibility
// contract: a zero windowStart (callers without a spawn anchor) keeps the
// pre-1659 ancestry-only behavior exactly.
func TestSCHEDGAP1659_GitMetricsZeroWindowStartPreservesAncestryOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)

	wt := gitWorktreeAt(t, dir, "wt/gap1659b", dir+"-wt2")
	gitCommitFileAt(t, wt, "worker.txt", "invisible to HEAD", time.Now())

	commits, _, err := gitWorkDelta(dir, preHead, 1, time.Time{})
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits != 0 {
		t.Fatalf("commits = %d, want 0 — zero windowStart must preserve ancestry-only behavior", commits)
	}
}

// TestSCHEDGAP1659_HeadAncestryDeltaStillHolds: the normal path does not
// regress — a HEAD commit inside the window is still counted.
func TestSCHEDGAP1659_GitMetricsHeadAncestryDeltaStillHolds(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()
	gitCommitFileAt(t, dir, "inwindow.txt", "on HEAD", spawn)

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits != 1 {
		t.Fatalf("commits = %d, want 1 — HEAD-ancestry delta must not regress", commits)
	}
}

// TestSCHEDGAP1659_CommitOutsideWindowNotCounted: the all-refs sweep is
// scoped by committer date — a worktree-branch commit DATED before the spawn
// anchor (backdated, like a rebase-carryover) does not count.
func TestSCHEDGAP1659_GitMetricsCommitOutsideWindowNotCounted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()

	wt := gitWorktreeAt(t, dir, "wt/gap1659c", dir+"-wt3")
	gitCommitFileAt(t, wt, "old.txt", "backdated", spawn.Add(-24*time.Hour))

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits != 0 {
		t.Fatalf("commits = %d, want 0 — out-of-window worktree-branch commit must be excluded", commits)
	}
}

// TestSCHEDGAP1659_ConcurrentBranchCommitAfterBaselineCounted pins the
// no-until design: a commit landing on ANOTHER branch after the baseline is
// still counted (never lost) — the window's upper edge is the moment of
// measurement, so a commit landing between the ancestry read and the window
// sweep cannot fall into a gap.
func TestSCHEDGAP1659_GitMetricsConcurrentBranchCommitAfterBaselineCounted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()

	// The commit is on a non-HEAD branch and dated after the window anchor —
	// the same data shape the ancestry-read-vs-sweep race produces.
	wt := gitWorktreeAt(t, dir, "wt/gap1659d", dir+"-wt4")
	gitCommitFileAt(t, wt, "late.txt", "after baseline", spawn)

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits < 1 {
		t.Fatalf("commits = %d, want >= 1 — a commit landing after the ancestry read must not be lost", commits)
	}
}

// TestSCHEDGAP1659_CommitOnRemoteTrackingRefCounted: a fetched remote-tracking
// ref (refs/remotes/...) is an --all ref too — e.g. another lane pushed and
// this repo fetched. Its in-window commits count.
func TestSCHEDGAP1659_GitMetricsCommitOnRemoteTrackingRefCounted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()

	// Build a second repo sharing history, commit there, fetch it into an
	// explicit remote-tracking ref (a bare `fetch <url> <branch>` writes only
	// FETCH_HEAD, which rev-list --all does not traverse).
	other := dir + "-other"
	out, err := exec.Command("git", "clone", "-q", dir, other).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("rm", "-rf", other).Run() })
	gitCommitFileAt(t, other, "remote.txt", "on other main", spawn)
	brOut, berr := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if berr != nil {
		t.Fatalf("abbrev-ref HEAD: %v", berr)
	}
	branch := strings.TrimSpace(string(brOut))
	out, err = exec.Command("git", "-C", dir, "fetch", "-q", other,
		branch+":refs/remotes/other/main").CombinedOutput()
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measurement must be clean, got %v", err)
	}
	if commits < 1 {
		t.Fatalf("commits = %d, want >= 1 — a remote-tracking-ref commit must be counted", commits)
	}
}

// TestSCHEDGAP1659_NoCommitsReturnsZero: an honest zero stays zero — full
// baseline, empty window (only backdated side-branch commits), no error.
func TestSCHEDGAP1659_GitMetricsNoCommitsReturnsZero(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, preHead := seedRepo(t)
	spawn := time.Now()

	wt := gitWorktreeAt(t, dir, "wt/gap1659e", dir+"-wt5")
	gitCommitFileAt(t, wt, "old.txt", "outside window", spawn.Add(-time.Hour))

	commits, _, err := gitWorkDelta(dir, preHead, 1, spawn)
	if err != nil {
		t.Fatalf("measured zero must not report an error, got %v", err)
	}
	if commits != 0 {
		t.Fatalf("commits = %d, want 0 (nothing in window)", commits)
	}
}

// TestSCHEDGAP1659_UnreadableRepoErrorsWithoutPanic: the never-panic contract
// holds on the new path — a non-repo returns the unreadable error and zeroed
// numbers regardless of the window argument.
func TestSCHEDGAP1659_GitMetricsUnreadableRepoErrorsWithoutPanic(t *testing.T) {
	c, f, err := gitWorkDelta(t.TempDir(), "", 0, time.Now())
	if err == nil {
		t.Fatal("non-git dir must report an error")
	}
	if c != 0 || f != 0 {
		t.Fatalf("want 0/0 on unreadable repo, got %d/%d", c, f)
	}
}
