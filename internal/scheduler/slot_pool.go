package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SlotPool manages concurrent tick slots using a buffered channel as a
// counting semaphore. Projects acquire a slot before spawning and release it
// when the tick completes or times out. The evaluation loop fires projects
// into the pool and returns immediately — it never blocks waiting for spawns.
//
// SCHED-GAP-021: the channel is a pure COUNTING semaphore (chan struct{});
// the set of project names occupying slots lives in a mutex-protected
// refcount map. A name-keyed channel cannot support "remove THIS project's
// marker" — drain-and-refill races a full semaphore (all receivers blocked
// on re-push = deadlock) and temporarily pulls tokens out of circulation
// (over-admission). The refcount map makes Release(name) exact.
type SlotPool struct {
	sem       chan struct{} // buffered channel = counting semaphore
	maxSlots  int
	spawner   *Spawner
	lifecycle *LifecycleTracker
	freedCh   chan struct{} // fires when a slot is released (single goroutine, no leak)

	// patience is how long a spawn waits for a free slot before the
	// project is dropped (ADV-R08/G3). Zero means the default
	// (defaultSlotPatience); see SetPatience. Guarded by mu — written by
	// startup setters, read by spawn goroutines.
	patience time.Duration

	// events optionally receives the slot-drop event (ADV-R08/G3). Nil
	// (the default) disables emission — the pool must work without a
	// logger. Mirrors Spawner.events.
	events *EventLogger

	// running maps project name -> number of slots it holds. Guarded by mu;
	// the mutex never covers a blocking channel wait (Acquire blocks on the
	// channel itself), so it serializes only the tiny map critical sections.
	mu      sync.Mutex
	running map[string]int

	// reserved holds project names claimed by an in-flight Spawn whose
	// goroutine has not yet been tracked by `running`. SCHED-GAP-103: Spawn
	// is fire-and-forget, so between the caller launching the goroutine and
	// that goroutine calling Acquire there is a window in which the project
	// looks idle to RunningSet — a second eval cycle (slot-freed debounce or
	// ForceEvaluate) in that window double-spawned the project. Guarded by
	// mu, same critical-section discipline as running.
	reserved map[string]bool

	// nsPending holds, per namespace, the spawn attempts that have passed the
	// namespace-cap gate but whose tick row is not yet `running` — the
	// SCHED-GAP-103 window in which the project would otherwise look
	// namespace-idle. The claim is dropped the moment the row starts running,
	// so a running tick is never double-counted and the effective cap stays
	// exact. Guarded by mu, same discipline as running/reserved.
	// SCHED-GAP-144.
	nsPending map[string]int
}

// NewSlotPool creates a slot pool with at most maxConcurrent active ticks.
func NewSlotPool(maxConcurrent int, spawner *Spawner, lifecycle *LifecycleTracker) *SlotPool {
	p := &SlotPool{
		sem:       make(chan struct{}, maxConcurrent),
		maxSlots:  maxConcurrent,
		spawner:   spawner,
		lifecycle: lifecycle,
		freedCh:   make(chan struct{}, maxConcurrent),
		running:   make(map[string]int),
		reserved:  make(map[string]bool),
		nsPending: make(map[string]int),
	}
	return p
}

// defaultSlotPatience is the historical hardcoded slot-wait window: how long
// a spawn goroutine waits for a free slot before the project is dropped
// (ADV-R08/G3). The default keeps production cadence byte-identical — the
// drop itself is unchanged; only its observability and configurability are
// new. Dropping must ALWAYS exist (a full fleet cannot queue spawns
// unboundedly), so the patience can never be disabled, only shortened or
// lengthened by an operator via --slot-patience / SCHEDULER_SLOT_PATIENCE /
// scheduler.slot_patience.
const defaultSlotPatience = 5 * time.Minute

// SetPatience sets how long a spawn waits for a free slot before being
// dropped (ADV-R08/G3). A d <= 0 means "keep the default" (5m) — it does NOT
// mean "never drop": the drop is the pool's backstop against unbounded
// queueing and must always exist. Must be called before Run()/the first
// Spawn (the daemon wires it during startup, same contract as the other
// startup setters).
func (p *SlotPool) SetPatience(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d <= 0 {
		return // keep the default / current value
	}
	p.patience = d
}

// Patience returns the effective slot-wait patience (ADV-R08/G3): the value
// set by SetPatience, or defaultSlotPatience when unset. Used by tests and
// by the resolved-config verification.
func (p *SlotPool) Patience() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.patience <= 0 {
		return defaultSlotPatience
	}
	return p.patience
}

// SetEventLogger wires an optional EventLogger for the slot-drop event
// (ADV-R08/G3), mirroring Spawner.SetEventLogger. Nil (the default)
// disables emission — no event, no panic.
func (p *SlotPool) SetEventLogger(el *EventLogger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = el
}

// Available returns the number of free slots.
func (p *SlotPool) Available() int {
	return p.maxSlots - len(p.sem)
}

// Running returns the number of currently occupied slots.
func (p *SlotPool) Running() int {
	return len(p.sem)
}

// RunningSet returns the set of project names currently occupying slots —
// plus names RESERVED by an in-flight Spawn that has not acquired its slot
// yet (SCHED-GAP-103). Used by the packer and the evaluation-loop dedup to
// prevent duplicate spawns, so it must be the conservative view: a project
// waiting on a slot is still "already scheduled" and must not be re-fired.
func (p *SlotPool) RunningSet() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := make(map[string]bool, len(p.running)+len(p.reserved))
	for name := range p.running {
		set[name] = true
	}
	for name := range p.reserved {
		set[name] = true
	}
	return set
}

// tryReserve atomically checks-and-sets a project name as reserved for
// spawning. Returns true if the project was NOT already running or reserved
// (caller should proceed to spawn). Returns false if the project already
// holds a slot or has been reserved by an in-flight Spawn call — the caller
// must skip (SCHED-GAP-103: the TOCTOU window between Spawn() returning
// and the goroutine calling Acquire() allowed a second eval cycle to
// double-spawn the same project).
func (p *SlotPool) tryReserve(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[name] > 0 || p.reserved[name] {
		return false
	}
	p.reserved[name] = true
	return true
}

// clearReserve removes a reservation. Called by the spawn goroutine on EVERY
// exit path (slot timeout, enqueue failure, spawn failure, normal
// completion) via a deferred call, so a reservation can never outlive the
// attempt that made it. Deferred panics still run defers, so a panic inside
// the goroutine does not leak the reservation either.
func (p *SlotPool) clearReserve(name string) {
	p.mu.Lock()
	delete(p.reserved, name)
	p.mu.Unlock()
}

// Acquire blocks until a slot is free, then marks it occupied with the
// given project name. Returns false if context is cancelled.
func (p *SlotPool) Acquire(ctx context.Context, name string) bool {
	select {
	case p.sem <- struct{}{}:
		p.mu.Lock()
		p.running[name]++
		p.mu.Unlock()
		return true
	case <-ctx.Done():
		return false
	}
}

// Release frees one slot held by the named project and signals SlotFreed.
// SCHED-GAP-021: release is project-scoped — a completing tick removes ONLY
// its own marker. Releasing a name that holds no slot is a no-op: it must
// never free another project's marker (the old FIFO release popped the oldest
// acquisition, evicting still-running projects from RunningSet and letting
// EVAL spawn duplicate concurrent ticks — ring-runner 2026-08-09).
func (p *SlotPool) Release(name string) {
	p.mu.Lock()
	if p.running[name] == 0 {
		p.mu.Unlock()
		return
	}
	p.running[name]--
	if p.running[name] == 0 {
		delete(p.running, name)
	}
	// The refcount said a slot was held, so a token is waiting in the
	// semaphore — except when ReleaseAll already drained it (its drain and
	// a racing Acquire's push+refcount are not atomic). Both cases leave
	// the count consistent, so a non-blocking receive is exact.
	select {
	case <-p.sem:
	default:
	}
	p.mu.Unlock()
	select {
	case p.freedCh <- struct{}{}:
	default:
	}
}

// ReleaseAll drains all currently-held slots and signals SlotFreed
// for each one released. Safe to call when no slots are held.
func (p *SlotPool) ReleaseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		select {
		case <-p.sem:
			select {
			case p.freedCh <- struct{}{}:
			default:
			}
		default:
			p.running = make(map[string]int)
			// SCHED-GAP-103: reservations belong to the in-flight spawn
			// attempts that ReleaseAll is abandoning (gateway dead → every
			// spawn from this cycle is void). Clearing them keeps the next
			// eval cycle from being blocked by phantom reservations.
			p.reserved = make(map[string]bool)
			return
		}
	}
}

// Spawn fires a project tick in a new goroutine. The goroutine acquires a
// slot from the pool, spawns via the gateway, and releases the slot on
// completion or timeout. Delivery and auto-slowdown are integrated.
// Spawn returns immediately — it is fire-and-forget.
//
// DOGFOOD-015: the tick id is generated HERE via database.NextTickID (the
// canonical UTC generator) instead of formatting the caller's local-time
// `now` — the old `now.Format("2006-01-02-15-04-05")` stamped rows with
// LOCAL time while the API spawn handler predicted UTC ids, so a returned
// tick_id could never resolve via GET /ticks/{id} on a non-UTC host.
// The generated id is returned so callers can correlate the row.
func (p *SlotPool) Spawn(proj PackedProject, now time.Time, noDeliver bool, db *sql.DB) string {
	tickID := database.NextTickID(proj.Name)
	p.spawn(proj, tickID, now, noDeliver, db, false)
	return tickID
}

// SpawnEnqueued fires a project tick whose row was ALREADY enqueued (by the
// caller, e.g. Loop.SpawnNow for the API spawn endpoint) into the slot pool.
// The goroutine acquires a slot, transitions the row to running, spawns via
// the gateway, and releases the slot on completion or timeout. The tick id
// must match the enqueued row — the caller generated it with
// database.NextTickID. Returns immediately — fire-and-forget.
func (p *SlotPool) SpawnEnqueued(proj PackedProject, tickID string, now time.Time, noDeliver bool, db *sql.DB) {
	p.spawn(proj, tickID, now, noDeliver, db, true)
}

// spawn is the shared goroutine body for Spawn and SpawnEnqueued. When
// enqueued is true the row already exists (status queued) and the goroutine
// only transitions it to running; otherwise it enqueues first.
func (p *SlotPool) spawn(proj PackedProject, tickID string, now time.Time, noDeliver bool, db *sql.DB, enqueued bool) {
	// SCHED-GAP-125: load-average gate (Bane 2026-09-16). Opt-in via
	// --load-gate-threshold (0 = off, byte-identical fleet); namespaces
	// opt out per-namespace with load_gate='off' (migration v31, threader
	// as LoadGateBypass by the packer). G7 ruling placement: admission
	// gates live in SlotPool.spawn. DEFER, not drop: the project keeps its
	// selection — the next evaluation re-picks it once load drops; no
	// cooldown is consumed and no progress penalty is recorded.
	if LoadGateShouldDefer(db, proj.NamespaceID) {
		l1, _ := currentLoad1m()
		log.Printf("LOAD-GATE: deferring %s (tick %s) — load %.2f >= threshold %.2f (work stays queued)",
			proj.Name, tickID, l1, loadGateThreshold())
		p.mu.Lock()
		events := p.events
		p.mu.Unlock()
		if events != nil {
			events.Emit(context.Background(), SeverityInfo, "slot_pool",
				"load gate deferred "+proj.Name,
				map[string]any{
					"project":   proj.Name,
					"tick_id":   tickID,
					"load_1m":   l1,
					"threshold": loadGateThreshold(),
				})
		}
		return
	}

	// SCHED-GAP-103: atomically check-and-reserve BEFORE launching the
	// goroutine. Spawn is fire-and-forget: the caller launches the goroutine
	// and returns, but the goroutine only becomes visible to RunningSet when
	// its Acquire() call lands. A second evaluation cycle in that window
	// (slot-freed debounce or ForceEvaluate) saw the project as idle and
	// fired a second Spawn for it — two concurrent ticks on one project,
	// bypassing cooldown (asce-qa: 55s gap against a 43200s cooldown;
	// crier-sync: 2s gap). Reserve here so the project is deduped from the
	// instant the caller decides to spawn it.
	if !p.tryReserve(proj.Name) {
		log.Printf("DEDUP: skipping %s (tick %s) — already running or reserved", proj.Name, tickID)
		return
	}

	go func() {
		// Clear the reservation on EVERY exit path. Deferred at goroutine
		// entry so it survives panics too; LIFO ordering means the
		// p.Release below runs FIRST on normal completion (dropping the
		// running refcount, which is what keeps RunningSet honest for a
		// completed tick) and the reservation is dropped last. On an early
		// exit before Acquire succeeds, Release is a no-op (running[name]
		// == 0) and clearReserve alone frees the claim.
		defer p.clearReserve(proj.Name)
		defer p.Release(proj.Name)

		// SCHED-GAP-144: namespace-cap admission at the DECLARED admission
		// point (G7). The packer and the orphan re-nudge both gate on the
		// namespace cap; this is the backstop that makes the cap authoritative
		// for every other entry into the pool (API spawn endpoint, wave
		// resume, queue replay/continuation). Measured defect it closes: the
		// 2026-09-17 23:51 restart admitted 3 duckbrain-sync lanes against a
		// cap of 1 together with all 8 foremen, taking every global slot.
		// DEFER, not drop: the row stays queued and is retried — no cooldown
		// is consumed and the lane records no failure for a busy fleet.
		nsID := proj.NamespaceID
		nsClaimed := false
		releaseNsClaim := func() {
			if nsClaimed {
				p.releaseNamespaceSlot(nsID)
				nsClaimed = false
			}
		}
		defer releaseNsClaim()
		if !namespaceCapGateDisabled() {
			if nsCap := namespaceCapDB(db, nsID); nsCap > 0 {
				nsStart := time.Now()
				nsCtx, nsCancel := context.WithTimeout(context.Background(), defaultNamespaceSlotPatience)
				ok := p.waitNamespaceSlot(nsCtx, nsID, db)
				nsCancel()
				if !ok {
					p.logNamespaceDeferral(proj, tickID, nsCap, namespaceRunningDB(db, nsID), time.Since(nsStart))
					return
				}
				nsClaimed = true
			}
		}

		// Wait for a free slot. The patience is configurable
		// (ADV-R08/G3); the default keeps the historical 5-minute
		// window byte-identical.
		waitStart := time.Now()
		patience := p.Patience()
		ctx, cancel := context.WithTimeout(context.Background(), patience)
		defer cancel()
		if !p.Acquire(ctx, proj.Name) {
			waited := time.Since(waitStart)
			log.Printf("SLOT: timeout waiting for free slot — dropping %s", proj.Name)
			// ADV-R08/G3: the drop was previously invisible to the
			// events API — whether it had ever fired was unknowable.
			// MEDIUM, matching the eval-stall demotion precedent
			// (SCHED-GAP-061): a drop means work the evaluator
			// selected never ran — visible, but not an alarm. Nil
			// logger = no event, no panic.
			p.mu.Lock()
			events := p.events
			p.mu.Unlock()
			if events != nil {
				events.Emit(context.Background(), SeverityMedium, "slot_pool",
					"slot wait expired — dropped "+proj.Name,
					map[string]any{
						"project":          proj.Name,
						"tick_id":          tickID,
						"waited_seconds":   waited.Seconds(),
						"patience_seconds": patience.Seconds(),
						"max_slots":        p.maxSlots,
						"running":          p.Running(),
					})
			}
			return
		}

		log.Printf("SLOT: acquired for %s (%d/%d running)", proj.Name, p.Running(), p.maxSlots)

		// Enqueue and start.
		if !enqueued {
			if err := p.lifecycle.Enqueue(proj.Name, tickID); err != nil {
				log.Printf("SPAWN: enqueue %s: %v", proj.Name, err)
				return
			}
		}
		if err := p.lifecycle.StartRunning(tickID); err != nil {
			log.Printf("SPAWN: start %s: %v", proj.Name, err)
			return
		}
		// SCHED-GAP-144: the row is `running` now, so the DB carries this
		// tick's namespace occupancy — release the in-process claim to keep
		// the cap exact (a held claim plus a running row would double-count).
		releaseNsClaim()
		// SCHED-GAP-107: flag the tick as a bump tick when the project has
		// an active bump — the flag both marks it for yield analysis and
		// makes its completion consume one bump tick.
		markBumpTick(db, proj.Name, tickID)

		// Spawn.
		st, err := p.spawner.Spawn(proj, tickID)
		if err != nil {
			log.Printf("SPAWN: %s failed: %v", proj.Name, err)
			// Finished MUST be set: lifecycle.Complete persists it as
			// completed_at, and the packer's cooldown/backoff/starvation logic
			// keys off that timestamp. Leaving it zero ("0001-01-01") froze the
			// last-attempt clock and let spawn failures storm (S-GAP-001).
			_ = p.lifecycle.Complete(TickOutcome{
				TickID:   tickID,
				Project:  proj.Name,
				Started:  now,
				Finished: time.Now(),
				Status:   TickFailed,
				Error:    err.Error(),
			})
			return
		}

		// Wait for completion or timeout.
		outcome := st.Wait()
		if err := p.lifecycle.Complete(outcome); err != nil {
			log.Printf("SPAWN: complete %s: %v", tickID, err)
		}

		// SCHED-GAP-110: ingest the foreman's wave manifest (S12 §9.3) now
		// that the tick row is terminal — ticks.worker_count + tick_workers
		// rows in one transaction. Runs for completed AND timed-out ticks
		// (the same gate resolveRealTickCost uses inside Wait): a timed-out
		// wave still spent its workers, and attribution rows are what the
		// reaper later marks abandoned. Best-effort by contract: a manifest
		// problem costs at most one WARN event and can never change the
		// tick's status, outcome, cost, or anything after this point — the
		// outcome was already persisted above.
		if db != nil && proj.Workdir != "" &&
			(outcome.Status == TickCompleted || outcome.Status == TickTimeout) {
			n, err := ingestWaveManifest(context.Background(), db, proj.Workdir, outcome.Project, outcome.TickID)
			if err != nil {
				// Infrastructure fault only (tx begin/commit); parse
				// problems were already surfaced as WARN events inside.
				log.Printf("WARN [wave]: manifest ingest %s: %v", outcome.TickID, err)
			} else if n > 0 {
				log.Printf("WAVE: %s tick=%s ingested %d worker rows", outcome.Project, outcome.TickID, n)
			}
			// SCHED-GAP-115 (S12 §11): per-worker cost attribution on the
			// same completion hook as ingest. Runs only when ingest left
			// worker rows (a serial tick is a no-op — its cost path stays
			// byte-identical to pre-115). W4: tick_workers.cost_usd is
			// attribution only; ticks.cost_usd gains JUST the worktree-side
			// GitReins judge cost — new money invisible to
			// resolveRealTickCost, never the manifest's model-cost figures.
			// Same fail-safe contract as ingest: a fault here is logged and
			// can never fail the completion path.
			if _, err := attributeTickWorkers(context.Background(), db, outcome.TickID, outcome.Started, outcome.Finished); err != nil {
				log.Printf("WARN [wave]: cost attribution %s: %v", outcome.TickID, err)
			}
		}

		// Deliver output (suppressed in test-verify mode).
		if !noDeliver {
			deliverOutput(outcome.Project, outcome.TickID, st.Deliver, st.Trigger, &st.Output)
		}

		// Auto-slowdown: if tick signals IDLE, gently slow down.
		// Adaptive cooldown (opt-in per project) takes precedence when
		// enabled — it accounts for the tick outcome itself (commits + board
		// row growth) instead of parsing the VERDICT line, and escalates
		// well past autoSlowdown's 1h operator-set guard, so the two must
		// never both run on the same tick.
		// SCHED-GAP-107: bump accounting runs FIRST. When it performs the
		// Phase A revert, the adaptiveCooldown call below doubles as the
		// Phase B re-evaluation against the restored baseline.
		if db != nil {
			bumpTickCompleted(db, outcome.Project, proj.Workdir, outcome)
			if !adaptiveCooldown(db, outcome.Project, proj.Workdir, outcome) {
				autoSlowdown(db, outcome.Project, &st.Output)
			}
		}

		// Timeout notification: log and alert, but do NOT back off.
		// The project is still eligible after its normal cooldown.
		if outcome.Status == TickTimeout {
			log.Printf("TIMEOUT: %s tick=%s duration=%v — project stays active, normal cooldown applies",
				outcome.Project, outcome.TickID, outcome.Duration)
			// Deliver timeout alert to chat so it's visible.
			if !noDeliver && st.Deliver != "" {
				deliverAlert(st.Deliver, outcome.Project, outcome.TickID, "timeout after "+outcome.Duration.String())
			}
		}
	}()
}

// SlotFreed returns a channel that receives when any slot is released.
// The channel is backed by a single goroutine (created in NewSlotPool) —
// no leaks. Use with debounce in the eval loop to avoid feedback floods.
func (p *SlotPool) SlotFreed() <-chan struct{} {
	return p.freedCh
}

// Wait blocks until all running ticks finish or the context is cancelled.
func (p *SlotPool) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if p.Running() == 0 {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
