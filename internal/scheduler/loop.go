package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
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
	weightBudget    int
	maxConcur       int
	namespaceMode   bool
	// minInterval is the configured --min-interval (ADV-R02). Immutable
	// after NewLoop; it drives the eval-stall watchdog threshold
	// (10x min-interval) via evalStallThreshold().
	minInterval   time.Duration
	gatewayClient *GatewayClient // HTTP client for Gateway API (FIX-STUCK)
	gatewayDead   bool           // true when last ping failed

	// autoDisablePolicy holds the configurable per-project failure-rate
	// auto-disable settings (SCHED-GAP-018). When the failure-rate threshold
	// is > 0, the escalator disables projects whose recent failure rate meets
	// or exceeds it. Zero = feature off.
	autoDisablePolicy autoDisablePolicy

	mu     sync.RWMutex
	stopCh chan struct{}
	// SCHED-GAP-1575-A: atomic mirror of the spawner gateway-response
	// timeout so /api/v1/status can read it WITHOUT taking Loop.mu.
	// Pre-fix, GatewayResponseTimeout() took the WRITE lock to read a
	// single duration; on a loaded host /api/v1/status joined the same
	// convoy as evaluate() and was stuck 28-80 min. The spawner remains
	// the source of truth (existing tests rely on
	// spawner.GatewayResponseTimeout()); this atomic is a write-through
	// cache read by the Loop getter only.
	gatewayResponseTimeoutNs atomic.Int64
	// evalWakeCh + evalDone are the SCHED-GAP-1575-A coalescing channel
	// for ForceEvaluate(). Buffered(1); non-blocking sends collapse N
	// concurrent calls into at most one pending pass. The drain goroutine
	// (started in NewLoop, stopped by closing stopCh) reads it and calls
	// l.evaluate() in a loop. Pre-fix ForceEvaluate() spawned
	// `go l.evaluate()` with no coalescing, so a board-wake / API POST
	// / evaluate storm queued 180+ evaluate goroutines behind one
	// write lock.
	evalWakeCh chan struct{}
	// pauseCh is a WAKE signal only (GAP-101): a parked legacy waiter or
	// ticker stall reacts to it. It carries no state — pause state is the
	// atomic paused flag. Buffered(1) + non-blocking sends in Pause/Resume.
	pauseCh chan struct{}
	// paused is the AUTHORITATIVE pause state (GAP-101, 2026-09-09).
	// The old channel-only protocol made pauseCh carry both state and
	// wakeup, so a redundant Resume() was consumed as a pause and wedged
	// the loop (DOGFOOD-020). State now lives here (atomic.Bool);
	// pauseCh is a pure, best-effort wake signal.
	paused   atomic.Bool
	evalCh   chan struct{} // event-driven eval trigger (SlotFreed → debounce → evalCh)
	lastEval time.Time
	// lastStallEvent is when the GAP-042 stall watchdog last emitted its
	// HIGH/MEDIUM stall event (zero = never). Guards the stall-event
	// throttle shared by both severities (SCHED-GAP-061).
	lastStallEvent time.Time
	// lastStallForce is when the watchdog last pushed a forced
	// re-evaluation (zero = never). The loop has recovered within one
	// cycle when lastEval is refreshed after this timestamp — the forced
	// eval was consumed and evaluate() ran. Distinguishes the
	// self-recovering idle cadence from a genuine wedge whose forced
	// evals never run.
	lastStallForce time.Time
	// stallMisses is the count of CONSECUTIVE non-recovered stall
	// crossings (DOGFOOD-018): the previous forced re-evaluation was
	// consumed by the loop, but lastEval was still frozen at the
	// pre-force value when the next detection ran — evaluate() ran but
	// did not refresh lastEval, or is so wedged the force never landed.
	// At evalStallEscalateAfter consecutive misses the detection escalates
	// to HIGH. Any recovered crossing resets the counter to zero.
	stallMisses int
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

	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk clockSeam

	// stopGrace is how long Stop() waits for in-flight ticks to finish
	// before aborting them (SCHED-GAP-077). Defaults to 15s in NewLoop;
	// tests may shorten it.
	stopGrace time.Duration

	// tickTimeout mirrors the configured per-tick session deadline
	// (--tick-timeout), initialized from the spawner's own default in
	// NewLoop and updated by SetTickTimeout. It exists so startup reaping
	// can derive an age window without reaching into the spawner
	// (SCHED-GAP-145: a queued row older than 2x this can only be a
	// leftover of a dead process). A zero value falls back to
	// spawner.timeout — see reapTickTimeout.
	tickTimeout time.Duration

	// SCHED-GAP-155 admission-decision counters. Per-PROCESS, monotonic,
	// reset only by a restart (matching the spawn/gateway counters):
	// admitCounts is the per-reason tally for the reason vocabulary and
	// admitNSAdmits the per-namespace admit tally. admitPasses counts the
	// evaluation passes the ADMIT emitter ran in — a boot-fresh value of 0
	// with a non-zero tick count is the signature of an emitter that never
	// fired. Guarded by admitMu (its own mutex, NOT l.mu: the emitter runs
	// from evaluate() while l.mu is already held, and the API reads the
	// counters from another goroutine).
	admitMu       sync.Mutex
	admitPasses   int
	admitCounts   map[string]int
	admitNSAdmits map[string]int

	// nudgeSource carries the SCHED-GAP-157 non-packer entry point
	// ("startup" | "manual" | "board_wake") from the caller that enqueues a
	// tick to the slot pool's spawn goroutine, which stamps it onto the tick
	// row and clears it. Swapped atomically; the pool reads-and-clears it
	// once per spawn so concurrent spawns cannot observe a previous
	// caller's value (see schedgap157.go).
	nudgeSource atomic.Value // string
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
	// SCHED-GAP-1582: 0/negative budget normalizes to the documented
	// default of 100 — "budget of 0 / unset behaves as the current default,
	// not as hold-everything". The packer's greedy loop with budget 0
	// would otherwise reject every weight>0 project (used+weight > 0
	// always), silently holding the entire fleet. Callers that truly want
	// a tiny budget pass a positive value.
	if budget < 1 {
		budget = 100
	}
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
		weightBudget:    budget,
		maxConcur:       maxConcur,
		namespaceMode:   nsMode,
		minInterval:     minI,
		pauseCh:         make(chan struct{}, 1),
		evalCh:          make(chan struct{}, 1),
		stopCh:          make(chan struct{}),
		stopGrace:       15 * time.Second,
		// SCHED-GAP-155: the admission counters start at zero for every
		// reason in the vocabulary so /api/v1/status reports a complete,
		// all-zero map from boot — an operator can tell "no deferrals"
		// from "counter missing".
		admitCounts:   make(map[string]int, len(admissionReasonVocabulary)),
		admitNSAdmits: make(map[string]int),
		// SCHED-GAP-1575-A: wake channel for the coalesced ForceEvaluate()
		// path. Buffered(1) + non-blocking send in ForceEvaluate() so N
		// concurrent calls collapse into at most one pending pass; the
		// drain goroutine (started below) reads it and calls evaluate().
		evalWakeCh: make(chan struct{}, 1),
	}
	for _, reason := range admissionReasonVocabulary {
		l.admitCounts[reason] = 0
	}
	// CI-003: eagerly create the slot pool. The former lazy-init sites
	// (Run, SpawnNow, evaluate) assigned l.slotPool from goroutines while
	// Stop() read it unsynchronized — a data race the CI Race Detector
	// step caught in TestNewLoop_Defaults (loop_test.go:29 Stop() vs
	// loop_test.go:23 go loop.Run()). Constructed here, before any
	// goroutine can observe the Loop, the field is immutable for the
	// Loop's lifetime; the SCHED-GAP-077 drain (Wait/abortInFlightTicks/
	// ReleaseAll in Stop) is unchanged.
	l.slotPool = NewSlotPool(l.maxConcur, l.spawner, l.lifecycle)
	// SCHED-GAP-145: mirror the spawner's configured tick deadline onto the
	// Loop (single source — NewSpawner's default until SetTickTimeout runs)
	// so the startup queued-row reaper can derive its 2x age window.
	l.tickTimeout = l.spawner.timeout

	// GAP-035: terminal gateway-key rejections in Spawn() emit HIGH events
	// through the loop's event logger.
	l.spawner.SetEventLogger(l.events)
	// SCHED-GAP-1575-A: prime the atomic mirror with the spawner's
	// resolved default so /api/v1/status reports the right value before
	// the daemon has called SetGatewayResponseTimeout (the SCHED-GAP-117
	// default is 30m, observable in schedgap117_status_test.go).
	// SetGatewayResponseTimeout will overwrite this whenever the daemon
	// applies --gateway-response-timeout or the env-var resolver changes
	// the value; this prime keeps the mirror aligned from boot.
	l.gatewayResponseTimeoutNs.Store(int64(l.spawner.GatewayResponseTimeout()))
	// ADV-R08/G3: slot-wait drops in SlotPool.spawn emit MEDIUM events
	// through the same logger.
	l.slotPool.SetEventLogger(l.events)
	// SCHED-GAP-157: the pool consumes the Loop's pending nudge-source
	// stamp at the admit → start boundary (SlotPool.spawn goroutine).
	l.slotPool.SetLoop(l)
	// SCHED-GAP-157: install the board-wake nudge-source hook — the next
	// BoardWakeWatcher this process constructs stamps board_wake on THIS
	// loop when one of its wakes fires (the daemon builds the watcher after
	// the loop). Same process-wide pattern as the gateway-health gate.
	boardWakeNudgeSource.Store(func() { l.SetNudgeSource(NudgeSourceBoardWake) })
	// SCHED-GAP-170 (observability): the gateway-health gate reports its
	// ONE-per-episode transitions (unhealthy / recovered) through the same
	// logger, so a fleet-wide gateway outage is visible in the events table
	// instead of being inferable only from the absence of tick rows. The gate
	// is PACKAGE state (one gateway = one verdict for the whole fleet), so this
	// is the logger every consult path — the evaluation pass and SpawnNow —
	// shares.
	SetGatewayHealthGateEvents(l.events)
	// SCHED-GAP-1575-A: single drain goroutine for the coalesced
	// ForceEvaluate() path. Reads l.evalWakeCh in a loop, calls
	// l.evaluate(), repeats. evaluate() takes the Loop write lock
	// internally — the drain must NOT hold it. Exits when stopCh is
	// closed. Buffered(1) on the channel + the non-blocking send in
	// ForceEvaluate() mean N concurrent wakeups collapse into at most
	// one pending pass.
	go l.evalDrain()
	return l
}

// SetClock installs the loop's clock seam (ADV-R04 / G6, SCHED-GAP-169). Every
// instant the loop reads and every wait it blocks on goes through this clock;
// the default is the wall clock, so production behavior under RealClock is
// identical to the direct time.Now()/time.Sleep() calls it replaced. Passing
// nil keeps the current seam.
//
// The clock is also propagated to every component the loop owns (spawner, slot
// pool, lifecycle tracker, sim spawner, alert escalator, gateway client, board
// watcher when attached) so a test that installs one simulator drives the whole
// tree rather than just evaluate()'s decision instant.
func (l *Loop) SetClock(c clock.Clock) {
	if c == nil {
		return // nil keeps the default/current seam
	}
	l.clk.Set(c)
	l.spawner.SetClock(c)
	if l.slotPool != nil {
		l.slotPool.SetClock(c)
	}
	if l.lifecycle != nil {
		l.lifecycle.SetClock(c)
	}
	if l.simSpawner != nil {
		l.simSpawner.SetClock(c)
	}
	if gwClient := l.gatewayClientOrNil(); gwClient != nil {
		gwClient.SetClock(c)
	}
	// SCHED-GAP-170: the gateway-health gate measures its 30s TTL window on the
	// same seam — propagated unconditionally, because the gate's cache is
	// process-wide and its clock is not a property of the client. A test that
	// installs a clock can therefore drive the cache deterministically instead
	// of sleeping through a real 30s.
	SetGatewayHealthGateClock(c)
}

// clock returns the loop's clock, never nil (the zero value of the seam reads
// as the wall clock, so a zero-value Loop still works).
func (l *Loop) clock() clock.Clock { return l.clk.Get() }

// nowLocked returns the current instant through the clock seam.
func (l *Loop) nowLocked() time.Time { return l.clock().Now() }

// SetNamespaceMode enables or disables multi-namespace scheduling.
func (l *Loop) SetNamespaceMode(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.namespaceMode = on
}

// SetGatewayClient wires the HTTP gateway client into the spawner (FEAT-003)
// and — SCHED-GAP-170 — into the Loop itself and the gateway-health gate.
//
// The Loop field is not decoration: the pre-existing liveness block in
// evaluate() guarded on `l.gatewayClient`, which NOTHING assigned, so the guard
// was permanently false and the fleet spawned straight into a dead gateway
// (793 of 859 failures in the 7 days to 2026-09-18). Registering the client
// here is what makes the gateway dependency observable from the loop at all.
//
// Registering with gatewayHealth (the process-wide cache) is what gives
// Loop.SpawnNow and the evaluation pass the same 30s verdict: the client is the
// probe target, so it must be installed by whoever installs the spawner's — a
// second wiring path would let the gate probe one endpoint while the spawns go
// to another.
func (l *Loop) SetGatewayClient(client *GatewayClient) {
	l.spawner.SetGatewayClient(client)
	// Written under the loop lock: the reconnector installs the client from a
	// background goroutine (GAP-048) while evaluate() and the API spawn handler
	// read it, so this is the loop's first real writer for the field.
	l.mu.Lock()
	l.gatewayClient = client
	l.mu.Unlock()
	SetGatewayHealthGateClient(client)
}

// gatewayClientOrNil returns the loop's gateway client (nil when HTTP spawning
// is not wired). Read under l.mu: the reconnector may replace the client at any
// time, so an unguarded read would race with SetGatewayClient.
func (l *Loop) gatewayClientOrNil() *GatewayClient {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.gatewayClient
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

// SetSimIdleRate sets the fraction of completed sim ticks that carry zero
// commits, so dry-runs can exercise the adaptive-cooldown slow-down path
// (Bane 2026-09-06). No-op outside simulation mode.
func (l *Loop) SetSimIdleRate(rate float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.simSpawner != nil {
		l.simSpawner.SetIdleRate(rate)
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

// SetTickTimeout updates the real spawner's per-tick timeout. The slot pool
// is created eagerly in NewLoop (CI-003) — no lazy init here. SCHED-GAP-145:
// the Loop's tickTimeout mirror is updated too (only for positive values, so a
// 0 timeout can never collapse the startup queued-row reap window to zero).
func (l *Loop) SetTickTimeout(timeout time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spawner != nil {
		l.spawner.timeout = timeout
	}
	if timeout > 0 {
		l.tickTimeout = timeout
	}
}

// SetGatewayResponseTimeout updates the real spawner's per-turn gateway POST
// deadline (SCHED-GAP-117). The daemon wires --gateway-response-timeout here
// after SetTickTimeout; 0 disables the per-turn deadline (pre-117 behavior),
// negative values are ignored by the spawner.
//
// SCHED-GAP-1575-A: also write through the atomic mirror used by
// GatewayResponseTimeout() so the GETTER can avoid taking Loop.mu. The
// spawner remains the source of truth (existing tests rely on
// spawner.GatewayResponseTimeout()); the atomic is a write-through cache
// read by the Loop getter only.
func (l *Loop) SetGatewayResponseTimeout(d time.Duration) {
	l.gatewayResponseTimeoutNs.Store(int64(d))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spawner != nil {
		l.spawner.SetGatewayResponseTimeout(d)
	}
}

// SetSlotPatience sets how long a spawn waits for a free slot before the
// project is dropped with a MEDIUM slot_pool event (ADV-R08/G3). Delegates
// to the slot pool; a d <= 0 keeps the default (5m) — the drop always
// exists. Must be called before Run(), same contract as the other startup
// setters (the daemon wires it during startup).
func (l *Loop) SetSlotPatience(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slotPool != nil {
		l.slotPool.SetPatience(d)
	}
}

// GatewayResponseTimeout reports the armed per-turn gateway POST deadline
// (SCHED-GAP-117), surfaced by /api/v1/status.
//
// SCHED-GAP-1575-A: read the atomic mirror; the loop mutex is no longer
// taken on the hot read path. Pre-fix this took the WRITE lock to read a
// single duration, joining the same convoy as evaluate() — see the
// SCHED-GAP-1575 wedge mechanism and the
// loop_gateway_timeout_atomic_test.go RED-proof.
func (l *Loop) GatewayResponseTimeout() time.Duration {
	return time.Duration(l.gatewayResponseTimeoutNs.Load())
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

	tick := l.clock().NewTicker(500 * time.Millisecond)
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
	l.clock().Sleep(1 * time.Second)
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

	// SCHED-GAP-145: reap 'queued' rows the previous process enqueued and
	// never dispatched. Runs immediately after the running-row reap and
	// BEFORE the first evaluation, so neither the in-flight dedup (which
	// refuses to re-spawn a project holding a queued row) nor the
	// namespace-cap admission count carries a dead row into this process.
	// A separate call rather than a step inside cleanDanglingOnStartup:
	// that function returns early when it finds no dead rows, and its
	// running-row contract stays byte-identical this way.
	l.reapStaleQueuedRows()

	// SCHED-GAP-091: if the daemon booted with a live gateway, any ticks
	// orphaned before the restart (gateway drop, previous crash) are
	// re-nudged now — before the first eval, so interrupted work resumes
	// instead of sitting until the next reconnect flip.
	l.resumeOrphansAtStartup()

	reaper := l.clock().NewTicker(60 * time.Second)
	defer reaper.Stop()

	healthTicker := l.clock().NewTicker(30 * time.Second)
	defer healthTicker.Stop()

	// SlotFreed() spawns one internal polling goroutine. Capture the channel
	// once so the select loop isn't creating new goroutines on every iteration.
	slotFreedCh := l.slotPool.SlotFreed()

	// Coalescing debounce: each slot-freed event resets a 5s timer.
	// Only after 5s of quiet does evaluation fire — this batches rapid
	// completions and prevents the feedback-loop flood (BUG-008).
	var (
		debounceTimer *clock.Timer
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
			// GAP-101 (2026-09-09): pauseCh is a pure WAKE signal now.
			// Pause/resume state lives in the paused flag (see Pause/
			// Resume); a wake while running (redundant resume) is a
			// no-op here. The old code treated ANY pauseCh value as
			// "park now", so a resume-on-a-running-loop wedged the
			// whole scheduler: "LOOP: paused" logged, slotFreedCh
			// stopped draining, evaluation triggers died until the
			// next pause/resume pair (DOGFOOD-020).
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
			debounceTimer = l.clock().AfterFunc(5*time.Second, func() {
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
//
// SCHED-GAP-077: the drain is driven by the SlotPool — a slot is held for the
// WHOLE tick (Acquire→Spawn→st.Wait()→Complete→Release), so
// slotPool.Running()/Wait(ctx) is the in-flight truth. The previous drain
// waited on l.running (a sync.WaitGroup with NO Add()/Done() callers anywhere
// in the repo), so it returned instantly while gateway requests were still
// blocking — logging 'all in-flight ticks completed' and orphaning running
// tick rows with stale heartbeats on shutdown (deepseek-dashboard 17:33:55
// incident, recovered only by a manual UPDATE).
//
// Stop waits up to stopGrace (default 15s) for in-flight ticks to finish. On
// drain timeout, every tick row still status='running' is marked 'failed'
// (schema-legal status; the ticks CHECK at internal/database/migrations.go
// allows queued/running/completed/failed/timeout) with a HIGH event, and all
// slots are released so the process can exit. No row survives shutdown as
// 'running' with a stale heartbeat. The startup reaper (tick_process.go
// reapZombies, S-GAP-003/SCHED-GAP-030) stays untouched as the backstop for
// rows orphaned by a hard crash.
func (l *Loop) Stop() {
	close(l.stopCh)
	if l.slotPool == nil {
		log.Println("LOOP: all in-flight ticks completed")
		return
	}
	grace := l.stopGrace
	if grace <= 0 {
		grace = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := l.slotPool.Wait(ctx); err == nil {
		log.Println("LOOP: all in-flight ticks completed")
		return
	}
	log.Printf("LOOP: shutdown drain timed out after %v — marking in-flight ticks failed", grace)
	l.abortInFlightTicks()
	l.slotPool.ReleaseAll()
}

// abortInFlightTicks marks every tick row still status='running' as 'failed'
// (schema-legal status — the ticks CHECK allows queued/running/completed/
// failed/timeout; no new status without a migration), logs each row with its
// tick id and project, and emits a HIGH event listing the affected tick ids.
// Called by Stop() when the drain grace expires with gateway requests still
// blocking.
func (l *Loop) abortInFlightTicks() {
	rows, err := l.db.Query(`SELECT id, project_name FROM ticks WHERE status = ?`, string(TickRunning))
	if err != nil {
		log.Printf("LOOP: shutdown drain: query running ticks: %v", err)
		l.EmitHighEvent("loop", "shutdown drain timed out — failed to query in-flight ticks", map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()

	type stuckTick struct {
		id      string
		project string
	}
	var stuck []stuckTick
	var ids []string
	for rows.Next() {
		var t stuckTick
		if err := rows.Scan(&t.id, &t.project); err != nil {
			log.Printf("LOOP: shutdown drain: scan running tick: %v", err)
			continue
		}
		stuck = append(stuck, t)
		ids = append(ids, t.id)
	}
	if err := rows.Err(); err != nil {
		log.Printf("LOOP: shutdown drain: iterate running ticks: %v", err)
	}

	for _, t := range stuck {
		finished := l.clock().Now()
		err := l.lifecycle.Complete(TickOutcome{
			TickID:   t.id,
			Project:  t.project,
			Started:  finished,
			Finished: finished,
			Status:   TickFailed,
			// SCHED-GAP-1597: -1 → exit_code NULL. No process was
			// reaped — the drain reaped the row, not the child — so
			// the struct-default 0 would be a fabricated "exited
			// cleanly".
			ExitCode: -1,
			Error:    OrphanAbortMarker + " — drain timed out with tick in flight",
		})
		if err != nil {
			log.Printf("LOOP: shutdown drain: mark tick %s (project %s) failed: %v", t.id, t.project, err)
			continue
		}
		// SCHED-GAP-091: the owner was alive but the drain gave up — this
		// is a drop. Stamp it so the gateway-health-return scan re-nudges
		// the tick instead of leaving the session lost.
		// SCHED-GAP-1608: when the OPERATOR approved this restart
		// (SCHEDULER_OPERATOR_RESTART_APPROVED — restart_approval.go), the
		// reap is an acknowledged, scheduled event, and the row carries the
		// DISTINCT reason 'operator_restart' so failure-rate/auto-disable
		// accounting (orphan_exclusion.go) can exclude it structurally.
		// Both spellings — this one and the legacy drain_timeout — are
		// excluded from lane accounting by the same shared predicate.
		reason := OrphanReasonDrainTimeout
		if OperatorRestartApproved() {
			reason = OrphanReasonOperatorRestart
		}
		l.stampOrphaned(t.id, reason)
		log.Printf("LOOP: shutdown drain timed out — marked tick %s (project %s) failed (orphan=%s)", t.id, t.project, reason)
	}
	if len(stuck) > 0 {
		l.EmitHighEvent("loop", fmt.Sprintf("shutdown drain timed out — marking %d in-flight ticks failed", len(stuck)), map[string]any{"tick_ids": ids})
	}
}

// ForceEvaluate triggers an immediate evaluation.
//
// SCHED-GAP-1575-A: coalesce via a buffered wake channel (cap 1) drained
// by a single goroutine started in NewLoop. The pre-fix implementation
// spawned `go l.evaluate()` with no coalescing, so a board-wake / API
// POST / evaluate storm queued 180+ evaluate goroutines behind one write
// lock and wedged the loop for 28-80 min on a loaded host. The coalesced
// path collapses N concurrent ForceEvaluate calls into at most one
// pending pass; once the drain finishes a pass the channel accepts
// another wake for the next pass.
//
// Falls back to `go l.evaluate()` if the wake channel was never
// installed (tests that build a Loop without the drain goroutine — see
// loop_force_evaluate_test.go for the path that uses the coalescer).
func (l *Loop) ForceEvaluate() {
	if l.evalWakeCh == nil {
		// Drain goroutine not started (older test path) — keep the
		// pre-fix behavior so legacy callers do not deadlock.
		go l.evaluate()
		return
	}
	select {
	case l.evalWakeCh <- struct{}{}:
	default:
		// Wake already pending; coalesce this call.
	}
}

// evalDrain is the single goroutine that services l.evalWakeCh for the
// coalesced ForceEvaluate() path (SCHED-GAP-1575-A). It blocks until
// either stopCh closes (graceful shutdown) or a wake arrives. On a wake
// it calls l.evaluate() and loops; evaluate() takes the Loop write
// lock internally, so this drain goroutine NEVER holds Loop.mu —
// holding it across evaluate() is exactly the convoy the pre-fix code
// caused.
//
// The function lives next to ForceEvaluate so the contract is local:
// ForceEvaluate() produces the wake, evalDrain consumes it.
func (l *Loop) evalDrain() {
	for {
		select {
		case <-l.stopCh:
			return
		case <-l.evalWakeCh:
			l.evaluate()
		}
	}
}

// ErrProjectRunning is returned by SpawnNow when the project already has a
// tick in flight (slot pool running set or DB running rows) — the
// SCHED-GAP-030 duplicate-spawn protection. The caller should surface it as
// a 409 rather than enqueue a second tick for the same project.
var ErrProjectRunning = errors.New("project already has a tick in flight")

// SpawnNow synchronously enqueues a tick for the project and fires the spawn
// session into the slot pool, returning the REAL stored tick id (DOGFOOD-015).
//
// The id is generated with database.NextTickID — the canonical UTC generator
// shared with SlotPool.Spawn — so the returned id always resolves via
// GET /ticks/{id}. The row is enqueued BEFORE the async spawn session runs,
// so the id is resolvable immediately (status queued) even when the pool is
// full and the goroutine has to wait for a slot.
//
// Duplicate-spawn protection (SCHED-GAP-030): a project that already holds a
// slot, or has a running/queued tick row, is refused with ErrProjectRunning —
// the same dedup the evaluation loop applies, so the manual spawn endpoint
// can never double-spawn a project that is mid-tick.
func (l *Loop) SpawnNow(project database.Project) (string, error) {
	l.mu.RLock()
	simulate := l.simulate
	noDeliver := l.noDeliver
	l.mu.RUnlock()

	// Dedup: refuse when the project already occupies a slot or has a
	// running/queued tick row (SCHED-GAP-030 — the eval loop merges both
	// sources; mirror it here so the manual endpoint cannot double-spawn).
	if l.slotPool != nil && l.slotPool.RunningSet()[project.Name] {
		return "", ErrProjectRunning
	}
	var inFlight int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND status IN ('queued','running')`, project.Name).Scan(&inFlight); err == nil && inFlight > 0 {
		return "", ErrProjectRunning
	}

	tickID := database.NextTickID(clock.WithClock(context.Background(), l.clock()), project.Name)

	proj := PackedProject{
		Name:             project.Name,
		Priority:         float64(project.Priority),
		Weight:           project.Weight,
		Workdir:          project.Workdir,
		RepoURL:          project.RepoURL,
		Command:          project.Command,
		Model:            project.Model,
		Provider:         project.Provider,
		FallbackModel:    project.FallbackModel,
		FallbackProvider: project.FallbackProvider,
		NoGlobalFallback: project.NoGlobalFallback,
		IdleModel:        project.IdleModel,
		IdleProvider:     project.IdleProvider,
		WorkerModel:      project.WorkerModel,
		WorkerProvider:   project.WorkerProvider,
		GatewayKey:       project.GatewayKey,
		Deliver:          project.Deliver,
		DeliverMode:      project.DeliverMode,
		// SCHED-GAP-111: thread the namespace id for effectiveTickTimeout
		// in the spawn path (manual spawns resolve the wave deadline too).
		NamespaceID: nsIDOf(project),
	}

	// Simulation mode: the sim spawner inserts the row itself (status
	// running) and completes it in 50-250ms — the returned id still
	// resolves, so the spawn→poll workflow holds in --simulate too.
	if simulate {
		if _, err := l.simSpawner.Spawn(proj, tickID); err != nil {
			return "", fmt.Errorf("sim spawn %s: %w", project.Name, err)
		}
		return tickID, nil
	}

	// Enqueue synchronously so the returned id is a real, stored row.
	if err := l.lifecycle.Enqueue(project.Name, tickID); err != nil {
		return "", fmt.Errorf("enqueue tick for %s: %w", project.Name, err)
	}

	// SCHED-GAP-171: the load gate is consulted HERE — after the enqueue that
	// makes the returned id resolvable, before the spawn session. The API
	// contract is preserved (the returned tickID is a stored row and the row
	// keeps status='queued'), and the deferral is now REPORTED instead of
	// being silent: `load_gate_deferred` with the tick id, so a caller can
	// tell "deferred under load" from "spawned" post-hoc. Without this the
	// row sat queued with no event naming why, and the only signal was the
	// 409 from the next SpawnNow (ErrProjectRunning) — which reads like a
	// stuck tick rather than a gate decision.
	if LoadGateShouldDefer(l.db, proj.NamespaceID) {
		l.emitLoadGateDeferred(proj.Name, proj.NamespaceID, tickID)
		return tickID, nil
	}

	// SCHED-GAP-170: the gateway-health gate, consulted at the SAME declared
	// admission point and with the SAME cached 30s verdict the evaluation pass
	// uses. The API contract is untouched (the returned tickID is a real stored
	// row and the row keeps status='queued'), and the deferral is REPORTED as a
	// `gateway_defer` event carrying that id — so a caller can tell "deferred
	// because the gateway is unreachable" from "spawned" without polling the
	// tick for a failure. Without this the row sat queued until the spawn
	// failed, and the failure was booked as the LANE's fault.
	//
	// No latch is written here on purpose: gatewayDead is the fleet-wide
	// transition state and only the evaluation pass (noteGatewayDeadOnce) owns
	// it — an API-triggered deferral must not flip fleet-wide scheduling state
	// from a request goroutine.
	if l.gatewayClientOrNil() != nil && !simulate {
		if deferSpawn, reason, probeErr := GatewayHealthGateShouldDefer(); deferSpawn {
			l.emitGatewayDeferred(proj.Name, proj.NamespaceID, tickID, reason, probeErr)
			return tickID, nil
		}
	}

	// Fire the spawn session (async — the row is already queued, so the
	// returned id resolves regardless of slot availability). The slot pool
	// exists from NewLoop (CI-003).
	// SCHED-GAP-157: this is the operator entry point — stamp the manual
	// nudge source so the tick row records how it came to exist. A
	// load-gate / gateway deferral below returns before SpawnEnqueued, and
	// nothing clears the stamp except a spawn, so a deferred manual spawn
	// stamps nothing (an honest empty).
	l.SetNudgeSource(NudgeSourceManual)
	l.slotPool.SpawnEnqueued(proj, tickID, l.clock().Now(), noDeliver, l.db)
	return tickID, nil
}

// Pause suspends evaluation. Transition-only: the state lives in the
// paused flag; pauseCh is a best-effort wake so a Run() parked on it
// (legacy) or a stalled ticker reacts. Never blocks on an unread wake.
func (l *Loop) Pause() {
	l.paused.Store(true)
	select {
	case l.pauseCh <- struct{}{}:
	default:
	}
}

// Resume clears the pause state (GAP-101, 2026-09-09). Redundant resume
// on a running loop is an idempotent no-op — it MUST NOT flip the loop
// into a park (the DOGFOOD-020 wedge). Same wake semantics as Pause().
func (l *Loop) Resume() {
	l.paused.Store(false)
	select {
	case l.pauseCh <- struct{}{}:
	default:
	}
}

// IsPaused reports the authoritative pause state (GAP-101 observability:
// /api/v1/status can finally distinguish "paused" from "idle").
func (l *Loop) IsPaused() bool { return l.paused.Load() }

// LastEvalTime returns when the last evaluation ran.
func (l *Loop) LastEvalTime() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastEval
}

// WeightBudget returns the scheduling weight budget this loop was built
// with (ADV-R09/G8 — budget authority chain). The value is immutable for
// the Loop's lifetime (set once in NewLoop from the --budget flag /
// SCHEDULER_BUDGET env / TOML weight_budget resolution), so reading it
// needs no lock. It is the SINGLE authority every budget surface (API
// /api/v1/status, MCP fleet status, dashboard) reports — no surface may
// hardcode a budget number again.
func (l *Loop) WeightBudget() int { return l.weightBudget }

// evalStallThreshold is the lastEval age at which the event-driven loop is
// considered stalled (GAP-042): 10x the configured min-interval (5 minutes
// at the default 30s min-interval). A
// healthy loop re-evaluates on every slot-freed event (5s debounce), so
// lastEval never ages this far while ticks are completing; when the fleet
// is fully idle (every project in cooldown, 0 running ticks) NOTHING
// triggers an evaluation — cooldown-expired projects can sit unscheduled
// for up to their cooldown (observed 66-min silent gap 2026-08-13
// 13:08-14:14 local, recovered only by manual POST /api/v1/evaluate).
//
// ADV-R02: derived from the Loop's configured min-interval at runtime
// instead of a hardcoded 30s constant, so a host running a non-default
// --min-interval gets a proportionally scaled stall threshold. The field
// is immutable for the Loop's lifetime (set once in NewLoop), so reading
// it here needs no lock.
func (l *Loop) evalStallThreshold() time.Duration {
	return 10 * l.minInterval
}

// evalStallReEmitGap re-emits the stall event while a stall persists
// (forced re-evaluations not restoring the loop), so a wedged loop stays
// visible without event spam. SCHED-GAP-061: the gap throttles BOTH
// severities — a persistent wedge re-alarms HIGH once per window, and a
// self-recovering idle fleet logs at most one MEDIUM per window — and
// doubles as the episode window: a detection counts as a one-cycle
// recovery only when the previous forced eval was pushed within the gap.
const evalStallReEmitGap = 30 * time.Minute

// evalStallEscalateAfter is the number of CONSECUTIVE non-recovered stall
// crossings (previous forced re-evaluation consumed but lastEval still
// frozen at the pre-force value) before the watchdog re-escalates to HIGH
// (DOGFOOD-018). The threshold is 2 so a single transient non-recovery —
// e.g. a slow evaluate() that refreshes lastEval shortly after the health
// ticker sampled it — does not alarm, while a genuinely wedged loop
// surfaces within two forced cycles. Any recovery resets the counter.
const evalStallEscalateAfter = 2

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

// allRunningRowsArePhantoms reports whether the DB's running ticks are ALL
// stale pid=0 phantoms — status='running' rows whose heartbeat (or
// spawned_at, for pre-S-GAP-003 rows that never wrote one) is older than
// gatewayZombieMaxAge (SCHED-GAP-135). This is the exact row shape the
// 2026-09-16 20:07:35Z gateway-restart burst left behind: the spawn wrote
// the placeholder session/heartbeat, exhausted its retries against a
// refused dial, and the row never reached a terminal status. Zero running
// rows returns false (nothing phantom to act on; the caller's legacy
// suppression stands), and any query error fails safe the same way — this
// helper only ever UNBLINDS the watchdog, it must never fire it on a
// healthy fleet. Uses the same julianday() comparison as
// staleGatewayTicksSQL: a raw string compare on RFC3339 across varying
// offsets would be wrong.
func (l *Loop) allRunningRowsArePhantoms() bool {
	const q = `
SELECT COUNT(*),
       SUM(CASE WHEN pid = 0 AND (
            (heartbeat_at IS NOT NULL AND julianday(heartbeat_at) < julianday('now', '-%[1]d minutes'))
         OR (heartbeat_at IS NULL     AND julianday(spawned_at)  < julianday('now', '-%[1]d minutes')))
           THEN 1 ELSE 0 END)
FROM ticks WHERE status = 'running'`
	var total, stale int
	if err := l.db.QueryRow(fmt.Sprintf(q, int(gatewayZombieMaxAge/time.Minute))).Scan(&total, &stale); err != nil {
		return false
	}
	return total > 0 && stale == total
}

// checkEvalStall is the GAP-042 in-loop stall watchdog. It runs from the
// 30s health ticker — which always fires, unlike the escalator
// (CheckSchedulerHealth only runs inside evaluate(), so a loop that never
// evaluates never escalates). When lastEval has been frozen past
// evalStallThreshold with zero in-flight ticks, the loop has no pending
// trigger: force one and emit a HIGH event at stall onset, re-emitted
// every evalStallReEmitGap while the stall persists.
//
// SCHED-GAP-061: on a healthy idle fleet the condition legitimately
// recurs every evalStallThreshold — the forced re-evaluation is consumed
// by the loop (evaluate() refreshes lastEval) and finds nothing to pick
// (all projects in cooldown), then lastEval ages again. Every such
// self-recovering detection was previously re-emitted HIGH every
// evalStallReEmitGap, desensitizing operators to real wedges (37 HIGH
// events observed 08-19T20:08Z..08-21T10:03Z). Severity follows the
// outcome of the previous forced re-evaluation:
//   - first onset (no previous force, or the previous episode's force is
//     older than the re-emit window) or a GENUINE wedge (consecutive
//     non-recoveries: the previous forced eval was consumed but lastEval
//     was still frozen at the pre-force value) → HIGH;
//   - one-cycle recovery (lastEval refreshed after a force pushed within
//     the re-emit window: the loop subsequently evaluated normally) →
//     INFO — the events table has no WARN level (CHECK constraint
//     CRITICAL/HIGH/MEDIUM/LOW/INFO), and the INFO tier matches the
//     "evaluation started" cadence while keeping the recovery visible in
//     the event stream.
//
// DOGFOOD-018: the SCHED-GAP-061 demotion (recovered → MEDIUM) still
// flooded the event log with a MEDIUM every evalStallReEmitGap on a
// healthy idle fleet (27 events 08-25T00:00Z..08-26, spacing 30-60 min),
// burying real escalations. Recoveries now emit at INFO, and a genuine
// wedge — where the forced re-evaluation does NOT restore lastEval —
// escalates HIGH only after evalStallEscalateAfter consecutive
// non-recovered crossings (see stallMisses), so a single transient miss
// cannot alarm while a persistently wedged loop still surfaces.
func (l *Loop) checkEvalStall(running int) {
	// GAP-101: a deliberately paused loop is healthy-by-choice — lastEval
	// freezing is expected, never force evaluation against an operator
	// pause (the old ForceEvaluate path ignored the pause entirely).
	if l.paused.Load() {
		return
	}
	l.mu.RLock()
	lastEval := l.lastEval
	lastForce := l.lastStallForce
	l.mu.RUnlock()
	if lastEval.IsZero() {
		return // never evaluated — the initial eval fires at startup
	}
	age := l.clock().Since(lastEval)
	if age < l.evalStallThreshold() {
		return // healthy: evaluating on cadence
	}
	// SCHED-GAP-135: `running > 0` must not silence the watchdog when every
	// running DB row is a stale pid=0 phantom (heartbeat older than
	// gatewayZombieMaxAge). The 2026-09-16 20:07:35Z gateway-restart wedge
	// left 4 such rows (9router, warpfs, off-by-one, gitreins-poc) holding
	// 4 of 5 packer slots with no EVAL/spawn line for 4m52s — and this
	// early return blinded GAP-042 against exactly the wedge it exists to
	// catch. When the running set is all phantoms there is no real work in
	// flight and no slot-freed event will ever arrive: force the
	// re-evaluation. Chosen shape: option (b) from the GAP-135 brief —
	// stale rows are excluded from the suppression count rather than
	// option (a)'s blanket age trigger, so a healthy busy fleet (live rows
	// present) still suppresses exactly as before.
	if running > 0 && !l.allRunningRowsArePhantoms() {
		return // healthy: work in flight
	}

	now := l.clock().Now()
	// onset: no recent forced eval — the stall episode is starting
	// fresh, or the previous episode closed more than
	// evalStallReEmitGap ago (fleet busy for hours, then idle again).
	onset := lastForce.IsZero() || now.Sub(lastForce) >= evalStallReEmitGap
	// One-cycle recovery: the previous forced re-evaluation was pushed
	// within the current episode window AND consumed by the loop —
	// evaluate() ran and refreshed lastEval after the force. A stale
	// force from a long-closed episode reads as a fresh onset, not a
	// recovery, even though lastEval is inevitably newer.
	recovered := !onset && lastEval.After(lastForce)

	l.mu.Lock()
	// DOGFOOD-018: consecutive-miss tracking. A recovered crossing
	// resets the counter to 0; a fresh onset starts a new episode at 1;
	// any other non-recovered crossing increments. Escalation to HIGH
	// requires evalStallEscalateAfter consecutive non-recovered
	// crossings within one episode — a single transient miss after a
	// healthy stretch must not alarm.
	switch {
	case recovered:
		l.stallMisses = 0
	case onset:
		l.stallMisses = 1
	default:
		l.stallMisses++
	}
	escalated := l.stallMisses >= evalStallEscalateAfter
	misses := l.stallMisses

	emit := l.lastStallEvent.IsZero() || now.Sub(l.lastStallEvent) >= evalStallReEmitGap
	if emit {
		l.lastStallEvent = now
	}
	l.lastStallForce = now
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

	severity := SeverityHigh
	message := "eval loop stalled — forced re-evaluation"
	switch {
	case onset:
		// Fresh episode: no recent forced eval. HIGH immediately so a
		// genuinely wedged loop still alarms at first detection.
	case escalated:
		// Genuine wedge (DOGFOOD-018): consecutive forced evals were
		// consumed but lastEval stayed frozen — evaluate() is not
		// refreshing. HIGH.
		message = "eval loop stalled — forced re-evaluation (unrecovered)"
	case recovered:
		// One-cycle recovery (DOGFOOD-018): the forced re-eval restored
		// the loop within one cycle — the watchdog working as designed on
		// an idle fleet. INFO keeps the event visible in the stream
		// without competing with operator alarms.
		severity = SeverityInfo
		message = "eval loop stalled — forced re-evaluation (recovered)"
	default:
		// Single non-recovered crossing below the escalation threshold:
		// the previous forced eval was consumed but lastEval stayed
		// frozen (transient slow evaluate(), or the start of a wedge).
		// INFO at first miss; the next consecutive miss escalates HIGH.
		severity = SeverityInfo
		message = "eval loop stalled — forced re-evaluation (unrecovered)"
	}
	log.Printf("EVAL-STALL: last eval %v ago with %d running ticks — forced re-evaluation (threshold %v, recovered=%t, misses=%d)",
		age.Round(time.Second), running, l.evalStallThreshold(), recovered, misses)
	l.events.Emit(context.Background(), severity, "loop", message, map[string]any{
		"age_seconds":  age.Seconds(),
		"last_eval":    lastEval.Format(time.RFC3339),
		"active_ticks": running,
		"threshold_s":  l.evalStallThreshold().Seconds(),
		"recovered":    recovered,
		"misses":       misses,
	})
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (l *Loop) SpawnMethodCounts() (httpCount, execCount int64) {
	return l.spawner.SpawnMethodCounts()
}

// GatewayErrorCount returns transient gateway spawn failures since last
// restart (SCHED-GAP-080); auth rejections are never counted.
func (l *Loop) GatewayErrorCount() int64 {
	return l.spawner.GatewayErrorCount()
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
		`SELECT name, cooldown_s, priority, COALESCE(last_tick_completed, ''), COALESCE(consecutive_failures, 0), COALESCE(bump_active, 0), COALESCE(bump_cooldown_s, 0), COALESCE(admission_mode, ''), COALESCE((SELECT admission_mode FROM namespaces WHERE id = projects.namespace_id), ''), COALESCE(workdir, ''), COALESCE(board_ownership, ''), COALESCE(last_tick_status, '') FROM projects WHERE enabled = 1`)
	if err != nil {
		log.Printf("EVAL-ZERO-SELECT: query eligible projects: %v", err)
		return 0
	}
	defer rows.Close()
	eligible := 0
	for rows.Next() {
		var name string
		var cooldown int
		var priority int
		var lastComp string
		var consecFailures int
		var bumpActive, bumpCD int
		var projMode, nsMode, workdir, boardOwnership, lastStatus string
		if err := rows.Scan(&name, &cooldown, &priority, &lastComp, &consecFailures, &bumpActive, &bumpCD,
			&projMode, &nsMode, &workdir, &boardOwnership, &lastStatus); err != nil {
			continue
		}
		if runningSet[name] {
			continue
		}
		// SCHED-GAP-107: an active bump owns the effective cooldown — the
		// eligibility mirror must use the bump value, exactly like the
		// packer's selection paths. (ADV-R03/G5: packer.go's Pick folds
		// bump_cooldown_s into its scored.cooldownS while scanning rows,
		// so its effectiveCooldownDur wrapper already sees the bumped
		// value; here the bump substitution stays SQL-side and the
		// bumped cooldown feeds the same shared effectiveCooldown.)
		if bumpActive == 1 && bumpCD > 0 {
			cooldown = bumpCD
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
		// ADV-R03/G5: the shared predicate — identical arithmetic to
		// every packer selection path (dynamic interval when cooldown
		// is 0, S-GAP-001 failure backoff, blackout multiplier +
		// skip-mode). The loop's calculator matches the packer's (both
		// are built from the same minI/maxI/numLevels in NewLoop).
		cooldownDur, skipMode := effectiveCooldown(cooldown, float64(priority), consecFailures, l.packer.blackoutWindows, now, l.calculator)
		if skipMode {
			continue // skip-mode blackout: packer skips this project
		}
		// SCHED-GAP-136: tasks-mode post-tick pacing mirror (base, no
		// jitter — deliberately the more conservative side; see
		// tasks_pacing.go). The packer-level mirror inside the tasks
		// branch lives in the three selection paths; this eligibility
		// mirror must match or the GAP-043 alarm lies. Applied regardless
		// of admission mode: in cooldown mode the cooldown check below is
		// already ≥ the pacing floor for every pinned project, so this
		// only bites tasks-mode rows (the intent).
		if tasksPacingDeferred(comp, now) {
			continue // post-tick pacing: packer defers this project
		}
		// SCHED-GAP-214 mirror: a tasks-mode lane whose last tick FAILED
		// paces on its full effective cooldown (waiver stood down) — the
		// three packer selection paths defer it, so the eligibility count
		// must too (GAP-050: a project the packer skips is never eligible).
		mode := admissionModeFor(projMode, name, map[string]string{name: nsMode})
		if mode == database.AdmissionModeTasks && tasksAdmissionDue(workdir, boardOwnership) &&
			lastTickStatusFailed(lastStatus) && now.Sub(comp) < cooldownDur {
			continue
		}
		if now.Sub(comp) >= cooldownDur {
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

// ─────────────────────────────────────────────────────────────────────────
// SCHED-GAP-155 — structured admission-decision log + counters.
//
// Operators could see WHAT was picked ("EVAL: N project(s) selected") and
// what the fleet-wide anomaly counters were (EVAL-STALL, EVAL-ZERO-SELECT),
// but never WHY an individual project was passed over: "why did project X
// not spawn in window Y" required reading the packer's aggregate lines,
// SQLite and the cooldown policy script side by side.
//
// One evaluation pass now emits ONE grep-stable line per candidate project:
//
//	ADMIT pass_id=7 eligible=12 admitted=2 deferred=10 ns=qa cap=1 \
//	      inflight_running=1 inflight_queued=0 project=sat-b reason=cap
//
//	grep -E '^ADMIT ' scheduler.log        # every decision
//	grep -E '^ADMIT .*project=<name>' ...  # one project's history
//
// The anchored grep works because the line is written prefix-free (see
// admitWriteLine): the daemon logs with LstdFlags|Lshortfile, so a
// log.Print-based line would start with "2026/09/18 02:28:57 loop.go:429: "
// and no anchored ADMIT pattern would ever match.
//
// The line is ADDITIVE: EVAL, EVAL-STALL and EVAL-ZERO-SELECT are untouched
// (operators grep ^EVAL). Every line starts with "ADMIT " and always carries
// project= and reason=. Fields:
//
//	pass_id           monotonic per-process evaluation-pass counter
//	eligible          candidates classified in this pass
//	                  (enabled AND not already in flight — see below)
//	admitted          candidates admitted (reason=ok) this pass
//	deferred          eligible-admitted
//	ns                the project's namespace ("" renders as "-")
//	cap               that namespace's max_concurrent (0 = unlimited)
//	inflight_running  ticks RUNNING in that namespace (DB count)
//	inflight_queued   spawn attempts past the namespace gate but not yet
//	                  running in that namespace (SlotPool claim count)
//	project           the project name (sanitized: whitespace/=/" -> "_",
//	                  truncated to 48 chars so the line stays < 250)
//	reason            one of the vocabulary strings below
//	cooldown_remaining_s  present only for reason=cooldown (seconds left)
//
// A project with a tick already in flight is NOT a candidate: it already
// spawned, it is not being passed over, and it gets no line (its own
// cooldown/timer will decide the next pass). "eligible" therefore counts
// projects eligible FOR A DECISION this pass, so
// eligible == admitted + deferred holds on every line of a pass.
//
// REASON VOCABULARY (exact strings) and the call site each one stands for:
//
//	ok              the packer selected the project and the spawn stage did
//	                not defer it — the project's tick is being fired.
//	cap             namespace at its max_concurrent (namespace_gate.go's
//	                gate, packer_select.go:276 / packer.go:300), or the
//	                GLOBAL slot cap consumed (packer_select.go:272
//	                globalRunning+globalSelected / packer.go:334
//	                currRunning). The vocabulary has no separate word for the
//	                global cap — "cap" is the closest, and the header's
//	                cap/inflight_* fields tell the reader which one bit
//	                (ns cap > 0 with inflight_running >= cap == the
//	                namespace gate).
//	load_gate       SlotPool.spawn deferred a SELECTED project because the
//	                1-minute load average is at/above --load-gate-threshold
//	                (load_gate.go, SCHED-GAP-125). Defer, not drop: the work
//	                stays selected and is re-picked once load drops.
//	cooldown        the project's own wall-clock pin has not elapsed
//	                (effectiveCooldown: cooldown_s, or the priority-derived
//	                dynamic interval when cooldown_s == 0, or the
//	                S-GAP-001 failure backoff, or a skip-mode blackout —
//	                packer_select.go:235 / packer.go:338/371). The only
//	                reason carrying cooldown_remaining_s.
//	tasks_no_work   tasks-mode project (SCHED-GAP-124) whose board IS owned
//	                by the lane but holds no non-perpetual open work
//	                (admission_mode.go tasksAdmissionDue), so the cooldown
//	                waiver does not fire.
//	board_unowned   tasks-mode project whose board resolves OUTSIDE its own
//	                workdir (admission_mode.go boardOwnedByLane /
//	                laneOwnsBoard, SCHED-GAP-141): a time-based lane. The
//	                waiver is refused and the cooldown pin decides — the
//	                reason names the refusal because that is the answer to
//	                "why is this tasks lane not running on work".
//	budget          a budget gate blocked the project: the SCHED-GAP-066
//	                per-project spend cap (daily/weekly/final, budget.go) or
//	                the weight-budget/namespace-allocation arithmetic that
//	                left no room in this pass (packer_select.go:281,
//	                packer.go:330). "budget" is the vocabulary entry for
//	                both — one is money, one is admission currency.
//	tasks_deferred  tasks-mode project that HAD admissible work and still
//	                was not admitted (the waiver was granted, so the block
//	                was downstream: SCHED-GAP-133 failure backoff >
//	                consecutive_failures 1, or the SCHED-GAP-136 post-tick
//	                pacing floor with jitter). This is also the residual for
//	                a tasks-mode project that failed every other check.
//
// The classification is a POST-HOC reconstruction from the same DB state
// the packer read (one SELECT over enabled projects + the per-namespace
// cap/inflight reads), in the order the live namespace-mode packer consults
// the gates: tasks-mode family first (waiver or refusal), then the project's
// own cooldown, then cap, then budget. A project blocked by several gates at
// once reports the FIRST one in that order — a single line per candidate is
// a hard contract (one decision per project per pass).

// Admission reason vocabulary (SCHED-GAP-155). These are literal, grep-stable
// strings shared with the admission_counters keys on /api/v1/status.
const (
	AdmissionReasonOK            = "ok"
	AdmissionReasonCap           = "cap"
	AdmissionReasonLoadGate      = "load_gate"
	AdmissionReasonCooldown      = "cooldown"
	AdmissionReasonTasksNoWork   = "tasks_no_work"
	AdmissionReasonBoardUnowned  = "board_unowned"
	AdmissionReasonBudget        = "budget"
	AdmissionReasonTasksDeferred = "tasks_deferred"
	// SCHED-GAP-214: a tasks-mode project whose last tick FAILED — the
	// SCHED-GAP-124 waiver stood down and the lane is pacing on its full
	// effective cooldown (the measured 91-ticks-in-90s retry storm this
	// closes). Carries cooldown_remaining_s like `cooldown`.
	AdmissionReasonFailedCooldown = "failed_cooldown"
)

// admissionReasonVocabulary is the frozen vocabulary in reporting order.
// Every entry is initialized to zero in the counters at boot and in every
// AdmissionCounters() snapshot, so a missing reason is never ambiguous.
var admissionReasonVocabulary = []string{
	AdmissionReasonOK,
	AdmissionReasonCap,
	AdmissionReasonLoadGate,
	AdmissionReasonCooldown,
	AdmissionReasonTasksNoWork,
	AdmissionReasonBoardUnowned,
	AdmissionReasonBudget,
	AdmissionReasonTasksDeferred,
	AdmissionReasonFailedCooldown,
}

// admissionReasonIsKnown reports whether reason is part of the vocabulary.
func admissionReasonIsKnown(reason string) bool {
	for _, r := range admissionReasonVocabulary {
		if r == reason {
			return true
		}
	}
	return false
}

// admissionCandidate is one enabled project as read for the admission log —
// only the fields the reason classification needs.
type admissionCandidate struct {
	Name                   string
	NS                     string
	Weight                 int
	CooldownS              int
	Priority               float64
	ConsecutiveFailures    int
	BumpActive             bool
	BumpCooldownS          int
	Workdir                string
	AdmissionMode          string // project override ('' = inherit)
	NamespaceAdmissionMode string // namespace default ('' = cooldown)
	BoardOwnership         string // '' = auto, 'owner', 'shared' (SCHED-GAP-141)
	LastCompleted          *time.Time
	// SCHED-GAP-214: terminal status of the most recent tick ("" = never).
	// Drives the failed_cooldown admission reason for tasks-mode lanes.
	LastTickStatus  string
	DailyBudgetUSD  float64
	WeeklyBudgetUSD float64
	FinalBudgetUSD  float64
}

// effectiveAdmissionMode resolves the candidate's admission mode exactly as
// the packers do (project override → namespace default → cooldown).
func (c admissionCandidate) effectiveAdmissionMode() string {
	return admissionModeFor(c.AdmissionMode, c.NS, map[string]string{c.NS: c.NamespaceAdmissionMode})
}

// admissionNSCounts is the per-namespace state the ADMIT header carries,
// read once per namespace per pass (not once per candidate).
type admissionNSCounts struct {
	cap            int  // namespaces.max_concurrent (0 = unlimited)
	inflightRun    int  // ticks RUNNING in the namespace (DB)
	inflightQueued int  // spawn attempts holding a namespace claim
	loadGateBlocks bool // SlotPool.spawn would defer a spawn into this ns
}

// admissionDecision is one project's decision in one evaluation pass.
type admissionDecision struct {
	Project            string
	NS                 string
	Reason             string
	Cap                int
	InflightRunning    int
	InflightQueued     int
	CooldownRemainingS float64
	HasCooldownRem     bool
}

// AdmissionCounters returns a snapshot of the SCHED-GAP-155 admission
// counters for /api/v1/status: one entry per reason in the vocabulary (all
// present, 0 when never seen), one "admitted:<namespace>" entry per
// namespace that admitted a tick (total admits per namespace), and "passes"
// (evaluation passes the ADMIT emitter ran in). Per-process and monotonic;
// a restart resets them, exactly like the spawn/gateway counters.
func (l *Loop) AdmissionCounters() map[string]int {
	l.admitMu.Lock()
	defer l.admitMu.Unlock()
	l.ensureAdmitCountersLocked()
	out := make(map[string]int, len(admissionReasonVocabulary)+len(l.admitNSAdmits)+1)
	for _, reason := range admissionReasonVocabulary {
		out[reason] = l.admitCounts[reason]
	}
	for ns, n := range l.admitNSAdmits {
		key := ns
		if key == "" {
			key = "-"
		}
		out["admitted:"+key] = n
	}
	out["passes"] = l.admitPasses
	return out
}

// ensureAdmitCountersLocked lazily initializes the counter maps so a Loop
// built as a struct literal (tests) behaves identically to a NewLoop one.
// Callers must hold admitMu.
func (l *Loop) ensureAdmitCountersLocked() {
	if l.admitCounts == nil {
		l.admitCounts = make(map[string]int, len(admissionReasonVocabulary))
		for _, reason := range admissionReasonVocabulary {
			l.admitCounts[reason] = 0
		}
	}
	if l.admitNSAdmits == nil {
		l.admitNSAdmits = make(map[string]int)
	}
}

// sanitizeAdmitField renders a project or namespace identifier safe for the
// key=value line: whitespace and the '=' / '"' delimiters become '_' so a
// reader can split on spaces, and the value is capped at 48 chars (the
// whole line then stays well under the ~250-char budget).
func sanitizeAdmitField(s string) string {
	if s == "" {
		return ""
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case ' ', '	', '\n', '\r', '"', '=':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return string(out)
}

// admitWriteLine writes one ADMIT line to the process logger's output
// destination with NO logger prefix.
//
// Why not log.Print: main.go installs log.SetFlags(LstdFlags|Lshortfile), so
// every log.Print line lands as
//
//	2026/09/18 02:28:57 loop.go:429: <message>
//
// — `grep -E '^ADMIT ' scheduler.log` could never match (verified live:
// `grep -c '^EVAL'` on the production scheduler.log is 0 for exactly this
// reason). The ADMIT line's whole point is the anchored grep, so it is
// written straight to log.Writer() — the same stdout + --log-file
// destination the logger uses, minus the prefix. ONE Write call per line so
// concurrent emitters never interleave within a line.
func admitWriteLine(line string) {
	w := log.Writer()
	if w == nil {
		return
	}
	_, _ = io.WriteString(w, line+"\n")
}

// emitAdmissionDecision writes ONE grep-stable ADMIT line for a single
// project decision and folds it into the counters. passID/eligible/admitted/
// deferred are the pass-level header values (identical on every line of a
// pass, so any single line answers the "why" question on its own).
//
// A missing reason is replaced with the vocabulary residual (a line must
// always carry a reason), but an UNKNOWN reason is emitted VERBATIM and NOT
// counted: a classification bug stays visible in the log instead of being
// rounded into a plausible-looking counter.
func (l *Loop) emitAdmissionDecision(passID, eligible, admitted, deferred int, d admissionDecision) {
	ns := sanitizeAdmitField(d.NS)
	if ns == "" {
		ns = "-"
	}
	reason := d.Reason
	if reason == "" {
		reason = AdmissionReasonTasksDeferred
	}
	line := fmt.Sprintf("ADMIT pass_id=%d eligible=%d admitted=%d deferred=%d ns=%s cap=%d inflight_running=%d inflight_queued=%d project=%s reason=%s",
		passID, eligible, admitted, deferred, ns, d.Cap, d.InflightRunning, d.InflightQueued,
		sanitizeAdmitField(d.Project), reason)
	if d.HasCooldownRem {
		line += fmt.Sprintf(" cooldown_remaining_s=%.1f", d.CooldownRemainingS)
	}
	admitWriteLine(line)

	// SCHED-GAP-157: the pass-over decision is persisted, not just logged —
	// "why was this lane skipped in window Y" becomes one SQL query over the
	// deferrals table (the row shape is justified in schedgap157.go: a
	// pass-over has no tick row, and a pseudo-row in ticks would corrupt the
	// packer's cooldown clock). Detail mirrors the line's pass header so the
	// row is self-contained. Best-effort: persistence failure is logged and
	// the decision is unchanged.
	detail := fmt.Sprintf("pass_id=%d eligible=%d admitted=%d deferred=%d ns=%s", passID, eligible, admitted, deferred, ns)
	l.recordDeferral(d.Project, reason, int64(passID), detail)

	if !admissionReasonIsKnown(reason) {
		return // emitted for diagnosis; not folded into the counters
	}
	l.admitMu.Lock()
	defer l.admitMu.Unlock()
	l.ensureAdmitCountersLocked()
	l.admitCounts[reason]++
	if reason == AdmissionReasonOK {
		l.admitNSAdmits[d.NS]++
	}
}

// admissionCandidates reads every enabled project with the fields the
// admission classification needs, in the same shape the packers see them.
func (l *Loop) admissionCandidates(ctx context.Context) ([]admissionCandidate, error) {
	rows, err := l.db.QueryContext(ctx, `
SELECT p.name, COALESCE(p.namespace_id, ''), COALESCE(p.weight, 0), COALESCE(p.cooldown_s, 0),
       COALESCE(p.priority, 0), COALESCE(p.consecutive_failures, 0),
       COALESCE(p.bump_active, 0), COALESCE(p.bump_cooldown_s, 0),
       COALESCE(p.workdir, ''), COALESCE(p.admission_mode, ''),
       COALESCE(ns.admission_mode, ''), COALESCE(p.board_ownership, ''),
       COALESCE(p.last_tick_completed, ''), COALESCE(p.last_tick_status, ''),
       COALESCE(p.daily_budget_usd, 0.0), COALESCE(p.weekly_budget_usd, 0.0), COALESCE(p.final_budget_usd, 0.0)
FROM projects p
LEFT JOIN namespaces ns ON ns.id = p.namespace_id
WHERE p.enabled = 1
ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []admissionCandidate
	for rows.Next() {
		var c admissionCandidate
		var lastStr string
		if err := rows.Scan(&c.Name, &c.NS, &c.Weight, &c.CooldownS,
			&c.Priority, &c.ConsecutiveFailures,
			&c.BumpActive, &c.BumpCooldownS,
			&c.Workdir, &c.AdmissionMode,
			&c.NamespaceAdmissionMode, &c.BoardOwnership,
			&lastStr, &c.LastTickStatus,
			&c.DailyBudgetUSD, &c.WeeklyBudgetUSD, &c.FinalBudgetUSD); err != nil {
			log.Printf("ADMIT: scan candidate row: %v", err)
			continue
		}
		if lastStr != "" {
			if t, err := time.Parse(time.RFC3339, lastStr); err == nil {
				c.LastCompleted = &t
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// admissionRunningSet is the conservative in-flight view for the admission
// log: the slot pool's running+reserved set (SCHED-GAP-103) merged with the
// DB's running tick rows (SCHED-GAP-030 — survivors from before a restart),
// the same union evaluate() dedups against.
func (l *Loop) admissionRunningSet() map[string]bool {
	set := make(map[string]bool)
	if l.slotPool != nil {
		for name := range l.slotPool.RunningSet() {
			set[name] = true
		}
	}
	if l.db != nil {
		rows, err := l.db.QueryContext(context.Background(),
			`SELECT DISTINCT project_name FROM ticks WHERE status = 'running'`)
		if err == nil {
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err == nil {
					set[name] = true
				}
			}
			rows.Close()
		}
	}
	return set
}

// admissionNamespaceQueued reports the namespace's in-flight spawn attempts
// that hold a claim but whose tick row is not yet running — the
// inflight_queued field of the ADMIT header (SCHED-GAP-144's claim map).
func (l *Loop) admissionNamespaceQueued(nsID string) int {
	if l.slotPool == nil || nsID == "" {
		return 0
	}
	return l.slotPool.NamespacePending(nsID)
}

// cooldownVerdict resolves whether the candidate is still inside its
// wall-clock pin, and (when it is) the seconds remaining. Mirrors the
// packer's gate exactly: the shared effectiveCooldown predicate, the
// SCHED-GAP-107 bump substitution for an active bump, a skip-mode blackout
// reported as deferred with no countdown (it never elapses), and a project
// that never completed treated as not cooldown-blocked (mirror of
// countEligibleProjects).
func (l *Loop) cooldownVerdict(c admissionCandidate, now time.Time) (deferred bool, remainingS float64, hasRemaining bool) {
	if c.LastCompleted == nil {
		return false, 0, false
	}
	var windows []config.BlackoutWindow
	if l.packer != nil {
		windows = l.packer.blackoutWindows
	}
	cd := c.CooldownS
	if c.BumpActive && c.BumpCooldownS > 0 {
		cd = c.BumpCooldownS
	}
	cooldownDur, skipMode := effectiveCooldown(cd, c.Priority, c.ConsecutiveFailures, windows, now, l.calculator)
	if skipMode {
		return true, 0, false // skip-mode blackout: never eligible, no countdown
	}
	age := now.Sub(*c.LastCompleted)
	if age >= cooldownDur {
		return false, 0, false
	}
	return true, (cooldownDur - age).Seconds(), true
}

// admissionSpendBlocked reports whether the SCHED-GAP-066 per-project spend
// gate blocks the candidate this cycle (nil gate / no caps = never blocked).
func (l *Loop) admissionSpendBlocked(c admissionCandidate) bool {
	if l.packer == nil || l.packer.budgetGate == nil {
		return false
	}
	_, blocked := l.packer.budgetGate(c.Name, c.DailyBudgetUSD, c.WeeklyBudgetUSD, c.FinalBudgetUSD)
	return blocked
}

// admissionStructuralDeferral maps the structural gates (concurrency, then
// budget) onto the vocabulary for a candidate the packers did not select.
// Returns "" when no structural gate explains the deferral.
//
// globalRunning is the in-flight count and globalSelected the projects THIS
// pass already packed — together they are the packer's own global-cap
// arithmetic (`globalRunning+globalSelected >= maxConcurrent`,
// packer_select.go; `currRunning >= maxConcurrent`, packer.go), so a pass
// that filled every slot before reaching this candidate reports "cap"
// instead of falling through to the budget residual.
//
// packedWeight is the weight this pass already consumed, so the weight-budget
// test is "would this project still fit", the same arithmetic packer.go's
// greedy pack applies (an approximation in namespace mode, where the packer
// spends EFFECTIVE weights against a per-namespace allocation — see the
// residual note on the vocabulary above).
func (l *Loop) admissionStructuralDeferral(c admissionCandidate, st admissionNSCounts, packedWeight, globalRunning, globalSelected int) string {
	if st.cap > 0 && st.inflightRun >= st.cap {
		return AdmissionReasonCap
	}
	if l.maxConcur > 0 && globalRunning+globalSelected >= l.maxConcur {
		return AdmissionReasonCap
	}
	if l.admissionSpendBlocked(c) {
		return AdmissionReasonBudget
	}
	if c.Weight > 0 && packedWeight+c.Weight > l.weightBudget {
		return AdmissionReasonBudget
	}
	return ""
}

// classifyAdmissionDeferral maps a candidate that was NOT admitted onto one
// vocabulary reason. Order (see the block comment above): the tasks-mode
// family, then the candidate's own cooldown, then the structural gates, then
// the residual.
func (l *Loop) classifyAdmissionDeferral(c admissionCandidate, now time.Time, st admissionNSCounts, packedWeight, globalRunning, globalSelected int) (reason string, remainingS float64, hasRemaining bool) {
	if c.effectiveAdmissionMode() == database.AdmissionModeTasks {
		// The SCHED-GAP-124 waiver: pending non-perpetual board work waives
		// the cooldown pin — but ONLY for a lane that owns the board it
		// reads (SCHED-GAP-141). Whichever half fails names the reason.
		if !boardOwnedByLane(c.Workdir, c.BoardOwnership) {
			return AdmissionReasonBoardUnowned, 0, false
		}
		if open, ok := boardOpenRows(c.Workdir); !ok || open == 0 {
			return AdmissionReasonTasksNoWork, 0, false
		}
		// Waiver granted: the block (if any) is structural, the
		// SCHED-GAP-133/136 floors downstream, or the SCHED-GAP-214
		// post-failure stand-down (last tick FAILED → full effective
		// cooldown; the measured 9s retry storm this reason names).
		if r := l.admissionStructuralDeferral(c, st, packedWeight, globalRunning, globalSelected); r != "" {
			return r, 0, false
		}
		if lastTickStatusFailed(c.LastTickStatus) && c.LastCompleted != nil {
			if deferred, rem, hasRem := l.cooldownVerdict(c, now); deferred {
				return AdmissionReasonFailedCooldown, rem, hasRem
			}
		}
		return AdmissionReasonTasksDeferred, 0, false
	}

	// Cooldown mode: the wall-clock pin is the first gate the live packer
	// consults, so report it first.
	if deferred, rem, hasRem := l.cooldownVerdict(c, now); deferred {
		return AdmissionReasonCooldown, rem, hasRem
	}
	if r := l.admissionStructuralDeferral(c, st, packedWeight, globalRunning, globalSelected); r != "" {
		return r, 0, false
	}
	// Residual: nothing else matched, so the packer skipped it on the
	// budget/allocation arithmetic ("budget" is the closest vocabulary
	// entry — weight budget or namespace allocation exhausted).
	return AdmissionReasonBudget, 0, false
}

// emitAdmissionPass classifies every candidate project in one evaluation
// pass and emits one ADMIT line per candidate. Called from evaluate() with
// the selection resolved (both packer paths done) and BEFORE anything is
// spawned, so the emitted decision is the decision the pass acted on.
// Returns the pass id the decisions were emitted under (0 = the pass bailed
// before emitting — no candidate query, no lines) so SCHED-GAP-1582's
// budget-hold records can share the same pass id.
//
// Callers hold l.mu (evaluate does); this function never locks it (a second
// acquisition would deadlock) and only touches admitMu, the slot pool's own
// mutex and the DB.
func (l *Loop) emitAdmissionPass(now time.Time, packed []PackedProject) int64 {
	if l.db == nil {
		return 0
	}
	cands, err := l.admissionCandidates(context.Background())
	if err != nil {
		log.Printf("ADMIT: candidate query failed: %v — no admission lines this pass", err)
		return 0
	}

	packedNames := make(map[string]bool, len(packed))
	packedWeight := 0
	for _, p := range packed {
		packedNames[p.Name] = true
		packedWeight += p.Weight
	}
	running := l.admissionRunningSet()
	globalRunning := len(running)
	globalSelected := len(packed)

	l.admitMu.Lock()
	l.ensureAdmitCountersLocked()
	l.admitPasses++
	passID := l.admitPasses
	l.admitMu.Unlock()

	nsStats := make(map[string]admissionNSCounts)
	decisions := make([]admissionDecision, 0, len(cands))
	for _, c := range cands {
		// A tick already in flight is not a candidate: it already spawned
		// and nothing passed it over this pass (no line).
		if running[c.Name] {
			continue
		}
		st, ok := nsStats[c.NS]
		if !ok {
			st = admissionNSCounts{
				cap:            namespaceCapDB(l.db, c.NS),
				inflightRun:    namespaceRunningDB(l.db, c.NS),
				inflightQueued: l.admissionNamespaceQueued(c.NS),
				loadGateBlocks: LoadGateShouldDefer(l.db, c.NS),
			}
			nsStats[c.NS] = st
		}
		d := admissionDecision{
			Project:         c.Name,
			NS:              c.NS,
			Cap:             st.cap,
			InflightRunning: st.inflightRun,
			InflightQueued:  st.inflightQueued,
		}
		if packedNames[c.Name] {
			// Selected by the packer. The load gate can still defer the
			// spawn (SCHED-GAP-171: it now runs in the evaluate LOOP, just
			// below the emitAdmissionPass call) — that deferral IS the
			// answer to "why is this not running", so it is its own reason.
			if st.loadGateBlocks {
				d.Reason = AdmissionReasonLoadGate
			} else {
				d.Reason = AdmissionReasonOK
			}
		} else {
			d.Reason, d.CooldownRemainingS, d.HasCooldownRem =
				l.classifyAdmissionDeferral(c, now, st, packedWeight, globalRunning, globalSelected)
		}
		decisions = append(decisions, d)
	}

	admitted := 0
	for _, d := range decisions {
		if d.Reason == AdmissionReasonOK {
			admitted++
		}
	}
	eligible := len(decisions)
	deferred := eligible - admitted
	for _, d := range decisions {
		l.emitAdmissionDecision(passID, eligible, admitted, deferred, d)
	}
	return int64(passID)
}
