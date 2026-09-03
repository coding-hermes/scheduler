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
	timeout   time.Duration
	spawner   *Spawner
	lifecycle *LifecycleTracker
	freedCh   chan struct{} // fires when a slot is released (single goroutine, no leak)

	// running maps project name -> number of slots it holds. Guarded by mu;
	// the mutex never covers a blocking channel wait (Acquire blocks on the
	// channel itself), so it serializes only the tiny map critical sections.
	mu      sync.Mutex
	running map[string]int
}

// NewSlotPool creates a slot pool with at most maxConcurrent active ticks.
func NewSlotPool(maxConcurrent int, timeout time.Duration, spawner *Spawner, lifecycle *LifecycleTracker) *SlotPool {
	p := &SlotPool{
		sem:       make(chan struct{}, maxConcurrent),
		maxSlots:  maxConcurrent,
		timeout:   timeout,
		spawner:   spawner,
		lifecycle: lifecycle,
		freedCh:   make(chan struct{}, maxConcurrent),
		running:   make(map[string]int),
	}
	return p
}

// Available returns the number of free slots.
func (p *SlotPool) Available() int {
	return p.maxSlots - len(p.sem)
}

// Running returns the number of currently occupied slots.
func (p *SlotPool) Running() int {
	return len(p.sem)
}

// RunningSet returns the set of project names currently occupying slots.
// Used by the packer to prevent duplicate spawns.
func (p *SlotPool) RunningSet() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := make(map[string]bool, len(p.running))
	for name := range p.running {
		set[name] = true
	}
	return set
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
	go func() {
		// Wait for a free slot.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if !p.Acquire(ctx, proj.Name) {
			log.Printf("SLOT: timeout waiting for free slot — dropping %s", proj.Name)
			return
		}
		defer p.Release(proj.Name)

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
		if db != nil {
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
