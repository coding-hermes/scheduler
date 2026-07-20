package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

// Loop runs the main evaluation cycle.
type Loop struct {
	calculator      *UrgencyCalculator
	packer          *Packer
	multiPoolPacker *MultiPoolPacker
	spawner         *Spawner
	slotPool        *SlotPool // concurrent spawn semaphore (BUG-007)
	simSpawner      *SimSpawner
	lifecycle       *LifecycleTracker
	events          *EventLogger
	db              *sql.DB
	interval        time.Duration
	weightBudget    int
	maxConcur       int
	namespaceMode   bool
	gatewayClient   *GatewayClient // HTTP client for Gateway API (FIX-STUCK)
	gatewayDead     bool           // true when last ping failed

	mu         sync.RWMutex
	running    sync.WaitGroup
	stopCh     chan struct{}
	pauseCh    chan bool
	evalCh     chan struct{} // event-driven eval trigger (SlotFreed → debounce → evalCh)
	lastEval   time.Time
	simulate   bool
	simSuccess float64
	noDeliver  bool // suppress Telegram delivery (verify mode, tests)
}

// SetNoDeliver suppresses Telegram delivery of tick output.
func (l *Loop) SetNoDeliver(v bool) { l.noDeliver = v }

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
		interval:        30 * time.Second,
		weightBudget:    budget,
		maxConcur:       maxConcur,
		namespaceMode:   nsMode,
		pauseCh:         make(chan bool, 1),
		evalCh:          make(chan struct{}, 1),
		stopCh:          make(chan struct{}),
	}
}

// SetNamespaceMode enables or disables multi-namespace scheduling.
func (l *Loop) SetNamespaceMode(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.namespaceMode = on
}

// SetGatewayClient wires the HTTP gateway client into the spawner (FEAT-003).
func (l *Loop) SetGatewayClient(client *GatewayClient) {
	l.spawner.SetGatewayClient(client)
}

// SetForemanHome overrides the default HERMES_HOME for foreman sessions.
func (l *Loop) SetForemanHome(path string) {
	l.spawner.SetForemanHome(path)
}

// SetNoExecFallback disables exec.Command fallback on gateway failure.
func (l *Loop) SetNoExecFallback(v bool) {
	l.spawner.SetNoExecFallback(v)
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

// SetTickTimeout updates the real spawner's per-tick timeout and initializes
// the concurrent slot pool if needed (BUG-007).
func (l *Loop) SetTickTimeout(timeout time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spawner != nil {
		l.spawner.timeout = timeout
	}
	if l.slotPool == nil {
		l.slotPool = NewSlotPool(l.maxConcur, timeout, l.spawner, l.lifecycle)
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
	time.Sleep(1 * time.Second)
	return nil
}

// Run starts the main event-driven evaluation loop. Blocks until Stop() is called.
//
// Architecture (event-driven, not timer-driven):
//   - SlotPool.SlotFreed() signals when a tick completes and frees a slot.
//   - Each signal resets a 5s coalescing debounce timer.
//   - When the debounce expires, l.evaluate() fires to fill freed slots.
//   - A 30s health ticker logs goroutine counts and running tick stats.
//   - Initial evaluation fires immediately on startup.
//   - 60s zombie reaper still runs in the background.
func (l *Loop) Run() {
	mode := "real"
	if l.simulate {
		mode = fmt.Sprintf("simulated (success=%.0f%%)", l.simSuccess*100)
	}
	log.Printf("LOOP: starting %s eval loop (event-driven, budget=%d, max_concurrent=%d, debounce=5s) goroutines=%d",
		mode, l.weightBudget, l.maxConcur, runtime.NumGoroutine())

	l.cleanDanglingOnStartup()

	reaper := time.NewTicker(60 * time.Second)
	defer reaper.Stop()

	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()

	// Initialize slot pool if lazy-init hasn't happened yet (test_verify, tests).
	if l.slotPool == nil {
		l.slotPool = NewSlotPool(l.maxConcur, 2*time.Hour, l.spawner, l.lifecycle)
	}

	// SlotFreed() spawns one internal polling goroutine. Capture the channel
	// once so the select loop isn't creating new goroutines on every iteration.
	slotFreedCh := l.slotPool.SlotFreed()

	// Coalescing debounce: each slot-freed event resets a 5s timer.
	// Only after 5s of quiet does evaluation fire — this batches rapid
	// completions and prevents the feedback-loop flood (BUG-008).
	var (
		debounceTimer *time.Timer
		debounceMu    sync.Mutex
	)

	// Fire initial evaluation so the fleet starts immediately instead of
	// waiting for the first tick to complete.
	select {
	case l.evalCh <- struct{}{}:
	default:
	}

	for {
		select {
		case <-l.stopCh:
			debounceMu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceMu.Unlock()
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
		case <-reaper.C:
			l.reapZombies()
		case <-healthTicker.C:
			running := 0
			if l.slotPool != nil {
				running = l.slotPool.Running()
			}
			log.Printf("LOOP: health (goroutines=%d, slots=%d/%d, last_eval=%v)",
				runtime.NumGoroutine(), running, l.maxConcur, l.lastEval.Format("15:04:05"))
		case <-slotFreedCh:
			debounceMu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(5*time.Second, func() {
				select {
				case l.evalCh <- struct{}{}:
				default:
				}
			})
			debounceMu.Unlock()
		case <-l.evalCh:
			l.evaluate()
		}
	}
}

// Stop stops the evaluation loop and waits for in-flight ticks.
func (l *Loop) Stop() {
	close(l.stopCh)
	done := make(chan struct{})
	go func() {
		l.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("LOOP: all in-flight ticks completed")
	case <-time.After(15 * time.Second):
		log.Println("LOOP: timed out waiting for in-flight ticks — forcing shutdown")
	}
}

// ForceEvaluate triggers an immediate evaluation.
func (l *Loop) ForceEvaluate() {
	go l.evaluate()
}

func (l *Loop) Pause()  { l.pauseCh <- false }
func (l *Loop) Resume() { l.pauseCh <- true }

// LastEvalTime returns when the last evaluation ran.
func (l *Loop) LastEvalTime() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastEval
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (l *Loop) SpawnMethodCounts() (httpCount, execCount int64) {
	return l.spawner.SpawnMethodCounts()
}
