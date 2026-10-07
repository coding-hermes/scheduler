package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SCHED-GAP-1664: the residual NIL-gateway drop path in Spawn() must book a
// DEFERRAL, not a lane failure.
//
// Measured 2026-09-28: a gateway crash cluster (7 restarts in 21s) left the
// scheduler up but with no gateway client reachable. 241 ticks failed in one
// hour, 209 of them with failure_reason=gateway_transport and the error text
// "no gateway client and exec fallback disabled for <lane>" — every one booked
// as a LANE FAILURE (noteSpawnFailure → consecutive_failures, GAP-133 backoff
// gate, auto-disable failure rate; bumpConsecutiveDrops alert noise; a
// cooldown burn per lane) while the actual cause was a fleet-wide transport
// outage.
//
// The nil-client path now reuses the SCHED-GAP-203 machinery
// (transientGatewayDeferral): the spawn returns a non-error SpawnedTick whose
// Wait() yields TickDeferred with the real reason text, slot_pool persists
// status=deferred/outcome=deferred, and the lane's consecutive_failures stays
// untouched. The HIGH event remains (auditability) with details saying
// DEFERRED. The exec-fallback-enabled path and the ErrGatewayKeyRejected
// terminal path are untouched.

// sgap1664DB is the per-test fresh database (newTestDBGap060 creates a
// temp-file SQLite with all migrations applied).
func sgap1664DB(t *testing.T) *sql.DB {
	t.Helper()
	return newTestDBGap060(t)
}

// sgap1664ConsecutiveFailures reads the project's lane-failure counter.
func sgap1664ConsecutiveFailures(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	return schedGap203BConsecutiveFailures(t, db, project)
}

// sgap1664NilGatewayDeferred drives the nil-client shape directly through
// Spawner.Spawn: gateway=nil, noExecFallback=true.
func TestSgap1664_NilGatewayDeferred(t *testing.T) {
	t.Helper()
	gap170BootState(t) // fresh, unarmed gate state (client nil; restores on cleanup)
	gap170ResetGate(t) // and no client left installed for the next test

	db := sgap1664DB(t)
	const projectName = "sgap1664-nil-gw"
	const tickID = "sgap1664-nil-gw-2026-09-28-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	spawner := NewSpawner(db, 4)
	spawner.SetClock(clock.NewSimClockAt(1000, time.Now()))
	// No SetGatewayClient call: the gateway client stays NIL.
	spawner.SetNoExecFallback(true)

	beforeDeferrals := gap170Deferrals()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — a nil gateway client with exec fallback disabled must be DEFERRED, not dropped as an error (SCHED-GAP-1664)", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on the nil-gateway path")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s — the nil-client residual must land as a deferral, not a failure", outcome.Status, TickDeferred)
	}
	if got := outcome.Status.Outcome(); got != "deferred" {
		t.Errorf("status.Outcome() = %q, want \"deferred\" (the outcome column must carry the deferral)", got)
	}
	if !strings.Contains(outcome.Error, "no gateway client and exec fallback disabled") {
		t.Errorf("outcome.Error = %q, want the real reason text (the deferral must be auditable)", outcome.Error)
	}
	if got := int(gap170Deferrals() - beforeDeferrals); got != 1 {
		t.Errorf("deferrals_total delta = %d, want 1 — the nil-client deferral must land in the SAME counter the gate uses", got)
	}
	// THE core assertion: the lane is NOT charged.
	if got := sgap1664ConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — a transport deferral is not the lane's failure (noteSpawnFailure must not fire)", got)
	}

	// Full stack: the same shape through the slot pool, asserting the ROW.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetClock(clock.NewSimClockAt(1001, time.Now()))
	loop.SetNoDeliver(true)
	// No SetGatewayClient call on the loop either: gateway client NIL.
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)

	fsTickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, fsTickID, 20*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state — the deferred outcome must still complete the tick row", fsTickID)
	}
	if status != "deferred" {
		t.Errorf("ticks.status = %q, want \"deferred\" — booking the nil-client residual as \"failed\" is the defect SCHED-GAP-1664 removes", status)
	}
	if got := schedGap203BOutcomeOf(t, db, fsTickID); got != "deferred" {
		t.Errorf("ticks.outcome = %q, want \"deferred\"", got)
	}
	if got := schedGap203BFailureReason(t, db, fsTickID); got != "" {
		t.Errorf("ticks.failure_reason = %q, want \"\" — the status column already names the class", got)
	}
	if got := sgap1664ConsecutiveFailures(t, db, projectName); got != 0 {
		t.Errorf("consecutive_failures after the slot-pool spawn = %d, want 0", got)
	}
}

// sgap1664NilGatewayEventSaysDeferred asserts the auditability half: exactly
// one HIGH spawn event is emitted for the nil-client deferral and its message
// says DEFERRED, not dropped.
func TestSgap1664_NilGatewayEventSaysDeferred(t *testing.T) {
	t.Helper()
	gap170BootState(t)
	gap170ResetGate(t)

	db := sgap1664DB(t)
	const projectName = "sgap1664-event"
	const tickID = "sgap1664-event-2026-09-28-00-00-01"
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	events := NewEventLogger(db)
	spawner := NewSpawner(db, 4)
	spawner.SetClock(clock.NewSimClockAt(1000, time.Now()))
	spawner.SetNoExecFallback(true)
	spawner.SetEventLogger(events)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn returned an error (%v)", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick on the nil-gateway path")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Fatalf("Wait() status = %s, want %s", outcome.Status, TickDeferred)
	}

	var msgs []string
	rows, err := db.Query(`SELECT message FROM events WHERE component = 'spawn' AND severity = 'HIGH' ORDER BY id`)
	if err != nil {
		t.Fatalf("query spawn events: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan event message: %v", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("HIGH spawn events = %d (%v), want exactly 1", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "deferred") {
		t.Errorf("event message = %q, want it to say the tick was DEFERRED (not dropped)", msgs[0])
	}
	if strings.Contains(msgs[0], "dropped") {
		t.Errorf("event message = %q, must NOT say the tick was dropped", msgs[0])
	}
}

// sgap1664ContextCancelledIsNotADeferral is a negative control: an explicit
// context.CANCELLED error must NOT route through the deferral site — the
// SCHED-GAP-203 predicate stays authoritative. Spawn() with a cancelled
// context behind a client returns through the normal gateway path; this test
// only checks the classifier split the nil-client path reuses, guarding
// against a future edit that widens the deferral to every error.
func TestSgap1664_ContextCancelledIsNotADeferral(t *testing.T) {
	t.Helper()
	gap170BootState(t)
	gap170ResetGate(t)

	if gatewayTransientBlip(context.Canceled) {
		t.Error("gatewayTransientBlip(context.Canceled) = true — the deferral predicate must stay narrow (a cancelled ctx is not a transport blip)")
	}
}
