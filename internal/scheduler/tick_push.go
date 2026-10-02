package scheduler

// SCHED-GAP-1694: push at tick exit.
//
// The tick prompt ASKS every lane to push after each commit, but a lane that
// commits and forgets strands the work on local disk — recovery then depends
// on the 30-minute fleet-strand-push cron, which is a backstop, not a
// contract. Here the SPAWNER pushes the workdir the moment a tick lands
// commits, so no lane depends on the model remembering to run `git push`.
//
// The push is best-effort and NEVER fails the tick: countGitChanges has
// already recorded the git metrics, and the cron re-covers a miss. But the
// outcome is logged AT the tick line (pushed / STRANDED), so a strand is
// visible immediately instead of surfacing 30 minutes later in the net.

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// pushTickWork pushes dir's current branch and verifies the local branch is
// level with its upstream afterwards. It is bounded to ~60s so a hung remote
// cannot stall tick close-out. Returns (ok, detail): ok is true only when the
// push succeeded AND the branch is not ahead of its upstream.
func pushTickWork(dir string) (bool, string) {
	if strings.TrimSpace(dir) == "" {
		return false, "no workdir"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "push").CombinedOutput(); err != nil {
		return false, "git push: " + firstClause(err.Error(), strings.TrimSpace(string(out)))
	}

	// Verify: nothing left ahead of the upstream. A push that "succeeded" but
	// still has commits ahead (e.g. a rejected side branch) is a strand.
	brOut, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return true, "pushed (verify skipped: " + err.Error() + ")"
	}
	branch := strings.TrimSpace(string(brOut))
	if branch == "" || branch == "HEAD" { // detached HEAD has no upstream
		return true, "pushed (detached HEAD; no upstream check)"
	}
	cntOut, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "--count", branch+"@{upstream}.."+branch).Output()
	if err != nil {
		return true, "pushed (no upstream for " + branch + ")"
	}
	if n := strings.TrimSpace(string(cntOut)); n != "0" {
		return false, "still " + n + " commit(s) ahead of upstream after push"
	}
	return true, "pushed + verified"
}

// firstClause returns the first non-empty line of out, else err's message,
// truncated — the tick log line stays one line.
func firstClause(errMsg, out string) string {
	msg := out
	if msg == "" {
		msg = errMsg
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
