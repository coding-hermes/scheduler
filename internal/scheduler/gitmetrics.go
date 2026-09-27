package scheduler

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// gitMetrics captures the git delta a foreman produced during its tick. Every
// call is best-effort and must never block or fail the tick lifecycle — but a
// FAILED measurement is no longer indistinguishable from a measured zero
// (SCHED-GAP-1652). Measured 2026-09-27 over 3,717 ticks: 2,533 carried
// outcome='committed' with commits=0, and 27% of those (539 of the 1,993 with a
// surviving transcript) had in fact committed — proven by git's own commit/push
// output in their session transcripts and by their SHAs existing in the repos.
// The old signature returned (0, 0) on ANY read error, so the store recorded a
// broken measurement as a fact about the work.

// gitBaseline snapshots the workdir repo at spawn time so Wait() can later
// measure what the tick added. Returns the HEAD sha ("" if the repo has no
// commits yet) and the total commit count (-1 if the workdir is not a git
// repo or unreadable).
func gitBaseline(dir string) (head string, total int) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", -1
	}
	head = strings.TrimSpace(string(out))
	n, err := gitCommitCount(dir)
	if err != nil {
		return head, -1
	}
	return head, n
}

// gitCommitCount returns the number of commits reachable from HEAD, or an
// error if the workdir is not a usable git repo.
func gitCommitCount(dir string) (int, error) {
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// gitWorkDelta returns (commits, filesChanged) added to the repo since the
// spawn-time baseline, plus an error that is non-nil whenever the measurement is
// incomplete or impossible.
//
// commits is the growth in total commit count (robust to branch moves);
// filesChanged is the number of files whose content differs between preHead and
// current HEAD (tree diff — does not require linear ancestry, so it is robust to
// resets/merges), plus anything staged but uncommitted.
//
// A non-nil error means the returned numbers must NOT be read as "the tick did
// nothing". Two shapes produce it:
//
//   - the repo is unreadable (commits/files are 0 and unknowable);
//   - there was no spawn baseline (preHead empty), so only staged files can be
//     counted and the commit count is a floor, not a total.
//
// The caller logs the error; the tick lifecycle is never blocked by it.
func gitWorkDelta(dir, preHead string, preTotal int) (commits, files int, err error) {
	curTotal, cerr := gitCommitCount(dir)
	if cerr != nil {
		return 0, 0, fmt.Errorf("repo unreadable at %s: %w", dir, cerr)
	}
	commits = curTotal - preTotal
	if commits < 0 {
		commits = 0
	}
	if preHead == "" {
		// No baseline — can't diff HEAD, but still count staged work.
		return commits, countStagedFiles(dir),
			fmt.Errorf("no spawn baseline captured for %s (preHead empty): commit count is a floor", dir)
	}
	out, derr := exec.Command("git", "-C", dir, "diff", "--name-only", preHead, "HEAD").Output()
	if derr != nil {
		// Still count staged-but-uncommitted work (the timeout case where the
		// foreman wrote files but never committed).
		return commits, countStagedFiles(dir),
			fmt.Errorf("tree diff vs %s failed in %s: %w", shortSHA(preHead), dir, derr)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			files++
		}
	}
	// A timeout tick may have staged-but-uncommitted work on top of any
	// committed delta; include it so the dashboard shows real progress.
	files += countStagedFiles(dir)
	return commits, files, nil
}

// shortSHA trims a sha for log/message readability; safe on any input length.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// countStagedFiles returns the number of files currently staged (git add) but
// not yet committed — the work-in-progress a timed-out tick left behind.
// Best-effort: 0 on any git error.
func countStagedFiles(dir string) int {
	out, err := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
