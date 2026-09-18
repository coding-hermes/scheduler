package database

import (
	"database/sql"
	"sync"
)

// Event fan-out (CTL-002).
//
// LogEvent publishes every successfully committed event to the subscribers of
// its database handle, which is how GET /api/v1/events/stream pushes events to
// connected SSE clients instead of polling the events table in a loop. The
// design keeps all existing LogEvent callers untouched: they keep calling
// LogEvent, and the publish happens inside it, after the INSERT commits.
//
// Hubs are keyed by *sql.DB so a process holding more than one database (the
// daemon holds one; tests build one per fixture) never leaks an event from one
// database into a stream reading another. An entry exists only while it has at
// least one subscriber, so an idle process pays one map lookup per committed
// event and nothing else.
//
// Delivery is best-effort by design: a subscriber whose buffer is full DROPS
// that event rather than blocking the writer. Blocking here would stall every
// event-producing path in the fleet (the scheduler loop, the API, the MCP
// server, the DuckBrain sync) on one slow SSE reader. The stream handler
// repairs the loss: it tracks the last ID it sent and, when a live event
// arrives with a gap in its ID, re-reads the log through ListEventsAfterID
// before emitting it (see api.streamReplay).
//
// Subscriber channels are NEVER closed: unsubscribe removes the channel from
// the hub, and a publisher that already snapshotted it may still deliver into
// the now-unreferenced buffer. That is harmless (the channel is garbage) and
// it removes the send-on-closed-channel panic hazard entirely. Readers stop on
// their own signal (the HTTP request context).

// defaultEventBuffer is the per-subscriber channel depth used when a caller
// passes a non-positive buffer size.
const defaultEventBuffer = 64

// eventHub fans committed events out to the subscribers of one database.
type eventHub struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan Event
}

// publish delivers e to every current subscriber without ever blocking.
func (h *eventHub) publish(e Event) {
	h.mu.Lock()
	subs := make([]chan Event, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	// Deliver outside the hub lock: the sends are non-blocking, but keeping
	// the lock free of any per-subscriber work means a burst of events can
	// never serialize behind a single slow channel.
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// Buffer full: drop. The stream handler repairs the gap from
			// the log when it sees the ID skip.
		}
	}
}

var (
	eventHubsMu sync.Mutex
	eventHubs   = make(map[*sql.DB]*eventHub)
)

// SubscribeEvents registers a subscriber for events committed to db and
// returns the receive channel plus an idempotent cancel function. The caller
// MUST call cancel when it stops reading (typically via defer) — the hub entry
// is removed once its last subscriber leaves.
//
// buffer bounds how many pending events a single subscriber may hold; values
// <= 0 use defaultEventBuffer. The channel is buffered so a subscriber that is
// briefly busy (an SSE client on a slow link) does not lose events, and a
// subscriber that never drains simply drops the overflow instead of blocking
// the writers.
//
// The returned channel is never closed; a reader stops by returning from its
// own loop (e.g. when its request context is canceled).
func SubscribeEvents(db *sql.DB, buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = defaultEventBuffer
	}

	eventHubsMu.Lock()
	hub := eventHubs[db]
	if hub == nil {
		hub = &eventHub{subs: make(map[int]chan Event)}
		eventHubs[db] = hub
	}
	hub.mu.Lock()
	id := hub.nextID
	hub.nextID++
	ch := make(chan Event, buffer)
	hub.subs[id] = ch
	hub.mu.Unlock()
	eventHubsMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Lock order everywhere: eventHubsMu then hub.mu. Doing the
			// emptiness re-check under both locks keeps a concurrent
			// SubscribeEvents from attaching to a hub we are about to drop.
			eventHubsMu.Lock()
			hub.mu.Lock()
			delete(hub.subs, id)
			if len(hub.subs) == 0 && eventHubs[db] == hub {
				delete(eventHubs, db)
			}
			hub.mu.Unlock()
			eventHubsMu.Unlock()
		})
	}
	return ch, cancel
}

// publishEvent hands a committed event to the hub of db, if anyone listens.
func publishEvent(db *sql.DB, e Event) {
	eventHubsMu.Lock()
	hub := eventHubs[db]
	eventHubsMu.Unlock()
	if hub == nil {
		return
	}
	hub.publish(e)
}

// SubscriberCount reports how many subscribers db currently has. It exists for
// observability and tests.
func SubscriberCount(db *sql.DB) int {
	eventHubsMu.Lock()
	hub := eventHubs[db]
	eventHubsMu.Unlock()
	if hub == nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.subs)
}
