package scheduler_test

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1705 — a tick whose terminal status is TickTimeout counts as a
// LANE failure for backoff purposes: LifecycleTracker.Complete increments
// projects.consecutive_failures the same way the spawn path does, UNLESS the
// failure is transport-class (failureReasonClass(outcome.Error) != "") — the
// SCHED-GAP-143 carve-out, preserved exactly: a drained/unreachable gateway
// is never the lane's fault.
//
// Measured evidence (task-router-foreman, 2026-10-02): two wave ticks each
// ended status=timeout at exactly 3h0m0s with worker_count=0 and tokens 0/0
// — and consecutive_failures stayed 0 at a 21600s cooldown, so the same dead
// shape retried every 6h. The counter is the only lever this change moves:
// packer.effectiveCooldown feeds it to FailureBackoff; nothing new is wired
// into auto-disable.
//
// Transport-class markers below are quoted from the HarnessFailure marker
// list (failureclass.go) — the single classifier authority (SCHED-GAP-173).

// timeoutCase is one (name, error text, expected counter delta) row.
type timeoutCase struct {
	name      string
	errText   string // "" = no error recorded (the common wave-stall row)
	transport bool   // true = expect the counter UNTOUCHED (carve-out)
}

func timeoutBackoffCases() []timeoutCase {
	return []timeoutCase{
		// AC1: non-transport timeouts increment — including the measured
		// shape: a wave tick that burned the whole wall in silence.
		{name: "wave stall, empty error", errText: ""},
		{name: "stalled session, project-side error", errText: "worker wave wedged: no worker terminal after 3h"},
		// AC2: transport-class timeouts are the carve-out — untouched.
		{name: "gateway drain", errText: "gateway refused: Gateway is draining", transport: true},
		{name: "gateway unreachable", errText: "gateway unreachable: connection refused", transport: true},
		{name: "tick deadline tearing a gateway POST", errText: "tick-timeout wall 3h0m0s: tick deadline exceeded", transport: true},
		{name: "session-silence watchdog kill", errText: "session silent for 45m0s — terminated", transport: true},
	}
}

// TestSCHEDGAP1705_TimeoutCountsAsLaneFailure walks each case through the
// full lifecycle (Enqueue → StartRunning → Complete) against a real sqlite
// DB and asserts the exact post-Complete counter: previous+1 when the
// timeout is the lane's fault, previous untouched when it is the harness's.
func TestSCHEDGAP1705_TimeoutCountsAsLaneFailure(t *testing.T) {
	cases := timeoutBackoffCases()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			const project = "gap1705-lane"
			mustCreateProject(t, db, project)
			const before = 5
			setConsecutiveFailures(t, db, project, before)

			lt := scheduler.NewLifecycleTracker(db)
			tickID := project + "-1"
			if err := lt.Enqueue(project, tickID); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := lt.StartRunning(tickID); err != nil {
				t.Fatalf("StartRunning: %v", err)
			}
			now := time.Now().UTC()
			if err := lt.Complete(scheduler.TickOutcome{
				TickID:   tickID,
				Project:  project,
				Started:  now.Add(-3 * time.Hour),
				Finished: now,
				Status:   scheduler.TickTimeout,
				Error:    tc.errText,
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			got := getConsecutiveFailures(t, db, project)
			want := before + 1
			if tc.transport {
				want = before
			}
			if got != want {
				t.Errorf("consecutive_failures = %d, want %d (errText=%q transport=%v)", got, want, tc.errText, tc.transport)
			}
		})
	}
}

// TestSCHEDGAP1705_TimeoutIncrementAccumulates pins the SQL statement shape:
// the increment is a relative +1 (consecutive_failures = consecutive_failures
// + 1), not an absolute stamp, so back-to-back silent timeouts accumulate
// exactly like back-to-back spawn-path failures do.
func TestSCHEDGAP1705_TimeoutIncrementAccumulates(t *testing.T) {
	db := newTestDB(t)
	const project = "gap1705-accum"
	mustCreateProject(t, db, project)
	setConsecutiveFailures(t, db, project, 2)

	lt := scheduler.NewLifecycleTracker(db)
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		tickID := project + "-" + time.Duration(i).String()
		if err := lt.Enqueue(project, tickID); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		if err := lt.StartRunning(tickID); err != nil {
			t.Fatalf("StartRunning %d: %v", i, err)
		}
		if err := lt.Complete(scheduler.TickOutcome{
			TickID:   tickID,
			Project:  project,
			Started:  now.Add(-3 * time.Hour),
			Finished: now,
			Status:   scheduler.TickTimeout,
		}); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}

	if got := getConsecutiveFailures(t, db, project); got != 5 {
		t.Errorf("consecutive_failures = %d after 3 silent timeouts from 2, want 5", got)
	}
}

// TestSCHEDGAP1705_FailureStillLeavesCounterAlone pins the boundary the
// change deliberately preserves: TickFailed does NOT increment here, because
// spawn-path failures were already counted at noteSpawnFailureClassed — a
// Complete-side increment for failed outcomes would double-charge the lane.
func TestSCHEDGAP1705_FailureStillLeavesCounterAlone(t *testing.T) {
	db := newTestDB(t)
	const project = "gap1705-failed"
	mustCreateProject(t, db, project)
	setConsecutiveFailures(t, db, project, 7)

	lt := scheduler.NewLifecycleTracker(db)
	tickID := project + "-1"
	if err := lt.Enqueue(project, tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  project,
		Started:  now.Add(-time.Minute),
		Finished: now,
		Status:   scheduler.TickFailed,
		ExitCode: 1,
		Error:    "worker exited 1",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := getConsecutiveFailures(t, db, project); got != 7 {
		t.Errorf("consecutive_failures = %d after TickFailed, want 7 (spawn path already counted it)", got)
	}
}

// TestSCHEDGAP1705_TimeoutStampsLastTickStatus regression-guards the
// SCHED-GAP-214 write the increment sits beside: the same Complete still
// stamps last_tick_status='timeout' on the project row.
func TestSCHEDGAP1705_TimeoutStampsLastTickStatus(t *testing.T) {
	db := newTestDB(t)
	const project = "gap1705-status"
	mustCreateProject(t, db, project)

	lt := scheduler.NewLifecycleTracker(db)
	tickID := project + "-1"
	if err := lt.Enqueue(project, tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(scheduler.TickOutcome{
		TickID:   tickID,
		Project:  project,
		Started:  now.Add(-3 * time.Hour),
		Finished: now,
		Status:   scheduler.TickTimeout,
		Error:    "stalled in silence",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var lastStatus string
	if err := db.QueryRow(`SELECT last_tick_status FROM projects WHERE name = ?`, project).Scan(&lastStatus); err != nil {
		t.Fatalf("query last_tick_status: %v", err)
	}
	if lastStatus != "timeout" {
		t.Errorf("last_tick_status = %q, want timeout", lastStatus)
	}
}
