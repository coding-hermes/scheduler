package scheduler

import "time"

// ClassifyGitCommits exposes classifyGitCommits (adaptive_cooldown.go) to
// callers outside this package — the SCHED-PERF-001-B back-fill tool
// (cmd/backfill-commit-signals) is the only one.
//
// This is a pass-through, never a reimplementation: the daemon stamps the
// live code/board split with the unexported function and the back-fill
// reconstructs historical rows with it, so the two can never disagree about
// what counts as code vs fleet bookkeeping (.coding-hermes/ prefix) or about
// when a measurement is untrustworthy (ok == false — no git repo, git
// failure, or git reporting fewer commits than the tick claimed).
func ClassifyGitCommits(workdir string, since time.Time, claimed int) (code, board int, ok bool) {
	return classifyGitCommits(workdir, since, claimed)
}
