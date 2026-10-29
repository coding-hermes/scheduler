package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
)

// SCHED-GAP-16939 — idle-stall → NEXT-HOP ADVANCE.
//
// The measured defect: 9 of 15 failures in a 3h window died as
// "stalled: no progress for 30m0s (gateway /v1/responses)". The scheduler made
// exactly ONE gateway call per tick, so a single stalled hop killed the whole
// tick even though the merged chain held other hops that could serve. Raising
// the deadline would only make a longer death (the requirement-weakening
// failure mode the row forbids), so the fix is a bounded ADVANCE: when the
// per-turn watchdog fires, classify the attempt, drop that provider/model FOR
// THIS TICK ONLY, re-resolve the next eligible hop and re-dispatch inside the
// same tick budget.
//
// Two contracts this file exists to keep honest:
//
//  1. The exclusion is PER TICK. Nothing here writes durable state: the failed
//     pair is dropped from a local set, so the lane stays eligible for every
//     other project (AC 6). The cross-tick circuit breaker is a separate
//     mechanism owned by TASK-ROUTER-002 and is not touched by this file.
//  2. The tick row NAMES every hop it tried — a completed tick that stalled
//     once and succeeded on hop 2 must be distinguishable from a tick that
//     worked first try (AC 3). The rows are persisted as a JSON array on
//     ticks.gateway_attempts (migration v79).

// Hop attempt terminal reasons. These are the vocabulary of the attempts[]
// rows; a reader distinguishes "succeeded first try" (one row, completed) from
// "stalled on hop 1, succeeded on hop 2" (two rows: idle-stall, completed).
const (
	hopReasonCompleted = "completed"
	hopReasonIdleStall = "idle-stall"
	hopReasonTickWall  = "tick-wall"
	hopReasonTransport = "transport"
	hopReasonAuth      = "auth"
	hopReasonGateway   = "gateway-error"
)

// hopAttempt is one gateway POST this tick made. Hop is the index into the
// tick's resolved hop list (0 = the pair the tick was dispatched with);
// -1 marks a pair that is NOT in the chain (the final foreman-fallback
// attempt), so an off-chain attempt never pretends to be a chain position.
type hopAttempt struct {
	Hop        int    `json:"hop"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	DurationMS int64  `json:"duration_ms"`
	Reason     string `json:"reason"`
}

// tickHopLog is the per-tick, in-memory record of every POST attempt. It is
// written only by the spawn goroutine for its own tick, but the mutex keeps
// the snapshot safe for the persistence call on every return path.
type tickHopLog struct {
	mu   sync.Mutex
	rows []hopAttempt
}

func (l *tickHopLog) add(a hopAttempt) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, a)
}

func (l *tickHopLog) snapshot() []hopAttempt {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]hopAttempt, len(l.rows))
	copy(out, l.rows)
	return out
}

// attemptedPairs returns the provider/model keys already POSTed this tick.
// The advance loop consults it instead of keeping a second set, so a pair can
// never be attempted twice by two different code paths (stall advance and the
// 401/403 chain hop) in the same tick.
func (l *tickHopLog) attemptedPairs() map[string]struct{} {
	out := map[string]struct{}{}
	for _, a := range l.snapshot() {
		out[hopPairKey(a.Provider, a.Model)] = struct{}{}
	}
	return out
}

// jsonBlob marshals the attempt rows. "" means "no POST attempt was recorded"
// — the honest empty for an exec spawn or a spawn-site refusal, never a
// fabricated completed row.
func (l *tickHopLog) jsonBlob() string {
	rows := l.snapshot()
	if len(rows) == 0 {
		return ""
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		log.Printf("WARN: marshal gateway attempts: %v", err)
		return ""
	}
	return string(blob)
}

func hopPairKey(provider, model string) string {
	return provider + "\x00" + model
}

// pairAttempted reports whether this tick already POSTed the pair. It is the
// per-tick exclusion set: the 401/403 chain hop and the final foreman
// fallback consult it so a pair that a stall advance already burned is never
// POSTed twice in one tick — and no durable state is written, so the lane
// stays eligible for every other project (AC 6).
func (l *tickHopLog) pairAttempted(provider, model string) bool {
	_, tried := l.attemptedPairs()[hopPairKey(provider, model)]
	return tried
}

// hopList resolves the tick's ordered, de-duplicated hop list from the merged
// chain. Each present entry contributes the pair it would resolve to when it
// is the head (resolveChain's back-fill semantics), so the list is exactly the
// sequence of DISTINCT pairs the chain can serve, in chain order.
//
// head is the pair the tick actually dispatched with (the router head can sit
// outside the merged chain). It is forced to position 0 — the tick's first
// attempt is hop 0 by construction, and asserting that in the test is what
// keeps the hop indexes meaningful when the chain and the head disagree.
func hopList(chain []chainEntry, head chainEntry) []chainEntry {
	var hops []chainEntry
	seen := map[string]struct{}{}
	add := func(e chainEntry) {
		if e.model == "" && e.provider == "" {
			return
		}
		key := hopPairKey(e.provider, e.model)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		hops = append(hops, e)
	}
	if head.model != "" || head.provider != "" {
		add(head)
	}
	for i, e := range chain {
		if !e.present() {
			continue
		}
		m, p := resolveChain(chain[i:])
		add(chainEntry{model: m, provider: p})
	}
	return hops
}

// hopIndexOf returns the position of (m,p) in hops, or -1 when the pair is
// off-chain (the final foreman-fallback attempt).
func hopIndexOf(hops []chainEntry, m, p string) int {
	key := hopPairKey(p, m)
	for i, e := range hops {
		if hopPairKey(e.provider, e.model) == key {
			return i
		}
	}
	return -1
}

// nextHopAfter returns the first hop in chain order whose pair has not been
// attempted this tick. ok=false means the chain is exhausted — every eligible
// hop already failed — and the tick must fail with the honest residual rather
// than loop.
func nextHopAfter(hops []chainEntry, attempted map[string]struct{}) (chainEntry, int, bool) {
	for i, e := range hops {
		if _, tried := attempted[hopPairKey(e.provider, e.model)]; tried {
			continue
		}
		return e, i, true
	}
	return chainEntry{}, -1, false
}

// classifyHopErr names the terminal reason of one POST attempt. turnStalled is
// the spawn path's own fold (true only when the per-turn watch fired while the
// TICK deadline was still alive), so a tick-wall abort is never mislabelled
// as a stall.
func classifyHopErr(err error, turnStalled bool) string {
	switch {
	case err == nil:
		return hopReasonCompleted
	case turnStalled:
		return hopReasonIdleStall
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrTickDeadlineExceeded):
		return hopReasonTickWall
	case errors.Is(err, ErrGatewayKeyRejected):
		return hopReasonAuth
	case IsTransientGatewayErr(err):
		return hopReasonTransport
	default:
		return hopReasonGateway
	}
}

// persistHopAttempts writes the tick's attempts[] JSON onto the row. It is
// called on EVERY gateway return path (completed, stalled, transport error) so
// the row can never claim a first-try success it did not have, nor hide a
// stall behind a later success. Best-effort like logPOSTTrace: a failed UPDATE
// logs a WARN and never fails the tick on its own.
func (s *Spawner) persistHopAttempts(tickID string, log_ *tickHopLog) {
	if s.db == nil {
		return
	}
	blob := log_.jsonBlob()
	if blob == "" {
		return
	}
	if _, err := s.db.Exec(`UPDATE ticks SET gateway_attempts = ? WHERE id = ?`, blob, tickID); err != nil {
		log.Printf("WARN: persist gateway attempts for %s: %v", tickID, err)
	}
}

// describeHopAdvance is the one stable log line a stall advance emits, kept
// greppable so an operator can count advances from scheduler.log without
// parsing JSON. fromHop is the stalled hop's index (-1 when the stalled pair
// is off-chain); toHop is the hop being dispatched next.
func describeHopAdvance(project, tickID string, from, to chainEntry, fromHop, toHop int, deadlineMS int64) string {
	return fmt.Sprintf("GATEWAY HOP ADVANCE: %s tick=%s stalled hop %d (%s/%s after %dms idle) — advancing to hop %d (%s/%s)",
		project, tickID, fromHop, from.provider, from.model, deadlineMS, toHop, to.provider, to.model)
}
