package scheduler

// SCHED-GAP-146 — the ENQUEUE-side half of the defer-not-drop contract; the
// companion to schedgap146_deferred_spawn_contract_test.go (the POOL-side half).
//
// The pool-side file pins what a deferral must not cost a lane once a spawn
// reaches SlotPool.spawn. This file pins the half that no test covered, and the
// half the fleet actually bled on: a gate that lives BELOW an enqueue must not
// be reached AFTER work has already been enqueued and a retry budget already
// spent.
//
// THE DEFECT, MEASURED (probe, 2026-09-18, before the fix in this commit).
// `resumeOrphans` (session_resume.go) spends a nudge (bumpNudgeCount) and
// enqueues the nudge row BEFORE handing the tick to the pool, and the LOAD gate
// (SCHED-GAP-125) is consulted inside SlotPool.spawn — i.e. after both. Under
// load the nudge was therefore deferred with the row already queued:
//
//	nudge_count 0 → 1, row "<orphan>-nudge1" status 'queued', forever.
//
// Nothing dispatches an EXISTING row in this process, so the stranded row is a
// zombie, and it is self-perpetuating: orphansToResume's NOT EXISTS clause
// (`r.status IN ('queued','running')`) then hides the orphan from every later
// pass — including passes after load has dropped — while SpawnNow refuses the
// project ("already has a tick in flight"). Observed in the same probe: with
// the gate reopened, the orphan's continuation never ran; the orphan scan
// stopped seeing it, and the lane came back only through the cold path or a
// restart + the SCHED-GAP-145 reaper.
//
// That contradicts the invariant SCHED-GAP-142 states in prose for the very
// same loop ("A candidate that does not fit is SKIPPED this pass — not
// enqueued, so no stranded 'queued' row and no nudge is consumed") for the load
// dimension, and it is the zombie shape operators had to reconcile by hand on
// 2026-09-18.
//
// THE FIX (non-test, minimal): the same predicate the pool would apply —
// LoadGateShouldDefer(db, ns) — is now consulted in resumeOrphans BEFORE the
// nudge is spent and before the row is enqueued, exactly where the
// namespace-cap check already sits. It is inert when the gate is off
// (threshold 0, the default): the branch can never fire, so default fleet
// behaviour is byte-identical.
//
// WHAT THESE TESTS PIN, and why each assertion is load-bearing:
//
//  1. The deferral is real and is the LOAD gate — the deferral log line fires
//     and the namespace cap is 0 (unlimited), so no other gate can be the
//     reason nothing ran.
//  2. A load-deferred nudge consumes NOTHING: nudge_count stays 0, no nudge row
//     exists, no queued/running row exists, no slot and no namespace claim is
//     held, and the lane's scheduling state is untouched.
//  3. The deferral is repeatable: passes under sustained load burn no part of
//     the MaxNudgesPerTick budget (a burned budget is what turns a deferral
//     into a needs-human failure).
//  4. DEFER, not drop: the moment the gate opens, the NEXT pass resumes the
//     SAME orphan as a continuation ("<orphan>-nudge1") and it runs to
//     completion on the real pool — with the gateway holding the response, so
//     "it ran" is an observation, not a race against an instant mock.
//
// RESIDUAL (not asserted here, because it is not fixed and a test may not pin
// behaviour the code does not have): the API/wave entry point has the same
// shape — Loop.SpawnNow enqueues a row and then hands it to the pool, so a load
// deferral there still strands that row and makes SpawnNow answer
// ErrProjectRunning ("project already has a tick in flight") until a restart
// reaps it. It is reported as a follow-up finding rather than silently
// papered over.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// gap146NudgeFixture builds the orphan-resume shape with the load gate as the
// ONLY gate that can act: one enabled project in an UNLIMITED namespace with
// one orphaned tick waiting to be re-nudged.
func gap146NudgeFixture(t *testing.T) (db *sql.DB, l *Loop, gw *heldResumeGateway, ns, name string) {
	t.Helper()
	db = newTestDB(t)
	ns = "gap146-lane-ns"
	name = "gap146-lane"
	// cap 0 = unlimited: the namespace-cap gate must not be the reason for
	// anything these tests observe (each test re-checks the premise itself).
	capTestNamespace(t, db, ns, 0, "cooldown")
	capTestProject(t, db, name, ns)
	orphanTickRow(t, db, name+"-tick", name, "failed", OrphanReasonDrainTimeout, 0)

	l = NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	l.noDeliver = true
	gw = newHeldResumeGateway(t)
	gw.wire(l) // resumeOrphans returns early unless the gateway is available
	return db, l, gw, ns, name
}

// gap146OpenLoadGate replaces the load sampler with an idle reading — how every
// test here simulates "load dropped below the threshold".
func gap146OpenLoadGate() {
	currentLoad1m = func() (float64, bool) { return 3.0, true }
}

// TestGAP146_LoadGateDefersNudgeWithoutStrandingQueuedRow: under load, no nudge
// is consumed and no row is stranded; once load drops, the SAME orphan is
// resumed as a continuation and runs to completion.
func TestGAP146_LoadGateDefersNudgeWithoutStrandingQueuedRow(t *testing.T) {
	db, l, gw, ns, name := gap146NudgeFixture(t)
	const orphanID = "gap146-lane-tick"
	contTick := orphanID + "-nudge1"

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	// PREMISES — the gate under test really trips, and it is the ONLY gate.
	if !LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: LoadGateShouldDefer(%q) = false with threshold %.0f and load 15.0 — the fixture does not trip the gate",
			ns, loadGateThreshold())
	}
	if cap := l.namespaceCap(context.Background(), ns); cap != 0 {
		t.Fatalf("premise: namespace %s max_concurrent = %d, want 0 (unlimited) — the cap gate must not be the gate under test", ns, cap)
	}
	before := gap146ReadState(t, db, name)

	logbuf := admitCaptureLog(t)
	l.resumeOrphans("gap146-defer")

	// The deferral itself: without this line the assertions below would also be
	// satisfied by a loop that never reached the candidate.
	if got := gap146LogCount(logbuf, "RESUME: deferring orphaned tick "+orphanID); got != 1 {
		t.Fatalf("deferral lines for %s = %d, want 1 — the orphan must have been seen and deferred by the LOAD gate", orphanID, got)
	}

	// CONTRACT 2 — a deferred nudge consumes nothing.
	if got := gap146TickNudgeCount(t, db, orphanID); got != 0 {
		t.Errorf("nudge_count = %d, want 0 — a load-deferred nudge must not spend the retry budget (it is the budget, not the pass, that decides how many times an orphan may be retried)", got)
	}
	if got := countNudgeRows(t, db, name); got != 0 {
		t.Errorf("nudge rows = %d, want 0 — the deferred candidate must be SKIPPED BEFORE the row is enqueued (an enqueued row that nothing dispatches is the zombie operator work of 2026-09-18)", got)
	}
	if got := gap146InFlightTicks(t, db); got != 0 {
		t.Errorf("queued/running rows = %d, want 0 — a load deferral must leave no in-flight row behind", got)
	}
	if got := l.namespaceInflight(context.Background(), ns); got != 0 {
		t.Errorf("namespace in-flight = %d, want 0 — a stranded queued row inflates this count, and the count is what defers the lane's siblings", got)
	}
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0 — a deferred nudge takes no global slot", got)
	}
	if l.slotPool.RunningSet()[name] {
		t.Errorf("%s is in RunningSet after a deferred nudge — no reservation may survive it", name)
	}
	gap146AssertStateUnchanged(t, db, name, before, "load-gate nudge deferral")

	// CONTRACT 4 — DEFER, not drop. Load drops → the next pass resumes the SAME
	// orphan, as a continuation, and it runs.
	gap146OpenLoadGate()
	l.resumeOrphans("gap146-open")

	waitUntil(t, 10*time.Second, "the deferred orphan's continuation to be dispatched once the gate opened", func() bool {
		return gap146TickStatus(t, db, contTick) == "running"
	})
	// Barrier: the continuation is provably in flight (parked on the held
	// gateway), so it started because the gate opened.
	waitForHeldSpawn(t, gw, 5*time.Second)

	if got := gap146TickNudgeCount(t, db, orphanID); got != 1 {
		t.Errorf("nudge_count after re-admission = %d, want 1 — exactly one continuation, spent only once the gate allowed it", got)
	}
	if got := countNudgeRows(t, db, name); got != 1 {
		t.Errorf("nudge rows after re-admission = %d, want 1 — the deferral must be retried, and retried as ONE continuation of the original orphan", got)
	}

	gw.releaseAll()
	waitForSettled(t, db, 15*time.Second)

	if got := gap146TickStatus(t, db, contTick); got != "completed" {
		t.Errorf("continuation %s status = %q, want \"completed\" — a deferred orphan must actually RUN once the gate clears (a gate that defers and never re-admits is a silent stall)", contTick, got)
	}
	if got := gap146TickRows(t, db, name, "failed"); got != 1 {
		t.Errorf("failed rows for %s = %d, want 1 (only the original orphan) — the deferral and the resume must charge the lane no extra failure", name, got)
	}
	if got := gap146ReadState(t, db, name).consecutiveFailures; got > before.consecutiveFailures {
		t.Errorf("consecutive_failures = %d, want <= %d — a load deferral records no failure for the lane", got, before.consecutiveFailures)
	}
}

// TestGAP146_RepeatedLoadGateDeferralsBurnNoNudgeBudget: sustained load must
// cost the orphan nothing at all, pass after pass. This is the anti-zombie
// property: MaxNudgesPerTick is 2, so a deferral that spends the budget turns
// into a needs-human failure while the lane never actually ran.
func TestGAP146_RepeatedLoadGateDeferralsBurnNoNudgeBudget(t *testing.T) {
	db, l, gw, ns, name := gap146NudgeFixture(t)
	const orphanID = "gap146-lane-tick"
	contTick := orphanID + "-nudge1"

	restore := gap146ArmLoadGate(15.0)
	defer restore()

	if !LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: LoadGateShouldDefer(%q) = false — the fixture does not trip the gate", ns)
	}
	logbuf := admitCaptureLog(t)

	const passes = 3
	for i := 0; i < passes; i++ {
		l.resumeOrphans(fmt.Sprintf("gap146-pass%d", i))
	}

	if got := gap146LogCount(logbuf, "RESUME: deferring orphaned tick "+orphanID); got != passes {
		t.Fatalf("deferral lines across %d passes = %d, want %d — every pass under load must defer the same orphan (not lose track of it)",
			passes, got, passes)
	}
	if got := gap146TickNudgeCount(t, db, orphanID); got != 0 {
		t.Errorf("nudge_count after %d deferred passes = %d, want 0 — a deferral must not burn the retry budget; MaxNudgesPerTick is %d, so a spent budget fails the orphan into needs-human without the work ever running",
			passes, got, MaxNudgesPerTick)
	}
	if got := countNudgeRows(t, db, name); got != 0 {
		t.Errorf("nudge rows after %d deferred passes = %d, want 0 — no pass may enqueue a row it cannot dispatch", passes, got)
	}
	if got := gap146InFlightTicks(t, db); got != 0 {
		t.Errorf("queued/running rows after %d deferred passes = %d, want 0", passes, got)
	}

	// Gate opens: ONE continuation, still of the original orphan.
	gap146OpenLoadGate()
	l.resumeOrphans("gap146-open")

	waitUntil(t, 10*time.Second, "the orphan to be resumed once the gate opened", func() bool {
		return gap146TickStatus(t, db, contTick) == "running"
	})
	if got := gap146TickNudgeCount(t, db, orphanID); got != 1 {
		t.Errorf("nudge_count after re-admission = %d, want 1 — the deferrals must not have consumed any nudge", got)
	}
	if got := countNudgeRows(t, db, name); got != 1 {
		t.Errorf("nudge rows after re-admission = %d, want 1", got)
	}

	gw.releaseAll()
	waitForSettled(t, db, 15*time.Second)
	if got := gap146TickStatus(t, db, contTick); got != "completed" {
		t.Errorf("continuation status = %q, want \"completed\" — the deferred orphan must run once load drops", got)
	}
}
