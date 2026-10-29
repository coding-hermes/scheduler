package scheduler_test

// SCHED-GAP-16940 — the PERSISTED failure vocabulary must name the failing
// HOP, not assert a cause it did not observe.
//
// Measured on the live scheduler DB (2026-10-29):
//
//	24h census, ticks.failure_reason:
//	  gateway_transport             27
//	  gateway_transport + orphan_reason=drain_timeout   17   <- ours, not the gateway
//	  '' (project-side)             41
//
//	3h census, verbatim error text (what the operator actually reads):
//	  "aborted by graceful shutdown — drain timed out with tick in flight"   9
//	  "...gateway POST: HTTP 502: {\"error\": \"timed out\", \"_router\": ...}"   7
//	  "...gateway POST: HTTP 500: {\"error\": \"Internal ...}"                6
//	  "stalled: no progress for 30m0s ..."                                      9
//
// Two lies in one column:
//
//  1. Our OWN drain reap — the scheduler killed the tick on restart — stamped
//     failure_reason='gateway_transport'. 17 of the 44 failed rows in 24h are
//     the scheduler's own restart charged to the gateway. The accounting
//     surfaces already exclude these rows (SCHED-GAP-1608 reads orphan_reason
//     / the abort text), but the operator-facing CLASS is wrong, so the P0
//     row's "how many ticks are in this class" question cannot be answered
//     from SQL, and criterion 4's "zero in this class" window is unreachable
//     by construction: every deploy mints a handful.
//
//  2. A gateway that ANSWERED — 502 from the router in front of it (the body
//     carries "_router": {"complexity_score": ...}), or its own 500 — was
//     wrapped as "gateway unreachable" and classed gateway_transport. The
//     gateway was reached; it refused because its UPSTREAM failed. That is a
//     different owner (provider lane) and a different runbook step (restart
//     the gateway vs look at the model hop).
//
// The vocabulary this test pins — closed set, one entry per observed hop:
//
//	""                 project-side / not transport-class (unchanged)
//	gateway_drain      the drain 503 refusal (unchanged; SCHED-GAP-143)
//	gateway_5xx        the gateway ANSWERED 5xx and it is not the drain (NEW)
//	gateway_transport  no HTTP answer: refused dial, deadline, unreadable (unchanged)
//	operator_restart   our own drain reap on restart (NEW)
//	session_silent     the silence watchdog's kill       (unchanged; 1707)
//	sleep_poll         the poll-loop guard's kill        (unchanged; 1698)
//	instant_turn       the one-shot gateway turn         (unchanged; 1641)
//
// Controls are deliberate: a refused dial must STAY gateway_transport, a
// drain 503 must STAY gateway_drain, and a project-side failure must STAY
// empty — otherwise "distinguish the classes" could be satisfied by renaming
// everything, which would be a new lie rather than a smaller one.

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// gap16940Case is one (verbatim error text, expected persisted class) row.
// errText values are quoted from the live ticks.error column.
type gap16940Case struct {
	name    string
	errText string
	want    string
	// status lets the same cases also prove the timeout carve-out survives:
	// the class is what decides whether consecutive_failures moves.
	status scheduler.TickStatus
}

func gap16940Cases() []gap16940Case {
	return []gap16940Case{
		{
			name:    "operator drain reap is ours, not the gateway's",
			errText: "aborted by graceful shutdown — drain timed out with tick in flight",
			want:    "operator_restart",
			status:  scheduler.TickFailed,
		},
		{
			name: "gateway answered 502 (router upstream timeout)",
			errText: "gateway unreachable and exec fallback disabled: gateway POST: HTTP 502: " +
				`{"error": "timed out", "_router": {"complexity_score": 0.71}}`,
			want:   "gateway_5xx",
			status: scheduler.TickFailed,
		},
		{
			name:    "gateway answered 500 (its own internal error)",
			errText: `gateway unreachable and exec fallback disabled: gateway POST: HTTP 500: {"error": "Internal Server Error"}`,
			want:    "gateway_5xx",
			status:  scheduler.TickFailed,
		},
		// CONTROLS — these must NOT move.
		{
			name: "refused dial stays transport",
			errText: "gateway unreachable and exec fallback disabled: gateway POST: Post " +
				`"http://127.0.0.1:9391/v1/responses": dial tcp 127.0.0.1:9391: connect: connection refused`,
			want:   "gateway_transport",
			status: scheduler.TickFailed,
		},
		{
			name:    "drain 503 stays gateway_drain",
			errText: `gateway POST: HTTP 503: {"error": "Gateway is draining existing work; retry shortly."}`,
			want:    "gateway_drain",
			status:  scheduler.TickFailed,
		},
		{
			name:    "project-side failure stays unclassified",
			errText: "worker wave wedged: no worker terminal after 3h",
			want:    "",
			status:  scheduler.TickFailed,
		},
	}
}

// TestSCHEDGAP16940_FailureReasonNamesTheHop walks each verbatim error text
// through the real lifecycle (Enqueue → StartRunning → Complete) and asserts
// the PERSISTED ticks.failure_reason — not the classifier's return value in
// isolation, because the row is the artifact an operator reads.
func TestSCHEDGAP16940_FailureReasonNamesTheHop(t *testing.T) {
	for _, tc := range gap16940Cases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			const project = "gap16940-lane"
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
				Started:  now.Add(-2 * time.Minute),
				Finished: now,
				Status:   tc.status,
				ExitCode: -1,
				Error:    tc.errText,
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			var got string
			if err := db.QueryRow(`SELECT failure_reason FROM ticks WHERE id = ?`, tickID).Scan(&got); err != nil {
				t.Fatalf("read failure_reason: %v", err)
			}
			if got != tc.want {
				t.Errorf("failure_reason = %q, want %q for error text %q", got, tc.want, tc.errText)
			}
		})
	}
}

// TestSCHEDGAP16940_OperatorRestartKeepsTheCarveOut pins that re-classing the
// drain reap does NOT change what it costs the lane: the reason is non-empty,
// so the SCHED-GAP-143/1705 carve-out still leaves consecutive_failures alone.
// A vocabulary fix that started charging restarts to the lane would be a
// regression dressed as a rename.
func TestSCHEDGAP16940_OperatorRestartKeepsTheCarveOut(t *testing.T) {
	db := newTestDB(t)
	const project = "gap16940-carveout"
	mustCreateProject(t, db, project)
	const before = 3
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
		ExitCode: -1,
		Error:    "aborted by graceful shutdown — drain timed out with tick in flight",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := getConsecutiveFailures(t, db, project); got != before {
		t.Errorf("consecutive_failures = %d, want %d (an operator restart must not charge the lane)", got, before)
	}
}
