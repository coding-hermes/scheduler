package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-157 — tick lifecycle persistence.
//
// The row's acceptance: for any window, "how long did this lane wait for a
// slot" and "why was it skipped" must be answerable from the DB alone; a
// starvation report (lane waited > N minutes) is a single query. Before this
// change both answers required reading log-line ORDER across a rotated
// scheduler.log.
//
// What is persisted where:
//
//   - ticks.slot_wait_ms  — the wait between a tick being handed to the slot
//     pool and the slot being acquired (SlotPool.spawn). 0 = never waited.
//   - ticks.admit_reason  — the SCHED-GAP-155 admission decision that let the
//     tick in ("ok" for every packer-selected tick; a nudge tick carries the
//     orphan-resume reason it was re-fired for).
//   - ticks.nudge_source  — why this tick row exists OUTSIDE the packer:
//     "startup" | "manual" | "board_wake". Every packer tick carries "" — the
//     packer path IS the default, and stamping it would be a fabricated value.
//   - deferrals (new table, migration v35) — one row per candidate project an
//     evaluation pass passed over, with the reason from the SCHED-GAP-155
//     vocabulary. This is deliberately a SEPARATE table, not a ticks column:
//     a pass-over decision has no tick row, and a pseudo-row with a synthetic
//     id in ticks would corrupt the scheduler's own clock — the packer reads
//     MAX(completed_at) over ALL non-running ticks as the cooldown baseline
//     (evalContext), so a deferral pseudo-row would masquerade as the lane's
//     last attempt and delay its next admission by a whole cooldown.
//
// Everything here is OBSERVABILITY, never a gate: every write is best-effort,
// logged on failure, and can never change an admission decision (the G7
// ruling stands — real admission gates live only in SlotPool.spawn).

// Nudge sources (SCHED-GAP-157) — why a tick row exists outside the packer.
const (
	// NudgeSourceStartup: a tick enqueued by the orphan-resume scan running
	// once at daemon boot (resumeOrphansAtStartup), after a restart.
	NudgeSourceStartup = "startup"
	// NudgeSourceManual: a tick enqueued by the operator — the API spawn
	// endpoint (POST /api/v1/projects/{name}/spawn → Loop.SpawnNow).
	NudgeSourceManual = "manual"
	// NudgeSourceBoardWake: a tick enqueued by the ADV-R07 board-wake path —
	// a board write forced a re-evaluation that selected the project.
	NudgeSourceBoardWake = "board_wake"
)

// boardWakeNudgeSource is the package-level hook the board watcher consumes
// (NewBoardWakeWatcher → w.onWake): NewLoop installs a closure that stamps
// NudgeSourceBoardWake on itself, mirroring how the gateway-health gate
// receives its client and event logger — the watcher is constructed at the
// daemon's wiring site (cmd/schedulerd) which owns no Loop reference beyond
// ForceEvaluate. Set-once per process, read once per watcher construction.
var boardWakeNudgeSource atomic.Value // func()

// SetNudgeSource records which non-packer entry point produced the tick a
// caller is about to hand to the slot pool. It MUST be called (or deliberately
// skipped) immediately before SlotPool.SpawnEnqueued/Spawn — the pool's spawn
// body reads it once at goroutine start and clears it, so concurrent spawns
// cannot observe a previous caller's value, and a value left behind by a
// caller that then deferred (load gate, gateway gate, dedup) can never leak
// into another tick's row.
func (l *Loop) SetNudgeSource(source string) {
	l.nudgeSource.Store(source)
}

// clearNudgeSource reads and clears the pending nudge-source stamp. The slot
// pool calls this from the spawn goroutine — the first thing the goroutine
// does — so the stamp applies to exactly the one spawn the caller wired it
// for.
func (l *Loop) clearNudgeSource() string {
	s, _ := l.nudgeSource.Swap("").(string)
	return s
}

// recordDeferral persists one SCHED-GAP-157 pass-over row (best-effort).
// Called from the admission pass for every candidate the pass decided not to
// admit; passID is the same per-process pass counter the ADMIT lines carry.
func (l *Loop) recordDeferral(project, reason string, passID int64, detail string) {
	if l.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := database.RecordDeferral(ctx, l.db, project, reason, passID, detail); err != nil {
		log.Printf("ADMIT: record deferral %s (%s): %v", project, reason, err)
	}
}

// stampTickAdmission persists the SCHED-GAP-157 admission-lifecycle stamp on
// an existing tick row: the measured slot wait, the admission decision that
// let the tick in, and (when non-empty) the non-packer entry point. Called by
// SlotPool.spawn at the admit → start boundary and by tests. Best-effort: the
// stamp is observability — a failed write is logged and never blocks the
// spawn.
func stampTickAdmission(db *sql.DB, id string, slotWait time.Duration, admitReason, nudgeSource string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := database.RecordTickAdmission(ctx, db, id, slotWait, admitReason, nudgeSource); err != nil {
		log.Printf("ADMIT: record admission stamp tick %s: %v", id, err)
	}
}

// StarvationThreshold is the slot wait at which a lane counts as starved in
// the acceptance starvation query (docs/api.md §7.1): 5 minutes, the slot
// pool's own default patience — a lane that waited past the patience the pool
// drops work at is the definition of a starved lane. Informational only.
const StarvationThreshold = 5 * time.Minute
