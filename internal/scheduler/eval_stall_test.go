package scheduler

import (
	"database/sql"
	"testing"
	"time"
)

// GAP-042 eval-stall watchdog tests. The event-driven loop has no periodic
// eval trigger, so a fully idle fleet (0 running ticks, everything in
// cooldown) silently stops scheduling — observed 66-min gap 2026-08-13
// 13:08-14:14 local, recovered only by manual POST /api/v1/evaluate.
// checkEvalStall runs from the 30s health ticker (always fires) and forces
// re-evaluation + a HIGH event when lastEval ages past evalStallThreshold
// with zero in-flight ticks.

func TestEvalStall_HealthyNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now()

	l.checkEvalStall(0)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_StaleWithRunningTicksNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	// Ticks in flight = the loop is busy; the slot-freed debounce will fire
	// an eval when a slot opens. Not a stall.
	l.checkEvalStall(2)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_NeverEvaluatedNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	// lastEval zero — startup window; the initial eval fires immediately.

	l.checkEvalStall(0)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_StaleIdleForcesEvalAndEmits(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	l.checkEvalStall(0)

	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

func TestEvalStall_ThrottledWhilePersisting(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// Still stalled a minute later: the forced re-evaluation continues on
	// every crossing, but the HIGH event is throttled until reEmitGap.
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

func TestEvalStall_ReEmitsAfterPersistGap(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// A wedged loop (forced evals never run, lastEval never refreshes)
	// re-emits so the stall stays visible.
	l.lastStallEvent = time.Now().Add(-(evalStallReEmitGap + time.Minute))
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 2)
}

func TestEvalStall_RecoveryWithinGapThrottlesNextOnset(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// Loop recovers (lastEval refreshes), then a new stall episode starts
	// within the re-emit gap: the forced re-evaluation still fires, but the
	// HIGH event is throttled (one event per evalStallReEmitGap window —
	// same philosophy as the SCHED-GAP-014 starvation throttle). A fresh
	// episode after the gap re-emits immediately (covered by
	// TestEvalStall_ReEmitsAfterPersistGap).
	l.lastEval = time.Now()
	l.checkEvalStall(0)
	assertStallEventCount(t, db, 1)

	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

// SCHED-GAP-061 regression tests. The watchdog previously re-emitted the
// HIGH "eval loop stalled" event every evalStallReEmitGap while the fleet
// idled (37 HIGH events 08-19T20:08Z..08-21T10:03Z, every one
// self-recovered by the next forced eval). The stall condition recurs on a
// healthy idle fleet by construction: the forced re-evaluation is consumed
// by the loop (evaluate() refreshes lastEval) and picks nothing, then
// lastEval ages past evalStallThreshold again. Now a detection whose
// previous forced eval recovered within one cycle is demoted to MEDIUM;
// first onset and non-recovery stay HIGH.

func TestEvalStall_OneCycleRecoveryDemotedToMedium(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	// First onset after the fleet goes idle: no previous forced eval —
	// HIGH.
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCountSeverity(t, db, "HIGH", 1)
	assertStallEventCountSeverity(t, db, "MEDIUM", 0)

	// The forced re-evaluation runs: evaluate() refreshes lastEval.
	l.lastEval = time.Now()

	// Five minutes later the condition recurs (idle cadence — nothing
	// re-triggers evaluation between forced evals). The previous force
	// was pushed within the episode window and consumed by the loop
	// (lastEval is newer than the force), so this detection is a
	// one-cycle recovery: MEDIUM, not HIGH. Open the re-emit window so
	// the demoted event actually lands.
	l.lastStallForce = time.Now().Add(-10 * time.Minute)
	l.lastStallEvent = time.Now().Add(-(evalStallReEmitGap + time.Minute))
	l.lastEval = time.Now().Add(-6 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCountSeverity(t, db, "HIGH", 1)
	assertStallEventCountSeverity(t, db, "MEDIUM", 1)
}

func TestEvalStall_NonRecoveryStaysHigh(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCountSeverity(t, db, "HIGH", 1)
	assertStallEventCountSeverity(t, db, "MEDIUM", 0)

	// A wedged loop never consumes the forced evals: lastEval stays
	// frozen at the pre-force value. After the re-emit gap the stall
	// re-alarms HIGH — the recovery demotion must not mask a loop that
	// never evaluates.
	l.lastStallEvent = time.Now().Add(-(evalStallReEmitGap + time.Minute))
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCountSeverity(t, db, "HIGH", 2)
	assertStallEventCountSeverity(t, db, "MEDIUM", 0)
}

func TestEvalStall_FreshOnsetAfterLongHealthyPeriodStaysHigh(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	// The fleet was busy for hours (last forced eval from a long-closed
	// idle episode), then went quiet: lastEval ages past the threshold.
	// The stale force is outside the episode window, so this is a fresh
	// onset — HIGH, not a one-cycle recovery.
	l.lastStallForce = time.Now().Add(-24 * time.Hour)
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCountSeverity(t, db, "HIGH", 1)
	assertStallEventCountSeverity(t, db, "MEDIUM", 0)
}

func assertForcedEval(t *testing.T, l *Loop) {
	t.Helper()
	select {
	case <-l.evalCh:
	case <-time.After(time.Second):
		t.Fatal("expected forced re-evaluation signal on evalCh")
	}
}

func assertNoForcedEval(t *testing.T, l *Loop) {
	t.Helper()
	select {
	case <-l.evalCh:
		t.Fatal("unexpected forced re-evaluation signal on evalCh")
	case <-time.After(50 * time.Millisecond):
	}
}

func assertStallEventCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	assertStallEventCountSeverity(t, db, "HIGH", want)
}

func assertStallEventCountSeverity(t *testing.T, db *sql.DB, severity string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE severity = ? AND component = 'loop' AND message LIKE 'eval loop stalled%'`,
		severity,
	).Scan(&n); err != nil {
		t.Fatalf("count %s stall events: %v", severity, err)
	}
	if n != want {
		t.Fatalf("%s stall events = %d, want %d", severity, n, want)
	}
}
