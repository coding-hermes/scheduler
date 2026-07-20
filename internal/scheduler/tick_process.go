package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
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
	if l.namespaceMode && l.multiPoolPacker != nil {
		ctx := context.Background()
		nss, _ := database.ListNamespaces(ctx, l.db, true)
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
		packed, err = l.packer.Pick(now, runningSet)
		if err != nil {
			log.Printf("EVAL: packer error: %v", err)
			l.mu.Unlock()
			return
		}
	}

	if len(packed) == 0 {
		l.mu.Unlock()
		return
	}

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used",
		len(packed), sumWeights(packed), l.weightBudget)

	// Snapshot before releasing lock.
	noDeliver := l.noDeliver

	l.mu.Unlock()
	// ---- Phase 2: spawn projects (lock-free, concurrent) ----

	// Lazy-init the slot pool if not already created (test_verify, tests).
	if l.slotPool == nil {
		l.slotPool = NewSlotPool(l.maxConcur, 2*time.Hour, l.spawner, l.lifecycle)
	}

	// Gateway liveness check: ping before spawning. If gateway is dead,
	// release all slots and skip this cycle. Retry next eval.
	if l.gatewayClient != nil {
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
		l.slotPool.Spawn(proj, now, noDeliver, l.db)
	}

	// Alert escalation runs while pool processes ticks.
	if len(packed) > 0 {
		escalator := NewAlertEscalator(l.db, l.events)
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
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status = 'completed' GROUP BY project_name`)
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

func (l *Loop) cleanDanglingOnStartup() {
	ctx := context.Background()

	// Update last_tick_completed for projects whose running ticks
	// are being cleaned, so the packer uses actual last-tick time
	// rather than created_at for urgency calculation.
	if _, err := l.db.ExecContext(ctx,
		`UPDATE projects SET last_tick_completed = strftime('%Y-%m-%dT%H:%M:%S', 'now')
 	 WHERE name IN (SELECT DISTINCT project_name FROM ticks WHERE status='running')`); err != nil {
		log.Printf("DANGLING: last_tick_completed update failed: %v", err)
	}

	result, err := l.db.ExecContext(ctx,
		`UPDATE ticks SET status='timeout' WHERE status='running'`)
	if err != nil {
		log.Printf("DANGLING: startup cleanup failed: %v", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		log.Printf("DANGLING: cleaned %d running ticks from previous process", n)
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
	defer rows.Close()

	var reaped int
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			if _, err := l.db.ExecContext(ctx,
				`UPDATE ticks SET status='timeout', outcome='zombie_reaped' WHERE id=?`, id); err != nil {
				log.Printf("ZOMBIE: reaping tick %s: %v", id, err)
				continue
			}
			reaped++
		}
	}
	if reaped > 0 {
		log.Printf("ZOMBIE: reaped %d ticks (process died)", reaped)
	}
}
