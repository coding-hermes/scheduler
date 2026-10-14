package bus

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// IngestedEvent is one valid, non-duplicate peer event ready for the
// REMOTE-006 ingest. It carries the envelope plus the receive-side facts:
// which topic/pattern delivered it, when it was received, whether it
// originated from this scheduler, and whether its scheduler is one this
// process knows (the local answer is always "only myself"; the peers
// registry widens it later).
type IngestedEvent struct {
	Envelope
	// Topic is the LITERAL published topic the frame named (never the
	// wildcard pattern — the frame carries it precisely so a wildcard
	// subscriber can tell which topic matched).
	Topic string
	// Pattern is the subscription pattern this frame arrived on.
	Pattern string
	// IngestedAt is the receive stamp (UTC).
	IngestedAt time.Time
	// Self marks events published by THIS scheduler (echoed back through
	// its own wildcard subscription).
	Self bool
	// KnownScheduler marks events whose scheduler_id this process can
	// vouch for (false = a scheduler the peers registry has not met).
	KnownScheduler bool
}

// Validate checks an envelope's required members (the §3 shape): every
// published event names its scheduler, kind, project, tick and status; the
// event id is present. Used by the subscriber to drop malformed peer
// events (log-and-drop, never an error).
func (e Envelope) Validate() error {
	var missing []string
	if e.SchedulerID == "" {
		missing = append(missing, "scheduler_id")
	}
	if e.EventID == "" {
		missing = append(missing, "event_id")
	}
	if e.Kind == "" {
		missing = append(missing, "kind")
	}
	if e.Project == "" {
		missing = append(missing, "project")
	}
	if e.TickID == "" {
		missing = append(missing, "tick_id")
	}
	if e.Status == "" {
		missing = append(missing, "status")
	}
	if len(missing) > 0 {
		return fmt.Errorf("bus: envelope missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// ParseEventID splits a counter-minted event id ("<scheduler_id>-<20-digit
// counter>") back into its owner id. ok=false for ids the mint format does
// not explain (foreign or hand-built ids are still valid ingest keys —
// only the counter-form carries a resolvable owner).
func ParseEventID(eventID string) (schedulerID string, ok bool) {
	const digits = 20
	if len(eventID) <= digits+1 {
		return "", false
	}
	tail := eventID[len(eventID)-digits:]
	head := eventID[:len(eventID)-digits-1]
	if head == "" || eventID[len(eventID)-digits-1] != '-' {
		return "", false
	}
	for i := 0; i < len(tail); i++ {
		if tail[i] < '0' || tail[i] > '9' {
			return "", false
		}
	}
	return head, true
}

// seenSet is the subscriber-side idempotency filter (spec §5: "Replay is
// idempotent on (scheduler_id, event_id) — the ingest side drops a
// duplicate key, so a replayed spool can never double-apply"). Memory-bounded
// per scheduler: once a scheduler's live set crosses the window the LOW
// watermark is pruned and the floor below which duplicates can no longer be
// recognized is recorded. REMOTE-006's durable replay dedupe supersedes
// this in-memory window at ingest time.
type seenSet struct {
	mu    sync.Mutex
	peers map[string]*peerSeen
}

// windowPerPeer bounds the remembered (event_id → counter) entries per
// scheduler; lowWater is the fraction the prune cuts back to.
const (
	windowPerPeer = 4096
	lowWater      = 2048
)

type peerSeen struct {
	counters map[string]uint64   // live: counter-form ids
	raw      map[string]struct{} // non-counter ids remembered verbatim
	floor    uint64              // counters at or below this were pruned
}

func newSeenSet() *seenSet { return &seenSet{peers: map[string]*peerSeen{}} }

// Allow reports whether the envelope's (scheduler_id, event_id) key is
// new. The FIRST observation is allowed; every later observation of the
// same key is refused.
func (ss *seenSet) Allow(env Envelope) bool {
	if env.SchedulerID == "" || env.EventID == "" {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	p := ss.peers[env.SchedulerID]
	if p == nil {
		p = &peerSeen{counters: map[string]uint64{}, raw: map[string]struct{}{}}
		ss.peers[env.SchedulerID] = p
	}
	if ctr, ok := ParseEventID(env.EventID); ok && ctr == env.SchedulerID {
		if _, seen := p.counters[env.EventID]; seen {
			return false
		}
		p.counters[env.EventID] = 0
		if len(p.counters) > windowPerPeer {
			p.pruneCounters()
		}
		return true
	}
	if _, seen := p.raw[env.EventID]; seen {
		return false
	}
	p.raw[env.EventID] = struct{}{}
	return true
}

// pruneCounters cuts the live counter set back to the low watermark,
// keeping the newest half by counter value and recording the floor below
// which duplicates can no longer be recognized.
func (p *peerSeen) pruneCounters() {
	type kv struct {
		id  string
		val uint64
	}
	all := make([]kv, 0, len(p.counters))
	for id := range p.counters {
		if v, _, ok := splitCounter(id); ok {
			all = append(all, kv{id: id, val: v})
		} else {
			delete(p.counters, id) // not counter-form after all — forget it
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].val < all[j].val })
	var floor uint64
	for i := 0; i < len(all)-lowWater; i++ {
		floor = all[i].val
		delete(p.counters, all[i].id)
	}
	p.floor = floor
}

// splitCounter decodes the 20-digit tail into a counter value.
func splitCounter(eventID string) (uint64, string, bool) {
	id, ok := ParseEventID(eventID)
	if !ok {
		return 0, "", false
	}
	tail := eventID[len(eventID)-20:]
	var v uint64
	for i := 0; i < len(tail); i++ {
		v = v*10 + uint64(tail[i]-'0')
	}
	return v, id, true
}

// ── Client-side Publisher implementation ──────────────────────────────────
// The subscriber resolves frames through this narrow surface so ingest
// tests can stub the client.

// SchedulerID returns this client's scheduler id ("" never happens for an
// enabled client — Enabled() gates on it).
func (c *Client) SchedulerID() string { return c.schedID }

// IsSchedulerID reports whether id is this scheduler's own id (self-event
// detection on ingest).
func (c *Client) IsSchedulerID(id string) bool { return id != "" && id == c.schedID }

// KnownSchedulerIDs returns the scheduler ids this process can vouch for:
// its own. The peers registry is REMOTE-003/006 territory — REMOTE-004's
// receive path ingests unknown ids rather than dropping them, so the
// answer is deliberately minimal and never nil.
func (c *Client) KnownSchedulerIDs() []string { return []string{c.schedID} }
