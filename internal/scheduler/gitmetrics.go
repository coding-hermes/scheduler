package scheduler

import (
	"os/exec"
	"strconv"
	"strings"
)

// gitMetrics captures the git delta a foreman produced during its tick. Every
// call is best-effort and must never block or fail the tick lifecycle — a
// repo read error simply yields zeros (which is the pre-existing behavior).

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
// spawn-time baseline. commits is the growth in total commit count (robust to
// branch moves); filesChanged is the number of files whose content differs
// between preHead and current HEAD (tree diff — does not require linear
// ancestry, so it is robust to resets/merges). All best-effort: on any read
// error it returns zeros rather than blocking the tick.
func gitWorkDelta(dir, preHead string, preTotal int) (commits, files int) {
	curTotal, err := gitCommitCount(dir)
	if err != nil {
		return 0, 0
	}
	commits = curTotal - preTotal
	if commits < 0 {
		commits = 0
	}
	if preHead == "" {
		// No baseline — can't diff HEAD, but still count staged work.
		return commits, countStagedFiles(dir)
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", preHead, "HEAD").Output()
	if err != nil {
		// Fall through: still count staged-but-uncommitted work (the timeout
		// case where the foreman wrote files but never committed).
		files = countStagedFiles(dir)
	} else {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) != "" {
				files++
			}
		}
		// A timeout tick may have staged-but-uncommitted work on top of any
		// committed delta; include it so the dashboard shows real progress.
		files += countStagedFiles(dir)
	}
	return commits, files
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
