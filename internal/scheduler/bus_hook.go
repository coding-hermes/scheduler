package scheduler

import (
	"context"
	"log"

	"github.com/coding-hermes/scheduler/internal/bus"
)

// SchedulerBus is the REMOTE-004 visibility hook inside the scheduler
// package. It wraps the process's single bus client (one publisher per
// scheduler — docs/remote-spec.md §3) so the lifecycle can fire best-effort
// publishes on tick-terminal and lane-state transitions without ever
// depending on the bus being up. A nil bus is legal everywhere and behaves
// byte-identically to a disabled one: nothing is sent, nothing is logged,
// no error exists on any path.
//
// THE AUTONOMY LAW (§3, and the reason this type exists): a Crier failure —
// down, timeout, refused — NEVER blocks or fails a tick. Publish is
// fire-and-forget with a short client-side deadline (internal/bus); the
// event is lost on failure and the log line is the only trace.
type SchedulerBus struct {
	client *bus.Client
}

// NewSchedulerBus wraps a bus client. nil (and a nil-wrapped client) is
// the pure no-op.
func NewSchedulerBus(c *bus.Client) *SchedulerBus {
	if c == nil {
		return nil
	}
	return &SchedulerBus{client: c}
}

// Enabled reports whether anything will actually be published.
func (b *SchedulerBus) Enabled() bool {
	return b != nil && b.client != nil && b.client.Enabled()
}

// PublishTickTerminal publishes the tick-terminal event for a tick that
// just reached a terminal status (completed / failed / deferred / timeout).
// Best-effort by construction: every failure mode inside Publish logs and
// drops; this method can only return nil.
func (b *SchedulerBus) PublishTickTerminal(ctx context.Context, project, tickID, status string) {
	if !b.Enabled() {
		return
	}
	// The publisher owns the monotonic event id; Publish fills it in.
	if err := b.client.PublishTickTerminal(ctx, project, tickID, status); err != nil {
		// Defensive: Publish is contractually nil-error, but the autonomy
		// law must hold even if that ever changes — swallow and log.
		log.Printf("CRIER: tick-terminal publish dropped: %v", err)
	}
}

// PublishLaneState publishes the lane-state event for a project whose
// enabled state just flipped (pause/resume/auto-disable). Best-effort by
// construction.
func (b *SchedulerBus) PublishLaneState(ctx context.Context, project, state string) {
	if !b.Enabled() {
		return
	}
	if err := b.client.Publish(ctx, bus.Envelope{
		Kind:    bus.KindLaneState,
		Project: project,
		Status:  state,
		TickID:  "-",
	}); err != nil {
		log.Printf("CRIER: lane-state publish dropped: %v", err)
	}
}

// Client exposes the wrapped client for subscriber construction and tests.
func (b *SchedulerBus) Client() *bus.Client {
	if b == nil {
		return nil
	}
	return b.client
}

// noopBus is the shared disabled bus — PublishTickTerminal on it is a
// guaranteed no-op with no allocation.
var noopBus = &SchedulerBus{}

// Bus returns the loop's bus, never nil (the disabled bus when unset), so
// call sites need no nil check.
func (l *Loop) Bus() *SchedulerBus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.schedulerBus == nil {
		return noopBus
	}
	return l.schedulerBus
}

// SetSchedulerBus installs the process bus client (REMOTE-004). nil or a
// disabled client leaves the loop with the no-op bus. Called once at boot
// by cmd/schedulerd before the loop starts evaluating; the bus propagates
// to the lifecycle tracker exactly the way SetClock does.
func (l *Loop) SetSchedulerBus(b *SchedulerBus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b == nil || !b.Enabled() {
		l.schedulerBus = nil
	} else {
		l.schedulerBus = b
	}
	if l.lifecycle != nil {
		l.lifecycle.SetSchedulerBus(b)
	}
}
