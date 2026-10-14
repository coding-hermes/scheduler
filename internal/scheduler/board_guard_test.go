package scheduler

// SCHED-GAP-1699: dispatcher-side board-touching commit guard. A CODE row
// commit must never touch .coding-hermes/board/* — worker worktrees hold a
// STALE board snapshot, so a branch that commits board + code deletes every
// row appended since the branch when merged (incidents REMOTE-006: 550-row
// rewrite; REMOTE-008: SCHED-GAP-1695/1696 deleted). These tests exercise
// the exported refusal helper, the classifier, the base resolution, and the
// preamble's BOARD RULE line. The dispatcher-side log surfacing and the
// sh-script hook/guard cases live in board_guard_hook_test.go.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// boardGuardCommit writes filename into dir and commits it (identity via
// env, like gitCommitFileAt but without pinned dates — this guard is
// date-insensitive).
func boardGuardCommit(t *testing.T, dir, filename, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(filename))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", filename)
	run("commit", "-q", "-m", "board-guard fixture: "+filename)
}

// boardGuardRepo builds a repo with a main branch carrying one code file
// (so main...branch diffs are well-formed) and returns its path.
func boardGuardRepo(t *testing.T) string {
	t.Helper()
	dir := initEmptyGitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "branch", "-M", "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch -M main: %v\n%s", err, out)
	}
	boardGuardCommit(t, dir, "README.md", "# seed\n")
	return dir
}

// boardGuardBranch creates branch off main in dir, commits the listed files
// on it, and checks main back out (so successive branches do not nest).
func boardGuardBranch(t *testing.T, dir, branch string, files map[string]string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "-b", branch, "main").CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", branch, err, out)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	for _, name := range names {
		boardGuardCommit(t, dir, name, files[name])
	}
	if out, err := exec.Command("git", "-C", dir, "checkout", "-q", "main").CombinedOutput(); err != nil {
		t.Fatalf("git checkout main: %v\n%s", err, out)
	}
}

// Test1699_EnsureNoBoardTouchingDiff_MixedRefused: a branch diff mixing a
// board path with a code path is refused, wrapping ErrBoardTouchingDiff,
// naming the branch and BOTH offending paths.
func Test1699_EnsureNoBoardTouchingDiff_MixedRefused(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/mixed", map[string]string{
		"internal/scheduler/feature.go":    "package scheduler\n",
		".coding-hermes/board/tasks.jsonl": "{\"id\":1}\n",
	})
	t.Chdir(dir) // the helper diffs the CALLER's cwd (daemon convention)

	err := EnsureNoBoardTouchingDiff("main", "wt/mixed")
	if err == nil {
		t.Fatal("mixed board+code diff must be refused, got nil")
	}
	if !errors.Is(err, ErrBoardTouchingDiff) {
		t.Fatalf("refusal must wrap ErrBoardTouchingDiff; got: %v", err)
	}
	for _, want := range []string{"wt/mixed", "internal/scheduler/feature.go", ".coding-hermes/board/tasks.jsonl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q; got:\n%v", want, err)
		}
	}
}

// Test1699_EnsureNoBoardTouchingDiff_BoardOnlyAndCodeOnlyAllowed: the two
// allowed shapes — every path under .coding-hermes/ (board-only
// truthkeeping) and no board path at all (pure code) — pass.
func Test1699_EnsureNoBoardTouchingDiff_BoardOnlyAndCodeOnlyAllowed(t *testing.T) {
	dir := boardGuardRepo(t)
	t.Chdir(dir) // the helper diffs the CALLER's cwd (daemon convention)

	boardGuardBranch(t, dir, "wt/boardonly", map[string]string{
		".coding-hermes/board/tasks.jsonl": "{\"id\":1}\n",
		".coding-hermes/waves/t1.json":     "{}\n",
	})
	if err := EnsureNoBoardTouchingDiff("main", "wt/boardonly"); err != nil {
		t.Fatalf("board-only changeset must be allowed; got: %v", err)
	}

	boardGuardBranch(t, dir, "wt/codeonly", map[string]string{
		"internal/scheduler/feature.go": "package scheduler\n",
		"docs/note.md":                  "note\n",
	})
	if err := EnsureNoBoardTouchingDiff("main", "wt/codeonly"); err != nil {
		t.Fatalf("pure code changeset must be allowed; got: %v", err)
	}
}

// Test1699_EnsureNoBoardTouchingDiff_MissingBaseIsErrorNotAllow: the check
// must never return a false allow when git cannot evaluate the diff — and
// an infrastructure failure is NOT classified as a board refusal.
func Test1699_EnsureNoBoardTouchingDiff_MissingBaseIsErrorNotAllow(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/any", map[string]string{"x.go": "package x\n"})
	t.Chdir(dir) // the helper diffs the CALLER's cwd (daemon convention)
	err := EnsureNoBoardTouchingDiff("no-such-base", "wt/any")
	if err == nil {
		t.Fatal("ungit-able diff must error, not silently allow")
	}
	if errors.Is(err, ErrBoardTouchingDiff) {
		t.Fatal("infrastructure failure must NOT be classified as a board refusal")
	}
}

// Test1699_EnsureNoBoardTouchingDiff_EmptyArgsRefused: an empty base or
// branch is a refusal (fail-closed), not a skipped check.
func Test1699_EnsureNoBoardTouchingDiff_EmptyArgsRefused(t *testing.T) {
	if err := EnsureNoBoardTouchingDiff("", "wt/x"); !errors.Is(err, ErrBoardTouchingDiff) {
		t.Fatalf("empty base must refuse; got %v", err)
	}
	if err := EnsureNoBoardTouchingDiff("main", "  "); !errors.Is(err, ErrBoardTouchingDiff) {
		t.Fatalf("empty branch must refuse; got %v", err)
	}
}

// Test1699_ClassifyNames: bucket boundaries — board/ is board; other
// .coding-hermes/ content is neither bucket; everything else is outside.
func Test1699_ClassifyNames(t *testing.T) {
	board, outside := classifyDiffNames([]string{
		".coding-hermes/board/tasks.jsonl",
		".coding-hermes/board",
		".coding-hermes/waves/t1.json",
		".coding-hermes/config.toml",
		"cmd/schedulerd/main.go",
		"docs/design-decisions.md",
	})
	if len(board) != 2 || board[0] != ".coding-hermes/board/tasks.jsonl" || board[1] != ".coding-hermes/board" {
		t.Errorf("board bucket = %v, want the two board paths", board)
	}
	if len(outside) != 2 || outside[0] != "cmd/schedulerd/main.go" || outside[1] != "docs/design-decisions.md" {
		t.Errorf("outside bucket = %v, want the two code paths (waves/config are neither)", outside)
	}
}

// Test1699_DetectBaseAndBranchDiff: detectBoardGuardBase resolves main for
// a branch off main, and ensureNoBoardTouchingBranchDiff refuses the mixed
// branch inside the repo dir (the dispatcher-side shape).
func Test1699_DetectBaseAndBranchDiff(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/mixed", map[string]string{
		"a.go":                             "package a\n",
		".coding-hermes/board/tasks.jsonl": "{}\n",
	})

	base, err := detectBoardGuardBase(dir, "wt/mixed")
	if err != nil {
		t.Fatalf("detectBoardGuardBase: %v", err)
	}
	if base != "main" {
		t.Fatalf("base = %q, want main", base)
	}

	if err := ensureNoBoardTouchingBranchDiff(dir, "wt/mixed"); !errors.Is(err, ErrBoardTouchingDiff) {
		t.Fatalf("branch check must refuse mixed diff; got %v", err)
	}
	// Non-git workdir: nil (nothing checkable must not fail the tick).
	if err := ensureNoBoardTouchingBranchDiff(filepath.Join(dir, "does-not-exist"), "wt/mixed"); err != nil {
		t.Fatalf("non-git workdir must return nil; got %v", err)
	}
}

// Test1699_PreambleCarriesBoardRule: the recovery preamble's merge
// instruction must carry the refusal rule (the dispatcher's merge-direction
// surface — scheduler has no merge authority, W6).
func Test1699_PreambleCarriesBoardRule(t *testing.T) {
	manifests := []*WaveManifest{{
		TickID: "t-1", StartedAt: "2026-10-04T10:00:00Z",
		Workers: []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1"}},
	}}
	got := waveRecoveryPreamble(manifests)
	for _, want := range []string{"BOARD RULE (SCHED-GAP-1699)", ".coding-hermes/board/", "EnsureNoBoardTouchingDiff"} {
		if !strings.Contains(got, want) {
			t.Errorf("preamble must contain %q; got:\n%s", want, got)
		}
	}
}

// Test1699_WorkerBranchesFromManifests: distinct first-seen branches, empty
// skipped, nil-safe.
func Test1699_WorkerBranchesFromManifests(t *testing.T) {
	got := workerBranchesFromManifests([]*WaveManifest{
		{Workers: []WaveWorker{{Branch: "wt/b"}, {Branch: "wt/a"}, {Branch: "wt/b"}}},
		nil,
		{Workers: []WaveWorker{{Branch: ""}, {Branch: "wt/c"}}},
	})
	if len(got) != 3 || got[0] != "wt/b" || got[1] != "wt/a" || got[2] != "wt/c" {
		t.Fatalf("branches = %v, want [wt/b wt/a wt/c]", got)
	}
	if branches := workerBranchesFromManifests(nil); branches != nil {
		t.Fatalf("nil manifests must give nil; got %v", branches)
	}
}
