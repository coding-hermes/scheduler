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

	// GAP-101 (2026-09-09): single choke-point gate. ForceEvaluate fires
	// evaluate() in a raw goroutine — with the old channel-only protocol
	// a paused loop still spawned (tick count kept growing while paused,
	// caught by the GAP-101 regression test). The atomic flag makes the
	// paused state authoritative regardless of entry path.
	if l.paused.Load() {
		l.mu.Unlock()
		log.Println("EVAL: skipped — loop paused")
		return
	}

	// ADV-R04 (G6): the decision instant comes from the loop's clock seam
	// (internal/clock; the wall clock by default) — the only clock read in the evaluate
	// path. The single `now` value flows to every consumer below (budget
	// gate, packers, zero-select note, sim tick IDs, slot-pool spawns,
	// escalator); no second read of the seam happens inside evaluate().
	now := l.nowLocked()
	l.lastEval = now

	// ADV-R13: persist exactly ONE host load/memory sample per evaluation
	// pass, immediately at entry — after the paused-loop gate (a paused
	// loop runs no evaluation pass, so it records nothing) and BEFORE all
	// selection logic, so every early-return below (packer error,
	// zero-select) still leaves its measurement. Placement is deliberate:
	// this is measurement only — recordHostSample must never gate, delay
	// or alter any admission/spawn/packing decision, and a sampler or
	// persist failure is swallowed into the log there, making evaluation
	// behavior byte-identical with and without a working sampler. It is
	// once per PASS, not per project or per spawned tick — off every hot
	// path.
	l.recordHostSample(now)

	if goroCount := runtime.NumGoroutine(); goroCount > 100 {
		log.Printf("WARN: goroutine count = %d (threshold: 100)", goroCount)
	}

	l.events.Emit(context.Background(), SeverityInfo, "loop", "evaluation started", map[string]any{
		"active_ticks": l.lifecycle.RunningCount(),
		"budget":       l.weightBudget,
	})

	// Cleanup stale ticks. SCHED-GAP-186: CleanupStaleProjects reports WHICH
	// projects it flipped terminal so the SlotPool claims can be reconciled —
	// without this, a project whose stale row was reaped here kept its
	// in-process running/reserved claim and the DEDUP view below skipped it
	// forever (hermes-dagger, 2026-09-19). This is also the per-eval
	// self-heal backstop for any wedge a missed reaper leaves behind.
	//
	// SCHED-GAP-217: the cutoff is now derived from the LIVE max effective
	// tick deadline across the in-flight running ticks (env > ns > flag
	// cascade, ceiling 4h), plus a 30m grace, with a 90m hard floor. The
	// prior hardcoded 90m killed legitimate 3h-wave ticks at 1.5h
	// (phantom "stale - timeout at 1h30m0s" rows that fed the failure-rate
	// counters). See wave_timeout.go:backstopMaxAge for the full derivation.
	if staleProjects, cleaned, _ := l.lifecycle.CleanupStaleProjects(l.backstopMaxAge()); cleaned > 0 {
		log.Printf("EVAL: cleaned up %d stale tick(s)", cleaned)
		if l.slotPool != nil {
			l.slotPool.releaseReaped(staleProjects)
		}
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
	// SCHED-GAP-113: arm the per-cycle wave-shed scan (S12 §6.2 admission
	// layer). The scan itself is inert unless a namespace sets
	// wave_workers_cap > 0 (resolveWaveShed returns nil before any query).
	if l.multiPoolPacker != nil {
		l.multiPoolPacker.SetWaveShedDB(l.db)
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

	// SCHED-GAP-155: emit one grep-stable ADMIT line per candidate project
	// for THIS pass — the selection is now final (both packer paths have
	// run) and nothing has been spawned yet, so these lines are exactly the
	// decision the pass acts on. Additive: EVAL / EVAL-STALL /
	// EVAL-ZERO-SELECT are untouched (operators grep ^EVAL). Must stay
	// before the zero-select early return below, or a pass that picked
	// nothing — the case operators most need explained — would log nothing.
	l.emitAdmissionPass(now, packed)

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

	// SCHED-GAP-170: gateway health is a PER-PROJECT admission decision now,
	// taken at the spawn loop below (after the dedup skip and the load-gate
	// check) against a cached 30s verdict — never a probe per packed project.
	//
	// The liveness block that used to sit here read `l.gatewayClient`, a field
	// NOTHING in the package assigns (`Loop.SetGatewayClient` forwarded the
	// client to the spawner only), so its guard was permanently false and the
	// fleet packed and spawned straight into a dead gateway: live scheduler.db,
	// 7 days to 2026-09-18, 793 of 859 failures were
	// "gateway unreachable and exec fallback disabled" (763 gateway-drain 503s,
	// 18 connection-refused, 12 probe/POST deadlines) — every one booked as a
	// lane fault. Its two real effects are preserved, moved to where the
	// verdict now lives:
	//   - the DEAD transition (gatewayDead latch + SlotPool.ReleaseAll) rides
	//     the first deferral of an outage episode — noteGatewayDeadOnce;
	//   - the RECONNECT transition (gatewayDead clear + SCHED-GAP-091 orphan
	//     re-nudge) rides the first project the gate admits again —
	//     noteGatewayReconnectedOnce — so it still runs BEFORE this pass's
	//     spawns, exactly as before.
	// gatewayDeferred counts this pass's deferrals: a pass that deferred
	// anything is an outage pass and (as before) does not run the escalator.
	gatewayDeferred := 0

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
			// SCHED-GAP-171: the load gate is NOT applied to the sim path —
			// it is an admission gate for REAL spawns (its whole purpose is
			// not to overload the host), and a simulated tick spawns no
			// process. Simulate behaviour is therefore byte-identical under
			// load, which is what DOGFOOD-007 requires.
			tickID := l.simTickID(proj.Name, now)
			if _, err := l.simSpawner.Spawn(proj, tickID); err != nil {
				log.Printf("SIM: spawn %s failed: %v", proj.Name, err)
			}
			continue
		}
		// SCHED-GAP-171: consult the load gate BEFORE the spawn — and
		// therefore before ANY row can be created. SlotPool.Spawn used to
		// return early on this predicate AFTER the caller had handed it a
		// tick id, and for every caller that enqueues first (Loop.SpawnNow,
		// resumeOrphans) that left the row stranded in status='queued' with
		// nothing in the daemon to dispatch it (SCHED-GAP-145 class: 5 rows
		// sat queued >1h on 2026-09-18 while the load was 14-20). Here the
		// deferral costs nothing at all: `continue` skips the spawn, the
		// project keeps its selection, and the next evaluation re-picks it
		// once load drops. DEFER, not drop — no cooldown consumed, no
		// failure recorded, no slot taken, no nudge budget spent, no row.
		if LoadGateShouldDefer(l.db, proj.NamespaceID) {
			l.emitLoadGateDeferred(proj.Name, proj.NamespaceID, "")
			continue
		}
		// SCHED-GAP-170: the gateway-health gate — the same defer-not-drop
		// contract as the load gate above, for the gateway dependency. A spawn
		// into an unreachable gateway is a spawn that WILL fail (793 of the
		// fleet's 859 failures in the 7 days to 2026-09-18), so it is not made:
		// the project keeps its selection, and `continue` costs no row, no
		// reservation, no slot, no cooldown, no failure and no nudge. The
		// verdict is cached for gatewayHealthTTL, so this consults one clock
		// read per project and at most ONE probe per pass (not per project).
		//
		// Guarded like the pre-existing liveness block: only a loop that owns a
		// gateway client can probe, and simulation never touches the gateway
		// (DOGFOOD-007). Fail-open on an absent client is the gate's own
		// contract (gateway_health_gate.go).
		if l.gatewayClientOrNil() != nil && !l.simulate {
			if deferSpawn, reason, probeErr := GatewayHealthGateShouldDefer(); deferSpawn {
				l.noteGatewayDeadOnce(reason)
				l.emitGatewayDeferred(proj.Name, proj.NamespaceID, "", reason, probeErr)
				gatewayDeferred++
				continue
			}
			l.noteGatewayReconnectedOnce()
		}
		l.slotPool.Spawn(proj, now, noDeliver, l.db)
	}

	// Alert escalation runs while pool processes ticks. SCHED-GAP-170: a pass
	// that deferred every packed project to an unreachable gateway spawned
	// nothing, so it must not run the escalator either — the pre-existing
	// liveness block returned before this code for that reason, and an outage
	// must never feed the failure-rate / auto-disable machinery.
	if len(packed) > 0 && gatewayDeferred == 0 {
		l.mu.RLock()
		policy := l.autoDisablePolicy
		l.mu.RUnlock()
		escalator := NewAlertEscalator(l.db, l.events, policy)
		// SCHED-GAP-169: the escalator reads/writes timestamps and throttle
		// windows, so it follows the loop's clock like every other component.
		escalator.SetClock(l.clock())
		if err := escalator.RunAll(context.Background(), now); err != nil {
			log.Printf("EVAL: escalation check error: %v", err)
		}
	}
}

// emitLoadGateDeferred records a load-gate deferral (SCHED-GAP-125 predicate,
// SCHED-GAP-171 placement): the grep-stable LOAD-GATE line operators already
// watch, plus the INFO event the SLOT POOL used to emit for the same decision.
// Keeping both in one place is what lets the deferral move out of
// SlotPool.spawn without losing its observability.
//
// tickID is "" when the deferral happens BEFORE any row exists — the
// evaluation path, where nothing was enqueued, so there is nothing to
// correlate. The API path (Loop.SpawnNow) passes the stored row's id: the row
// stays `queued` there (its id must resolve), so the id in the event is how a
// caller tells "deferred" from "spawned" without polling the tick.
//
// The event is machine-detectable on purpose: `reason=load_gate_deferred`
// (plus `deferred=true`) so an operator or a test can query deferrals on their
// own instead of pattern-matching a log line. The payload otherwise matches
// the pre-171 slot-pool event (project, tick_id when known, load_1m,
// threshold) and adds the namespace the gate was consulted for — without it a
// deferral cannot be attributed to the `load_gate='off'` opt-out decision.
func (l *Loop) emitLoadGateDeferred(project, nsID, tickID string) {
	l1, _ := currentLoad1m()
	threshold := loadGateThreshold()
	if tickID == "" {
		log.Printf("LOAD-GATE: deferring %s — load %.2f >= threshold %.2f (no row enqueued; re-picked when load drops)",
			project, l1, threshold)
	} else {
		log.Printf("LOAD-GATE: deferring %s (tick %s) — load %.2f >= threshold %.2f (row stays queued, not started)",
			project, tickID, l1, threshold)
	}
	if l.events == nil {
		return
	}
	details := map[string]any{
		"project":   project,
		"namespace": nsID,
		"load_1m":   l1,
		"threshold": threshold,
		"reason":    "load_gate_deferred",
		"deferred":  true,
	}
	if tickID != "" {
		details["tick_id"] = tickID
	}
	l.events.Emit(context.Background(), SeverityInfo, "load_gate",
		"load gate deferred "+project, details)
}

// emitGatewayDeferred records a gateway-health deferral (SCHED-GAP-170): the
// grep-stable GATEWAY-DEFER line plus one INFO `loop` event per deferral with a
// stable `event_type=gateway_defer` marker, so a deferred spawn is queryable
// after the fact instead of being inferable only from the absence of a tick row.
//
// The event is what makes the gate auditable in both directions:
//
//   - `event_type=gateway_defer` is the machine marker (queryable;
//     `json_extract(details,'$.event_type')`);
//   - `reason` carries the CACHED PROBE ERROR VERBATIM (the ticket's "do not
//     swallow the real error"): "connection refused", "context deadline
//     exceeded", "gateway health: HTTP 503 …" — the operator sees the actual
//     failure, not a generic "unreachable";
//   - `project` + `namespace` attribute the deferral (a namespace opt-out or a
//     single lane can then be ruled in/out), and `deferred=true` mirrors the
//     load-gate payload so both deferral kinds can be counted by one query.
//
// tickID is "" when the deferral precedes any row (the evaluation path, where
// nothing was enqueued). Loop.SpawnNow passes the stored row's id — the row
// stays `queued` there because the API contract requires the returned id to
// resolve, so the id is how a caller tells "deferred" from "spawned".
func (l *Loop) emitGatewayDeferred(project, nsID, tickID, reason string, probeErr error) {
	if reason == "" && probeErr != nil {
		reason = probeErr.Error()
	}
	if tickID == "" {
		log.Printf("GATEWAY-DEFER: deferring %s — %s (no row enqueued; re-picked when the gateway answers)",
			project, reason)
	} else {
		log.Printf("GATEWAY-DEFER: deferring %s (tick %s) — %s (row stays queued, not started)",
			project, tickID, reason)
	}
	if l.events == nil {
		return
	}
	details := map[string]any{
		"project":    project,
		"namespace":  nsID,
		"event_type": "gateway_defer",
		"reason":     reason,
		"deferred":   true,
	}
	if tickID != "" {
		details["tick_id"] = tickID
	}
	l.events.Emit(context.Background(), SeverityInfo, "loop",
		"gateway deferral for "+project, details)
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
	reapedProjects := make([]string, 0, len(dead))
	for _, dt := range dead {
		// outcome stays unset — the CHECK constraint only allows
		// ('committed','dry_run','failed','timeout'); 'zombie_reaped'
		// violates it. BOTH cleanup paths (cleanDanglingOnStartup and
		// reapZombies) must be outcome-free for the same reason — an
		// UPDATE that sets outcome='zombie_reaped' is rejected by SQLite
		// and the tick silently stays 'running' forever. completed_at IS
		// stamped (GAP-045) so reaped rows are terminal for duration math.
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, l.clock().Now().Format(time.RFC3339), dt.id); err != nil {
			log.Printf("DANGLING: reaping tick %s (pid=%d): %v", dt.id, dt.pid, err)
			continue
		}
		cleaned++
		reapedProjects = append(reapedProjects, dt.project)
		// SCHED-GAP-091: reap succeeded — the pid/heartbeat died while the
		// daemon was absent (crash / restart), so this is a drop. Stamp it
		// so the resume scan re-nudges the tick.
		reapedOrphans[dt.id] = OrphanReasonStartupReap
		// SCHED-GAP-114 (S12 §8.2 item 3): a reaped wave tick's worker rows
		// flip to 'abandoned' (§10.3). Shared step, unchanged tick semantics.
		l.reapWaveAbandoned(ctx, dt.id)
	}
	if cleaned > 0 {
		log.Printf("DANGLING: cleaned %d dead running tick(s) from previous process (dead pid or stale gateway heartbeat)", cleaned)
		// SCHED-GAP-186: on a clean boot the fresh SlotPool starts empty, so
		// this reconcile is normally a no-op — but a same-process restart
		// path reusing this Loop (and any future caller that re-enters with
		// a warm pool) must not keep claims for rows just flipped terminal.
		l.slotPool.releaseReaped(reapedProjects)
	}
	// SCHED-GAP-091: stamp the reaped rows as orphans so the resume scan
	// (startup, when the gateway is back) re-nudges them.
	for id, reason := range reapedOrphans {
		l.stampOrphaned(id, reason)
	}
}

// queuedReapAgeMultiplier is how many tick-timeouts a 'queued' row may age
// before startup treats it as a leftover of a dead process (SCHED-GAP-145).
// A queued row is created by the evaluation/nudge path and dispatched by the
// SAME process that created it (SlotPool.spawn transitions queued→running as
// its first step), so nothing legitimate survives a full tick budget; 2x is
// deliberately generous — a live-but-slow process keeps its own rows.
const queuedReapAgeMultiplier = 2

// staleQueuedRowsSQL selects 'queued' ticks older than the cutoff bound as its
// only argument (an RFC3339 timestamp). julianday() parses RFC3339 with
// varying offsets, so a raw string comparison would be wrong (same reasoning
// as staleGatewayTicksSQL). COALESCE covers the nullable spawned_at: both
// columns are stamped by every enqueue path and created_at is NOT NULL by
// schema, so the comparison always has a usable clock and can never fail open
// on a legacy row shape.
const staleQueuedRowsSQL = `
SELECT id, project_name FROM ticks
WHERE status = 'queued'
  AND julianday(COALESCE(spawned_at, created_at)) < julianday(?)`

// reapStaleQueuedRows marks 'queued' ticks left over from a previous process
// as terminal (SCHED-GAP-145). It is the queued-row twin of
// cleanDanglingOnStartup: that one reaps 'running' rows whose owner is gone,
// this one reaps rows that were enqueued and then never dispatched.
//
// Measured defect (2026-09-17): the fleet DB held 7 rows with status='queued',
// session_id NULL and spawned_at from 2026-09-16T20:17 / 2026-09-17T01:10
// (gitreins-poc, warpfs, off-by-one, 9router, coding-hermes-scheduler,
// duckbrain, heading) — enqueued by a process that then exited without ever
// starting them (restart, load-gate deferral, or a slot/namespace-patience
// drop: every one of those paths returns from SlotPool.spawn leaving the row
// queued with nobody owning it). Nothing reclaims them: the in-flight dedup
// the evaluation loop and the manual spawn endpoint share
// (`status IN ('queued','running')`, loop.go) refuses to re-spawn the
// project, the orphan re-nudge scan excludes it (session_resume.go), the
// namespace in-flight admission count includes it (session_resume.go), and
// /api/v1/metrics reports it as `queued` — so the projects were unschedulable
// indefinitely and the queue read as live work.
//
// Threshold: queuedReapAgeMultiplier x tick_timeout (the Loop field wired by
// NewLoop/SetTickTimeout, falling back to the spawner's configured timeout).
// Older than that and the originating process is provably gone, or the spawn
// loop never ran — no config knob, by design.
//
// Like cleanDanglingOnStartup the reap is outcome-free (timeoutReapSQL): the
// outcome CHECK constraint only allows ('committed','dry_run','failed',
// 'timeout'), so stamping a 'queued_reaped'-style value would be rejected and
// silently leave the row queued. completed_at IS stamped (GAP-045) so the row
// is terminal for duration / failure-window math. projects.last_tick_completed
// is deliberately NOT bumped (unlike the dead-pid reap): this row never ran a
// tick, so faking a completion would delay the project's next spawn by a whole
// cooldown. Returns the number of rows reaped.
func (l *Loop) reapStaleQueuedRows() int {
	ctx := context.Background()
	tt := l.reapTickTimeout()
	if tt <= 0 {
		log.Printf("QUEUED: startup reap skipped — no usable tick timeout; queued rows left untouched")
		return 0
	}
	cutoff := l.clock().Now().Add(-queuedReapAgeMultiplier * tt)

	// Rows are consumed and CLOSED before any UPDATE: SQLite allows a single
	// writer, so an UPDATE issued while this SELECT still holds the pool's
	// only connection blocks forever (same contract as staleGatewayTicks).
	rows, err := l.db.QueryContext(ctx, staleQueuedRowsSQL, cutoff.Format(time.RFC3339))
	if err != nil {
		log.Printf("QUEUED: startup reap query failed: %v", err)
		return 0
	}
	type queuedTick struct{ id, project string }
	var stale []queuedTick
	for rows.Next() {
		var q queuedTick
		if err := rows.Scan(&q.id, &q.project); err != nil {
			continue
		}
		stale = append(stale, q)
	}
	rows.Close()

	if len(stale) == 0 {
		log.Printf("QUEUED: startup reap — no stale queued rows (threshold %v = %dx tick timeout %v)",
			queuedReapAgeMultiplier*tt, queuedReapAgeMultiplier, tt)
		return 0
	}

	var reaped int
	for _, q := range stale {
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, l.clock().Now().Format(time.RFC3339), q.id); err != nil {
			log.Printf("QUEUED: reaping tick %s (project %s): %v", q.id, q.project, err)
			continue
		}
		reaped++
		// SCHED-GAP-114 (S12 §8.2 item 3): the shared wave step, exactly as
		// cleanDanglingOnStartup runs it right after timeoutReapSQL. A
		// never-dispatched row has no worker rows, so this is normally a
		// no-op — it stays here so both reap paths share one contract.
		l.reapWaveAbandoned(ctx, q.id)
	}
	if reaped > 0 {
		log.Printf("QUEUED: reaped %d stale queued tick(s) never dispatched by the previous process (older than %v = %dx tick timeout %v)",
			reaped, queuedReapAgeMultiplier*tt, queuedReapAgeMultiplier, tt)
	}
	return reaped
}

// reapTickTimeout resolves the tick-timeout the queued-row reaper derives its
// age window from: the Loop field wired by NewLoop and SetTickTimeout, falling
// back to the spawner's own configured timeout (NewSpawner's default when the
// daemon never called SetTickTimeout). Zero means no usable window — the
// caller skips the reap rather than reaping everything.
func (l *Loop) reapTickTimeout() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	tt := l.tickTimeout
	if tt <= 0 && l.spawner != nil {
		tt = l.spawner.timeout
	}
	return tt
}

func (l *Loop) reapZombies() {
	ctx := context.Background()
	// SCHED-GAP-186: project_name rides along so a successful reap can
	// reconcile the SlotPool claim for that project (the DB row going
	// terminal here does not, by itself, free the in-process slot).
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, pid, project_name FROM ticks WHERE status='running' AND pid > 0`)
	if err != nil {
		log.Printf("ZOMBIE: reaper query failed: %v", err)
		return
	}

	// Collect dead tick IDs first and close rows BEFORE issuing UPDATEs —
	// SQLite allows a single writer, and an UPDATE issued while this SELECT
	// still holds the pool's only connection blocks forever (pool deadlock).
	type zombieTick struct {
		id      string
		project string
	}
	var dead []zombieTick
	for rows.Next() {
		var id, project string
		var pid int
		if err := rows.Scan(&id, &pid, &project); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			dead = append(dead, zombieTick{id: id, project: project})
		}
	}
	rows.Close()

	// SCHED-GAP-091: rows successfully reaped below get the zombie-reap
	// orphan stamp so a gateway drop that outlived a live daemon is also
	// resumable.
	reapedOrphans := make(map[string]string)

	var reaped int
	reapedProjects := make([]string, 0)
	for _, zt := range dead {
		id := zt.id
		// outcome stays unset — see the CHECK-constraint comment in
		// cleanDanglingOnStartup above; setting outcome here makes
		// SQLite reject the UPDATE and the zombie is never reaped.
		// completed_at IS stamped (GAP-045) — see timeoutReapSQL.
		if _, err := l.db.ExecContext(ctx,
			timeoutReapSQL, l.clock().Now().Format(time.RFC3339), id); err != nil {
			log.Printf("ZOMBIE: reaping tick %s: %v", id, err)
			continue
		}
		reaped++
		reapedProjects = append(reapedProjects, zt.project)
		reapedOrphans[id] = OrphanReasonZombieReap
		// SCHED-GAP-114 (S12 §8.2 item 3): same shared wave step as the
		// startup path — reaped wave tick → its worker rows go 'abandoned'.
		l.reapWaveAbandoned(ctx, id)
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
			timeoutReapSQL, l.clock().Now().Format(time.RFC3339), gt.id); err != nil {
			log.Printf("ZOMBIE: reaping gateway tick %s: %v", gt.id, err)
			continue
		}
		gwReaped++
		reapedProjects = append(reapedProjects, gt.project)
		reapedOrphans[gt.id] = OrphanReasonZombieReap
		// SCHED-GAP-114: gateway-drop wave ticks abandon their workers too.
		l.reapWaveAbandoned(ctx, gt.id)
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
	// SCHED-GAP-186: the DB rows are terminal, so any SlotPool claim the
	// spawn goroutine still holds for these projects (its session hung in
	// Wait() while the pid/heartbeat died — the 2026-09-19 hermes-dagger
	// wedge) must be dropped, or RunningSet() keeps reporting the project
	// as "already running" and the evaluation DEDUP skips it until a
	// daemon restart. releaseReaped is a no-op for claims already freed by
	// the goroutine's own deferred Release.
	l.slotPool.releaseReaped(reapedProjects)
}
