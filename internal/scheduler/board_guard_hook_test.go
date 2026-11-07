package scheduler

// SCHED-GAP-1699: the tracked githooks/pre-commit entrypoint drives
// scripts/board-commit-guard.sh. These tests run the hook + guard scripts
// in throwaway git repos via sh(1) and assert the three brief cases:
// (a) mixed board+code commit REFUSED with a message naming the rule,
// (b) board-only commit accepted, (c) pure code commit accepted — plus the
// arg/stdin/git-fallback input modes, the fail-closed no-source case, the
// githooks/pre-commit chaining (including the tier-1 hand-off), and the
// dispatcher-side Go check on real branches.
//
// RELEASE-046 (stale-base row drop): the board-only branch additionally
// refuses a staged index blob that would drop any row id the same path
// carries on origin/main. The TestBoardGuard_StaleBase* cases below fail if
// that rule is removed or weakened.

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

// fixtureGit runs one git command in repo with a pinned identity (throwaway
// fixtures must not depend on ambient git config).
func fixtureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", repo, args, err, out)
	}
	return string(out)
}

// fixtureCommit stages everything in repo and commits it.
func fixtureCommit(t *testing.T, repo, msg string) {
	t.Helper()
	fixtureGit(t, repo, "add", "-A")
	fixtureGit(t, repo, "commit", "-q", "-m", msg)
}

// fixtureClone clones src into dst and pins identity in the clone.
func fixtureClone(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("git", "clone", "-q", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone %s %s: %v\n%s", src, dst, err, out)
	}
	fixtureGit(t, dst, "config", "user.email", "test@example.com")
	fixtureGit(t, dst, "config", "user.name", "Test")
}

// guardRepo builds a throwaway git repo (own object store, no worktree
// interference) with identity configured and one seed commit on main. The
// repo is cloned from a local origin so origin/main exists — the
// RELEASE-046 stale-base rule compares staged board blobs against it (a
// repo WITHOUT an origin ref is fail-closed by design and has its own
// test).
func guardRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	dir := filepath.Join(root, "work")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, origin, "init", "-q")
	fixtureGit(t, origin, "config", "user.email", "test@example.com")
	fixtureGit(t, origin, "config", "user.name", "Test")
	fixtureGit(t, origin, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureCommit(t, origin, "seed")
	fixtureClone(t, origin, dir)
	return dir
}

// guardRepoNoOrigin is the origin-less variant: plain init + seed commit, no
// remote. The stale-base rule cannot verify a base here and must fail
// closed.
func guardRepoNoOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, "init", "-q")
	fixtureGit(t, dir, "config", "user.email", "test@example.com")
	fixtureGit(t, dir, "config", "user.name", "Test")
	fixtureGit(t, dir, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureCommit(t, dir, "seed")
	return dir
}

// writeBoardFixture writes the board file (without staging) in repo.
func writeBoardFixture(t *testing.T, repo, content string) {
	t.Helper()
	p := filepath.Join(repo, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// boardFixtureRepo builds the RELEASE-046 stale-base fixture: origin/main's
// board carries rows alpha and beta. With stale=true the local checkout is
// cloned after the alpha-only commit (one behind origin/main); with
// stale=false the clone carries both rows.
func boardFixtureRepo(t *testing.T, stale bool) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	dir := filepath.Join(root, "work")
	alpha := `{"id": "alpha", "status": "open"}` + "\n"
	both := alpha + `{"id": "beta", "status": "open"}` + "\n"
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, origin, "init", "-q")
	fixtureGit(t, origin, "config", "user.email", "test@example.com")
	fixtureGit(t, origin, "config", "user.name", "Test")
	fixtureGit(t, origin, "checkout", "-q", "-b", "main")
	writeBoardFixture(t, origin, alpha)
	fixtureCommit(t, origin, "board: alpha")
	if stale {
		fixtureClone(t, origin, dir)
		writeBoardFixture(t, origin, both)
		fixtureCommit(t, origin, "board: append beta")
		// The clone's origin/main moves only on fetch: after the fetch it
		// carries alpha+beta while the local branch sits at alpha — the
		// post-fetch, pre-rebase stale-base state (the guard compares
		// against the locally-fetched origin/main; no network I/O).
		fixtureGit(t, dir, "fetch", "-q", "origin")
	} else {
		writeBoardFixture(t, origin, both)
		fixtureCommit(t, origin, "board: append beta")
		fixtureClone(t, origin, dir)
	}
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

// runGuardEnv runs scripts/board-commit-guard.sh with the given input mode
// and extra environment entries (KEY=VALUE).
func runGuardEnv(t *testing.T, dir, mode string, args []string, stdin string, env []string) (string, int) {
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
	cmd.Env = append(os.Environ(), env...)
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

// runGuard runs scripts/board-commit-guard.sh with the given input mode.
func runGuard(t *testing.T, dir, mode string, args []string, stdin string) (string, int) {
	t.Helper()
	return runGuardEnv(t, dir, mode, args, stdin, nil)
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

// TestBoardGuard_StaleBase_DropRefused (RELEASE-046 case a): a board-only
// commit staged from a base one commit behind origin/main, whose index blob
// drops row beta, is REFUSED with exit 1; the refusal names the dropped id,
// the file, and the remedy. Fails if the stale-base rule is removed.
func TestBoardGuard_StaleBase_DropRefused(t *testing.T) {
	dir := boardFixtureRepo(t, true)
	// The writer's own edit on a stale snapshot: flip alpha's status and
	// append gamma — the staged blob still lacks beta, which origin/main has.
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl",
		`{"id": "alpha", "status": "done"}`+"\n"+`{"id": "gamma", "status": "open"}`+"\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 1 {
		t.Fatalf("stale-base row drop must be REFUSED with rc=1; got rc=%d output:\n%s", rc, out)
	}
	for _, want := range []string{
		"RELEASE-046", "REFUSED", "beta", ".coding-hermes/board/tasks.jsonl",
		"git fetch origin && git rebase origin/main", "BOARD_GUARD_ALLOW_ROW_DROP",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal must contain %q; got:\n%s", want, out)
		}
	}
}

// TestBoardGuard_StaleBase_StatusEditAccepted (RELEASE-046 case b): a
// board-only edit that keeps every id the base carries (status flip on
// alpha, beta untouched) is ACCEPTED — the rule is "no id may disappear",
// not "the file may not change". Fails if the rule refuses any edit.
func TestBoardGuard_StaleBase_StatusEditAccepted(t *testing.T) {
	dir := boardFixtureRepo(t, false)
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl",
		`{"id": "alpha", "status": "done"}`+"\n"+`{"id": "beta", "status": "pending"}`+"\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 0 {
		t.Fatalf("id-preserving board edit must be ACCEPTED; rc=%d output:\n%s", rc, out)
	}
}

// TestBoardGuard_StaleBase_RowDropEscapeHatch (RELEASE-046 case c):
// BOARD_GUARD_ALLOW_ROW_DROP naming the dropped id admits it — loudly, via
// a NOTICE line; naming a different id still refuses. Fails if the escape
// hatch is removed or if it admits unlisted ids.
func TestBoardGuard_StaleBase_RowDropEscapeHatch(t *testing.T) {
	dir := boardFixtureRepo(t, true)
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl",
		`{"id": "alpha", "status": "done"}`+"\n"+`{"id": "gamma", "status": "open"}`+"\n")

	out, rc := runGuardEnv(t, dir, "git", nil, "", []string{"BOARD_GUARD_ALLOW_ROW_DROP=beta"})
	if rc != 0 {
		t.Fatalf("named drop must be ADMITTED; rc=%d output:\n%s", rc, out)
	}
	for _, want := range []string{"NOTICE", "BOARD_GUARD_ALLOW_ROW_DROP", "beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("admission must be non-silent and name %q; got:\n%s", want, out)
		}
	}

	out, rc = runGuardEnv(t, dir, "git", nil, "", []string{"BOARD_GUARD_ALLOW_ROW_DROP=some-other-row"})
	if rc != 1 {
		t.Fatalf("a drop the allowlist does not name must still REFUSE; rc=%d output:\n%s", rc, out)
	}
	if !strings.Contains(out, "beta") {
		t.Errorf("refusal must still name the unallowed dropped id beta; got:\n%s", out)
	}
}

// TestBoardGuard_StaleBase_MixingAndPureCodePreserved (RELEASE-046 case d):
// in a stale-base repo the pre-existing contract still holds — a mixed
// board+code changeset refuses on the MIXING rule (not the stale-base
// rule), and a pure code changeset passes. Fails if the new rule broke the
// old contract.
func TestBoardGuard_StaleBase_MixingAndPureCodePreserved(t *testing.T) {
	dir := boardFixtureRepo(t, true)
	stageIn(t, dir, "feature.go", "package main\n")
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl",
		`{"id": "alpha", "status": "done"}`+"\n"+`{"id": "gamma", "status": "open"}`+"\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc == 0 {
		t.Fatalf("mixed changeset must still be REJECTED; output:\n%s", out)
	}
	if !strings.Contains(out, "SCHED-GAP-1699") {
		t.Errorf("mixed refusal must come from the mixing rule (SCHED-GAP-1699); got:\n%s", out)
	}

	dir2 := boardFixtureRepo(t, true)
	stageIn(t, dir2, "feature.go", "package main\n")
	if out, rc := runGuard(t, dir2, "git", nil, ""); rc != 0 {
		t.Fatalf("pure code changeset must still be ACCEPTED; rc=%d output:\n%s", rc, out)
	}
}

// TestBoardGuard_StaleBase_AppendOnlyAccepted (RELEASE-046 case e): an
// append-only board commit (alpha+beta plus the new gamma, no id dropped)
// is ACCEPTED. Fails if the rule refuses appends.
func TestBoardGuard_StaleBase_AppendOnlyAccepted(t *testing.T) {
	dir := boardFixtureRepo(t, false)
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl",
		`{"id": "alpha", "status": "open"}`+"\n"+
			`{"id": "beta", "status": "open"}`+"\n"+
			`{"id": "gamma", "status": "open"}`+"\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 0 {
		t.Fatalf("append-only board commit must be ACCEPTED; rc=%d output:\n%s", rc, out)
	}
}

// TestBoardGuard_StaleBase_FailClosedWithoutOrigin (RELEASE-046): a repo
// with no origin/main or origin/master cannot verify the base — exit 2,
// never a silent pass. Fails if the fail-closed path was weakened.
func TestBoardGuard_StaleBase_FailClosedWithoutOrigin(t *testing.T) {
	dir := guardRepoNoOrigin(t)
	stageIn(t, dir, ".coding-hermes/board/tasks.jsonl", `{"id": "alpha"}`+"\n")

	out, rc := runGuard(t, dir, "git", nil, "")
	if rc != 2 {
		t.Fatalf("stale-base check without an origin ref must FAIL CLOSED with rc=2; got rc=%d output:\n%s", rc, out)
	}
	if !strings.Contains(out, "origin") {
		t.Errorf("fail-closed message must name the missing condition (origin ref); got:\n%s", out)
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
