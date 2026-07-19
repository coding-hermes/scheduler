package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// SlotPool manages concurrent tick slots using a buffered channel as a
// semaphore. Projects acquire a slot before spawning and release it when
// the tick completes or times out. The evaluation loop fires projects into
// the pool and returns immediately — it never blocks waiting for spawns.
type SlotPool struct {
	sem       chan string // buffered channel = semaphore, value = project name
	maxSlots  int
	timeout   time.Duration
	spawner   *Spawner
	lifecycle *LifecycleTracker
	freedCh   chan struct{} // fires when a slot is released (single goroutine, no leak)
}

// NewSlotPool creates a slot pool with at most maxConcurrent active ticks.
func NewSlotPool(maxConcurrent int, timeout time.Duration, spawner *Spawner, lifecycle *LifecycleTracker) *SlotPool {
	p := &SlotPool{
		sem:       make(chan string, maxConcurrent),
		maxSlots:  maxConcurrent,
		timeout:   timeout,
		spawner:   spawner,
		lifecycle: lifecycle,
		freedCh:   make(chan struct{}, maxConcurrent),
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
	set := make(map[string]bool)
	// Drain and re-fill the channel to snapshot names. Non-blocking.
	for i := 0; i < len(p.sem); i++ {
		name := <-p.sem
		set[name] = true
		p.sem <- name
	}
	return set
}

// Acquire blocks until a slot is free, then marks it occupied with the
// given project name. Returns false if context is cancelled.
func (p *SlotPool) Acquire(ctx context.Context, name string) bool {
	select {
	case p.sem <- name:
		return true
	case <-ctx.Done():
		return false
	}
}

// Release frees one slot and signals SlotFreed.
func (p *SlotPool) Release() {
	select {
	case <-p.sem:
		select {
		case p.freedCh <- struct{}{}:
		default:
		}
	default:
	}
}

// Spawn fires a project tick in a new goroutine. The goroutine acquires a
// slot from the pool, spawns via the gateway, and releases the slot on
// completion or timeout. Delivery and auto-slowdown are integrated.
// Spawn returns immediately — it is fire-and-forget.
func (p *SlotPool) Spawn(proj PackedProject, now time.Time, noDeliver bool, db *sql.DB) {
	go func() {
		tickID := fmt.Sprintf("%s-%s", proj.Name, now.Format("2006-01-02-15-04-05"))

		// Wait for a free slot.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if !p.Acquire(ctx, proj.Name) {
			log.Printf("SLOT: timeout waiting for free slot — dropping %s", proj.Name)
			return
		}
		defer p.Release()

		log.Printf("SLOT: acquired for %s (%d/%d running)", proj.Name, p.Running(), p.maxSlots)

		// Enqueue and start.
		if err := p.lifecycle.Enqueue(proj.Name, tickID); err != nil {
			log.Printf("SPAWN: enqueue %s: %v", proj.Name, err)
			return
		}
		if err := p.lifecycle.StartRunning(tickID); err != nil {
			log.Printf("SPAWN: start %s: %v", proj.Name, err)
			return
		}

		// Spawn.
		st, err := p.spawner.Spawn(proj, tickID)
		if err != nil {
			log.Printf("SPAWN: %s failed: %v", proj.Name, err)
			_ = p.lifecycle.Complete(TickOutcome{
				TickID:  tickID,
				Project: proj.Name,
				Started: now,
				Status:  TickFailed,
				Error:   err.Error(),
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
			deliverOutput(outcome.Project, outcome.TickID, st.Deliver, &st.Output)
		}

		// Auto-slowdown: if tick signals IDLE, double the cooldown.
		if db != nil {
			autoSlowdown(db, outcome.Project, &st.Output)
			// Timeout backoff: if tick timed out, double cooldown to
			// prevent the spawn→timeout→spawn loop. Cap at 1 hour.
			if outcome.Status == TickTimeout {
				TimeoutBackoff(db, outcome.Project)
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
