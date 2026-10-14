package api

// REMOTE-008 (docs/federation-query-spec.md §2.5): the replay window.
//
// (caller_scheduler_id, corr_id, op) is the identity of a query. The peer
// keeps a SHORT window mapping that key → the reply it already produced, so
// a retried query (a reconnect, a bus redelivery) returns the identical
// answer instead of re-reading or double-counting — "duplicate is a drop,
// not an error" (the ingest idempotency law of remote-spec.md §5, mirrored
// on the read path).
//
// Default window is 5 minutes (spec §2.5 default; §8.2 leaves per-peer
// configurability to a later row). Replay serves the STORED BYTES: the
// identical answer means identical as_of/age_ms too — a replayed reply
// carries the original answer's freshness, which is exactly the point (§2.2:
// "A cached read carries its own as_of").

import (
	"encoding/json"
	"sync"
	"time"
)

// federationReplayWindowDefault is the replay window (spec §2.5 default).
const federationReplayWindowDefault = 5 * time.Minute

// federationReplayMax bounds the window's entry count. The eviction is
// first-in-first-out over an ordered key ring: a peer under a redelivery
// storm evicts its OLDEST replay entries rather than growing without bound.
// 4096 entries ≈ every op answered once per second for an hour — far above
// any legitimate retry pattern, far below a memory concern.
const federationReplayMax = 4096

// federationReply is one stored reply: the HTTP status and the exact JSON
// body that was (or would have been) written. Storing BYTES (not the
// envelope struct) is what makes the replay byte-identical by construction —
// re-marshaling a struct twice can differ the moment any member gains map
// ordering or a timestamp.
type federationReply struct {
	status int
	body   []byte
}

// federationReplay is the replay window. All time reads go through the
// deadline seam the server hands in (SCHED-GAP-169: no direct time.Now in
// non-test code) — the window takes a now func so tests can drive it with
// the manual sim clock.
type federationReplay struct {
	mu      sync.Mutex
	entries map[string]federationReply
	order   []string // FIFO eviction ring, keys in insertion order
	window  time.Duration
	now     func() time.Time
	stored  map[string]time.Time
}

// newFederationReplay builds the window with the default 5-minute TTL.
func newFederationReplay(now func() time.Time) *federationReplay {
	if now == nil {
		now = time.Now
	}
	return &federationReplay{
		entries: make(map[string]federationReply),
		stored:  make(map[string]time.Time),
		window:  federationReplayWindowDefault,
		now:     now,
	}
}

// replayKey is the spec §2.5 identity of a query:
// (caller_scheduler_id, corr_id, op).
func replayKey(caller, corrID, op string) string {
	return caller + "\x00" + corrID + "\x00" + op
}

// lookup returns the stored reply for the key when one is inside the
// window. An EXPIRED entry is deleted on sight (the next store re-admits
// the key cleanly) and reported as a miss.
func (f *federationReplay) lookup(caller, corrID, op string) (federationReply, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := replayKey(caller, corrID, op)
	body, ok := f.entries[key]
	if !ok {
		return federationReply{}, false
	}
	if f.now().Sub(f.stored[key]) > f.window {
		delete(f.entries, key)
		delete(f.stored, key)
		return federationReply{}, false
	}
	return body, true
}

// store admits a reply under the key. A key already inside the window is
// NOT overwritten — the first answer wins (§2.5: the replay returns the
// FIRST answer; a second store under the same identity is itself a
// duplicate and a drop). Oldest entries are evicted FIFO at the cap.
func (f *federationReplay) store(caller, corrID, op string, status int, resp responseEnvelope) {
	body, err := json.Marshal(resp)
	if err != nil {
		// A reply that cannot be marshaled cannot be stored NOR served;
		// the query path marshals the same struct again on write, so the
		// write path surfaces the failure — nothing to bury here.
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := replayKey(caller, corrID, op)
	if _, exists := f.entries[key]; exists {
		return
	}
	for len(f.order) >= federationReplayMax {
		oldest := f.order[0]
		f.order = f.order[1:]
		delete(f.entries, oldest)
		delete(f.stored, oldest)
	}
	f.entries[key] = federationReply{status: status, body: body}
	f.stored[key] = f.now()
	f.order = append(f.order, key)
}

// fedReplay is the server's replay window. Built lazily by the query
// handler (NewServer stays signature-stable; the window needs the server's
// clock seam, which SetClock can install after construction).
func (s *Server) federationReplayWindow() *federationReplay {
	s.fedReplayOnce.Do(func() {
		clk := s.clock()
		s.fedReplay = newFederationReplay(clk.Now)
	})
	return s.fedReplay
}

// replayStoreForTest is intentionally absent — tests admit replies through
// the same store() path the handler uses.
