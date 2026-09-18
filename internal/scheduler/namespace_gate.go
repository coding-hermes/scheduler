package scheduler

// SCHED-GAP-144 — namespace-cap admission gate at the pool's declared
// admission point.
//
// Background. The namespace cap (`namespaces.max_concurrent`) keeps a satellite
// family from eating the fleet: the operator's shape is global 10, foremen 8,
// one slot per satellite namespace. Two places enforced it and a third did not:
//
//   - the packer gates on `nsRunning + selected >= nsCap` (packer_select.go);
//   - the orphan re-nudge gates per pass (SCHED-GAP-142, session_resume.go);
//   - SlotPool.spawn — the DECLARED admission point per the G7 ruling ("any
//     future admission gate MUST live in SlotPool.spawn") — had only the global
//     slot semaphore and the load gate. Every other entry into the pool (the
//     API spawn endpoint, wave resume, queue replay/continuation) therefore
//     bypassed the namespace cap entirely.
//
// Measured live consequence: after the 2026-09-17 23:51 restart, three
// `duckbrain-sync` lanes (`eduos-sync`, `heading-sync`, `off-by-one-sync`) were
// admitted within seconds against a cap of ONE, together with all 8 foremen —
// the burst took every global slot, which is why the qa/pm/dogfood families got
// none. That restart ran a binary that predated the nudge-path fix; the gate
// here makes the cap authoritative regardless of which path calls spawn.
//
// Semantics (deliberately conservative):
//   - DEFER, not drop: a tick whose namespace is full stays QUEUED and is
//     retried — no cooldown is consumed, no failure is recorded against the
//     lane, and the project is not penalised for the fleet being busy.
//   - Occupancy = ticks RUNNING in the namespace (read from the DB, so it spans
//     every writer) + spawn attempts that have passed this gate but whose row
//     has not yet transitioned to running (the SCHED-GAP-103 TOCTOU window).
//     A claim is dropped the moment the row becomes `running`, so a running
//     tick is never double-counted and the effective cap stays exact.
//   - FAIL OPEN on every error (unknown namespace, read failure, no DB): a
//     bookkeeping problem must never freeze the fleet. `cap <= 0` means
//     unlimited — that is the declared override, alongside the
//     SCHEDULER_NAMESPACE_CAP_GATE=off emergency switch.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"time"
)

const (
	// nsGatePollInterval is how often a blocked spawn re-checks its namespace.
	// A poll (rather than a channel) is deliberate: occupancy is partly held in
	// the DB by other writers, and the wait is short-lived by construction
	// (a tick slot frees when a sibling tick completes).
	nsGatePollInterval = 250 * time.Millisecond

	// defaultNamespaceSlotPatience bounds the namespace wait. It is much longer
	// than defaultSlotPatience (5m, the GLOBAL slot wait) because waiting for a
	// namespace slot is the intended behaviour, not fleet saturation: a cap-1
	// satellite family legitimately waits for its sibling to finish. Kept well
	// under the 2h tick timeout so a stuck waiter cannot outlive a tick.
	defaultNamespaceSlotPatience = 30 * time.Minute
)

// namespaceCapGateDisabled reports the operator emergency switch. Default is
// enabled; SCHEDULER_NAMESPACE_CAP_GATE=off|0|false|disabled turns the gate off
// fleet-wide (e.g. to evacuate a backlog after a bad cap value).
func namespaceCapGateDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SCHEDULER_NAMESPACE_CAP_GATE"))) {
	case "off", "0", "false", "disabled", "no":
		return true
	}
	return false
}

// namespaceCapDB returns a namespace's max_concurrent. 0 means unlimited or
// unknown — the caller must treat it as "no gate". Fails OPEN on any error:
// a missing/renamed namespace row must not freeze spawning.
func namespaceCapDB(db *sql.DB, nsID string) int {
	if db == nil || nsID == "" {
		return 0
	}
	var cap int
	if err := db.QueryRow(`SELECT max_concurrent FROM namespaces WHERE id = ?`, nsID).Scan(&cap); err != nil {
		return 0
	}
	if cap < 0 {
		return 0
	}
	return cap
}

// namespaceRunningDB counts the ticks currently RUNNING in a namespace. It reads
// the table rather than an in-process map so the count spans every writer (the
// daemon, a restarted daemon's leftovers, manual rows). Errors count as 0 (fail
// open, matching namespaceCapDB).
func namespaceRunningDB(db *sql.DB, nsID string) int {
	if db == nil || nsID == "" {
		return 0
	}
	var n int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM ticks t
JOIN projects p ON p.name = t.project_name
WHERE p.namespace_id = ? AND t.status = 'running'`, nsID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// tryClaimNamespaceSlot atomically claims one namespace slot when the
// namespace has room. dbRunning is the just-read running count; p.nsPending
// holds the attempts that have passed the gate but are not yet `running` in the
// DB. Called with the pool mutex held only for the tiny map section.
func (p *SlotPool) tryClaimNamespaceSlot(nsID string, cap, dbRunning int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if dbRunning+p.nsPending[nsID] >= cap {
		return false
	}
	p.nsPending[nsID]++
	return true
}

// releaseNamespaceSlot drops a claim taken by tryClaimNamespaceSlot. Called
// exactly once per claim: when the row transitions to `running` (the DB now
// carries the occupancy) or on any early exit.
func (p *SlotPool) releaseNamespaceSlot(nsID string) {
	if nsID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nsPending[nsID] > 0 {
		p.nsPending[nsID]--
	}
	if p.nsPending[nsID] == 0 {
		delete(p.nsPending, nsID)
	}
}

// NamespacePending returns the number of in-flight spawn attempts in a
// namespace that hold a claim but are not yet `running` (observability; used by
// tests and diagnostics).
func (p *SlotPool) NamespacePending(nsID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nsPending[nsID]
}

// waitNamespaceSlot blocks until the namespace has room for one more tick, or
// ctx expires. Returns true when the caller holds a claim (which MUST be
// released via releaseNamespaceSlot), false when it gave up (the caller leaves
// the tick queued).
func (p *SlotPool) waitNamespaceSlot(ctx context.Context, nsID string, db *sql.DB) bool {
	cap := namespaceCapDB(db, nsID)
	if cap <= 0 {
		return true // unlimited / unknown / no DB: no gate
	}
	for {
		if p.tryClaimNamespaceSlot(nsID, cap, namespaceRunningDB(db, nsID)) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(nsGatePollInterval):
		}
	}
}

// logNamespaceDeferral records a namespace-cap deferral: the tick was NOT
// started, the row stays queued, and nothing is charged to the lane. INFO-level
// and grep-able (`NS-CAP:`) — this is scheduled behaviour, not an error.
func (p *SlotPool) logNamespaceDeferral(proj PackedProject, tickID string, cap, running int, waited time.Duration) {
	log.Printf("NS-CAP: namespace %s at cap (%d/%d running) — %s (tick %s) left QUEUED after waiting %s",
		proj.NamespaceID, running, cap, proj.Name, tickID, waited.Round(time.Second))
}
