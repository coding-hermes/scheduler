package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// evaluate runs one evaluation cycle.
// Phase 1 (locked): state update, cleanup, pick projects.
// Phase 2 (lock-free): fire into slot pool, alert escalation.
func (l *Loop) evaluate() {
	l.mu.Lock()

	now := time.Now()
	l.lastEval = now

	if goroCount := runtime.NumGoroutine(); goroCount > 100 {
		log.Printf("WARN: goroutine count = %d (threshold: 100)", goroCount)
	}

	l.events.Emit(context.Background(), SeverityInfo, "loop", "evaluation started", map[string]any{
		"active_ticks": l.lifecycle.RunningCount(),
		"budget":       l.weightBudget,
	})

	// Cleanup stale ticks.
	cleaned, _ := l.lifecycle.CleanupStale(90 * time.Minute)
	if cleaned > 0 {
		log.Printf("EVAL: cleaned up %d stale tick(s)", cleaned)
	}

	// Pick projects.
	var packed []PackedProject
	// SCHED-GAP-066: install the per-cycle budget gate on BOTH selection
	// paths (multi-pool + flat fallback). Spends are precomputed in one
	// query anchored at this eval's `now`; a nil gate (query failure) is
	// fail-open so a broken spend query never halts scheduling. The gate
	// filters NEW spawns only — running ticks are never touched.
	if gate := NewBudgetGate(context.Background(), l.db, now); gate != nil {
		if l.multiPoolPacker != nil {
			l.multiPoolPacker.SetBudgetGate(gate)
		}
		if l.packer != nil {
			l.packer.SetBudgetGate(gate)
		}
	}
	if l.namespaceMode && l.multiPoolPacker != nil {
		ctx := context.Background()
		// Pass ALL namespaces (enabled + disabled). Pack() skips disabled
		// namespaces itself; seeing them lets it distinguish "project points
		// at a disabled namespace" (paused — leave alone) from "project points
		// at a namespace that doesn't exist" (dangling — flat-pack fallback).
		nss, _ := database.ListNamespaces(ctx, l.db, false)
		if len(nss) > 0 {
			projs, _ := database.ListProjects(ctx, l.db, false)
			running, lastComp := l.evalContext(ctx)
			result := l.multiPoolPacker.Pack(projs, nss, l.calculator, lastComp, running, now)
			packed = result.Projects
			tickGroup := now.Format("2006-01-02-15-04-05")
			for _, nt := range result.NamespaceTicks {
				_ = database.InsertNamespaceTick(ctx, l.db, &database.NamespaceTick{
					TickGroup: tickGroup, NamespaceID: nt.NamespaceID,
					Allocated: nt.Allocated, Used: nt.Used,
					Borrowed: nt.Borrowed, Lent: nt.Lent, JobCount: nt.JobCount,
				})
			}
		}
	}
	if len(packed) == 0 {
		var err error
		runningSet := l.spawner.RunningSet()
		if l.slotPool != nil {
			runningSet = l.slotPool.RunningSet()
		}
		// SCHED-GAP-030: after a daemon restart, in-flight gateway ticks
		// (pid=0 rows left 'running' by cleanDanglingOnStartup) are NOT in
		// the in-memory slot pool, so a fresh daemon's first EVAL would
		// double-spawn every project with an in-flight tick (INFRA-012
		// regression, observed 2026-08-11 restart). Merge the DB running
		// set — the slot pool stays authoritative for in-process spawns
		// (no SQLite race), the DB set adds survivors from before restart.
		if dbRunning, _ := l.evalContext(context.Background()); len(dbRunning) > 0 {
			for _, name := range dbRunning {
				runningSet[name] = true
			}
		}
		packed, err = l.packer.Pick(now, runningSet)
		if err != nil {
			log.Printf("EVAL: packer error: %v", err)
			l.mu.Unlock()
			return
		}
	}

	if len(packed) == 0 {
		// GAP-043: a zero-select eval with eligible projects present is an
		// anomaly (evaluations log nothing on empty picks — operator cannot
		// distinguish "evaluating" from "evaluating nothing"). The DB
		// running set is authoritative here (same source as SCHED-GAP-030).
		running, _ := l.evalContext(context.Background())
		runningSet := make(map[string]bool, len(running))
		for _, name := range running {
			runningSet[name] = true
		}
		l.noteZeroSelect(now, runningSet)
		l.mu.Unlock()
		return
	}

	l.resetZeroSelect()

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used",
		len(packed), sumWeights(packed), l.weightBudget)

	// Snapshot before releasing lock.
	noDeliver := l.noDeliver

	l.mu.Unlock()
	// ---- Phase 2: spawn projects (lock-free, concurrent) ----

	// Gateway liveness check: ping before spawning. If gateway is dead,
	// release all slots and skip this cycle. Retry next eval.
	// DOGFOOD-007: simulation mode must not depend on a live gateway —
	// simulated spawns never touch the real spawner.
	if l.gatewayClient != nil && !l.simulate {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := l.gatewayClient.Ping(ctx)
		cancel()
		if err != nil {
			if !l.gatewayDead {
				log.Printf("GATEWAY DEAD — pausing spawns, will retry in 30s: %v", err)
				l.gatewayDead = true
				l.slotPool.ReleaseAll()
			}
			return
		}
		if l.gatewayDead {
			log.Printf("GATEWAY reconnected — resuming spawns")
			l.gatewayDead = false
			// SCHED-GAP-091: gateway health returned — scan for ticks
			// orphaned by the drop and re-nudge them with continuation
			// prompts (the orphan-scan job rides this existing flip; no
			// new cron, scheduler-owned timing per fleet doctrine).
			l.resumeOrphans("reconnect")
			l.resumeNeedsHuman("reconnect")
		}
	}

	// Fire each project into the slot pool. The pool's semaphore limits
	// concurrency — projects acquire a slot, spawn via gateway in their
	// own goroutine, and release the slot on completion/timeout.
	// evaluate() returns immediately; the pool runs autonomously.
	//
	// Dedup: skip projects already occupying a slot to prevent
	// the timeout→re-spawn→duplicate processes problem.
	alreadyRunning := l.slotPool.RunningSet()
	for _, proj := range packed {
		if alreadyRunning[proj.Name] {
			log.Printf("DEDUP: skipping %s — already running", proj.Name)
			continue
		}
		if l.simulate {
			// DOGFOOD-007: --simulate daemon mode must simulate, never
			// spawn real foremen. The sim spawner inserts a tick row and
			// completes it in 50-250ms; unique IDs come from simTickID.
			tickID := l.simTickID(proj.Name, now)
			if _, err := l.simSpawner.Spawn(proj, tickID); err != nil {
				log.Printf("SIM: spawn %s failed: %v", proj.Name, err)
			}
			continue
		}
		l.slotPool.Spawn(proj, now, noDeliver, l.db)
	}

	// Alert escalation runs while pool processes ticks.
	if len(packed) > 0 {
		l.mu.RLock()
		policy := l.autoDisablePolicy
		l.mu.RUnlock()
		escalator := NewAlertEscalator(l.db, l.events, policy)
		if err := escalator.RunAll(context.Background(), now); err != nil {
			log.Printf("EVAL: escalation check error: %v", err)
		}
	}
}
func (l *Loop) evalContext(ctx context.Context) ([]string, map[string]time.Time) {
	running := make([]string, 0)
	rrows, err := l.db.QueryContext(ctx, `SELECT DISTINCT project_name FROM ticks WHERE status = 'running'`)
	if err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var name string
			if err := rrows.Scan(&name); err == nil {
				running = append(running, name)
			}
		}
	}

	lastCompleted := make(map[string]time.Time)
	crows, err := l.db.QueryContext(ctx,
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status != 'running' GROUP BY project_name`)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var name string
			var ts string
			if err := crows.Scan(&name, &ts); err == nil {
				if t, err2 := time.Parse(time.RFC3339, ts); err2 == nil {
					lastCompleted[name] = t
				}
			}
		}
	}
	return running, lastCompleted
}

func sumWeights(packed []PackedProject) int {
	total := 0
	for _, p := range packed {
		total += p.Weight
	}
	return total
}

// gatewayZombieMaxAge is the maximum age of a pid=0 (gateway) tick's
// heartbeat before the row is treated as an orphaned zombie (S-GAP-003).
// The heartbeat goroutine in spawn.go refreshes heartbeat_at every 5 min, so
// 15 min tolerates two missed beats while still reaping ~6x faster than the
// 90-min CleanupStale backstop.
const gatewayZombieMaxAge = 15 * time.Minute

// staleGatewayTicksSQL selects running gateway-spawn ticks (pid=0) whose
// heartbeat has gone stale — or was never written (pre-S-GAP-003 rows) while
// spawned_at is itself older than the threshold. julianday() parses RFC3339
// with varying offsets; a raw string comparison would be wrong. (CleanupStale's
// raw compare is pre-existing and unchanged.)
var staleGatewayTicksSQL = fmt.Sprintf(`
SELECT id, project_name FROM ticks
WHERE status='running' AND pid = 0 AND (
    (heartbeat_at IS NOT NULL AND julianday(heartbeat_at) < julianday('now', '-%d minutes'))
 OR (heartbeat_at IS NULL     AND julianday(spawned_at)  < julianday('now', '-%d minutes'))
)`, int(gatewayZombieMaxAge/time.Minute), int(gatewayZombieMaxAge/time.Minute))

// staleGatewayTick identifies one orphaned running gateway tick.
type staleGatewayTick struct {
	id      string
	project string
}

// staleGatewayTicks returns running pid=0 ticks whose liveness signal is
// older than gatewayZombieMaxAge. Rows are consumed and closed inside the
// helper so callers can issue UPDATEs immediately (SQLite single-writer —
// an UPDATE while a SELECT still holds the pool's only connection deadlocks).
func (l *Loop) staleGatewayTicks(ctx context.Context) []staleGatewayTick {
	rows, err := l.db.QueryContext(ctx, staleGatewayTicksSQL)
	if err != nil {
		log.Printf("ZOMBIE: stale-gateway query failed: %v", err)
		return nil
	}
	defer rows.Close()
	var out []staleGatewayTick
	for rows.Next() {
		var t staleGatewayTick
		if err := rows.Scan(&t.id, &t.project); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}

// timeoutReapSQL marks a running tick as timed out. GAP-045: completed_at is
// stamped exactly like the failed/completed paths (lifecycle.Complete,
// CleanupStale) so a timeout row is terminal for duration / failure-window /
// p99-latency math instead of reading as in-flight forever. outcome stays
// unset — the CHECK constraint only allows ('committed','dry_run','failed',
// 'timeout'); 'zombie_reaped' violates it (see cleanDanglingOnStartup).
const timeoutReapSQL = `UPDATE ticks SET status='timeout', completed_at=? WHERE id=?`

// cleanDanglingOnStartup reaps ticks whose recorded pid no longer exists
// (exec-fallback children die with the daemon), plus pid=0 gateway rows whose
// heartbeat is stale (S-GAP-003): the heartbeat goroutine dies with the
// daemon, so a heartbeat older than gatewayZombieMaxAge means the session's
// owner is gone. LIVE gateway ticks — fresh heartbeat, or NULL heartbeat with
// a fresh spawned_at (pre-S-GAP-003 rows inside the grace window) — are left
// 'running': their HTTP sessions SURVIVE a daemon restart.
// Regression: INFRA-012 (2026-08-01) — restart marked live gateway ticks
// 'timeout' and the packer spawned duplicate ticks for in-flight projects.
func (l *Loop) cleanDanglingOnStartup() {
	ctx := context.Background()

	// Ticks with a real pid are checked against /proc. pid=0 rows are gateway
	// spawns checked by heartbeat staleness below — never against /proc.
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, project_name, pid FROM ticks WHERE status='running' AND pid > 0`)
	if err != nil {
		log.Printf("DANGLING: startup cleanup query failed: %v", err)
		return
	}

	type deadTick struct {
		id      string
		project string
		pid     int
	}
	var dead []deadTick
	for rows.Next() {
		var id, project string
		var pid int
		if err := rows.Scan(&id, &project, &pid); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			dead = append(dead, deadTick{id: id, project: project, pid: pid})
		}
	}
	rows.Close()

	// S-GAP-003: orphaned gateway ticks (stale heartbeat) are reaped exactly
	// like dead-pid rows. Rows younger than 15 min are NOT selected, so a
	// restart never marks live gateway ticks timeout and spawns duplicates
	// (INFRA-012 regression guard).
	for _, gt := range l.staleGatewayTicks(ctx) {
		dead = append(dead, deadTick{id: gt.id, project: gt.project, pid: 0})
	}

	// SCHED-GAP-091: remember which reaped rows were orphaned by the
	// daemon's own absence (crash / restart mid-tick) so the
	// gateway-health-return scan can re-nudge them.
	reapedOrphans := make(map[string]string, len(dead))

	if len(dead) == 0 {
		log.Printf("DANGLING: startup cleanup — no dead-pid or stale-gateway ticks found (live gateway ticks left running)")
		return
	}

	// Bump last_tick_completed ONLY for projects whose ticks were actually
	// reaped, so the packer uses actual last-tick time for urgency. Projects
	// with live pid=0 running ticks are untouched.
	projects := make(map[string]struct{}, len(dead))
	for _, dt := range dead {
		projects[dt.project] = struct{}{}
	}
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	placeholders := make([]string, len(names))
	args := make([]any, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		args[i] = name
	}
	if _, err := l.db.ExecContext(ctx,
		`UPDATE projects SET last_tick_completed = strftime('%Y-%m-%dT%H:%M:%S', 'now')
		 WHERE name IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		log.Printf("DANGLING: last_tick_completed update failed: %v", err)
	}

	var cleaned int
	for _, dt := range dead {
		// outcome stays unset — the CHECK constraint only allows
		// ('committed','dry_run','failed','timeout'); 'zombie_reaped'
		// violates it. BOTH cleanup paths (cleanDanglingOnStartup and
		// reapZombies) must be outcome-free for the same reason — an
		// UPDATE that sets outcome='zombie_reaped' is rejected by SQLite
		// and the tick silently stays 'running' forever. completed_at IS
		// stamped (GAP-045) so reaped rows are terminal for duration math.
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, time.Now().Format(time.RFC3339), dt.id); err != nil {
			log.Printf("DANGLING: reaping tick %s (pid=%d): %v", dt.id, dt.pid, err)
			continue
		}
		cleaned++
		// SCHED-GAP-091: reap succeeded — the pid/heartbeat died while the
		// daemon was absent (crash / restart), so this is a drop. Stamp it
		// so the resume scan re-nudges the tick.
		reapedOrphans[dt.id] = OrphanReasonStartupReap
	}
	if cleaned > 0 {
		log.Printf("DANGLING: cleaned %d dead running tick(s) from previous process (dead pid or stale gateway heartbeat)", cleaned)
	}
	// SCHED-GAP-091: stamp the reaped rows as orphans so the resume scan
	// (startup, when the gateway is back) re-nudges them.
	for id, reason := range reapedOrphans {
		l.stampOrphaned(id, reason)
	}
}

func (l *Loop) reapZombies() {
	ctx := context.Background()
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, pid FROM ticks WHERE status='running' AND pid > 0`)
	if err != nil {
		log.Printf("ZOMBIE: reaper query failed: %v", err)
		return
	}

	// Collect dead tick IDs first and close rows BEFORE issuing UPDATEs —
	// SQLite allows a single writer, and an UPDATE issued while this SELECT
	// still holds the pool's only connection blocks forever (pool deadlock).
	var dead []string
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			dead = append(dead, id)
		}
	}
	rows.Close()

	// SCHED-GAP-091: rows successfully reaped below get the zombie-reap
	// orphan stamp so a gateway drop that outlived a live daemon is also
	// resumable.
	reapedOrphans := make(map[string]string)

	var reaped int
	for _, id := range dead {
		// outcome stays unset — see the CHECK-constraint comment in
		// cleanDanglingOnStartup above; setting outcome here makes
		// SQLite reject the UPDATE and the zombie is never reaped.
		// completed_at IS stamped (GAP-045) — see timeoutReapSQL.
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, time.Now().Format(time.RFC3339), id); err != nil {
			log.Printf("ZOMBIE: reaping tick %s: %v", id, err)
			continue
		}
		reaped++
		reapedOrphans[id] = OrphanReasonZombieReap
	}
	if reaped > 0 {
		log.Printf("ZOMBIE: reaped %d ticks (process died)", reaped)
	}

	// S-GAP-003: gateway ticks (pid=0) have no /proc entry — their liveness
	// signal is the spawn-loop heartbeat. A heartbeat older than
	// gatewayZombieMaxAge (or never written, with an equally old spawned_at)
	// means the daemon that owned the session is gone; reap exactly like
	// dead-pid ticks (status='timeout', outcome unset — CHECK constraint).
	var gwReaped int
	for _, gt := range l.staleGatewayTicks(ctx) {
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, time.Now().Format(time.RFC3339), gt.id); err != nil {
			log.Printf("ZOMBIE: reaping gateway tick %s: %v", gt.id, err)
			continue
		}
		gwReaped++
		reapedOrphans[gt.id] = OrphanReasonZombieReap
	}
	if gwReaped > 0 {
		log.Printf("ZOMBIE: reaped %d gateway tick(s) (stale heartbeat)", gwReaped)
	}
	// SCHED-GAP-091: stamp all zombie-reaped rows (dead-pid + stale
	// heartbeat) as orphans — the resume scan re-nudges them on the next
	// gateway-health-return.
	for id, reason := range reapedOrphans {
		l.stampOrphaned(id, reason)
	}
}
