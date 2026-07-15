package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

// Loop runs the main evaluation cycle.
type Loop struct {
	calculator      *UrgencyCalculator
	packer          *Packer
	multiPoolPacker *MultiPoolPacker
	spawner         *Spawner
	simSpawner      *SimSpawner
	lifecycle       *LifecycleTracker
	events          *EventLogger
	db              *sql.DB
	interval        time.Duration
	weightBudget    int
	maxConcur       int
	namespaceMode   bool

	mu         sync.RWMutex
	running    sync.WaitGroup
	stopCh     chan struct{}
	pauseCh    chan bool
	lastEval   time.Time
	simulate   bool
	simSuccess float64
}

// NewLoop creates the evaluation loop. namespaceMode is optional for backward
// compatibility with existing callers; omitted values default to false.
func NewLoop(db *sql.DB, minI, maxI time.Duration, numLevels, budget, maxConcur int, namespaceMode ...bool) *Loop {
	calc := NewUrgencyCalculator(minI, maxI, numLevels)
	nsMode := false
	if len(namespaceMode) > 0 {
		nsMode = namespaceMode[0]
	}
	return &Loop{
		calculator:      calc,
		packer:          NewPacker(db, calc, budget, maxConcur),
		multiPoolPacker: NewMultiPoolPacker(budget, maxConcur),
		spawner:         NewSpawner(db, maxConcur),
		simSpawner:      NewSimSpawner(db, 0.85),
		lifecycle:       NewLifecycleTracker(db),
		events:          NewEventLogger(db),
		db:              db,
		interval:        60 * time.Second,
		weightBudget:    budget,
		maxConcur:       maxConcur,
		namespaceMode:   nsMode,
		pauseCh:         make(chan bool, 1),
		stopCh:          make(chan struct{}),
	}
}

// SetNamespaceMode enables or disables multi-namespace scheduling.
func (l *Loop) SetNamespaceMode(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.namespaceMode = on
}

// SetSimulation enables simulation/dry-run mode.
func (l *Loop) SetSimulation(successRate float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.simulate = true
	l.simSuccess = successRate
	if l.simSpawner != nil {
		l.simSpawner.success = successRate
	}
}

// RunBulkSim generates N simulated ticks and exits.
func (l *Loop) RunBulkSim(ctx context.Context, count int) error {
	l.simulate = true
	l.simSpawner.success = l.simSuccess

	projects, err := l.packer.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if len(projects) == 0 {
		return fmt.Errorf("no enabled projects for simulation")
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	generated := 0
	for generated < count {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			// Round-robin through projects, 8 at a time.
			n := min(8, len(projects), count-generated)
			for i := 0; i < n; i++ {
				proj := projects[(generated+i)%len(projects)]
				tickID := fmt.Sprintf("sim-%s-%s", proj.Name, now.Format("150405"))
				if _, err := l.simSpawner.Spawn(proj, tickID); err != nil {
					return fmt.Errorf("spawn: %w", err)
				}
			}
			generated += n
			log.Printf("SIM: %d/%d ticks generated", generated, count)
		}
	}
	log.Printf("SIM: all %d ticks generated — waiting for simulated completion", count)
	time.Sleep(1 * time.Second) // let goroutines finish
	return nil
}

// Run starts the main loop. Blocks until Stop() is called.
func (l *Loop) Run() {
	mode := "real"
	if l.simulate {
		mode = fmt.Sprintf("simulated (success=%.0f%%)", l.simSuccess*100)
	}
	log.Printf("LOOP: starting %s eval loop (interval=%v, budget=%d, max_concurrent=%d)",
		mode, l.interval, l.weightBudget, l.maxConcur)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			log.Println("LOOP: stopping")
			return
		case <-l.pauseCh:
			log.Println("LOOP: paused")
			select {
			case <-l.stopCh:
				return
			case resume := <-l.pauseCh:
				if resume {
					log.Println("LOOP: resumed")
				}
			}
		case <-ticker.C:
			l.evaluate()
		}
	}
}

// Stop stops the evaluation loop.
func (l *Loop) Stop() {
	close(l.stopCh)
}

// ForceEvaluate triggers an immediate evaluation.
func (l *Loop) ForceEvaluate() {
	go l.evaluate()
}

// Pause pauses the evaluation loop.
func (l *Loop) Pause() {
	l.pauseCh <- false
}

// Resume resumes the evaluation loop.
func (l *Loop) Resume() {
	l.pauseCh <- true
}

// evaluate runs one evaluation cycle.
func (l *Loop) evaluate() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.lastEval = now

	// EVAL_START event.
	l.events.Emit(context.Background(), SeverityInfo, "loop", "evaluation started", map[string]any{
		"active_ticks": l.lifecycle.RunningCount(),
		"budget":       l.weightBudget,
	})

	// 1. Cleanup stale running ticks.
	cleaned, err := l.lifecycle.CleanupStale(90 * time.Minute)
	if err != nil {
		log.Printf("EVAL: cleanup error: %v", err)
	} else if cleaned > 0 {
		log.Printf("EVAL: cleaned up %d stale tick(s)", cleaned)
	}

	// 2. Pick projects to run.
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
		packed, err = l.packer.Pick(now)
		if err != nil {
			log.Printf("EVAL: packer error: %v", err)
			return
		}
	}

	if len(packed) == 0 {
		return // nothing to do
	}

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used",
		len(packed), sumWeights(packed), l.weightBudget)

	l.events.Emit(context.Background(), SeverityInfo, "loop", "projects selected", map[string]any{
		"count":       len(packed),
		"budget_used": sumWeights(packed),
		"budget":      l.weightBudget,
	})

	// 3. Spawn each selected project.
	for _, proj := range packed {
		tickID := fmt.Sprintf("%s-%s", proj.Name, now.Format("2006-01-02-15-04-05"))

		if err := l.lifecycle.Enqueue(proj.Name, tickID); err != nil {
			log.Printf("EVAL: enqueue %s: %v", proj.Name, err)
			continue
		}

		if err := l.lifecycle.StartRunning(tickID); err != nil {
			log.Printf("EVAL: start %s: %v", proj.Name, err)
			continue
		}

		if l.simulate {
			// Simulated spawn — completes instantly.
			if _, err := l.simSpawner.Spawn(proj, tickID); err != nil {
				log.Printf("EVAL: sim-spawn %s: %v", proj.Name, err)
				_ = l.lifecycle.Complete(TickOutcome{
					TickID:  tickID,
					Project: proj.Name,
					Started: now,
					Status:  TickFailed,
					Error:   err.Error(),
				})
			}
			continue
		}

		// Real spawn.
		st, err := l.spawner.Spawn(proj, tickID)
		if err != nil {
			log.Printf("EVAL: spawn %s: %v", proj.Name, err)
			_ = l.lifecycle.Complete(TickOutcome{
				TickID:  tickID,
				Project: proj.Name,
				Started: now,
				Status:  TickFailed,
				Error:   err.Error(),
			})
			l.events.Emit(context.Background(), SeverityLow, "spawner", "tick spawned", map[string]any{
				"project": proj.Name, "tick_id": tickID, "status": "failed",
			})
			continue
		}

		l.events.Emit(context.Background(), SeverityInfo, "spawner", "tick spawned", map[string]any{
			"project": proj.Name, "tick_id": tickID, "status": "running",
		})

		// Track completion asynchronously.
		l.running.Add(1)
		go func(tick *SpawnedTick) {
			defer l.running.Done()
			outcome := tick.Wait()
			// Cost data (TokensIn, TokensOut, CostUSD) is estimated by
			// SpawnedTick.Wait() for completed ticks and persisted to DB
			// by lifecycle.Complete() — no extra capture needed here.
			if err := l.lifecycle.Complete(outcome); err != nil {
				log.Printf("EVAL: complete %s: %v", tick.TickID, err)
			}
			l.events.Emit(context.Background(), SeverityInfo, "spawner", "tick completed", map[string]any{
				"project":    outcome.Project,
				"tick_id":    outcome.TickID,
				"status":     string(outcome.Status),
				"duration":   outcome.Duration.String(),
				"tokens_in":  outcome.TokensIn,
				"tokens_out": outcome.TokensOut,
				"cost_usd":   outcome.CostUSD,
			})
		}(st)
	}

	// 4. Run alert escalation checks
	escalator := NewAlertEscalator(l.db, l.events)
	if err := escalator.RunAll(context.Background(), now); err != nil {
		log.Printf("EVAL: escalation check error: %v", err)
	}
}

// LastEvalTime returns when the last evaluation ran.
func (l *Loop) LastEvalTime() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastEval
}

// evalContext returns the set of currently-running project names and a map of
// project → last completed timestamp. Used by the multi-pool packing path.
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
