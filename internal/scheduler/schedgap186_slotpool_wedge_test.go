package scheduler

import (
	"context"
	"testing"
	"time"
)

// SCHED-GAP-186 (2026-09-19, hermes-dagger): a tick's session died while its
// spawn goroutine hung in st.Wait(); the zombie reaper flipped the tick row
// status='timeout' directly (timeoutReapSQL), but the goroutine that owns the
// SlotPool claim never came back to run its deferred Release. RunningSet()
// kept reporting the project as "already running" and the evaluation loop's
// DEDUP (tick_process.go: alreadyRunning := l.slotPool.RunningSet()) skipped
// it on every cycle — /api/v1/evaluate did NOT clear it, only a daemon
// restart un-wedged the project.
//
// The claim state a hung goroutine holds is exactly what the spawn goroutine
// builds between entry and Wait(): a SCHED-GAP-103 reservation plus one slot
// refcount from Acquire. RunningSet() = running ∪ reserved, so the fix must
// drain BOTH. These tests recreate that claim state and walk the reaper and
// evaluate paths over it.

// TestSchedGap186_ReaperReleasesSlotPoolWedge — Phase A reproduces the wedge
// with the pre-fix mechanics (the raw timeoutReapSQL UPDATE, which touches no
// in-process state) and asserts the claim survives; Phase B runs the real
// reapZombies and asserts the claim is drained and the project is spawnable
// again.
func TestSchedGap186_ReaperReleasesSlotPoolWedge(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap186-wedge")
	insertRunningTick(t, db, "gap186-wedge-tick-1", "gap186-wedge", deadTestPID)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)

	// Recreate the incident's in-process claim state: the spawn goroutine
	// reserved the project, acquired its slot, and hung forever in
	// st.Wait() while the recorded pid was already dead.
	if !loop.slotPool.tryReserve("gap186-wedge") {
		t.Fatal("tryReserve = false for an idle project (precondition)")
	}
	if !loop.slotPool.Acquire(context.Background(), "gap186-wedge") {
		t.Fatal("Acquire = false (precondition)")
	}
	if !loop.slotPool.RunningSet()["gap186-wedge"] {
		t.Fatal("claim not visible to RunningSet (precondition)")
	}

	// Phase A — the wedge, played by the pre-fix reaper: the raw
	// timeoutReapSQL flips the DB row terminal WITHOUT touching the pool.
	now := time.Now().Format(time.RFC3339)
	if _, err := db.Exec(timeoutReapSQL, now, "gap186-wedge-tick-1"); err != nil {
		t.Fatalf("raw timeoutReapSQL: %v", err)
	}
	if got := tickStatusOf(t, db, "gap186-wedge-tick-1"); got != "timeout" {
		t.Fatalf("Phase A: tick status = %q, want timeout (row flip failed)", got)
	}
	if !loop.slotPool.RunningSet()["gap186-wedge"] {
		t.Fatal("Phase A: claim already free — the wedge did not reproduce; the pre-fix path cannot hold this scenario")
	}
	if loop.slotPool.tryReserve("gap186-wedge") {
		t.Fatal("Phase A: tryReserve succeeded for a wedged project — the DEDUP would not have skipped it; scenario not reproduced")
	}

	// Phase B — the fix: restore the row to 'running' and let the real
	// reaper (the same call the daemon's 60-second ticker makes) take the
	// production path.
	if _, err := db.Exec(`UPDATE ticks SET status='running', completed_at=NULL WHERE id = ?`, "gap186-wedge-tick-1"); err != nil {
		t.Fatalf("restore running row: %v", err)
	}
	loop.reapZombies()

	if got := tickStatusOf(t, db, "gap186-wedge-tick-1"); got != "timeout" {
		t.Fatalf("Phase B: reaper did not reap the dead-pid tick, status = %q", got)
	}
	if loop.slotPool.RunningSet()["gap186-wedge"] {
		t.Error("SCHED-GAP-186 wedge: reapZombies flipped the tick row terminal but the SlotPool claim survived — RunningSet keeps reporting the project as already running and the evaluation DEDUP skips it until a daemon restart")
	}
	if !loop.slotPool.tryReserve("gap186-wedge") {
		t.Error("spawn not admitted after the un-wedge: tryReserve = false, want true (project claimable again)")
	}
}

// TestSchedGap186_EvaluateCleanupStaleReleasesWedge — the evaluate() path's
// CleanupStale backstop (90-minute window) must reconcile the SlotPool claim
// for the project whose stale row it flips terminal. This is the per-eval
// self-heal: even a wedge a reaper somehow missed gets cleared on the next
// evaluation cycle instead of persisting until a restart.
func TestSchedGap186_EvaluateCleanupStaleReleasesWedge(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap186-stale")
	insertRunningTick(t, db, "gap186-stale-tick", "gap186-stale", 0)

	// Age the row past CleanupStale's 90-minute window (insertRunningTick
	// stamps spawned_at = now). Formatted LOCAL with offset — the same shape
	// lifecycle.Enqueue writes — because CleanupStale's spawned_at compare is
	// a raw string compare (pre-existing, deliberately unchanged here): a UTC
	// 'Z' row against a local-offset cutoff string mis-orders across the hour
	// boundary in negative-UTC-offset timezones.
	staleAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE ticks SET spawned_at = ? WHERE id = ?`, staleAt, "gap186-stale-tick"); err != nil {
		t.Fatalf("age spawned_at: %v", err)
	}
	// Keep the project inside its cooldown for this eval so the ONLY thing
	// under test is the cleanup/release wiring — the packer must not select
	// it and no spawn may fire.
	if _, err := db.Exec(`UPDATE projects SET cooldown_s = 86400, last_tick_completed = ? WHERE name = ?`,
		time.Now().UTC().Format(time.RFC3339), "gap186-stale"); err != nil {
		t.Fatalf("pin project into cooldown: %v", err)
	}

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	if !loop.slotPool.Acquire(context.Background(), "gap186-stale") {
		t.Fatal("Acquire = false (precondition)")
	}
	if !loop.slotPool.RunningSet()["gap186-stale"] {
		t.Fatal("claim not visible to RunningSet (precondition)")
	}

	loop.evaluate()

	if got := tickStatusOf(t, db, "gap186-stale-tick"); got != "timeout" {
		t.Fatalf("evaluate did not flip the stale row terminal, status = %q", got)
	}
	if loop.slotPool.RunningSet()["gap186-stale"] {
		t.Error("SCHED-GAP-186 wedge: evaluate()'s CleanupStale flipped the stale row terminal but the SlotPool claim survived — the DEDUP keeps skipping the project")
	}
	if !loop.slotPool.tryReserve("gap186-stale") {
		t.Error("spawn not admitted after the self-heal: tryReserve = false, want true")
	}
}

// TestSchedGap186_ReaperSparesLiveSlot — the reconcile must be surgical: a
// project with a LIVE tick (pid=0, fresh heartbeat — the INFRA-012 class the
// reapers deliberately leave 'running') keeps its slot while a dead-pid
// sibling is released. Freeing a live tick's slot would let the packer
// double-spawn it (the exact SCHED-GAP-021 over-admission class).
func TestSchedGap186_ReaperSparesLiveSlot(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap186-live")
	mustCreateProjectINFRA012(t, db, "gap186-dead")

	insertRunningTick(t, db, "gap186-live-tick", "gap186-live", 0)
	if _, err := db.Exec(`UPDATE ticks SET heartbeat_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), "gap186-live-tick"); err != nil {
		t.Fatalf("stamp fresh heartbeat: %v", err)
	}
	insertRunningTick(t, db, "gap186-dead-tick", "gap186-dead", deadTestPID)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	if !loop.slotPool.Acquire(context.Background(), "gap186-live") {
		t.Fatal("Acquire live = false (precondition)")
	}
	if !loop.slotPool.Acquire(context.Background(), "gap186-dead") {
		t.Fatal("Acquire dead = false (precondition)")
	}

	loop.reapZombies()

	if got := tickStatusOf(t, db, "gap186-dead-tick"); got != "timeout" {
		t.Errorf("dead-pid tick status = %q, want timeout", got)
	}
	if loop.slotPool.RunningSet()["gap186-dead"] {
		t.Error("dead-pid project claim survived the reaper — SCHED-GAP-186 wedge")
	}
	if got := tickStatusOf(t, db, "gap186-live-tick"); got != "running" {
		t.Errorf("live gateway tick status = %q, want running (INFRA-012: must survive)", got)
	}
	if !loop.slotPool.RunningSet()["gap186-live"] {
		t.Error("reaper released a LIVE tick's slot — over-admission: the packer would double-spawn the project")
	}
}

// TestSchedGap186_ReleaseReapedNoopWithoutClaims pins releaseReaped's edge
// behaviour: unknown names are a no-op (no panic, no semaphore token drained)
// and a reservation-only claim is cleared without touching the slot count.
func TestSchedGap186_ReleaseReapedNoopWithoutClaims(t *testing.T) {
	db := newTestDB(t)
	pool := NewSlotPool(2, NewSpawner(db, 2), NewLifecycleTracker(db))

	pool.releaseReaped([]string{"gap186-ghost-a", "gap186-ghost-b"})
	if !pool.tryReserve("gap186-ghost-a") {
		t.Error("reservation refused after a no-claim releaseReaped — pool state corrupted by the no-op path")
	}

	// Reservation-only claim (SCHED-GAP-103 window): cleared, with zero
	// effect on the semaphore.
	if !pool.tryReserve("gap186-resonly") {
		t.Fatal("tryReserve = false for an idle project (precondition)")
	}
	pool.releaseReaped([]string{"gap186-resonly"})
	if pool.RunningSet()["gap186-resonly"] {
		t.Error("reservation-only claim survived releaseReaped")
	}
	if got := pool.Running(); got != 0 {
		t.Errorf("Running = %d after a reservation-only release, want 0 — a semaphore token was drained", got)
	}
}
