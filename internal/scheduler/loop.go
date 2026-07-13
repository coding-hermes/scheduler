package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// Loop runs the main evaluation cycle.
type Loop struct {
	calculator    *UrgencyCalculator
	packer        *Packer
	spawner       *Spawner
	simSpawner    *SimSpawner
	lifecycle     *LifecycleTracker
	db            *sql.DB
	interval      time.Duration
	weightBudget  int
	maxConcurrent int
	running       sync.WaitGroup
	pauseCh       chan bool
	stopCh        chan struct{}
	mu            sync.Mutex
	lastEval      time.Time

	// Simulation mode.
	simulate   bool
	simSuccess float64
}

// NewLoop creates the evaluation loop.
func NewLoop(db *sql.DB, minI, maxI time.Duration, numLevels, budget, maxConcurrent int) *Loop {
	calc := NewUrgencyCalculator(minI, maxI, numLevels)
	return &Loop{
		calculator:    calc,
		packer:        NewPacker(db, calc, budget, maxConcurrent),
		spawner:       NewSpawner(db, maxConcurrent),
		simSpawner:    NewSimSpawner(db, 0.85),
		lifecycle:     NewLifecycleTracker(db),
		db:            db,
		interval:      60 * time.Second,
		weightBudget:  budget,
		maxConcurrent: maxConcurrent,
		pauseCh:       make(chan bool, 1),
		stopCh:        make(chan struct{}),
	}
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
		mode, l.interval, l.weightBudget, l.maxConcurrent)
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

	// 1. Cleanup stale running ticks.
	cleaned, err := l.lifecycle.CleanupStale(90 * time.Minute)
	if err != nil {
		log.Printf("EVAL: cleanup error: %v", err)
	} else if cleaned > 0 {
		log.Printf("EVAL: cleaned up %d stale tick(s)", cleaned)
	}

	// 2. Pick projects to run.
	packed, err := l.packer.Pick(now)
	if err != nil {
		log.Printf("EVAL: packer error: %v", err)
		return
	}

	if len(packed) == 0 {
		return // nothing to do
	}

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used",
		len(packed), sumWeights(packed), l.weightBudget)

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
			continue
		}

		// Track completion asynchronously.
		l.running.Add(1)
		go func(tick *SpawnedTick) {
			defer l.running.Done()
			outcome := tick.Wait()
			if err := l.lifecycle.Complete(outcome); err != nil {
				log.Printf("EVAL: complete %s: %v", tick.TickID, err)
			}
		}(st)
	}
}

// LastEvalTime returns when the last evaluation ran.
func (l *Loop) LastEvalTime() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastEval
}

func sumWeights(packed []PackedProject) int {
	total := 0
	for _, p := range packed {
		total += p.Weight
	}
	return total
}
