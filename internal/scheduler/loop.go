package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"runtime"
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
