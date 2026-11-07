package scheduler

import (
	"database/sql"
	"log"
	"sync"
	"time"
)

// NOOP-PACING: a tasks-admitted lane that produces nothing must still be
// paced. Tasks admission (SCHED-GAP-124) deliberately waives cooldown_s /
// cooldown_pin_s while the board holds workable rows — that is the mode's
// contract, not a bug. What was missing is the other half of the same
// contract: the waiver is only justified for a lane that ACTS on the work.
//
// Measured defect (live, 2026-11-06, task-router-foreman): admission_mode
// 'tasks', cooldown_s = cooldown_pin_s = cooldown_floor_s = 21600s, board
// holding 78 pending rows. In one calendar day the lane was spawned 76 times;
// 63 ticks recorded outcome='dry_run' with commits=0 and files_changed=0, 59
// of them with admit_reason='early', and 50 of the 75 inter-spawn gaps were
// under 5 minutes (min 2.0 min, mean 13.0 min). projects.no_progress_ticks
// stayed 0 throughout: the only writer of that column runs behind the opt-in
// adaptive_cooldown check (adaptive_cooldown.go), which this lane has at 0,
// and the tasks-mode branch of that function returns before the streak is
// ever touched. So a no-op tick burned nothing: it reset no counter, charged
// no failure, and the lane re-admitted on the 60s SCHED-GAP-136 pacing floor
// alone, forever, because 'tasks' kept finding work it never did.
//
// The stand-down mechanism already existed for ONE no-op class: SCHED-GAP-214
// stands the waiver down after a FAILED tick (last_tick_status='failed').
// A dry_run completes CLEANLY — lifecycle.Complete stamps 'completed' — so it
// never reaches that path, and neither does a 3h wall-clock timeout
// (last_tick_status='timeout', which lastTickStatusFailed does not match).
// Both of the measured failure modes are therefore invisible to every gate.
//
// The rule here: N consecutive no-artifact ticks on a tasks-admitted lane
// space the next admission by the lane's own cooldown_floor_s, doubling per
// consecutive no-op tick up to 8x the floor (ADV-R10's derived ceiling shape,
// database.AdaptiveCeilingFloorMultiplier). The first no-op already costs the
// full floor — 6h here — so a lane that produced nothing can no longer be
// respawned two minutes later. The pause is bounded (48h at this lane's
// 21600s floor) and re-derived from the streak on every pass, so the lane is
// never abandoned: it retries, and ANY artifact snaps it straight back to the
// normal waiver.

// noopPacingMaxDoublings bounds the growth below the floor multiplier that
// ADV-R10 already treats as the ceiling: streak 1 -> floor, 2 -> 2x,
// 3 -> 4x, 4+ -> 8x. Expressed as doublings so it composes with
// database.AdaptiveCeilingFloorMultiplier without importing a second constant
// that could drift from it.
const noopPacingMaxDoublings = 3

// tasksNoopCooldown is the spacing a tasks-admitted lane owes after `streak`
// consecutive no-artifact ticks. 0 means "no no-op pause" — either the streak
// is empty (the lane is productive) or the lane has no configured floor, in
// which case the SCHED-GAP-136 pacing floor is the only spacing it can have
// and inventing one would be an operator-visible policy change on a lane
// nobody measured.
func tasksNoopCooldown(streak, floorS int) time.Duration {
	if streak <= 0 || floorS <= 0 {
		return 0
	}
	n := streak - 1
	if n > noopPacingMaxDoublings {
		n = noopPacingMaxDoublings
	}
	return (time.Duration(floorS) * time.Second) << uint(n)
}

// tasksNoopStreakDeferred reports whether the lane's last terminal tick is
// still inside the no-op pause its streak entitles it to. A nil lastTick
// (never ticked) is never deferred — the pause is a POST-tick spacing, the
// same contract tasksPacingDeferredJittered holds.
func tasksNoopStreakDeferred(streak, floorS int, lastTick *time.Time, now time.Time) bool {
	if lastTick == nil {
		return false
	}
	need := tasksNoopCooldown(streak, floorS)
	if need <= 0 {
		return false
	}
	return now.Sub(*lastTick) < need
}

// noopPacingLog dedupes the operator-visible line: the packers run every few
// seconds, so the deferral is logged once per (lane, streak) transition
// instead of once per pass. Mirrors noteOwnershipRefusal's shape.
var (
	noopPacingMu   sync.Mutex
	noopPacingSeen = map[string]int{}
)

// noteNoopPacing logs the first pass that defers a lane at a given streak.
func noteNoopPacing(lane string, streak int, need time.Duration) {
	noopPacingMu.Lock()
	prev, seen := noopPacingSeen[lane]
	noopPacingSeen[lane] = streak
	noopPacingMu.Unlock()
	if seen && prev == streak {
		return
	}
	log.Printf("NOOP-PACING: %s deferred — %d consecutive no-op tick(s), next admission spaced %.0fs (cooldown floor)",
		lane, streak, need.Seconds())
}

// recordTasksNoopStreak maintains projects.no_progress_ticks as the count of
// CONSECUTIVE ticks that produced no artifact of the lane's own work.
//
// WHY THIS COLUMN: no_progress_ticks already means exactly this ("current
// consecutive no-progress streak" — the API/MCP surfaces document it that
// way), it is already bump-saved/restored, and for a tasks lane it was dead:
// the only writer sat behind the opt-in adaptive_cooldown check. Reusing it is
// the smallest correct change — no migration, no new column, one meaning.
//
// WHAT COUNTS AS PROGRESS (SCHED-GAP-104, reused verbatim): a CODE commit.
// Board bookkeeping is not work — a tick that only rewrote its own
// tasks.jsonl/events.jsonl commits with board_commits>0 and code_commits==0,
// and must extend the streak, otherwise the fleet's board-churning lanes
// self-report as productive forever (the exact defect SCHED-GAP-104 closed for
// adaptive cooldown). A DuckBrain memory-key write is the non-commit artifact
// SCHED-GAP-1680 already treats as landed, so it counts too.
//
// FAIL OPEN on an unmeasurable commit window: if git could not classify
// (no .git, exec error, claimed commits with none in the window) any commit,
// file delta or memory key counts as progress. A measurement gap must never
// charge a lane a 6h pause.
func recordTasksNoopStreak(db *sql.DB, outcome TickOutcome, codeCommits int, measured bool) {
	if db == nil {
		return
	}
	progress := tickProducedWork(outcome, codeCommits, measured)
	if progress {
		// Guarded so a productive lane pays no write: the streak is already 0.
		if _, err := db.Exec(
			`UPDATE projects SET no_progress_ticks = 0 WHERE name = ? AND no_progress_ticks != 0`,
			outcome.Project); err != nil {
			log.Printf("NOOP-PACING: %s streak reset failed: %v", outcome.Project, err)
		}
		return
	}
	if _, err := db.Exec(
		`UPDATE projects SET no_progress_ticks = no_progress_ticks + 1 WHERE name = ?`,
		outcome.Project); err != nil {
		log.Printf("NOOP-PACING: %s streak write failed: %v", outcome.Project, err)
	}
}

// tickProducedWork is the ONE definition of "this tick produced an artifact of
// the lane's own work" (SCHED-GAP-104): a CODE commit or a DuckBrain memory key.
// Board bookkeeping is not work. An unmeasurable commit window fails OPEN — any
// commit, file delta or memory key counts — because a measurement gap must never
// charge a lane a pause.
//
// Shared by the streak writer and the dry-run throttle so the two can never
// disagree about what a dry tick is (a lane that resets its streak while the
// throttle counts it as dry is a permanent suspension bug).
func tickProducedWork(outcome TickOutcome, codeCommits int, measured bool) bool {
	if !measured {
		return outcome.Commits > 0 || outcome.FilesChanged > 0 || outcome.MemoryKeys > 0
	}
	return codeCommits > 0 || outcome.MemoryKeys > 0
}

// tickProducedWorkForThrottle is the DRY-RUN THROTTLE's definition, and it is
// deliberately one leg WIDER than the pacing streak: a board commit counts.
//
// Why the asymmetry: the streak feeds a pacing decision that only spaces a
// lane, so a board-churning lane self-reporting as productive is a cost, not a
// stop. The throttle SUSPENDS, and a suspension must be liftable by the lane's
// own output or it is permanent. A docs/PM lane's product IS the board — its
// rows, its events — so judging it dry on code commits alone would suspend the
// one lane class that never writes code and then never let it back: the pin
// could only be cleared by work it is not supposed to do.
//
// A formatter/lint lane that has nothing to format commits nothing at all (no
// diff, no board write), so it is still exactly what this feature targets.
func tickProducedWorkForThrottle(outcome TickOutcome, codeCommits, boardCommits int, measured bool) bool {
	if !measured {
		return outcome.Commits > 0 || outcome.FilesChanged > 0 || outcome.MemoryKeys > 0
	}
	return codeCommits > 0 || boardCommits > 0 || outcome.MemoryKeys > 0
}
