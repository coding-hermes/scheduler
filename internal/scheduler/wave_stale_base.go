package scheduler

// SCHED-GAP-1609: stale-base guard for wave harvest merges.
//
// The wave-harvest merge path (the foreman's recover/harvest phase, directed
// by the wave-recovery preamble) merges worker branches without checking
// whether the branch base advanced. A second worker branch cut BEFORE an
// earlier same-wave merge carries a snapshot of every file that merge
// touched, so merging it silently reverts the earlier merge (measured on
// hermes-dagger 2026-09-24: merge 0b1e36e reverted SCHED-WH-006's doc
// corrections from 58a21bd, re-pended two rows and deleted a wave manifest;
// caught only by judge verdict ac91fb9c).
//
// The scheduler has NO merge authority (W6, wave_recovery.go) — the foreman
// executes the merges — so the guard follows the SCHED-GAP-1699 shape: the
// check helper is exported for the direction surface, the dispatcher-side
// log surfacing (logStaleWaveBranches) names stale branches at
// recovery-spawn time BEFORE the foreman follows the preamble's merge
// instruction, and the preamble carries the skip rule.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// waveStaleBaseGuardBases is the ordered base-ref fallback for worker
// branches (same fleet convention as boardGuardDefaultBases: wt/<task>
// branches off main).
const waveStaleBaseGuardBases = "main,master"

// waveStaleBaseMaxBranchChecks bounds the git subprocess fan-out on the
// spawn path (same bound discipline as boardGuardMaxBranchChecks).
const waveStaleBaseMaxBranchChecks = 8

// WaveBranchBaseCheck is the result of one stale-base check. Base is the
// resolved base ref ("main"/"master"); BaseSHA is that ref's current tip
// (origin/main HEAD equivalent for a workdir-local checkout); BranchSHA is
// the branch tip; Stale is true when the branch's merge-base with the base
// is BEHIND the base tip (the branch predates a main advance — merging it
// can silently revert work); Merged is true when the branch tip is already
// fully contained in the base (harvested — skip silently, never warn);
// Checked is false only for a non-git workdir (nothing checkable).
type WaveBranchBaseCheck struct {
	Base      string
	BaseSHA   string
	BranchSHA string
	Stale     bool
	Merged    bool
	Checked   bool
}

// gitOut runs one git command in dir and returns trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// checkWaveBranchBase runs the stale-base detection for one worker branch
// inside workdir: resolve the base ref, compare the branch's merge-base
// against the base tip (git merge-base <base> <branch> vs rev-parse <base>).
// A non-git workdir returns Checked=false with no error — never a false
// stale and never a spawn failure. Infrastructure failure (git error,
// unknown branch) returns a plain error — never a false allow.
func checkWaveBranchBase(workdir, branch string) (WaveBranchBaseCheck, error) {
	if strings.TrimSpace(workdir) == "" || strings.TrimSpace(branch) == "" {
		return WaveBranchBaseCheck{}, fmt.Errorf("stale-base guard (SCHED-GAP-1609): check skipped: empty workdir or branch")
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		return WaveBranchBaseCheck{}, nil // non-git workdir: nothing checkable
	}
	var base string
	var err error
	for _, b := range strings.Split(waveStaleBaseGuardBases, ",") {
		if _, err = gitOut(workdir, "rev-parse", "--verify", b+"^{commit}"); err == nil {
			base = b
			break
		}
	}
	if err != nil {
		return WaveBranchBaseCheck{}, fmt.Errorf("stale-base guard (SCHED-GAP-1609): branch %s: no resolvable base among %q: %w",
			branch, waveStaleBaseGuardBases, err)
	}
	baseSHA, err := gitOut(workdir, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return WaveBranchBaseCheck{}, fmt.Errorf("stale-base guard (SCHED-GAP-1609): branch %s: resolve %s: %w", branch, base, err)
	}
	branchSHA, err := gitOut(workdir, "rev-parse", "--verify", branch+"^{commit}")
	if err != nil {
		return WaveBranchBaseCheck{}, fmt.Errorf("stale-base guard (SCHED-GAP-1609): branch %s: resolve branch: %w", branch, err)
	}
	chk := WaveBranchBaseCheck{Base: base, BaseSHA: baseSHA, BranchSHA: branchSHA, Checked: true}
	if branchSHA == baseSHA {
		// Tip equal to main tip: merged (or empty branch) — not stale.
		chk.Merged = true
		return chk, nil
	}
	// Already merged: branch tip is an ancestor of the base tip.
	if err := exec.Command("git", "-C", workdir, "merge-base", "--is-ancestor", branchSHA, baseSHA).Run(); err == nil {
		chk.Merged = true
		return chk, nil
	}
	// Stale: the merge-base of branch and base is behind the base tip.
	mergeBase, err := gitOut(workdir, "merge-base", base, branch)
	if err != nil {
		return WaveBranchBaseCheck{}, fmt.Errorf("stale-base guard (SCHED-GAP-1609): branch %s: merge-base: %w", branch, err)
	}
	chk.Stale = mergeBase != baseSHA
	return chk, nil
}

// logStaleWaveBranches is the dispatcher-side surfacing of the stale-base
// check (the analog of logBoardTouchingBranches for SCHED-GAP-1699): at
// recovery-spawn time each preserved worker branch is checked and a STALE
// one is logged as STALE-BASE-WARN — SKIP the merge and rebase onto the
// current base tip — BEFORE the foreman follows the preamble's merge
// instruction. Best-effort and bounded: never fails the spawn.
func logStaleWaveBranches(workdir, projectName string, branches []string) {
	if strings.TrimSpace(workdir) == "" || len(branches) == 0 {
		return
	}
	checked := 0
	for _, branch := range branches {
		if checked >= waveStaleBaseMaxBranchChecks {
			log.Printf("WARN [wave]: stale-base guard (SCHED-GAP-1609): %s — %d more branch(es) unchecked (cap %d)",
				projectName, len(branches)-checked, waveStaleBaseMaxBranchChecks)
			return
		}
		checked++
		chk, err := checkWaveBranchBase(workdir, branch)
		if err != nil {
			log.Printf("WARN [wave]: stale-base guard (SCHED-GAP-1609): %s branch %s: %v", projectName, branch, err)
			continue
		}
		if chk.Checked && chk.Stale {
			log.Printf("STALE-BASE-WARN (SCHED-GAP-1609): %s branch %s — base %s advanced past the branch (base_sha=%s branch_sha=%s): SKIP the merge; rebase onto %s and re-run gates before merging",
				projectName, branch, chk.Base, chk.BaseSHA, chk.BranchSHA, chk.Base)
		}
	}
}
