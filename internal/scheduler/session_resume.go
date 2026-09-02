package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-091 — session resume-after-restart (Bane 2026-09-01):
// when the gateway drops or the daemon restarts, in-flight sessions must be
// RESUMABLE, not lost. Four pieces:
//
//  1. DROP DETECTION — every path that terminates a previously-live running
//     tick because its OWNER is gone (gateway drain timeout, daemon crash
//     startup reap, zombie heartbeat reap) stamps the tick row
//     orphaned_at + orphan_reason alongside its terminal status.
//  2. ORPHAN SCAN — on gateway health-return (daemon startup with a live
//     gateway, or the existing reconnect flip in evaluate), find orphaned
//     ticks whose project work is still incomplete.
//  3. THE NUDGE — re-queue the tick with a CONTINUATION prompt ("you were
//     doing X — continue") instead of a cold restart. The re-spawn rides
//     the normal slot pool (SpawnEnqueued), so completion, delivery,
//     timeout, and the idempotence guard are all inherited.
//  4. IDEMPOTENCE / BUDGET — the guard already checks tree state before
//     commit, so a resumed session cannot double-commit. Nudges are capped
//     at MaxNudgesPerTick per tick row: beyond the cap the tick gets a
//     needs-human event instead of another resurrection (anti
//     zombie-resurrection — the fleet has 29k reasons to fear zombie loops).

// MaxNudgesPerTick caps continuation re-spawns per tick row. Beyond this,
// the tick is failed into needs-human (HIGH event) instead of re-spawned.
const MaxNudgesPerTick = 2

// Orphan reasons — which drop path stamped the row.
const (
	// OrphanReasonDrainTimeout: graceful-shutdown drain expired with the
	// tick still in flight (loop.Stop → abortInFlightTicks).
	OrphanReasonDrainTimeout = "drain_timeout"
	// OrphanReasonStartupReap: daemon (re)started and found the tick's
	// owner gone — dead pid or stale gateway heartbeat
	// (cleanDanglingOnStartup).
	OrphanReasonStartupReap = "startup_reap"
	// OrphanReasonZombieReap: the 60s reaper found a live-daemon tick whose
	// pid/heartbeat died (reapZombies).
	OrphanReasonZombieReap = "zombie_reap"
)

// orphanedTick is one resumable tick row selected by orphansToResume.
type orphanedTick struct {
	id         string
	project    string
	sessionID  string
	nudgeCount int
	prompt     string
	reason     string
}

// stampOrphaned marks one tick row as orphaned at its terminal transition.
// reason records the drop path; it is shown in the continuation prompt and
// the resume HIGH event. Best-effort: a failed stamp is logged, never
// blocks the terminal transition itself.
func (l *Loop) stampOrphaned(tickID, reason string) {
	_, err := l.db.Exec(
		`UPDATE ticks SET orphaned_at = ?, orphan_reason = ? WHERE id = ?`,
		time.Now().Format(time.RFC3339), reason, tickID)
	if err != nil {
		log.Printf("RESUME: stamp orphaned tick %s (%s): %v", tickID, reason, err)
	}
}

// orphansToResume selects resumable orphaned ticks: orphaned rows that are
// terminal AND whose project has NO queued/running tick right now (the
// project's work is genuinely stalled, not merely between ticks). Selection
// is fully consumed before any UPDATE — SQLite single-writer discipline.
// The project prompt is joined in so the continuation prompt can carry the
// project's original instructions (state snapshot inside the nudge).
func (l *Loop) orphansToResume(ctx context.Context) []orphanedTick {
	rows, err := l.db.QueryContext(ctx, `
SELECT t.id, t.project_name, COALESCE(t.session_id, ''), t.nudge_count,
       COALESCE(t.orphan_reason, ''), COALESCE(p.prompt, '')
FROM ticks t
JOIN projects p ON p.name = t.project_name
WHERE t.orphaned_at IS NOT NULL
  AND t.status IN ('failed','timeout')
  AND t.nudge_count < ?
  AND p.enabled = 1
  AND NOT EXISTS (
      SELECT 1 FROM ticks r
      WHERE r.project_name = t.project_name AND r.status IN ('queued','running'))`,
		MaxNudgesPerTick)
	if err != nil {
		log.Printf("RESUME: orphan-scan query failed: %v", err)
		return nil
	}
	defer rows.Close()
	var out []orphanedTick
	for rows.Next() {
		var o orphanedTick
		if err := rows.Scan(&o.id, &o.project, &o.sessionID, &o.nudgeCount, &o.reason, &o.prompt); err != nil {
			continue
		}
		out = append(out, o)
	}
	return out
}

// buildContinuationPrompt assembles the nudge prompt for an orphaned tick:
// the project's own foreman prompt (namespace default / append / replace
// is NOT re-resolved here — p.prompt is the project-level prompt; the base
// buildForemanPrompt rules re-apply through the normal spawn path via the
// Prompt field) plus the continuation preamble. The preamble says WHAT
// happened (gateway drop), WHICH tick is being continued, and — when the
// gateway recorded a session — its id, so the resumed foreman can inspect
// its previous session state instead of re-exploring.
func buildContinuationPrompt(projectPrompt, tickID, sessionID, reason string) string {
	var b strings.Builder
	b.WriteString("[SCHEDULER RESUME: your previous session for tick " + tickID)
	b.WriteString(" was interrupted (gateway drop: " + reason + "). ")
	b.WriteString("This is a CONTINUATION, not a restart — check the worktree, git log, and board for work your previous session already committed before picking a task; do not redo completed work.")
	if sessionID != "" {
		b.WriteString(" Previous session id: " + sessionID + ".")
	}
	b.WriteString("]\n")
	if projectPrompt != "" {
		b.WriteString(projectPrompt)
		b.WriteString("\n")
	}
	return b.String()
}

// bumpNudgeCount increments the tick's nudge counter. Returns the new count.
func (l *Loop) bumpNudgeCount(tickID string) int {
	if _, err := l.db.Exec(
		`UPDATE ticks SET nudge_count = nudge_count + 1 WHERE id = ?`, tickID); err != nil {
		log.Printf("RESUME: bump nudge_count tick %s: %v", tickID, err)
	}
	var n int
	_ = l.db.QueryRow(`SELECT nudge_count FROM ticks WHERE id = ?`, tickID).Scan(&n)
	return n
}

// resumeOrphans is the orphan scan + nudge. Callers invoke it when the
// gateway is healthy again (daemon startup, or the reconnect flip in
// evaluate): it re-verifies gateway health itself (fail-safe — a dead
// gateway must not trigger spawns), scans for resumable orphans, and
// re-queues each through SpawnEnqueued so the nudge inherits the slot
// pool's completion/delivery/timeout handling. Ticks beyond the nudge cap
// never reach this scan; ticks whose resumed spawn errors get a HIGH
// needs-human event so the drop is visible instead of silent.
func (l *Loop) resumeOrphans(trigger string) {
	if l.spawner == nil || !l.spawner.GatewayAvailable() {
		return
	}
	l.mu.RLock()
	simulate, noDeliver := l.simulate, l.noDeliver
	l.mu.RUnlock()
	if simulate {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	orphans := l.orphansToResume(ctx)
	if len(orphans) == 0 {
		return
	}

	// Project rows are re-fetched fresh (Workdir/chain config may have
	// changed while the tick was orphaned) — GetProject, never a stale
	// join snapshot.
	resumed := 0
	for _, o := range orphans {
		proj, err := database.GetProject(ctx, l.db, o.project)
		if err != nil {
			log.Printf("RESUME: orphaned tick %s: project %s gone: %v", o.id, o.project, err)
			continue
		}
		count := l.bumpNudgeCount(o.id)
		tickID := fmt.Sprintf("%s-nudge%d", o.id, count)
		// Enqueue the nudge row BEFORE SpawnEnqueued — the pool's spawn
		// body only transitions queued→running (StartRunning) and the
		// completion UPDATE no-ops on a missing row, so without this the
		// nudge tick would be invisible in /ticks and never terminal.
		if err := l.lifecycle.Enqueue(o.project, tickID); err != nil {
			log.Printf("RESUME: enqueue nudge tick %s: %v", tickID, err)
			continue
		}
		packed := l.packedProjectFrom(*proj)
		// Append semantics: the builtin base prompt (skill-loading etc.)
		// stays, the continuation preamble + original project prompt ride
		// on top — a resumed session loads the foreman skill AND knows it
		// is continuing, not starting cold.
		packed.Prompt = buildContinuationPrompt(o.prompt, o.id, o.sessionID, o.reason)
		packed.PromptMode = "append"
		l.slotPool.SpawnEnqueued(packed, tickID, time.Now(), noDeliver, l.db)
		resumed++
		log.Printf("RESUME: nudged orphaned tick %s (project %s, reason=%s, nudge %d/%d) as %s",
			o.id, o.project, o.reason, count, MaxNudgesPerTick, tickID)
		l.EmitHighEvent("resume", fmt.Sprintf(
			"orphaned tick resumed — project %s tick %s re-nudged (%s, attempt %d/%d)",
			o.project, o.id, o.reason, count, MaxNudgesPerTick),
			map[string]any{
				"project": o.project, "tick_id": o.id, "nudge_tick_id": tickID,
				"reason": o.reason, "nudge_count": count, "trigger": trigger,
			})
	}
	if resumed > 0 {
		log.Printf("RESUME: %d orphaned tick(s) re-nudged after gateway recovery (%s)", resumed, trigger)
	}
}

// resumeNeedsHuman reports orphaned ticks that exceeded the nudge cap (or
// belong to a disabled project) — they are visible as HIGH events, never
// silently resurrected. Runs alongside resumeOrphans on the same triggers.
func (l *Loop) resumeNeedsHuman(trigger string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := l.db.QueryContext(ctx, `
SELECT t.id, t.project_name, t.nudge_count, COALESCE(t.orphan_reason, ''), p.enabled
FROM ticks t
JOIN projects p ON p.name = t.project_name
WHERE t.orphaned_at IS NOT NULL AND t.status IN ('failed','timeout')
  AND (t.nudge_count >= ? OR p.enabled = 0)
  AND NOT EXISTS (
      SELECT 1 FROM ticks r
      WHERE r.project_name = t.project_name AND r.status IN ('queued','running'))`,
		MaxNudgesPerTick)
	if err != nil {
		log.Printf("RESUME: needs-human scan failed: %v", err)
		return
	}
	defer rows.Close()
	type nh struct {
		id      string
		project string
		count   int
		reason  string
		enabled bool
	}
	var out []nh
	for rows.Next() {
		var n nh
		if err := rows.Scan(&n.id, &n.project, &n.count, &n.reason, &n.enabled); err != nil {
			continue
		}
		out = append(out, n)
	}
	for _, n := range out {
		why := fmt.Sprintf("nudge cap %d/%d reached", n.count, MaxNudgesPerTick)
		if !n.enabled {
			why = "project disabled"
		}
		l.EmitHighEvent("resume", fmt.Sprintf(
			"orphaned tick needs human — project %s tick %s not resumed (%s)",
			n.project, n.id, why),
			map[string]any{
				"project": n.project, "tick_id": n.id, "reason": n.reason,
				"nudge_count": n.count, "why": why, "trigger": trigger,
			})
	}
}

// packedProjectFrom maps a database.Project to a PackedProject for resume
// spawns — the same field set SpawnNow builds, plus the prompt fields the
// normal eval path carries (the continuation preamble rides Prompt with
// PromptMode=replace).
func (l *Loop) packedProjectFrom(p database.Project) PackedProject {
	return PackedProject{
		Name:             p.Name,
		Priority:         float64(p.Priority),
		Weight:           p.Weight,
		Workdir:          p.Workdir,
		RepoURL:          p.RepoURL,
		Command:          p.Command,
		Model:            p.Model,
		Provider:         p.Provider,
		FallbackModel:    p.FallbackModel,
		FallbackProvider: p.FallbackProvider,
		NoGlobalFallback: p.NoGlobalFallback,
		ModelChain:       p.ModelChain,
		IdleModel:        p.IdleModel,
		IdleProvider:     p.IdleProvider,
		WorkerModel:      p.WorkerModel,
		WorkerProvider:   p.WorkerProvider,
		GatewayKey:       p.GatewayKey,
		Deliver:          p.Deliver,
		Prompt:           p.Prompt,
		PromptMode:       p.PromptMode,
	}
}

// resumeOrphansAtStartup runs once from Loop.Run before the event loop
// starts: if the daemon booted with a live gateway (restart AFTER a gateway
// drop), any orphaned ticks from before the restart are re-nudged
// immediately instead of waiting for the first reconnect flip.
func (l *Loop) resumeOrphansAtStartup() {
	l.resumeOrphans("startup")
	l.resumeNeedsHuman("startup")
}
