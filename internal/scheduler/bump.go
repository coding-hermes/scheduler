package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Task bump lifecycle (SCHED-GAP-107).
//
// A bump temporarily accelerates a project to bump_cooldown_s (>= the 7200s
// killer-lane floor) for up to 8 completed ticks, then auto-reverts. The
// bump API (database.BumpProject) snapshots the pre-bump cooldown policy
// into bump_saved_* columns so the revert is exact.
//
// This file owns the tick-side machinery:
//
//   - flagTickBump stamps ticks.bump=1 on rows spawned while their project
//     had an active bump (yield analysis can compare bump vs normal ticks);
//   - markBumpTick enqueues the bump flag at spawn time (called from the
//     spawn paths before the row transitions to running);
//   - bumpTickCompleted runs at tick completion: it consumes one bump tick,
//     force-reverts on the 12h hard cap, and — when the countdown reaches
//     zero — performs the two-phase revert:
//       Phase A: database.ClearBump restores the saved pre-bump state
//                (cooldown_s, floor, ceiling, no_progress_ticks);
//       Phase B: adaptiveCooldown is re-run over THIS tick's outcome so the
//                normal law immediately re-evaluates the project. Real work
//                during the bump → progress → cooldown lands at the floor;
//                idle bump ticks → the restored pre-bump streak resumes
//                exactly where it was (bump ticks neither forgive nor
//                punish).

// bumpHardCap bounds how long a single bump may stay active regardless of
// remaining ticks — a bumped project that never completes ticks (all
// failing, daemon paused) cannot hold the boost forever. 12h from
// bump_started_at.
const bumpHardCap = 12 * time.Hour

// markBumpTick stamps the tick row as a bump tick (bump=1) when its project
// has an active bump at spawn time. Called by the spawn paths right after
// the row exists (queued or running). Best-effort: observability only.
func markBumpTick(db *sql.DB, project, tickID string) {
	if db == nil || tickID == "" {
		return
	}
	var active int
	if err := db.QueryRow(`SELECT COALESCE(bump_active, 0) FROM projects WHERE name = ?`, project).Scan(&active); err != nil {
		return
	}
	if active != 1 {
		return
	}
	if _, err := db.Exec(`UPDATE ticks SET bump = 1 WHERE id = ?`, tickID); err != nil {
		log.Printf("BUMP: flag tick %s (%s) failed: %v", tickID, project, err)
	}
}

// bumpTickCompleted processes a COMPLETED tick for bump accounting. Called
// from the slot-pool / sim-spawner completion hooks, right BEFORE the
// adaptiveCooldown/autoSlowdown handoff (which doubles as Phase B of the
// auto-revert). db/workdir mirror the adaptiveCooldown call signature.
//
// Semantics:
//   - Only ticks flagged bump=1 consume the countdown (idle bump ticks burn
//     the budget without extending anything — the point of the bump is
//     measurement, not extension).
//   - The 12h hard cap force-reverts regardless of remaining ticks.
//   - When the countdown hits zero, Phase A (restore saved state) runs
//     immediately; Phase B is the caller's adaptiveCooldown call which now
//     evaluates against the restored baseline.
//
// Returns true when this call performed the Phase A revert (caller MUST
// still run adaptiveCooldown — it is Phase B).
func bumpTickCompleted(db *sql.DB, project, workdir string, outcome TickOutcome) bool {
	if db == nil {
		return false
	}

	// 12h hard cap: force-revert an old bump regardless of the tick flag
	// or remaining count. Checked on every bump-project tick completion so
	// a stuck bump cannot outlive its window.
	var (
		active    int
		remaining int
		startedAt string
	)
	err := db.QueryRow(`SELECT COALESCE(bump_active, 0), COALESCE(bump_remaining_ticks, 0), COALESCE(bump_started_at, '')
FROM projects WHERE name = ?`, project).Scan(&active, &remaining, &startedAt)
	if err != nil || active != 1 {
		return false
	}

	// The tick must actually be a bump tick to consume the countdown...
	isBumpTick := false
	if outcome.TickID != "" {
		var flagged int
		if err := db.QueryRow(`SELECT COALESCE(bump, 0) FROM ticks WHERE id = ?`, outcome.TickID).Scan(&flagged); err == nil {
			isBumpTick = flagged == 1
		}
	}

	if !isBumpTick {
		// A non-bump tick completing against a bump-active project (e.g.
		// a manual SpawnNow raced the bump activation): does not consume
		// the countdown, but the hard cap still applies.
		if !bumpExpired(startedAt, time.Now()) {
			return false
		}
		log.Printf("BUMP: %s 12h hard cap exceeded (started %s) — force-reverting with %d tick(s) remaining", project, startedAt, remaining)
		revertBump(db, project, outcome, "hard-cap")
		return true
	}

	if bumpExpired(startedAt, time.Now()) {
		log.Printf("BUMP: %s 12h hard cap exceeded (started %s) — force-reverting with %d tick(s) remaining", project, startedAt, remaining)
		revertBump(db, project, outcome, "hard-cap")
		return true
	}

	if remaining > 1 {
		if _, err := db.Exec(`UPDATE projects SET bump_remaining_ticks = bump_remaining_ticks - 1 WHERE name = ?`, project); err != nil {
			log.Printf("BUMP: %s countdown write failed: %v", project, err)
		}
		return false
	}

	// Countdown exhausted (this was the last bump tick) — auto-revert.
	log.Printf("BUMP: %s final bump tick consumed — auto-reverting (Phase A restore + Phase B adaptive re-eval)", project)
	revertBump(db, project, outcome, "countdown")
	return true
}

// bumpExpired reports whether a bump started at the RFC3339 startedAt is
// older than the 12h hard cap. Unparseable/empty timestamps never expire
// (fail-open: a corrupt stamp must not brick the revert path — the
// countdown still bounds the bump).
func bumpExpired(startedAt string, now time.Time) bool {
	if startedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return false
	}
	return now.Sub(t) > bumpHardCap
}

// revertBump is Phase A of the auto-revert: restore the saved pre-bump
// state via database.ClearBump. Phase B is the caller's adaptiveCooldown
// call, which runs AFTER this restore so it evaluates the correct baseline.
// reason names the revert trigger for the events table.
func revertBump(db *sql.DB, project string, outcome TickOutcome, reason string) {
	if err := database.ClearBump(context.Background(), db, project); err != nil {
		// Never fail the tick lifecycle over a bump bookkeeping error —
		// the countdown keeps decreasing and the hard cap guarantees an
		// eventual retry, so log loudly and move on.
		log.Printf("BUMP: %s Phase A revert (%s) failed: %v — hard cap will retry", project, reason, err)
		return
	}
	details, _ := json.Marshal(map[string]any{
		"project": project,
		"reason":  reason,
		"tick_id": outcome.TickID,
		"commits": outcome.Commits,
	})
	_ = database.LogEvent(context.Background(), db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "bump",
		Message:   "bump auto-reverted: " + project + " (" + reason + ")",
		Details:   string(details),
	})
}
