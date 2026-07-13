package scheduler

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// Loop runs the main evaluation cycle.
type Loop struct {
	calculator *UrgencyCalculator
	packer     *Packer
	spawner    *Spawner
	lifecycle  *LifecycleTracker
	db         *sql.DB

	interval       time.Duration
	weightBudget   int
	maxConcurrent  int
	running        sync.WaitGroup

	pauseCh    chan bool
	stopCh     chan struct{}
	dirty      bool
	mu         sync.Mutex
}

// NewLoop creates the evaluation loop.
func NewLoop(db *sql.DB, minI, maxI time.Duration, numLevels, budget, maxConcurrent int) *Loop {
	calc := NewUrgencyCalculator(minI, maxI, numLevels)
	return &Loop{
		calculator:    calc,
		packer:        NewPacker(db, calc, budget, maxConcurrent),
		spawner:       NewSpawner(db, maxConcurrent),
		lifecycle:     NewLifecycleTracker(db),
		db:            db,
		interval:      60 * time.Second,
		weightBudget:  budget,
		maxConcurrent: maxConcurrent,
		pauseCh:       make(chan bool, 1),
		stopCh:        make(chan struct{}),
	}
}

// Run starts the main loop. Blocks until Stop() is called.
func (l *Loop) Run() {
	log.Printf("LOOP: starting eval loop (interval=%v, budget=%d, max_concurrent=%d)", l.interval, l.weightBudget, l.maxConcurrent)
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

	// 1. Cleanup stale running ticks (older than 90 min).
	if _, err := l.lifecycle.CleanupStale(90 * time.Minute); err != nil {
		log.Printf("EVAL: cleanup error: %v", err)
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

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used", len(packed), sumWeights(packed), l.weightBudget)

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

func sumWeights(packed []PackedProject) int {
	total := 0
	for _, p := range packed {
		total += p.Weight
	}
	return total
}
