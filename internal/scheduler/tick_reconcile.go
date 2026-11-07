package scheduler

// tick_reconcile.go — RELEASE-048: late-commit reconciliation for failed and
// timeout ticks.
//
// THE DEFECT. A tick whose gateway turn is torn down without a terminal
// event (the scheduler's own session wall firing inside the POST, an SSE
// stream that dies mid-run, a gateway restart) is booked terminal — usually
// status=failed via the SCHED-GAP-16940 drop path (spawn.go) — with
// commits=0, because the drop path has no SpawnedTick and never runs the
// git-window measurement the completed path runs in Wait(). The gateway-side
// session, however, is a SERVER-side process the scheduler never signals on
// any terminal path (the only kill surface, CancelTickSession, cancels the
// scheduler's own request context; nothing calls a gateway-side session
// kill). The session keeps working and its commits land AFTER the row says
// the tick ended — measured on coding-hermes-scheduler-releng on
// 2026-11-07: tick stamped failed 13:25:22, release commits landed
// 15:38:11-15:42:38, the session's model usage continuing to 16:29:23.
//
// THE FIX has two halves (arms 1, 2 and 4 of RELEASE-048 are one fix seen
// from three sides):
//
//  1. lifecycle.Complete measures the git window for failed/timeout
//     outcomes too (measureTerminalWindow below), so a row booked at a
//     moment its window already holds commits carries them.
//
//  2. THIS pass reconciles the rows whose commits landed AFTER booking. It
//     walks recently failed/timeout rows whose commits are still 0,
//     re-measures their window extended by at most ONE EXTRA TICK-WALL of
//     grace ([spawned_at, spawned_at + 2×wall], capped at now), and
//     annotates the row (commits, files_changed, the SCHED-GAP-104
//     code/board split) when commits actually landed. It NEVER rewrites
//     status: the failure is real (the harness did tear the tick down);
//     only the artifact accounting was missing.
//
// IDEMPOTENCE. The candidate set is `status IN (failed, timeout) AND
// commits = 0`, so an annotated row (commits > 0) can never be re-annotated.
// ticks.reconciled_at (migration v87) is stamped on EVERY check, annotating
// or not, and a row checked within the recheck interval is skipped — a
// genuinely zero row costs one git sweep per recheck interval for as long
// as it stays inside the lookback, not one per eval.
//
// COMPLETION-INSTANT TRUTH (arm 4). When a row is annotated, its
// completed_at advances to the newest commit's committer timestamp in the
// extended window (never backward), and the project's last_tick_completed
// advances with it ONLY while the project row still points at this tick
// (the old completed_at is the ownership guard: a newer tick's Complete
// would have overwritten it). last_tick_status is deliberately left
// failed/timeout — the status is the harness verdict and stays; the
// completion instant is the point the lane's work actually stopped, which
// is what cooldown pacing and every staleness reader should key off after
// the reconciliation.
//
// OVER-APPROXIMATION. The window measurement is the shared-window kind
// (tick_metrics.go's known limitation): commits by OTHER lanes in the same
// repo inside the extended window are attributed to the reconciled tick.
// Commits are never lost; they may be counted more than once. The
// code/board split (classifyGitCommits) makes the board-only traffic
// distinguishable on the row.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

const (
	// ReconcileLookbackDefault bounds how far back the pass looks for
	// failed/timeout rows. Older rows are historical: their windows have
	// long closed and annotating them would fold days of sibling traffic
	// into one row (the over-approximation above).
	ReconcileLookbackDefault = 12 * time.Hour

	// ReconcileRecheckDefault is how long a reconciled_at stamp suppresses
	// re-checking a still-zero row. The releng evidence had commits landing
	// 2h13m after booking, so the recheck must be much shorter than the
	// lookback; 15m keeps a zero row's cost at ~48 git sweeps over its
	// whole candidate life.
	ReconcileRecheckDefault = 15 * time.Minute

	// ReconcileCadenceDefault is the minimum interval between passes on the
	// daemon's periodic path (tick_process.go evaluate). The CLI passes 0
	// to always run.
	ReconcileCadenceDefault = 5 * time.Minute
)

// ReconcileReport summarizes one reconciliation pass.
type ReconcileReport struct {
	Examined     int      // candidate rows that passed the SQL prefilter
	Annotated    int      // rows whose commits were stamped > 0
	Advanced     int      // rows whose completed_at moved forward
	StillZero    int      // rows re-measured, still zero (stamped, not annotated)
	Unmeasurable int      // rows whose resolved workdir has no git repo (stamped once)
	Skipped      []string // id + reason for every row not annotated
}

// ReconcileOpts parameterizes one reconciliation pass.
type ReconcileOpts struct {
	// Now is the pass instant (the caller's clock — the daemon passes its
	// lifecycle clock, the CLI the wall clock).
	Now time.Time
	// Lookback bounds the candidate window (rows completed within
	// Now-Lookback). 0 = ReconcileLookbackDefault.
	Lookback time.Duration
	// RecheckAfter suppresses re-checking rows stamped within that
	// interval. 0 = ReconcileRecheckDefault.
	RecheckAfter time.Duration
	// MinCadence is the minimum interval between passes on THIS tracker
	// (0 = always run — the CLI).
	MinCadence time.Duration
	// Project restricts the pass to one project's rows (the CLI's --project,
	// for a deliberate targeted run). Empty = every project.
	Project string
	// Apply=false is the dry-run mode: everything is measured and reported,
	// nothing is written.
	Apply bool
}

// ReconcileLateTickCommits runs one reconciliation pass over the DB. The
// second return reports whether a pass actually ran (a cadence-gated call
// inside its minimum interval reports false).
func (lt *LifecycleTracker) ReconcileLateTickCommits(opts ReconcileOpts) (ReconcileReport, bool) {
	now := opts.Now
	if minCadence := opts.MinCadence; minCadence > 0 {
		lt.reconcileMu.Lock()
		if !lt.lastReconcileAt.IsZero() && now.Sub(lt.lastReconcileAt) < minCadence {
			lt.reconcileMu.Unlock()
			return ReconcileReport{}, false
		}
		lt.lastReconcileAt = now
		lt.reconcileMu.Unlock()
	}
	var rep ReconcileReport
	lookback := opts.Lookback
	if lookback <= 0 {
		lookback = ReconcileLookbackDefault
	}
	recheckAfter := opts.RecheckAfter
	if recheckAfter <= 0 {
		recheckAfter = ReconcileRecheckDefault
	}

	// The SQL side applies only the equality-class filters plus a raw-string
	// completed_at prefilter: RFC3339 with this fleet's single uniform local
	// offset sorts lexicographically, and every surviving row is re-parsed
	// and re-filtered precisely in Go below (so a non-uniform offset can
	// only over-select, never mis-annotate). idx_ticks_status_completed
	// serves the range scan.
	cutoff := now.Add(-lookback).Format(time.RFC3339)
	query := `
		SELECT id, project_name, spawned_at, completed_at, COALESCE(reconciled_at, '')
		FROM ticks
		WHERE status IN ('failed', 'timeout')
		  AND commits = 0
		  AND spawned_at IS NOT NULL AND spawned_at != ''
		  AND completed_at IS NOT NULL AND completed_at != ''
		  AND completed_at >= ?
		ORDER BY completed_at
	`
	args := []any{cutoff}
	if opts.Project != "" {
		query = strings.Replace(query, "ORDER BY", "AND project_name = ?\n		ORDER BY", 1)
		args = append(args, opts.Project)
	}
	rows, err := lt.db.Query(query, args...)
	if err != nil {
		rep.Skipped = append(rep.Skipped, fmt.Sprintf("query: %v", err))
		return rep, true
	}
	defer rows.Close()

	type candidate struct {
		id, project, spawnedAt, completedAt, reconciledAt string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.project, &c.spawnedAt, &c.completedAt, &c.reconciledAt); err != nil {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("scan: %v", err))
			continue
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rep.Skipped = append(rep.Skipped, fmt.Sprintf("rows: %v", err))
	}

	for _, c := range cands {
		rep.Examined++
		spawned, err := time.Parse(time.RFC3339, c.spawnedAt)
		if err != nil {
			rep.Skipped = append(rep.Skipped, c.id+": spawned_at unparsable: "+c.spawnedAt)
			continue
		}
		completed, err := time.Parse(time.RFC3339, c.completedAt)
		if err != nil {
			rep.Skipped = append(rep.Skipped, c.id+": completed_at unparsable: "+c.completedAt)
			continue
		}
		if completed.Before(now.Add(-lookback)) {
			rep.Skipped = append(rep.Skipped, c.id+": outside lookback")
			continue
		}
		if c.reconciledAt != "" {
			if last, err := time.Parse(time.RFC3339, c.reconciledAt); err == nil {
				if now.Sub(last) < recheckAfter {
					continue // checked recently — the watermark suppresses the sweep
				}
			}
		}
		workdir, ok := resolveMeasureWorkdir(lt.db, c.project)
		if !ok {
			// No git repo reachable from this project row — and none will
			// appear later (workdir/repo_url are static row facts). Stamp
			// once so the row stops costing a scan; commits stays 0, which
			// is the honest figure for an unmeasurable window.
			rep.Unmeasurable++
			rep.Skipped = append(rep.Skipped, c.id+": no git workdir for "+c.project)
			if opts.Apply {
				lt.stampReconciled(c.id, now, &rep)
			}
			continue
		}

		// The EXTENDED window: [spawned_at, spawned_at + 2×tick-wall] — the
		// booking instant plus at most one extra wall of grace. The grace is
		// bounded on purpose: an unbounded extension to `now` folds every
		// sibling lane's traffic in the same repo into the row (measured on
		// the live releng case: 76 commits of mostly-foreign work vs 6 in
		// the capped window). Two walls is the honest ceiling — the
		// gateway-side session was spawned under the same wall the tick was,
		// so work landing within one extra wall is plausibly still that
		// session's, and the measured driver (release commits 2h13m-2h48m
		// after booking against a 3h wall) fits well inside it. Sibling
		// traffic inside the cap is still attributed (the accepted
		// over-approximation below); the code/board split makes it
		// distinguishable on the row.
		extendedUntil := completed.Add(completed.Sub(spawned))
		if extendedUntil.After(now) {
			extendedUntil = now
		}
		commits, files := countGitChanges(workdir, spawned, extendedUntil)
		if commits <= 0 {
			rep.StillZero++
			if opts.Apply {
				lt.stampReconciled(c.id, now, &rep)
			}
			continue
		}

		// Annotate. The split reuses the canonical SCHED-GAP-104 classifier
		// through its bounded core, enumerating exactly the capped window
		// the count came from (so the split's sum can never exceed the
		// row's commits figure); its impossible/empty verdicts stamp the
		// explicit -1/-1 unmeasured marker (SCHED-GAP-202), never a
		// fabricated 0/0.
		code, board := -1, -1
		if cCode, cBoard, okSplit := classifyGitCommitsBounded(workdir, spawned, extendedUntil, commits); okSplit {
			code, board = cCode, cBoard
		}
		newCompleted := completed
		if newest, ok := gitNewestCommitTime(workdir, spawned, extendedUntil); ok && newest.After(newCompleted) {
			newCompleted = newest
		}
		if !opts.Apply {
			rep.Annotated++
			if newCompleted.After(completed) {
				rep.Advanced++
			}
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: dry-run: commits=%d files=%d code=%d board=%d completed_at %s -> %s",
				c.id, commits, files, code, board, c.completedAt, newCompleted.Format(time.RFC3339)))
			continue
		}
		res, err := lt.db.Exec(`
			UPDATE ticks
			SET commits = ?, files_changed = ?, code_commits = ?, board_commits = ?,
			    completed_at = ?, reconciled_at = ?
			WHERE id = ?
			  AND status IN ('failed', 'timeout')
			  AND commits = 0
		`, commits, files, code, board, newCompleted.Format(time.RFC3339), now.Format(time.RFC3339), c.id)
		if err != nil {
			rep.Skipped = append(rep.Skipped, c.id+": update failed: "+err.Error())
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// A concurrent writer annotated it first — nothing left to do.
			rep.Skipped = append(rep.Skipped, c.id+": lost the annotation race (already annotated)")
			continue
		}
		rep.Annotated++
		advanced := newCompleted.After(completed)
		if advanced {
			rep.Advanced++
			// Arm 4: the project row follows — but ONLY while it still
			// points at this tick (the old completed_at is the ownership
			// guard; a newer tick's Complete has by then written its own
			// instant and this row is no longer the project's newest).
			if _, err := lt.db.Exec(`
				UPDATE projects SET last_tick_completed = ?
				WHERE name = ? AND last_tick_completed = ?
			`, newCompleted.Format(time.RFC3339), c.project, c.completedAt); err != nil {
				log.Printf("RECONCILE: %s tick=%s annotated commits=%d but the project-row advance failed: %v",
					c.project, c.id, commits, err)
			}
		}
		log.Printf("RECONCILE: %s tick=%s failed/timeout row annotated: commits=%d files=%d code=%d board=%d completed_at %s -> %s",
			c.project, c.id, commits, files, code, board, c.completedAt, newCompleted.Format(time.RFC3339))
		// Best-effort audit event — the row changed shape (commits 0 -> N,
		// completed_at moved) and a reader comparing old snapshots must be
		// able to find out why. context.Background() matches the stand-down
		// event precedent in lifecycle.Complete: created_at on an audit row
		// is a wall-clock stamp, never a component-clock one.
		_ = database.LogEvent(context.Background(), lt.db, &database.Event{
			Severity:  database.SeverityLow,
			Component: "lifecycle",
			Message: fmt.Sprintf("tick commits reconciled: %s (%s) failed/timeout row carried commits=%d code=%d board=%d, completed_at advanced to %s (RELEASE-048)",
				c.id, c.project, commits, code, board, newCompleted.Format(time.RFC3339)),
			Details: fmt.Sprintf(`{"tick_id":%q,"project":%q,"commits":%d,"files_changed":%d,"code_commits":%d,"board_commits":%d,"completed_at":%q}`,
				c.id, c.project, commits, files, code, board, newCompleted.Format(time.RFC3339)),
		})
	}
	return rep, true
}

// stampReconciled records the check watermark on a still-zero (or
// unmeasurable) row. The commits=0 guard keeps a row a concurrent writer
// just annotated from being watermarked back down.
func (lt *LifecycleTracker) stampReconciled(id string, now time.Time, rep *ReconcileReport) {
	if _, err := lt.db.Exec(`
		UPDATE ticks SET reconciled_at = ?
		WHERE id = ? AND commits = 0 AND status IN ('failed', 'timeout')
	`, now.Format(time.RFC3339), id); err != nil {
		rep.Skipped = append(rep.Skipped, id+": watermark write failed: "+err.Error())
	}
}

// resolveMeasureWorkdir resolves the git workdir a project's commit
// accounting must measure: the project's own workdir when it IS a git
// checkout, else the local checkout named by repo_url's local: scheme.
//
// The fallback exists because stand-in workdirs are deliberately NOT git
// roots on this fleet (the releng lane runs in
// ~/.hermes/stand-in/<lane>/<project> while its product — the release cut —
// lands in the main checkout the repo_url names). The local: prefix is
// internal/target's KindLocal scheme (target.go); a repo_url in any other
// scheme (https:, git@:) names a remote and yields no measurable workdir.
// ok=false means no git repo could be resolved — callers must treat the
// window as UNMEASURABLE (-1 sentinel), never a measured zero.
func resolveMeasureWorkdir(db *sql.DB, project string) (string, bool) {
	var workdir, repoURL string
	err := db.QueryRow(`SELECT COALESCE(workdir, ''), COALESCE(repo_url, '') FROM projects WHERE name = ?`, project).
		Scan(&workdir, &repoURL)
	if err != nil {
		return "", false
	}
	if isGitWorkdir(workdir) {
		return workdir, true
	}
	if path, ok := localRepoURLPath(repoURL); ok && isGitWorkdir(path) {
		return path, true
	}
	return "", false
}

// isGitWorkdir mirrors the git-repo probe the canonical classifiers use
// (adaptive_cooldown.classifyGitCommits): a directory containing a .git
// entry. git worktrees carry a .git FILE, not a dir — os.Stat alone covers
// both, the IsDir check the classifiers do would exclude worktrees, so this
// probe accepts any .git entry (a file's good enough: git -C resolves it).
func isGitWorkdir(dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && fi != nil
}

// localRepoURLPath strips the local: scheme from a repo_url, mirroring
// internal/target's KindLocal parse (target.go:142).
func localRepoURLPath(repoURL string) (string, bool) {
	const scheme = "local:"
	if !strings.HasPrefix(repoURL, scheme) {
		return "", false
	}
	path := filepath.Clean(strings.TrimPrefix(repoURL, scheme))
	if path == "" || path == "." {
		return "", false
	}
	return path, true
}

// gitNewestCommitTime returns the newest commit's committer timestamp within
// [since, until] across ALL refs of workdir (the same all-refs visibility as
// gitCommitCountInWindow — SCHED-GAP-1659). ok=false on any git error or an
// empty window.
func gitNewestCommitTime(workdir string, since, until time.Time) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runGit(ctx, workdir, "log", "--all", "--format=%cI", "-1",
		"--since="+since.Format(time.RFC3339), "--until="+until.Format(time.RFC3339))
	if err != nil {
		return time.Time{}, false
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, line)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// measureTerminalWindow is the failed/timeout half of the fix (fix 1): the
// git-window measurement the completed path has always run in Wait(), now
// applied when a terminal row is booked. It is called from
// lifecycle.Complete so EVERY failed/timeout outcome — the spawn drop path
// included, which has no SpawnedTick and previously wrote the struct-default
// 0 — carries the commits its own start..end window holds.
//
// Sentinel contract (SCHED-GAP-1652/202): a MEASURED window writes its real
// figures (0 included — a measured zero is a fact, not a fake); an
// UNMEASURABLE one (no workdir, no git repo) writes the explicit -1/-1
// unmeasured marker so it can never read as a zero-commit tick. An outcome
// that already carries a measurement (the spawn path's pre-counted
// gwFailCommits) is respected and left untouched.
func measureTerminalWindow(db *sql.DB, project string, started, finished time.Time) (commits, files int) {
	workdir, ok := resolveMeasureWorkdir(db, project)
	if !ok {
		return -1, -1
	}
	return countGitChanges(workdir, started, finished)
}
