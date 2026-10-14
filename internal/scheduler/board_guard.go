package scheduler

// SCHED-GAP-1699: a CODE row commit must never touch .coding-hermes/board/*.
//
// Workers run in per-task worktrees whose .coding-hermes/board/tasks.jsonl
// is a stale snapshot taken when the worktree was created. When a worker
// board-writes and commits code, the branch diff is a full-file rewrite of
// tasks.jsonl that DELETES every row appended since the branch — three
// incidents (REMOTE-006 550-row rewrite; REMOTE-008 deleted
// SCHED-GAP-1695/1696). Enforcement lives at two points:
//
//  1. Commit time — scripts/board-commit-guard.sh, wired as a tracked
//     pre-commit hook under githooks/pre-commit (core.hooksPath-compatible).
//  2. Pre-merge — EnsureNoBoardTouchingDiff, called by the dispatcher's
//     merge-direction surface: the wave-recovery preamble builder embeds the
//     refusal so the foreman's recover-before-dispatch instructions carry
//     the check for every preserved worker branch (the scheduler itself has
//     no merge authority — W6, wave_recovery.go — the foreman executes the
//     merges, so the scheduler refuses IN THE DIRECTION it directs them).

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBoardTouchingDiff is the sentinel wrapped by every refusal this guard
// produces. Match with errors.Is to distinguish "board-mixing changeset"
// from infrastructure failure.
var ErrBoardTouchingDiff = fmt.Errorf("board-touching commit refused (SCHED-GAP-1699)")

// boardPathPrefix is the board directory relative to a repo root (POSIX,
// forward slashes — the form git reports paths in).
const boardPathPrefix = ".coding-hermes/board/"

// isBoardDirPath reports whether p is under the board directory
// (.coding-hermes/board/). Exact-prefix on the POSIX form; the trailing-
// slash variant is normalized so ".coding-hermes/board/" itself also
// classifies as board. Distinct from adaptive_cooldown.go's isBoardPath,
// which classifies the WHOLE .coding-hermes/ tree as bookkeeping for git
// metrics — this guard's board bucket is only the board/ subtree.
func isBoardDirPath(p string) bool {
	p = filepath.ToSlash(p)
	return p == ".coding-hermes/board" || strings.HasPrefix(p, boardPathPrefix)
}

// isUnderDotCodingHermes reports whether p is anywhere under
// .coding-hermes/ (board, waves, config, ...). A changeset whose every
// path is under .coding-hermes/ is board-truthkeeping, not a code row,
// and is allowed.
func isUnderDotCodingHermes(p string) bool {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	return p == ".coding-hermes" || strings.HasPrefix(p, ".coding-hermes/")
}

// gitDiffNames runs `git diff <range> --name-only` in dir and returns the
// changed paths (POSIX form, as git reports them).
func gitDiffNames(dir, rangeSpec string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "diff", rangeSpec, "--name-only")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", rangeSpec, err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// classifyDiffNames splits a changeset into board-touched paths and paths
// outside .coding-hermes/. Paths inside .coding-hermes/ but outside board/
// (waves, config) are neither: they cannot rewrite the board and are not
// code, so they never trip the guard on their own.
func classifyDiffNames(names []string) (board, outside []string) {
	for _, p := range names {
		switch {
		case isBoardDirPath(p):
			board = append(board, p)
		case !isUnderDotCodingHermes(p):
			outside = append(outside, p)
		}
	}
	return board, outside
}

// boardTouchingDiffErr renders the named refusal for a changeset. branch
// labels the changeset (a wt/<task> branch name, or the literal "staged
// changeset" at hook time); base labels the diff base.
func boardTouchingDiffErr(base, branch string, board, outside []string) error {
	return fmt.Errorf("%w: base=%s branch=%s\n  board paths (%d):\n%s  code paths outside .coding-hermes/ (%d):\n%s  split the changeset: board-only truthkeeping OR code-only — never mixed",
		ErrBoardTouchingDiff, base, branch, len(board), indentPaths(board), len(outside), indentPaths(outside))
}

// indentPaths renders one indented path per line (empty-safe).
func indentPaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "    %s\n", p)
	}
	return b.String()
}

// EnsureNoBoardTouchingDiff refuses when the diff base...branch mixes board
// paths with code paths (SCHED-GAP-1699). The refusal wraps
// ErrBoardTouchingDiff and names the branch plus every offending path, so a
// caller (foreman-side merge gate, wave-recovery instruction builder) can
// surface exactly what to split. A diff that touches NO board path, or one
// whose every changed path is under .coding-hermes/ (board-only
// truthkeeping), is allowed. Infrastructure failure (git error, missing
// repo) returns a plain wrapped error — never a false allow and never the
// sentinel.
func EnsureNoBoardTouchingDiff(base, branch string) error {
	if strings.TrimSpace(base) == "" || strings.TrimSpace(branch) == "" {
		return fmt.Errorf("%w: check skipped: empty base or branch", ErrBoardTouchingDiff)
	}
	names, err := gitDiffNames(".", base+"..."+branch)
	if err != nil {
		return fmt.Errorf("board guard (SCHED-GAP-1699): %w", err)
	}
	board, outside := classifyDiffNames(names)
	if len(board) > 0 && len(outside) > 0 {
		return boardTouchingDiffErr(base, branch, board, outside)
	}
	return nil
}

// boardGuardDefaultBases is the ordered base-ref fallback for worker
// branches (fleet convention: wt/<task> branches off main).
const boardGuardDefaultBases = "main,master"

// boardGuardMaxBranchChecks bounds the git subprocess fan-out on the spawn
// path: at most this many worker branches are checked per recovery spawn
// (recovery ticks are rare; the wave-recovery scan already caps manifests
// at waveScanMaxManifests, but 64×8 git calls would be reckless here).
const boardGuardMaxBranchChecks = 8

// detectBoardGuardBase resolves the base ref a worker branch grew from:
// the first of boardGuardDefaultBases that has a merge-base with branch.
func detectBoardGuardBase(workdir, branch string) (string, error) {
	for _, base := range strings.Split(boardGuardDefaultBases, ",") {
		cmd := exec.Command("git", "-C", workdir, "merge-base", base, branch)
		if err := cmd.Run(); err == nil {
			return base, nil
		}
	}
	return "", fmt.Errorf("no common ancestor with any of %q", boardGuardDefaultBases)
}

// ensureNoBoardTouchingBranchDiff runs the SCHED-GAP-1699 check for one
// worker branch inside workdir (base auto-resolved). Refusal wraps
// ErrBoardTouchingDiff; infrastructure failure returns a plain error; a
// non-git workdir returns nil (nothing checkable — callers must not fail
// the tick on an unreadable repo).
func ensureNoBoardTouchingBranchDiff(workdir, branch string) error {
	if strings.TrimSpace(workdir) == "" || strings.TrimSpace(branch) == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		return nil
	}
	base, err := detectBoardGuardBase(workdir, branch)
	if err != nil {
		return fmt.Errorf("board guard (SCHED-GAP-1699): branch %s: %w", branch, err)
	}
	names, err := gitDiffNames(workdir, base+"..."+branch)
	if err != nil {
		return fmt.Errorf("board guard (SCHED-GAP-1699): branch %s: %w", branch, err)
	}
	board, outside := classifyDiffNames(names)
	if len(board) > 0 && len(outside) > 0 {
		return boardTouchingDiffErr(base, branch, board, outside)
	}
	return nil
}

// logBoardTouchingBranches is the dispatcher-side surfacing of the check
// (SCHED-GAP-1699): at recovery-spawn time each preserved worker branch is
// checked and a board-mixing one is logged as BOARD-GUARD-REFUSAL — the
// scheduler has no merge authority (W6, wave_recovery.go) but it does
// direct the foreman's merges, so a branch that would delete board rows is
// named in the log BEFORE the foreman follows the preamble's merge
// instruction. Best-effort and bounded: never fails the spawn.
func logBoardTouchingBranches(workdir, projectName string, branches []string) {
	if strings.TrimSpace(workdir) == "" || len(branches) == 0 {
		return
	}
	checked := 0
	for _, branch := range branches {
		if checked >= boardGuardMaxBranchChecks {
			log.Printf("WARN [wave]: board guard (SCHED-GAP-1699): %s — %d more branch(es) unchecked (cap %d)",
				projectName, len(branches)-checked, boardGuardMaxBranchChecks)
			return
		}
		checked++
		if err := ensureNoBoardTouchingBranchDiff(workdir, branch); err != nil {
			if errors.Is(err, ErrBoardTouchingDiff) {
				log.Printf("BOARD-GUARD-REFUSAL (SCHED-GAP-1699): %s branch %s — merge REFUSED until changeset is split:\n%v",
					projectName, branch, err)
			} else {
				log.Printf("WARN [wave]: board guard (SCHED-GAP-1699): %s branch %s: %v", projectName, branch, err)
			}
		}
	}
}
