package scheduler

// SCHED-GAP-1609: stale-base guard for wave harvest merges. The wave-harvest
// path (the foreman's merge phase, directed by the recovery preamble) merges
// worker branches without checking whether the branch base advanced — a
// second worker branch cut before an earlier same-wave merge silently
// reverts that merge (measured on hermes-dagger 2026-09-24: merge 0b1e36e
// reverted SCHED-WH-006 doc corrections from 58a21bd). The scheduler has no
// merge authority (W6, wave_recovery.go) but directs the merges, so — like
// the SCHED-GAP-1699 board guard — the refusal is surfaced dispatcher-side
// (STALE-BASE-WARN at recovery-spawn time) and the preamble carries the
// check instruction. These tests exercise the check helper on real git
// repos, the dispatcher-side log line, and the preamble rule line.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// staleBaseCommit writes filename into dir and commits it (same shape as
// boardGuardCommit — date-insensitive, identity via env).
func staleBaseCommit(t *testing.T, dir, filename, content string) {
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
	run("commit", "-q", "-m", "stale-base fixture: "+filename)
}

// Test1609_CheckWaveBranchBase_FreshBranchNotStale: a branch cut from the
// current main tip and not yet merged is NOT stale and NOT merged.
func Test1609_CheckWaveBranchBase_FreshBranchNotStale(t *testing.T) {
	dir := boardGuardRepo(t) // main @ seed
	boardGuardBranch(t, dir, "wt/fresh", map[string]string{
		"feature.go": "package f\n",
	})

	chk, err := checkWaveBranchBase(dir, "wt/fresh")
	if err != nil {
		t.Fatalf("checkWaveBranchBase: %v", err)
	}
	if !chk.Checked {
		t.Fatalf("branch must be checked; got %+v", chk)
	}
	if chk.Stale {
		t.Errorf("fresh branch must not be stale; got %+v", chk)
	}
	if chk.Merged {
		t.Errorf("unmerged branch must not report merged; got %+v", chk)
	}
	if chk.Base == "" || chk.BaseSHA == "" || chk.BranchSHA == "" {
		t.Errorf("check must resolve base/base_sha/branch_sha; got %+v", chk)
	}
}

// Test1609_CheckWaveBranchBase_StaleAfterMainAdvances: a branch cut BEFORE
// main advanced (the earlier same-wave merge) reports Stale=true — the
// incident shape: branch base_sha behind main HEAD.
func Test1609_CheckWaveBranchBase_StaleAfterMainAdvances(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/second", map[string]string{
		"second.go": "package s\n",
	})
	// main advances AFTER the branch was cut (earlier same-wave merge).
	boardGuardBranch(t, dir, "wt/first-merged", map[string]string{
		"first.go": "package f\n",
	})
	if out, err := exec.Command("git", "-C", dir, "merge", "-q", "--ff-only", "wt/first-merged").CombinedOutput(); err != nil {
		t.Fatalf("merge wt/first-merged: %v\n%s", err, out)
	}

	chk, err := checkWaveBranchBase(dir, "wt/second")
	if err != nil {
		t.Fatalf("checkWaveBranchBase: %v", err)
	}
	if !chk.Stale {
		t.Fatalf("branch cut before the main advance must be STALE; got %+v", chk)
	}
	if chk.Merged {
		t.Errorf("stale unmerged branch must not report merged; got %+v", chk)
	}
	if chk.BaseSHA == chk.BranchSHA {
		t.Errorf("base_sha must differ from branch_sha on a stale branch; got %+v", chk)
	}
}

// Test1609_CheckWaveBranchBase_AlreadyMergedNotWarned: a branch whose tip is
// fully contained in main (already harvested) is NOT stale — the guard must
// not tell the foreman to skip/delete an already-merged branch.
func Test1609_CheckWaveBranchBase_AlreadyMergedNotStale(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/harvested", map[string]string{
		"harvest.go": "package h\n",
	})
	if out, err := exec.Command("git", "-C", dir, "merge", "-q", "--ff-only", "wt/harvested").CombinedOutput(); err != nil {
		t.Fatalf("merge wt/harvested: %v\n%s", err, out)
	}

	chk, err := checkWaveBranchBase(dir, "wt/harvested")
	if err != nil {
		t.Fatalf("checkWaveBranchBase: %v", err)
	}
	if !chk.Merged {
		t.Fatalf("branch contained in main must report merged=true; got %+v", chk)
	}
	if chk.Stale {
		t.Errorf("merged branch must not be stale; got %+v", chk)
	}

	// The --no-ff shape exercises the true ancestor path (branch tip behind
	// a merge commit on main — the real harvest shape): still merged, still
	// not stale, never warned.
	if out, err := exec.Command("git", "-C", dir, "merge", "-q", "--no-ff", "-m", "harvest", "wt/advance").CombinedOutput(); err == nil {
		t.Logf("merge --no-ff wt/advance: %s", out)
		chk2, err := checkWaveBranchBase(dir, "wt/advance")
		if err != nil {
			t.Fatalf("checkWaveBranchBase(--no-ff): %v", err)
		}
		if !chk2.Merged || chk2.Stale {
			t.Errorf("fast-forwarded branch behind a merge commit must be merged=true stale=false; got %+v", chk2)
		}
	}
}

// Test1609_CheckWaveBranchBase_NonGitWorkdirSkipped: a workdir without .git
// (recovery-scan tempdirs) yields Checked=false with no error — callers must
// not fail the spawn on an unreadable repo.
func Test1609_CheckWaveBranchBase_NonGitWorkdirSkipped(t *testing.T) {
	dir := t.TempDir()
	chk, err := checkWaveBranchBase(dir, "wt/anything")
	if err != nil {
		t.Fatalf("non-git workdir must not error; got %v", err)
	}
	if chk.Checked {
		t.Errorf("non-git workdir must report Checked=false; got %+v", chk)
	}
	if chk.Stale {
		t.Errorf("non-git workdir must never report stale; got %+v", chk)
	}
}

// Test1609_LogStaleWaveBranches_WarnNamed: the dispatcher-side surfacing
// logs STALE-BASE-WARN with the branch, the resolved base, and the SKIP
// instruction for a stale branch — and nothing for a fresh one.
func Test1609_LogStaleWaveBranches_WarnNamed(t *testing.T) {
	dir := boardGuardRepo(t)
	boardGuardBranch(t, dir, "wt/stale", map[string]string{
		"stale.go": "package s\n",
	})
	boardGuardBranch(t, dir, "wt/advance", map[string]string{
		"advance.go": "package a\n",
	})
	if out, err := exec.Command("git", "-C", dir, "merge", "-q", "--ff-only", "wt/advance").CombinedOutput(); err != nil {
		t.Fatalf("merge wt/advance: %v\n%s", err, out)
	}

	buf := captureBoardGuardLog(t)
	logStaleWaveBranches(dir, "proj", []string{"wt/stale", "wt/nope"})
	out := buf.String()
	for _, want := range []string{"STALE-BASE-WARN (SCHED-GAP-1609)", "proj", "wt/stale", "SKIP"} {
		if !strings.Contains(out, want) {
			t.Errorf("log must contain %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "WARN [wave]: stale-base guard (SCHED-GAP-1609): proj branch wt/nope") {
		t.Errorf("unknown branch (no common ancestor) must surface as an infra WARN, not a stale skip; got:\n%s", out)
	}

	buf2 := captureBoardGuardLog(t)
	logStaleWaveBranches(dir, "proj", []string{"wt/advance"})
	if out2 := buf2.String(); strings.Contains(out2, "STALE-BASE-WARN") {
		t.Errorf("fresh branch must not be warned; got:\n%s", out2)
	}
}

// Test1609_Preamble_CarriesStaleBaseRule: the recover-before-dispatch
// preamble instructs the foreman to verify the branch base against current
// main HEAD and skip+rebase stale branches (the rule must ride the prompt —
// the scheduler directs but never executes the merge).
func Test1609_Preamble_CarriesStaleBaseRule(t *testing.T) {
	manifests := []*WaveManifest{{
		TickID: "t1609", Project: "p", StartedAt: "2026-10-06T00:00:00Z",
		Workers: []WaveWorker{{TaskID: "SCHED-GAP-1609-A", Branch: "wt/a"}},
	}}
	got := waveRecoveryPreamble(manifests)
	for _, want := range []string{"SCHED-GAP-1609", "merge-base", "STALE", "rebase"} {
		if !strings.Contains(got, want) {
			t.Errorf("preamble must contain %q; got:\n%s", want, got)
		}
	}
}
