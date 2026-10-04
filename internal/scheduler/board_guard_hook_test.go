package scheduler

// SCHED-GAP-1699: the tracked githooks/pre-commit entrypoint drives
// scripts/board-commit-guard.sh. These tests run the hook + guard scripts
// in throwaway git repos via sh(1) and assert the three brief cases:
// (a) mixed board+code commit REFUSED with a message naming the rule,
// (b) board-only commit accepted, (c) pure code commit accepted — plus the
// arg/stdin/git-fallback input modes, the fail-closed no-source case, the
// githooks/pre-commit chaining (including the tier-1 hand-off), and the
// dispatcher-side Go check on real branches.

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// guardRepo builds a throwaway git repo (own object store, no worktree
// interference) with identity configured and one seed commit on main.
func guardRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("commit", "-q", "-m", "seed")
	return dir
}

// stageIn writes+stages one path in the repo dir.
func stageIn(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", name).CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", name, err, out)
	}
}

// repoScript locates a repo-tracked script from a test (tests run with the
// package dir as cwd).
func repoScript(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("missing repo script %s: %v", rel, err)
	}
	return abs
}

// runGuard runs scripts/board-commit-guard.sh with the given input mode.
func runGuard(t *testing.T, dir, mode string, args []string, stdin string) (string, int) {
	t.Helper()
	script := repoScript(t, "scripts/board-commit-guard.sh")
	var cmd *exec.Cmd
	switch mode {
	case "args":
		cmd = exec.Command("sh", append([]string{script}, args...)...)
	case "stdin":
		cmd = exec.Command("sh", script)
		cmd.Stdin = strings.NewReader(stdin)
	default: // "git"
		cmd = exec.Command("sh", script)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	rc := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run guard: %v\n%s", err, out)
		}
		rc = ee.ExitCode()
	}
	return string(out), rc
}

// Test1699_Guard_MixedCommitRejected: a changeset mixing a board path with
// a code path exits non-zero and the message names the rule and both paths.
func Test1699_Guard_MixedCommitRejected(t *testing.T) {
	dir := guardRepo(t)
	stageIn(t, dir, "feature.go", "package main\n")
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl", "{\"id\":1}\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc == 0 {
		t.Fatalf("mixed changeset must be REJECTED; guard exited 0. output:\n%s", out)
	}
	for _, want := range []string{"SCHED-GAP-1699", "REFUSED", ".coding-hermes/board/", "feature.go", "board-only", "code-only"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal message must contain %q; got:\n%s", want, out)
		}
	}
}

// Test1699_Guard_BoardOnlyCommitAccepted: every staged path under
// .coding-hermes/ — board truthkeeping — exits 0.
func Test1699_Guard_BoardOnlyCommitAccepted(t *testing.T) {
	dir := guardRepo(t)
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl", "{\"id\":1}\n")
	stageIn(t, dir, ".coding-hermes/waves/t1.json", "{}\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 0 {
		t.Fatalf("board-only changeset must be ACCEPTED; rc=%d output:\n%s", rc, out)
	}
}

// Test1699_Guard_PureCodeCommitAccepted: no board path anywhere exits 0.
func Test1699_Guard_PureCodeCommitAccepted(t *testing.T) {
	dir := guardRepo(t)
	stageIn(t, dir, "internal/a/a.go", "package a\n")
	stageIn(t, dir, "docs/note.md", "note\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 0 {
		t.Fatalf("pure code changeset must be ACCEPTED; rc=%d output:\n%s", rc, out)
	}
}

// Test1699_Guard_ArgsAndStdinModes: both explicit input modes agree with
// the git fallback — mixed refused (rc=1), board-only accepted.
func Test1699_Guard_ArgsAndStdinModes(t *testing.T) {
	mixed := []string{"feature.go", ".coding-hermes/board/tasks.jsonl"}
	if out, rc := runGuard(t, t.TempDir(), "args", mixed, ""); rc == 0 {
		t.Fatalf("args mode: mixed must be rejected; output:\n%s", out)
	}
	if out, rc := runGuard(t, t.TempDir(), "stdin", nil, "feature.go\n.coding-hermes/board/tasks.jsonl\n"); rc == 0 {
		t.Fatalf("stdin mode: mixed must be rejected; output:\n%s", out)
	}
	if out, rc := runGuard(t, t.TempDir(), "stdin", nil, ".coding-hermes/board/tasks.jsonl\n"); rc != 0 {
		t.Fatalf("stdin mode: board-only must pass; rc=%d output:\n%s", rc, out)
	}
	if out, rc := runGuard(t, t.TempDir(), "stdin", nil, ".coding-hermes/waves/t.json\n"); rc != 0 {
		t.Fatalf("stdin mode: non-board .coding-hermes/ path must pass; rc=%d output:\n%s", rc, out)
	}
}

// Test1699_Guard_FailClosedWithoutSource: empty stdin, no args, no git repo
// → exit 2 (cannot verify = refuse).
func Test1699_Guard_FailClosedWithoutSource(t *testing.T) {
	out, rc := runGuard(t, t.TempDir(), "stdin", nil, "")
	if rc != 2 {
		t.Fatalf("no-source must fail closed with rc=2; got rc=%d output:\n%s", rc, out)
	}
}

// Test1699_Hook_ChainsGuard: githooks/pre-commit drives the guard (args,
// stdin and git fallback modes), and hands off to the installed tier-1
// hook (fake COMMON hook) after the guard passes.
func Test1699_Hook_ChainsGuard(t *testing.T) {
	dir := guardRepo(t)
	hook := repoScript(t, "githooks/pre-commit")

	// 1. args mode — mixed refused with the rule named.
	out, err := exec.Command("sh", hook, "feature.go", ".coding-hermes/board/tasks.jsonl").CombinedOutput()
	if err == nil {
		t.Fatalf("hook must refuse mixed args; output:\n%s", out)
	}
	if !strings.Contains(string(out), "SCHED-GAP-1699") {
		t.Errorf("hook refusal must name the rule; got:\n%s", out)
	}

	// 2. git-fallback mode — staged mixed changeset refused.
	stageIn(t, dir, "feature.go", "package main\n")
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl", "{\"id\":1}\n")
	cmd := exec.Command("sh", hook)
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook must refuse staged mixed changeset; output:\n%s", out)
	}

	// 3. clean tree + fake installed tier-1 hook — the chain reaches it.
	if out, err := exec.Command("git", "-C", dir, "reset", "-q", "HEAD", "--", "feature.go", ".coding-hermes/board/tasks.jsonl").CombinedOutput(); err != nil {
		t.Fatalf("reset staged: %v\n%s", err, out)
	}
	commonDir := strings.TrimSpace(string(mustOutput(t, "git", "-C", dir, "rev-parse", "--git-common-dir")))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}
	if err := os.MkdirAll(filepath.Join(commonDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tier1 := filepath.Join(commonDir, "hooks", "pre-commit")
	if err := os.WriteFile(tier1, []byte("#!/bin/sh\necho TIER1-RAN\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", hook)
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clean changeset must pass the guard and chain tier-1; err=%v output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "TIER1-RAN") {
		t.Errorf("tier-1 hook must run after the guard passes; got:\n%s", out)
	}
}

// mustOutput is a tiny strict runner for one-off git calls.
func mustOutput(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

// captureBoardGuardLog swaps the std logger for a mutex-guarded sink.
func captureBoardGuardLog(t *testing.T) *boardGuardBuffer {
	t.Helper()
	orig := log.Writer()
	buf := &boardGuardBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf
}

type boardGuardBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *boardGuardBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *boardGuardBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Test1699_LogBoardTouchingBranches_RefusalNamed: the dispatcher-side
// surfacing logs BOARD-GUARD-REFUSAL with the branch name for a mixed
// branch, and nothing refusal-shaped for a clean one. This is the test
// that fails if the dispatcher-side check is removed (the log line
// disappears).
func Test1699_LogBoardTouchingBranches_RefusalNamed(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/mixed", map[string]string{
		"feature.go":                       "package f\n",
		".coding-hermes/board/tasks.jsonl": "{}\n",
	})

	buf := captureBoardGuardLog(t)
	logBoardTouchingBranches(dir, "proj", []string{"wt/mixed"})
	out := buf.String()
	for _, want := range []string{"BOARD-GUARD-REFUSAL (SCHED-GAP-1699)", "wt/mixed", "feature.go", ".coding-hermes/board/tasks.jsonl"} {
		if !strings.Contains(out, want) {
			t.Errorf("log must contain %q; got:\n%s", want, out)
		}
	}

	buf2 := captureBoardGuardLog(t)
	logBoardTouchingBranches(dir, "proj", []string{"wt/nope"})
	if out2 := buf2.String(); strings.Contains(out2, "BOARD-GUARD-REFUSAL") {
		t.Errorf("missing branch is an infra WARN, never a refusal; got:\n%s", out2)
	}
}

// Test1699_LogBoardTouchingBranches_CapBounded: the check fan-out is capped
// at boardGuardMaxBranchChecks with a loud WARN for the remainder.
func Test1699_LogBoardTouchingBranches_CapBounded(t *testing.T) {
	buf := captureBoardGuardLog(t)
	var branches []string
	for i := 0; i < boardGuardMaxBranchChecks+3; i++ {
		branches = append(branches, "wt/none-"+string(rune('a'+i)))
	}
	logBoardTouchingBranches(t.TempDir(), "proj", branches) // non-git dir → all nil, no refusals
	if out := buf.String(); !strings.Contains(out, "unchecked (cap 8)") {
		t.Errorf("over-cap branches must be named; got:\n%s", out)
	}
}
