package scheduler

// SCHED-GAP-146 — the deferred-spawn contract, executable.
//
// Two admission gates DEFER a spawn instead of dropping it:
//
//   - SCHED-GAP-125 load-average gate (load_gate.go), consulted at the top of
//     SlotPool.spawn BEFORE tryReserve and BEFORE any enqueue;
//   - SCHED-GAP-142 namespace-cap gate (namespace_gate.go), consulted inside the
//     spawn goroutine after tryReserve, before the global slot wait.
//
// Both are documented in PROSE only: "DEFER, not drop: the project keeps its
// selection — the next evaluation re-picks it once load drops; no cooldown is
// consumed and no progress penalty is recorded" (slot_pool.go:284-290) and
// "the row stays queued and is retried — no cooldown is consumed and the lane
// records no failure for a busy fleet" (slot_pool.go:336-344). Nothing executed
// that contract until this file. It matters because a gate that defers
// correctly but still charges the lane is indistinguishable from one that
// DROPS it: the fleet only ever sees the charge (a consumed cooldown, a
// failure counter, a spent nudge).
//
// The contract pinned here, per gate:
//
//  1. The deferred project's scheduling state is identical across the deferral:
//     cooldown_s, last_tick_completed, last_tick_started, consecutive_failures,
//     no_progress_ticks.
//  2. No tick row is created for the deferred project, and an already-enqueued
//     row is neither started nor failed — so no nudge/retry budget is spent.
//  3. No slot and no namespace claim is leaked (no phantom occupancy, which
//     would halve an effective cap or wedge the fleet's admission).
//  4. The project is RE-ADMITTED: it runs as soon as the gate clears. The load
//     gate keeps re-picking it on consecutive evaluations; the namespace-capped
//     attempt takes the slot the moment its sibling releases it. A gate that
//     defers but never re-admits is a silent stall (the reason this test exists).
//
// Every assertion is made against the REAL admission path — SlotPool.spawn for
// tests 1/2/4 and Loop.evaluate for test 3 — never against the gate helpers
// alone, so a gate that is wired out of that path fails here.
//
// THE FIXTURES ARE DISTINCTIVE ON PURPOSE. A freshly created project carries
// cooldown 900, failures 0, no_progress 0, no attempt clock — "unchanged" over
// those values is satisfied by a mutation that charges the lane and then
// zeroes it. gap146SeedLane writes non-default values (21600 / 2 / 3 + both
// clocks) so an equality assertion has something to lose.
//
// Two deliberate divergences from the task brief, both forced by reading the
// code (they are stated here so a reviewer does not read them as omissions):
//
//   - The brief expects the namespace-cap-deferred project to be ABSENT from
//     RunningSet. That is only true AFTER the parked attempt exits: spawn()
//     reserves the project (slot_pool.go:320) synchronously in the CALLER,
//     before the goroutine is launched, and the reservation is held for the
//     whole deferral — that IS the SCHED-GAP-103 dedup, and releasing it early
//     would let the next evaluation double-spawn the lane. Test 2 therefore
//     asserts the reservation is HELD during the deferral and released once the
//     attempt settles.
//   - The namespace wait is bounded by the package const
//     defaultNamespaceSlotPatience (30m, namespace_gate.go:60), so spawn's
//     timeout branch (the `return` after logNamespaceDeferral) is unreachable
//     inside a test's wall clock. Test 2 pins that branch DIRECTLY instead,
//     against the same live pool with the namespace genuinely at its cap
//     (waitNamespaceSlot on a short context must return false and leave no
//     claim). The re-admission contract is exercised the way production reaches
//     it: the parked attempt starts the tick the moment the sibling releases
//     the slot.
//   - consecutive_failures may go DOWN after re-admission — a successful spawn
//     resets it to 0 by design — so Test 2 asserts "never increased" for the
//     post-re-admission read; the strict-equality assertion runs across the
//     deferral window itself, where nothing at all may move.
//
// RED proof (SCHED-GAP-146 acceptance 8). Two mutations, each reverted before
// the commit, both captured in the task report:
//
//	M1 — slot_pool.go, the load-gate deferral path (:291-309): charge the lane
//	     before its `return` (UPDATE projects SET last_tick_completed = now,
//	     consecutive_failures = consecutive_failures + 1).
//	     → TestGAP146_LoadGateDeferPreservesAllState,
//	       TestGAP146_DeferDoesNotDoubleCountAcrossEvaluations and
//	       TestGAP146_DeferredSpawnLeavesNoTickRow all RED on the state
//	       assertion (got consecutiveFailures:3, lastTickCompleted:<now> —
//	       want the fixture's 2 / 2026-09-17T00:00:00Z).
//	M2 — namespace_gate.go tryClaimNamespaceSlot (:116): `>= cap` → `> cap`
//	     (admit one spawn past max_concurrent).
//	     → TestGAP146_NamespaceCapDeferPreservesAllState RED at
//	       `gap146-b tick status = "running", want "queued"`.
//
// A mutation inside spawn's namespace TIMEOUT branch (charging the lane before
// the `return` that follows logNamespaceDeferral) is deliberately NOT claimed as
// RED-able: that branch needs 30 minutes of wall clock to reach, which is why
// test 2 pins the branch directly instead (see below).

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// gap146State is the scheduling state a deferral must never touch.
type gap146State struct {
	cooldownS           int
	consecutiveFailures int
	noProgressTicks     int
	lastTickCompleted   string
	lastTickStarted     string
}

// gap146ReadState reads the five scheduling fields straight from the row (not
// through the API layer, so nothing normalizes them on the way out).
func gap146ReadState(t *testing.T, db *sql.DB, name string) gap146State {
	t.Helper()
	var st gap146State
	if err := db.QueryRow(`SELECT cooldown_s, consecutive_failures, no_progress_ticks,
		COALESCE(last_tick_completed, ''), COALESCE(last_tick_started, '')
		FROM projects WHERE name = ?`, name).Scan(
		&st.cooldownS, &st.consecutiveFailures, &st.noProgressTicks,
		&st.lastTickCompleted, &st.lastTickStarted); err != nil {
		t.Fatalf("read scheduling state for %s: %v", name, err)
	}
	return st
}

// gap146SeedLane creates an enabled lane in nsID and stamps a distinctive,
// non-default scheduling state on it, returning the snapshot the test compares
// against. Distinctive values are load-bearing: see the file header.
func gap146SeedLane(t *testing.T, db *sql.DB, name, nsID string) gap146State {
	t.Helper()
	capTestProject(t, db, name, nsID)
	if _, err := db.Exec(`UPDATE projects
		SET cooldown_s = 21600,
		    last_tick_completed = '2026-09-17T00:00:00Z',
		    last_tick_started   = '2026-09-17T00:05:00Z',
		    consecutive_failures = 2,
		    no_progress_ticks = 3
		WHERE name = ?`, name); err != nil {
		t.Fatalf("seed scheduling state for %s: %v", name, err)
	}
	st := gap146ReadState(t, db, name)
	if st.cooldownS != 21600 || st.consecutiveFailures != 2 || st.noProgressTicks != 3 ||
		st.lastTickCompleted == "" || st.lastTickStarted == "" {
		t.Fatalf("seed %s: state did not round-trip: %+v", name, st)
	}
	return st
}

// gap146AssertStateUnchanged fails when any scheduling field moved across a
// deferral. phase names the gate so the message says which contract broke.
func gap146AssertStateUnchanged(t *testing.T, db *sql.DB, name string, want gap146State, phase string) {
	t.Helper()
	got := gap146ReadState(t, db, name)
	if got != want {
		t.Errorf("%s: %s scheduling state changed across a DEFERRED spawn — got %+v, want %+v "+
			"(a deferral must consume no cooldown, record no failure, and levy no progress penalty)",
			phase, name, got, want)
	}
}

// gap146TickRows counts tick rows for a project; status "" counts every status.
func gap146TickRows(t *testing.T, db *sql.DB, project, status string) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM ticks WHERE project_name = ?`
	args := []any{project}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count ticks for %s (status=%q): %v", project, status, err)
	}
	return n
}

// gap146TickCountAll counts every tick row in the database — the "no row was
// created at all" assertion, which a per-project filter could mask.
func gap146TickCountAll(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&n); err != nil {
		t.Fatalf("count all ticks: %v", err)
	}
	return n
}

// gap146InFlightTicks counts queued+running rows (the pool's outstanding work).
func gap146InFlightTicks(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status IN ('queued','running')`).Scan(&n); err != nil {
		t.Fatalf("count in-flight ticks: %v", err)
	}
	return n
}

// gap146TickStatus returns a tick row's status, or "" when the row is absent.
func gap146TickStatus(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var st string
	err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&st)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read status of tick %s: %v", tickID, err)
	}
	return st
}

// gap146TickNudgeCount reads the retry/nudge budget spent by a tick row.
func gap146TickNudgeCount(t *testing.T, db *sql.DB, tickID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT nudge_count FROM ticks WHERE id = ?`, tickID).Scan(&n); err != nil {
		t.Fatalf("read nudge_count of tick %s: %v", tickID, err)
	}
	return n
}

// gap146LogCount counts captured log lines containing substr. The spawn
// goroutines write to the shared logger, so the read takes the buffer's mutex —
// these tests must stay -race clean.
func gap146LogCount(buf *admitLogBuf, substr string) int {
	buf.mu.Lock()
	defer buf.mu.Unlock()
	n := 0
	for _, ln := range strings.Split(buf.b.String(), "\n") {
		if strings.Contains(ln, substr) {
			n++
		}
	}
	return n
}

// gap146ArmLoadGate installs a deterministic load sampler plus the operator
// threshold (12) and returns the restore func the caller defers.
func gap146ArmLoadGate(load float64) func() {
	orig := currentLoad1m
	currentLoad1m = func() (float64, bool) { return load, true }
	SetLoadGateThreshold(12)
	return func() {
		SetLoadGateThreshold(0)
		currentLoad1m = orig
	}
}

// gap146ArmLoadGateOff makes the gate definitively INACTIVE and restores on
// cleanup: used by tests where the gate under test must be the only one firing.
func gap146ArmLoadGateOff(t *testing.T) {
	t.Helper()
	SetLoadGateThreshold(0)
	t.Cleanup(func() { SetLoadGateThreshold(0) })
}

// TestGAP146_LoadGateDeferPreservesAllState: the load-average gate trips, a
// real slot-pool spawn is attempted, and NOTHING is charged to the lane and
// nothing is written to the DB. The early return is in the CALLER's goroutine
// (slot_pool.go:291-309, before `go func()` at :325), so the assertions are
// synchronous by construction — no goroutine wait, no race.
func TestGAP146_LoadGateDeferPreservesAllState(t *testing.T) {
	db := newTestDB(t)
	const ns = "coding-hermes"
	// cap 0 = unlimited: the namespace gate must not be the reason for anything
	// that happens below (premise checked explicitly further down).
	capTestNamespace(t, db, ns, 0, "cooldown")

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	const name = "gap146-lg"
	want := gap146SeedLane(t, db, name, ns)

	// PREMISE — the gate really trips here. Without this, "nothing changed"
	// would also be satisfied by a spawn that never entered the gate at all.
	if !LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: LoadGateShouldDefer(%q) = false with threshold %.0f and load 15.0 — the fixture does not trip the gate",
			ns, loadGateThreshold())
	}
	if got := namespaceCapDB(db, ns); got != 0 {
		t.Fatalf("premise: namespace %s cap = %d, want 0 (unlimited) — the cap gate must not be the gate under test", ns, got)
	}

	logbuf := admitCaptureLog(t)
	tickID := l.slotPool.Spawn(PackedProject{Name: name, NamespaceID: ns}, time.Now(), true, db)

	// PREMISE — the spawn was ATTEMPTED and DEFERRED (not merely never fired).
	// This line is the gate's own evidence that the admission point was reached.
	if got := gap146LogCount(logbuf, "LOAD-GATE: deferring "+name); got != 1 {
		t.Fatalf("LOAD-GATE deferral lines for %s = %d, want 1 — the spawn must have reached the gate and deferred (tickID=%s)",
			name, got, tickID)
	}

	if got := gap146TickStatus(t, db, tickID); got != "" {
		t.Errorf("tick row %s exists with status %q — a load-gate deferral writes no row (the enqueue sits after the gate)", tickID, got)
	}
	if got := gap146TickRows(t, db, name, ""); got != 0 {
		t.Errorf("tick rows for %s = %d, want 0 — a deferred spawn creates no row, not even a queued one", name, got)
	}
	if got := gap146TickRows(t, db, name, "running"); got != 0 {
		t.Errorf("running rows for %s = %d, want 0", name, got)
	}
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0 — a deferred spawn takes no global slot", got)
	}
	if l.slotPool.RunningSet()[name] {
		t.Errorf("%s is in RunningSet after a load-gate deferral — the gate returns before tryReserve, so no reservation (and therefore no phantom occupancy) may survive it", name)
	}
	gap146AssertStateUnchanged(t, db, name, want, "load gate")
}

// TestGAP146_NamespaceCapDeferPreservesAllState: a namespace capped at 1 already
// has one tick running; a second lane's spawn attempt reaches the pool, is
// reserved, and parks at the namespace gate — where it must charge nothing, and
// from where it must run as soon as the sibling releases the slot.
func TestGAP146_NamespaceCapDeferPreservesAllState(t *testing.T) {
	db := newTestDB(t)
	const ns = "gap146-capped"
	capTestNamespace(t, db, ns, 1, "cooldown") // cap 1
	gap146ArmLoadGateOff(t)

	const a, b = "gap146-a", "gap146-b"
	gap146SeedLane(t, db, a, ns)
	wantB := gap146SeedLane(t, db, b, ns)

	// PREMISE — the gate under test is the CAP, not the load gate.
	if LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: the load gate is active (threshold=%.0f) — it must be off for this test", loadGateThreshold())
	}

	// Both lanes already have the queued row a nudge/API/wave path would have
	// enqueued before handing the tick to the pool, so "the deferred row stays
	// queued" is observable.
	queuedTickRow(t, db, a+"-t1", a)
	queuedTickRow(t, db, b+"-t1", b)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	l.noDeliver = true
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	// gap146-a enters the pool and becomes the namespace's single running tick.
	l.slotPool.SpawnEnqueued(PackedProject{Name: a, NamespaceID: ns}, a+"-t1", time.Now(), true, db)
	waitUntil(t, 20*time.Second, "gap146-a to hold the namespace's only slot", func() bool {
		return namespaceRunningDB(db, ns) == 1
	})

	// gap146-b is admitted into the pool (reserved — SCHED-GAP-103) and parks at
	// the namespace gate.
	l.slotPool.SpawnEnqueued(PackedProject{Name: b, NamespaceID: ns}, b+"-t1", time.Now(), true, db)

	// Well past the 250ms gate poll interval: a spawn that slipped through would
	// have started by now, so a still-queued row is evidence of the deferral.
	time.Sleep(1200 * time.Millisecond)

	if got := gap146TickStatus(t, db, b+"-t1"); got != "queued" {
		t.Fatalf("gap146-b tick status = %q, want \"queued\" — a namespace-capped spawn DEFERS: the row waits, it is not started, and it is not failed", got)
	}
	if got := gap146TickRows(t, db, b, "running"); got != 0 {
		t.Errorf("gap146-b running rows = %d, want 0 — the cap (1) was breached", got)
	}
	if got := gap146TickRows(t, db, b, ""); got != 1 {
		t.Errorf("gap146-b tick rows = %d, want 1 (its own queued row) — a deferred spawn must create no additional row", got)
	}
	if got := gap146TickRows(t, db, b, "failed"); got != 0 {
		t.Errorf("gap146-b failed rows = %d, want 0 — a busy namespace must not charge the lane a failure", got)
	}
	if got := gap146TickNudgeCount(t, db, b+"-t1"); got != 0 {
		t.Errorf("gap146-b nudge_count = %d, want 0 — the deferral must not spend the retry budget", got)
	}
	if got := namespaceRunningDB(db, ns); got != 1 {
		t.Errorf("namespace running ticks = %d, want 1 — the deferred attempt must not occupy the namespace", got)
	}
	if got := l.slotPool.NamespacePending(ns); got != 0 {
		t.Errorf("namespace claims pending = %d, want 0 — a waiter that never got the slot holds no claim (a leaked claim would halve the effective cap)", got)
	}
	if got := l.slotPool.Running(); got != 1 {
		t.Errorf("slot pool Running() = %d, want 1 (only gap146-a) — the deferred attempt must not take a global slot", got)
	}
	// Divergence from the brief, documented in the file header: the reservation
	// is deliberately HELD for the duration of the parked attempt.
	if !l.slotPool.RunningSet()[b] {
		t.Errorf("gap146-b is NOT in RunningSet during its deferral — the SCHED-GAP-103 reservation must be held while the attempt is in flight, or the next evaluation double-spawns the lane")
	}
	gap146AssertStateUnchanged(t, db, b, wantB, "namespace cap")

	// The gate's OTHER exit — waitNamespaceSlot giving up (slot_pool.go:360-363:
	// logNamespaceDeferral + `return`) — is bounded by the package const
	// defaultNamespaceSlotPatience (30m), so it cannot be reached through spawn()
	// inside a test's wall clock. Pin that branch here instead, against the same
	// live pool: a waiter that times out takes NO claim (so the effective cap
	// stays exact for the next attempt) and charges the lane nothing.
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	gaveUp := l.slotPool.waitNamespaceSlot(timeoutCtx, ns, db)
	timeoutCancel()
	if gaveUp {
		t.Fatalf("waitNamespaceSlot returned true while the namespace was at its cap (1 running) — the gate admitted a spawn past max_concurrent")
	}
	// The deferral logger spawn calls on that exit: it must be harmless (no
	// state, no panic) — the state assertion below covers the no-write half.
	l.slotPool.logNamespaceDeferral(PackedProject{Name: b, NamespaceID: ns}, b+"-t1", 1, 1, 300*time.Millisecond)
	if got := l.slotPool.NamespacePending(ns); got != 0 {
		t.Fatalf("namespace claims pending after a timed-out wait = %d, want 0 — a waiter that gives up took nothing, so it may release nothing (a negative claim would corrupt the cap)", got)
	}
	gap146AssertStateUnchanged(t, db, b, wantB, "namespace cap (timed-out wait)")

	// The cap clears (the sibling finishes) → the deferred attempt must be
	// RE-ADMITTED and run to completion. Defer, never drop.
	gw.releaseAll()
	waitUntil(t, 30*time.Second, "the deferred lane to be re-admitted and both ticks to settle", func() bool {
		return gap146InFlightTicks(t, db) == 0
	})

	if got := gap146TickStatus(t, db, b+"-t1"); got != "completed" {
		t.Errorf("gap146-b tick status after the cap cleared = %q, want \"completed\" — a deferred project must run (a gate that defers but never re-admits is a silent stall)", got)
	}
	if got := gap146TickStatus(t, db, a+"-t1"); got != "completed" {
		t.Errorf("gap146-a tick status = %q, want \"completed\" (premise: the sibling released the slot)", got)
	}
	if got := gap146TickRows(t, db, b, ""); got != 1 {
		t.Errorf("gap146-b tick rows = %d, want 1 — re-admission must reuse the queued row, never mint a duplicate", got)
	}
	if got := gap146TickRows(t, db, b, "failed"); got != 0 {
		t.Errorf("gap146-b failed rows = %d, want 0 after re-admission", got)
	}
	if got := namespaceRunningDB(db, ns); got != 0 {
		t.Errorf("namespace running ticks = %d, want 0 once both ticks are terminal", got)
	}
	if got := l.slotPool.NamespacePending(ns); got != 0 {
		t.Errorf("namespace claims pending = %d, want 0 after re-admission (a claim that outlives its tick wedges the cap)", got)
	}
	if set := l.slotPool.RunningSet(); len(set) != 0 {
		t.Errorf("RunningSet = %v, want empty — reservations must be dropped on every exit path", set)
	}
	// The deferral itself was never charged. The counter may go DOWN here (a
	// successful spawn resets it to 0) but it must never go UP: the deferral is
	// the only thing that happened between the two reads that could have
	// incremented it.
	if got := gap146ReadState(t, db, b).consecutiveFailures; got > wantB.consecutiveFailures {
		t.Errorf("gap146-b consecutive_failures = %d, want <= %d — a cap deferral records no failure for the lane", got, wantB.consecutiveFailures)
	}
}

// TestGAP146_DeferDoesNotDoubleCountAcrossEvaluations: after a defer,
// consecutive evaluations keep re-picking the same project until it runs. This
// is the "not the next 5-minute run forever" half of the contract, driven
// through the real evaluation loop (the ADMIT/LOAD-GATE lines are the
// observable that the project was re-picked on every pass).
func TestGAP146_DeferDoesNotDoubleCountAcrossEvaluations(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	const ns = "gap146-iter-ns"
	admitInsertNamespace(t, db, ns, 0, "cooldown")
	const name = "gap146-iter"
	admitInsertProject(t, db, admitProjectSpec{
		Name: name, NS: ns, CooldownS: 60,
		Last: now.Add(-5 * time.Minute), Workdir: t.TempDir(),
	})
	want := gap146ReadState(t, db, name)

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetClock(clock.NewFixed(now)) // deterministic selection instant
	l.noDeliver = true
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	restore := gap146ArmLoadGate(15.0)
	defer restore()
	if !LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: LoadGateShouldDefer(%q) = false — the fixture does not trip the gate", ns)
	}

	logbuf := admitCaptureLog(t)

	// Three consecutive evaluations while the gate is up.
	const passes = 3
	for i := 0; i < passes; i++ {
		l.evaluate()
	}
	if got := gap146LogCount(logbuf, "LOAD-GATE: deferring "+name); got != passes {
		t.Fatalf("load-gate deferrals across %d consecutive evaluations = %d, want %d — the deferred project must be re-picked on EVERY pass "+
			"(a deferral that consumes the cooldown or the attempt clock makes the packer skip it, and the fleet sees a silent stall)",
			passes, got, passes)
	}
	if got := gap146TickCountAll(t, db); got != 0 {
		t.Errorf("tick rows after %d deferred evaluations = %d, want 0 — a deferral must not enqueue anything", passes, got)
	}
	gap146AssertStateUnchanged(t, db, name, want, "load gate x3 evaluations")

	// The gate clears → the VERY NEXT evaluation admits the project. No residual
	// cooldown, no leftover reservation, no waiting for a cooldown window.
	currentLoad1m = func() (float64, bool) { return 3.0, true }
	l.evaluate()

	waitUntil(t, 20*time.Second, "the previously deferred lane to start once the gate cleared", func() bool {
		return gap146TickRows(t, db, name, "running") == 1
	})
	// Barrier: the tick is parked on the held gateway, so it provably started
	// because the gate cleared (not because it was never deferred).
	waitForHeldSpawn(t, gw, 5*time.Second)

	gw.releaseAll()
	waitUntil(t, 30*time.Second, "the re-admitted tick to settle", func() bool {
		return gap146InFlightTicks(t, db) == 0
	})

	if got := gap146TickRows(t, db, name, "completed"); got != 1 {
		t.Errorf("completed rows for %s = %d, want 1 — a deferred project must RUN once the gate clears", name, got)
	}
	if got := gap146TickRows(t, db, name, "failed"); got != 0 {
		t.Errorf("failed rows for %s = %d, want 0 — a gate deferral is not a lane failure", name, got)
	}
	if got := gap146TickCountAll(t, db); got != 1 {
		t.Errorf("tick rows for the whole run = %d, want 1 — the deferrals must not have left extra rows behind", got)
	}
}

// TestGAP146_DeferredSpawnLeavesNoTickRow: the DB-write half of the contract,
// for both entry points into the pool. A load-gated spawn must leave the ticks
// table untouched — no new row for an un-enqueued spawn, and no status change
// for a row the caller had already enqueued (the API/nudge/wave shape).
func TestGAP146_DeferredSpawnLeavesNoTickRow(t *testing.T) {
	db := newTestDB(t)
	const ns = "coding-hermes"
	capTestNamespace(t, db, ns, 0, "cooldown")

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	t.Run("plain spawn writes no row", func(t *testing.T) {
		const name = "gap146-lg-norow"
		want := gap146SeedLane(t, db, name, ns)

		tickID := l.slotPool.Spawn(PackedProject{Name: name, NamespaceID: ns}, time.Now(), true, db)

		if got := gap146TickRows(t, db, name, "running"); got != 0 {
			t.Errorf("running rows for %s = %d, want 0", name, got)
		}
		if got := gap146TickRows(t, db, name, ""); got != 0 {
			t.Errorf("tick rows for %s = %d, want 0 (tickID=%s was never persisted)", name, got, tickID)
		}
		gap146AssertStateUnchanged(t, db, name, want, "load gate")
	})

	t.Run("enqueued row is not started", func(t *testing.T) {
		const name = "gap146-lg-enqueued"
		const tickID = "gap146-lg-enqueued-t1"
		want := gap146SeedLane(t, db, name, ns)
		queuedTickRow(t, db, tickID, name)

		l.slotPool.SpawnEnqueued(PackedProject{Name: name, NamespaceID: ns}, tickID, time.Now(), true, db)

		// Synchronous by construction: the load gate returns before `go func()`,
		// so there is nothing to wait for here.
		if got := gap146TickStatus(t, db, tickID); got != "queued" {
			t.Errorf("enqueued tick status = %q, want \"queued\" — a load-gate deferral must not start (or fail) a row it did not create", got)
		}
		if got := gap146TickRows(t, db, name, "running"); got != 0 {
			t.Errorf("running rows for %s = %d, want 0", name, got)
		}
		if got := gap146TickRows(t, db, name, ""); got != 1 {
			t.Errorf("tick rows for %s = %d, want 1 (only the caller's queued row)", name, got)
		}
		if got := gap146TickNudgeCount(t, db, tickID); got != 0 {
			t.Errorf("nudge_count = %d, want 0 — a deferral spends no retry budget", got)
		}
		if l.slotPool.RunningSet()[name] {
			t.Errorf("%s is in RunningSet after a load-gate deferral — no reservation may outlive the deferred attempt", name)
		}
		if got := l.slotPool.Running(); got != 0 {
			t.Errorf("slot pool Running() = %d, want 0", got)
		}
		gap146AssertStateUnchanged(t, db, name, want, "load gate (enqueued)")
	})
}
