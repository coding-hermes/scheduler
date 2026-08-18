package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
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

	// autoDisablePolicy holds the configurable per-project failure-rate
	// auto-disable settings (SCHED-GAP-018). When the failure-rate threshold
	// is > 0, the escalator disables projects whose recent failure rate meets
	// or exceeds it. Zero = feature off.
	autoDisablePolicy autoDisablePolicy

	mu       sync.RWMutex
	running  sync.WaitGroup
	stopCh   chan struct{}
	pauseCh  chan bool
	evalCh   chan struct{} // event-driven eval trigger (SlotFreed → debounce → evalCh)
	lastEval time.Time
	// lastStallEvent is when the GAP-042 stall watchdog last emitted its
	// HIGH event (zero = never). Guards the stall-event throttle.
	lastStallEvent time.Time
	// GAP-043 zero-select monitoring: consecutive evals that selected 0
	// projects while eligible (enabled, not running, cooldown elapsed)
	// projects existed. Evaluations log nothing on a zero select, so an
	// operator cannot distinguish "evaluating" from "evaluating nothing"
	// (observed 2026-08-13 20:55-21:08Z). Once zeroSelectThreshold
	// consecutive zero-selects accumulate, a distinct EVAL-ZERO-SELECT
	// line + HIGH event fire, re-emitted at most every zeroSelectReEmitGap.
	zeroSelectCount     int
	zeroSelectEligible  int
	lastZeroSelectEvent time.Time
	simulate            bool
	simSuccess          float64
	// simSeq guarantees unique simulated tick IDs even when multiple
	// simulated ticks are generated within the same second (DOGFOOD-007:
	// RunBulkSim crashed with "UNIQUE constraint failed: ticks.id" because
	// sim-<project>-<HHMMSS> collided for same-project ticks spawned within
	// one second).
	simSeq    atomic.Uint64
	noDeliver bool // suppress Telegram delivery (verify mode, tests)
}

// autoDisablePolicy is the configurable failure-rate auto-disable policy.
type autoDisablePolicy struct {
	failureRate float64 // 0 = off; 0.0–1.0 threshold
	window      int     // ticks per project to examine
	minTicks    int     // minimum sample size before disable can fire
}

// SetAutoDisablePolicy configures the per-project failure-rate auto-disable
// policy (SCHED-GAP-018). A failureRate of 0 or less disables the feature.
func (l *Loop) SetAutoDisablePolicy(failureRate float64, window, minTicks int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.autoDisablePolicy = autoDisablePolicy{
		failureRate: failureRate,
		window:      window,
		minTicks:    minTicks,
	}
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
	l := &Loop{
		calculator:      calc,
		packer:          NewPacker(db, calc, budget, maxConcur, nil),
		multiPoolPacker: NewMultiPoolPacker(budget, maxConcur, nil),
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
	// GAP-035: terminal gateway-key rejections in Spawn() emit HIGH events
	// through the loop's event logger.
	l.spawner.SetEventLogger(l.events)
	return l
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

// SetBlackoutWindows updates the blackout slowdown windows on both packers.
func (l *Loop) SetBlackoutWindows(windows []config.BlackoutWindow) {
	l.packer.blackoutWindows = windows
	l.multiPoolPacker.blackoutWindows = windows
}

// SetForemanHome overrides the default HERMES_HOME for foreman sessions.
func (l *Loop) SetForemanHome(path string) {
	l.spawner.SetForemanHome(path)
}

// SetNoExecFallback disables exec.Command fallback on gateway failure.
func (l *Loop) SetNoExecFallback(v bool) {
	l.spawner.SetNoExecFallback(v)
}

// EmitHighEvent writes a HIGH severity event to the events table (GAP-048).
// This exported method lets callers outside the scheduler package (e.g. the
// daemon's startup wiring in cmd/schedulerd) emit structured events through
// the loop's event logger without accessing the unexported events field.
func (l *Loop) EmitHighEvent(component, message string, details map[string]any) {
	l.events.Emit(context.Background(), SeverityHigh, component, message, details)
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

// simTickID builds a unique tick ID for a simulated spawn. The sequence
// suffix guarantees uniqueness even when multiple simulated ticks are
// generated in the same second (DOGFOOD-007: RunBulkSim crashed with
// "UNIQUE constraint failed: ticks.id" because sim-<project>-<HHMMSS>
// collided for same-project ticks spawned within one second).
func (l *Loop) simTickID(projName string, now time.Time) string {
	seq := l.simSeq.Add(1) - 1
	return fmt.Sprintf("sim-%s-%s-%d", projName, now.Format("150405"), seq)
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
				tickID := l.simTickID(proj.Name, now)
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
			// GAP-042: the event-driven loop has no periodic eval trigger —
			// an idle fleet (0 running, everything in cooldown) never
			// re-evaluates, so cooldown-expired projects sit unscheduled
			// indefinitely (observed 66-min silent gap 2026-08-13). Detect
			// the stale lastEval here, where the health ticker always fires,
			// and force re-evaluation + HIGH event.
			l.checkEvalStall(running)
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

// evalStallThreshold is the lastEval age at which the event-driven loop is
// considered stalled (GAP-042): 10x the 30s min-interval = 5 minutes. A
// healthy loop re-evaluates on every slot-freed event (5s debounce), so
// lastEval never ages this far while ticks are completing; when the fleet
// is fully idle (every project in cooldown, 0 running ticks) NOTHING
// triggers an evaluation — cooldown-expired projects can sit unscheduled
// for up to their cooldown (observed 66-min silent gap 2026-08-13
// 13:08-14:14 local, recovered only by manual POST /api/v1/evaluate).
const evalStallThreshold = 10 * 30 * time.Second // 10 x min-interval

// evalStallReEmitGap re-emits the HIGH stall event while a stall persists
// (forced re-evaluations not restoring the loop), so a wedged loop stays
// visible without event spam.
const evalStallReEmitGap = 30 * time.Minute

// zeroSelectThreshold is the number of consecutive zero-select evaluations
// (with eligible projects present) before EVAL-ZERO-SELECT fires (GAP-043).
// Two is chosen so a single transient empty pick (e.g. every project in
// cooldown) does not alarm, while a persistent pattern surfaces within
// ~2 eval cycles as required by the acceptance criteria.
const zeroSelectThreshold = 2

// zeroSelectReEmitGap re-emits the HIGH zero-select event while the
// condition persists (mirrors evalStallReEmitGap) — the first emit happens
// at threshold, subsequent ones at most once per gap.
const zeroSelectReEmitGap = 30 * time.Minute

// checkEvalStall is the GAP-042 in-loop stall watchdog. It runs from the
// 30s health ticker — which always fires, unlike the escalator
// (CheckSchedulerHealth only runs inside evaluate(), so a loop that never
// evaluates never escalates). When lastEval has been frozen past
// evalStallThreshold with zero in-flight ticks, the loop has no pending
// trigger: force one and emit a HIGH event at stall onset, re-emitted
// every evalStallReEmitGap while the stall persists.
func (l *Loop) checkEvalStall(running int) {
	l.mu.RLock()
	lastEval := l.lastEval
	l.mu.RUnlock()
	if lastEval.IsZero() {
		return // never evaluated — the initial eval fires at startup
	}
	age := time.Since(lastEval)
	if age < evalStallThreshold || running > 0 {
		return // healthy: evaluating on cadence, or work in flight
	}

	now := time.Now()
	l.mu.Lock()
	emit := l.lastStallEvent.IsZero() || now.Sub(l.lastStallEvent) >= evalStallReEmitGap
	if emit {
		l.lastStallEvent = now
	}
	l.mu.Unlock()

	// Force re-evaluation on every stall crossing (cheap: an idle fleet
	// evaluates to zero picks). A wedged evaluate() cannot consume the
	// channel, so this never makes a broken loop worse.
	select {
	case l.evalCh <- struct{}{}:
	default:
	}
	if !emit {
		return
	}
	log.Printf("EVAL-STALL: last eval %v ago with %d running ticks — forced re-evaluation (threshold %v)",
		age.Round(time.Second), running, evalStallThreshold)
	l.events.Emit(context.Background(), SeverityHigh, "loop", "eval loop stalled — forced re-evaluation", map[string]any{
		"age_seconds":  age.Seconds(),
		"last_eval":    lastEval.Format(time.RFC3339),
		"active_ticks": running,
		"threshold_s":  evalStallThreshold.Seconds(),
	})
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (l *Loop) SpawnMethodCounts() (httpCount, execCount int64) {
	return l.spawner.SpawnMethodCounts()
}

// noteZeroSelect records a zero-project evaluation (GAP-043). Called from
// evaluate() while l.mu is held (write lock). When eligible projects exist
// and the consecutive count reaches zeroSelectThreshold, a distinct
// EVAL-ZERO-SELECT log line and a HIGH event fire — re-emitted at most
// once per zeroSelectReEmitGap so a persistent condition stays visible
// without event spam. A zero select with NO eligible projects (normal
// fleet-idle) resets the counter.
func (l *Loop) noteZeroSelect(now time.Time, runningSet map[string]bool) {
	// GAP-050: a zero select while every slot is busy is expected — the
	// packer's global maxConcurrent cap breaks out of selection before
	// picking anything, so eligible projects exist but NOTHING is wrong.
	// Treat saturation like fleet-idle: reset the counter, emit nothing.
	if len(runningSet) >= l.maxConcur {
		l.zeroSelectCount = 0
		l.zeroSelectEligible = 0
		return
	}
	eligible := l.countEligibleProjects(now, runningSet)
	if eligible == 0 {
		l.zeroSelectCount = 0
		l.zeroSelectEligible = 0
		return
	}
	l.zeroSelectCount++
	l.zeroSelectEligible = eligible
	if l.zeroSelectCount < zeroSelectThreshold {
		return
	}

	emit := l.lastZeroSelectEvent.IsZero() || now.Sub(l.lastZeroSelectEvent) >= zeroSelectReEmitGap
	if emit {
		l.lastZeroSelectEvent = now
	}
	log.Printf("EVAL-ZERO-SELECT: %d consecutive zero-select eval(s) with %d eligible project(s) — evaluation is picking nothing",
		l.zeroSelectCount, eligible)
	if !emit {
		return
	}
	l.events.Emit(context.Background(), SeverityHigh, "loop",
		"evaluation selected 0 projects while eligible projects exist", map[string]any{
			"consecutive":  l.zeroSelectCount,
			"eligible":     eligible,
			"threshold":    zeroSelectThreshold,
			"reemit_gap_s": zeroSelectReEmitGap.Seconds(),
			"last_eval":    now.UTC().Format(time.RFC3339),
			"active_ticks": len(runningSet),
		})
}

// resetZeroSelect clears the GAP-043 consecutive zero-select counter.
// Called from evaluate() when a selection did occur.
func (l *Loop) resetZeroSelect() {
	l.zeroSelectCount = 0
	l.zeroSelectEligible = 0
}

// countEligibleProjects counts enabled projects that are not currently
// running and whose cooldown has elapsed (never completed counts as
// eligible). These are the projects a healthy evaluation COULD have
// picked — a zero select with eligible > 0 is the GAP-043 anomaly signal.
// The cooldown predicate mirrors the packer's exactly (packer_select.go):
// failure backoff first, then the blackout-window multiplier, so a project
// the packer would skip is never counted as eligible (GAP-050).
func (l *Loop) countEligibleProjects(now time.Time, runningSet map[string]bool) int {
	rows, err := l.db.QueryContext(context.Background(),
		`SELECT name, cooldown_s, COALESCE(last_tick_completed, ''), COALESCE(consecutive_failures, 0) FROM projects WHERE enabled = 1`)
	if err != nil {
		log.Printf("EVAL-ZERO-SELECT: query eligible projects: %v", err)
		return 0
	}
	defer rows.Close()
	eligible := 0
	for rows.Next() {
		var name string
		var cooldown int
		var lastComp string
		var consecFailures int
		if err := rows.Scan(&name, &cooldown, &lastComp, &consecFailures); err != nil {
			continue
		}
		if runningSet[name] {
			continue
		}
		if lastComp == "" {
			eligible++
			continue
		}
		comp, err := time.Parse(time.RFC3339, lastComp)
		if err != nil {
			eligible++ // unknown completion time — treat as eligible
			continue
		}
		base := time.Duration(cooldown) * time.Second
		// S-GAP-001: consecutive spawn failures back off exponentially
		// (identical arithmetic to the packer's cooldown check).
		if consecFailures > 0 {
			base = FailureBackoff(base, consecFailures)
		}
		// Apply blackout slowdown if inside a peak-pricing window.
		if mult, inBlackout := config.ActiveMultiplier(l.packer.blackoutWindows, now); inBlackout {
			if mult <= 0 {
				continue // skip-mode blackout: packer skips this project
			}
			if mult > 1.0 {
				base = time.Duration(float64(base) * mult)
			}
		}
		if now.Sub(comp) >= base {
			eligible++
		}
	}
	return eligible
}

// ZeroSelectStats exposes GAP-043 diagnostics for /api/v1/status: the
// consecutive zero-select count, the eligible-project count at the last
// zero select, and the last zero-select event time ("" = never).
func (l *Loop) ZeroSelectStats() (consecutive, eligible int, lastEvent string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.lastZeroSelectEvent.IsZero() {
		lastEvent = l.lastZeroSelectEvent.UTC().Format(time.RFC3339)
	}
	return l.zeroSelectCount, l.zeroSelectEligible, lastEvent
}
