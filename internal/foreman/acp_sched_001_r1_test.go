package foreman_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

// TestRegressionOutOfOrderEvidenceRejectedAndTerminalCannotResurrect proves
// the evidence stream is append-only against the durable sequence and a
// terminal attempt can never be resurrected by later evidence.
func TestRegressionOutOfOrderEvidenceRejectedAndTerminalCannotResurrect(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	// Out-of-order sequence: the caller cannot skip the durable stream.
	if _, _, err := h.durable.AppendEvidence(h.ctx, foreman.EvidenceRecord{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Sequence: 5, Kind: "progress", PayloadDigest: execution.EvidenceDigest("progress", "skipped"),
	}); !errors.Is(err, execution.ErrEvidenceSequence) {
		t.Fatalf("out-of-order sequence error=%v", err)
	}
	if got := h.eventCount(); got != 0 {
		t.Fatalf("out-of-order evidence left %d rows", got)
	}

	// Stop the attempt with full evidence.
	outcome, connector, independent := h.settleTriple()
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}

	// Later state-bearing evidence cannot resurrect the terminal attempt.
	ev, _, err := h.durable.AppendEvidence(h.ctx, foreman.EvidenceRecord{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Kind: "progress", ProposedState: execution.AttemptStateWorking,
		PayloadDigest: execution.EvidenceDigest("progress", "resurrect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Accepted {
		t.Fatalf("resurrecting evidence accepted: %+v", ev)
	}
	var state string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM execution_attempts WHERE attempt_id=?`, attempt.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != execution.AttemptStateCompleted {
		t.Fatalf("terminal attempt resurrected to %q", state)
	}
}

// TestRegressionInventedEvidenceRefsCannotSettle proves settlement references
// must resolve to durable records bound to the exact parent/attempt —
// nonempty invented strings are never evidence.
func TestRegressionInventedEvidenceRefsCannotSettle(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	receipt := h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	outcome, connector, independent := h.settleTriple()

	// Nothing durable exists yet: every reference is invented.
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); !errors.Is(err, execution.ErrEvidenceNotDurable) {
		t.Fatalf("invented refs settled attempt: error=%v", err)
	}
	// The foreman report is no escape hatch for invented evidence either.
	if _, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
		ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
		Outcome: outcome, Connector: connector, Independent: independent,
	}); !errors.Is(err, execution.ErrEvidenceNotDurable) {
		t.Fatalf("invented refs admitted report: error=%v", err)
	}

	// Even with the event references recorded, an invented verification
	// reference cannot settle: verification_receipts are the authority.
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindTerminalOutcome, outcome.OutcomeRef)
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindConnectorSettlement, connector.SettlementReceiptRef)
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindConnectorQuiescence, connector.QuiescenceReceiptRef)
	badVerification := independent
	badVerification.VerificationRef = "invented-verification"
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: badVerification,
	}}); !errors.Is(err, execution.ErrEvidenceNotDurable) {
		t.Fatalf("invented verification settled attempt: error=%v", err)
	}

	// With the attempt settled for real, parent settlement still refuses
	// invented parent-level references.
	h.recordVerification(attempt.AttemptID, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RequestCancellation(h.ctx, h.parentID, "operator abort"); err != nil {
		t.Fatal(err)
	}
	inventedParent := outcome
	inventedParent.State = execution.AttemptStateCanceled
	inventedParent.OutcomeRef = "invented-parent-outcome"
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: inventedParent, Connector: connector, Independent: independent,
		QuiescenceReceiptRef: "quiescence-1",
	}}); !errors.Is(err, execution.ErrEvidenceNotDurable) {
		t.Fatalf("invented refs settled parent: error=%v", err)
	}
	var attempts int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_attempts WHERE settled_at IS NOT NULL`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("%d attempts settled, want only the evidenced one", attempts)
	}
	var parentState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT parent_state FROM execution_runs WHERE run_id=?`, h.parentID).Scan(&parentState); err != nil {
		t.Fatal(err)
	}
	if parentState == string(execution.ParentSettled) {
		t.Fatal("invented references settled the parent")
	}
}

// TestRegressionReconciliationCannotCreateHalfSettledParent proves
// reconciliation produces a durable resolution and then follows the normal
// settlement path — it can never manufacture `settled` on its own.
func TestRegressionReconciliationCannotCreateHalfSettledParent(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	h.advance(61 * time.Second)
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.BeginReconciliation(h.ctx, foreman.ReconcileRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil {
		t.Fatal(err)
	}
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindNoStartProof, "no-start-proof-1")

	if _, err := h.store.CompleteReconciliation(h.ctx, foreman.ReconcileOutcome{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Next:   execution.ParentSettled,
		Result: foreman.ReconcileResult{Kind: foreman.ReconcileNoStart, EvidenceRef: "no-start-proof-1"},
	}); !errors.Is(err, execution.ErrReconciliationSettled) {
		t.Fatalf("reconciliation manufactured settled: error=%v", err)
	}
	// The durable store refuses the same shortcut directly.
	if _, err := h.durable.CompleteReconciliation(h.ctx, foreman.ReconcileOutcome{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Next:   execution.ParentSettled,
		Result: foreman.ReconcileResult{Kind: foreman.ReconcileNoStart, EvidenceRef: "no-start-proof-1"},
	}); !errors.Is(err, execution.ErrReconciliationSettled) {
		t.Fatalf("store reconciliation manufactured settled: error=%v", err)
	}
	rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ParentState != execution.ParentReconciling || rec.SettledAt != "" ||
		rec.TerminalOutcomeRef != "" || rec.ConnectorSettlementRef != "" || rec.IndependentVerificationRef != "" {
		t.Fatalf("half-settled parent: %+v", rec)
	}
	var projections int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_projections`).Scan(&projections); err != nil {
		t.Fatal(err)
	}
	if projections != 0 {
		t.Fatalf("reconciliation queued %d settlement projections", projections)
	}
}

// TestRegressionStaleOrPrematureTimeoutCannotForceUncertain proves a start
// timeout requires the current active attempt and an elapsed persisted start
// deadline.
func TestRegressionStaleOrPrematureTimeoutCannotForceUncertain(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	// Premature: the persisted start deadline has not elapsed.
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "timeout",
	}); !errors.Is(err, execution.ErrStartDeadlineNotElapsed) {
		t.Fatalf("premature timeout error=%v", err)
	}
	// Foreign generation: the evidence names another generation.
	h.advance(61 * time.Second)
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 2, Reason: "timeout",
	}); err == nil {
		t.Fatal("foreign-generation evidence forced uncertain")
	}
	// Stale/completed: the attempt has already stopped.
	outcome, connector, independent := h.settleTriple()
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "timeout",
	}); err == nil {
		t.Fatal("completed attempt forced uncertain")
	}
	rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ParentState == execution.ParentUncertain {
		t.Fatalf("stale/premature timeout moved parent to %q", rec.ParentState)
	}
}

// TestRegressionForeignLeaseOwnerCannotAuthorizeAttempt proves durable lease
// ownership — not just epoch/state/expiry — gates just-in-time admission.
func TestRegressionForeignLeaseOwnerCannotAuthorizeAttempt(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	// A lease owned by another parent.
	foreignBrief, _ := mustParentBrief(t, "other-parent")
	if _, _, err := h.store.AdmitParent(h.ctx, foreman.ParentRunSpec{
		ParentRunID: "other-parent", SubmittingAuthority: "scheduler:test",
		SubmissionIdempotencyKey: "submit-other-parent", ParentBriefJSON: foreignBrief,
	}); err != nil {
		t.Fatal(err)
	}
	h.acquireLeaseOwned("foreign-lease", 1, 0, "other-parent", "")
	h.invoke()
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", h.intent("step-1", 1, "foreign-lease", 1, h.route())); !errors.Is(err, execution.ErrLeaseOwnership) {
		t.Fatalf("foreign parent lease error=%v", err)
	}
	// A lease owned by another attempt of the same parent.
	h.acquireLeaseOwned("attempt-lease", 1, 0, h.parentID, "some-other-attempt")
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", h.intent("step-1", 1, "attempt-lease", 1, h.route())); !errors.Is(err, execution.ErrLeaseOwnership) {
		t.Fatalf("foreign attempt lease error=%v", err)
	}
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("foreign lease admitted %d attempts", got)
	}
}

// TestRegressionChangedReplayContractConflicts proves a changed connector,
// candidate, route, lease or brief field on the same (parent, step,
// generation) conflicts instead of producing a second attempt.
func TestRegressionChangedReplayContractConflicts(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	intent := h.intent("step-1", 1, "lease-1", 1, h.route())
	h.prepare("attempt-1", intent)

	cases := []struct {
		name string
		mut  func(*execution.ReadyStepIntent)
	}{
		{"connector version", func(i *execution.ReadyStepIntent) { i.Choice.ConnectorVersion = "2.0" }},
		{"connector id", func(i *execution.ReadyStepIntent) { i.Choice.ConnectorID = "other-connector" }},
		{"candidate rank", func(i *execution.ReadyStepIntent) {
			i.Route.Candidates[0].ExclusionEvidence = "health-circuit-open"
			digest, err := execution.RouteReceiptDigest(i.Route)
			if err != nil {
				t.Fatal(err)
			}
			i.Route.ReceiptDigest = digest
			i.Choice.CandidateRank = 2
			i.Choice.CompatibleModel = "longcat-2.0"
		}},
		{"route receipt", func(i *execution.ReadyStepIntent) {
			i.Route.Candidates[1].ExclusionEvidence = "quota"
			digest, err := execution.RouteReceiptDigest(i.Route)
			if err != nil {
				t.Fatal(err)
			}
			i.Route.ReceiptDigest = digest
		}},
		{"lease epoch", func(i *execution.ReadyStepIntent) { i.Lease.LeaseEpoch = 2 }},
		{"lease profile", func(i *execution.ReadyStepIntent) { i.Lease.ProfileID = "other-profile" }},
		{"enforcement receipt", func(i *execution.ReadyStepIntent) { i.Lease.EnforcementReceiptRef = "other-receipt" }},
		{"brief digest", func(i *execution.ReadyStepIntent) {
			i.BriefDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := intent
			tc.mut(&changed)
			if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", changed); !errors.Is(err, database.ErrExecutionConflict) {
				t.Fatalf("changed %s error=%v", tc.name, err)
			}
		})
	}
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("replay conflicts created attempts: %d", got)
	}
}

// TestRegressionRejectedEvidenceReplayRemainsRejected proves a replay of
// evidence that the durable stream rejected preserves the original verdict —
// rejected stale evidence never replays as accepted.
func TestRegressionRejectedEvidenceReplayRemainsRejected(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	// Move the parent's current generation past this attempt.
	outcome, connector, independent := h.settleTriple()
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}
	h.prepare("attempt-2", h.intent("step-2", 1, "lease-1", 1, h.route()))

	stale := foreman.EvidenceRecord{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Kind: "progress", PayloadDigest: execution.EvidenceDigest("progress", "late"),
	}
	first, _, err := h.durable.AppendEvidence(h.ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if first.Accepted || first.RejectionReason == "" {
		t.Fatalf("stale evidence accepted: %+v", first)
	}
	replay, wasReplay, err := h.durable.AppendEvidence(h.ctx, stale)
	if err != nil || !wasReplay {
		t.Fatalf("replay=%v err=%v", wasReplay, err)
	}
	if replay.Accepted != first.Accepted || replay.RejectionReason != first.RejectionReason {
		t.Fatalf("rejected evidence replayed as accepted: first=%+v replay=%+v", first, replay)
	}
}

// TestRegressionFailedCancelledOrUnverifiedDependencyCannotUnlock proves
// dependency readiness: a prerequisite must be a successful terminal result
// with passing independent verification (unless an explicit policy says
// otherwise), and unready steps create zero attempt and zero outbox rows.
func TestRegressionFailedCancelledOrUnverifiedDependencyCannotUnlock(t *testing.T) {
	settlePrereq := func(t *testing.T, h *harness, settleState, verdict string) {
		t.Helper()
		pre := h.prepare("attempt-1", h.intent("prereq-step", 1, "lease-1", 1, h.route()))
		outcome, connector, independent := h.settleTriple()
		outcome.State = settleState
		independent.Verdict = verdict
		h.recordSettlementEvidence(pre.AttemptID, 1, outcome, connector, independent)
		if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
			ParentRunID: h.parentID, AttemptID: pre.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
		}}); err != nil {
			t.Fatalf("settle prerequisite: %v", err)
		}
	}
	for _, tc := range []struct {
		name    string
		state   string
		verdict string
	}{
		{"failed prerequisite", execution.AttemptStateFailed, execution.VerifierFail},
		{"cancelled prerequisite", execution.AttemptStateCanceled, execution.VerifierPass},
		{"unverified prerequisite", execution.AttemptStateCompleted, execution.VerifierFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.admit(1)
			h.acquireLease("lease-1", 1, 0)
			h.invoke()
			settlePrereq(t, h, tc.state, tc.verdict)
			dependent := h.intent("dependent-step", 1, "lease-1", 1, h.route())
			dependent.Dependencies = []execution.StepDependency{{StepID: "prereq-step"}}
			if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-2", dependent); !errors.Is(err, execution.ErrDependencyNotReady) {
				t.Fatalf("dependency gate error=%v", err)
			}
		})
	}

	// The strict gate also refuses with no prerequisite attempt at all, and
	// planned/unready steps leave zero attempt and zero outbox rows.
	h := newHarness(t)
	h.admit(1)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	dependent := h.intent("dependent-step", 1, "lease-1", 1, h.route())
	dependent.Dependencies = []execution.StepDependency{{StepID: "planned-step"}}
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", dependent); !errors.Is(err, execution.ErrDependencyNotReady) {
		t.Fatalf("planned prerequisite error=%v", err)
	}
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("unready step created %d attempt rows", got)
	}
	if got := h.outboxCount(); got != 0 {
		t.Fatalf("unready step created %d outbox rows", got)
	}

	// Positive control: a verified successful prerequisite unlocks the step.
	h = newHarness(t)
	h.admit(1)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	settlePrereq(t, h, execution.AttemptStateCompleted, execution.VerifierPass)
	unlocked := h.intent("dependent-step", 1, "lease-1", 1, h.route())
	unlocked.Dependencies = []execution.StepDependency{{StepID: "prereq-step"}}
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-2", unlocked); err != nil {
		t.Fatalf("verified prerequisite refused: %v", err)
	}
}

// TestRegressionCancellationCannotSettleCompleted proves cancellation is
// monotonic: after a cancellation request, success cannot be supplied as the
// terminal result of an attempt or the parent.
func TestRegressionCancellationCannotSettleCompleted(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	if _, err := h.store.RequestCancellation(h.ctx, h.parentID, "operator abort"); err != nil {
		t.Fatal(err)
	}
	outcome, connector, independent := h.settleTriple() // completed
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); !errors.Is(err, execution.ErrCancellationMonotonic) {
		t.Fatalf("cancelled attempt settled completed: error=%v", err)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
		QuiescenceReceiptRef: "quiescence-1",
	}}); !errors.Is(err, execution.ErrCancellationMonotonic) {
		t.Fatalf("cancelled parent settled completed: error=%v", err)
	}

	// The monotonic model: cancelled result → quiescent settlement.
	cancelled := outcome
	cancelled.State = execution.AttemptStateCanceled
	cancelled.OutcomeRef = "outcome-cancelled"
	h.recordSettlementEvidence(attempt.AttemptID, 1, cancelled, connector, independent)
	settled, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: cancelled, Connector: connector, Independent: independent,
	}})
	if err != nil || settled.State != execution.AttemptStateCanceled {
		t.Fatalf("cancelled result: %+v err=%v", settled, err)
	}
	parent, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: cancelled, Connector: connector, Independent: independent,
		QuiescenceReceiptRef: "quiescence-1",
	}})
	if err != nil || parent.ParentState != execution.ParentSettled {
		t.Fatalf("quiescent cancellation settlement: %+v err=%v", parent, err)
	}
}

// TestRegressionConnectorQuiescenceRequiredAndResolved proves process-backed
// work settles only with a resolved zero-descendant quiescence receipt.
func TestRegressionConnectorQuiescenceRequiredAndResolved(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	receipt := h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	if err := database.SetAttemptProcessIncarnation(h.ctx, h.db, attempt.AttemptID, attempt.Generation, 123, 123, "container-1"); err != nil {
		t.Fatal(err)
	}
	outcome, connector, independent := h.settleTriple()

	// Missing quiescence receipt on process-backed work is refused.
	withoutQuiescence := connector
	withoutQuiescence.QuiescenceReceiptRef = ""
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, withoutQuiescence, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: withoutQuiescence, Independent: independent,
	}}); !errors.Is(err, execution.ErrQuiescenceEvidenceRequired) {
		t.Fatalf("process-backed attempt without quiescence error=%v", err)
	}
	// An invented quiescence reference does not resolve.
	invented := connector
	invented.QuiescenceReceiptRef = "invented-quiescence"
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: invented, Independent: independent,
	}}); !errors.Is(err, execution.ErrEvidenceNotDurable) {
		t.Fatalf("invented quiescence error=%v", err)
	}
	// The recorded receipt settles the attempt and the parent.
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
		ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
		Outcome: outcome, Connector: connector, Independent: independent,
	}); err != nil {
		t.Fatal(err)
	}
	// The parent needs its own zero-descendant authority reference too.
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); !errors.Is(err, execution.ErrQuiescenceEvidenceRequired) {
		t.Fatalf("parent without zero-descendant authority error=%v", err)
	}
	parent, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
		QuiescenceReceiptRef: "quiescence-1",
	}})
	if err != nil || parent.ParentState != execution.ParentSettled {
		t.Fatalf("parent=%+v err=%v", parent, err)
	}
}

// TestRegressionIncompleteParentBriefRejected proves the canonical parent
// brief must carry complete scope, context, constraints, non-goals,
// acceptance criteria, budget and provenance — not merely hashes and IDs.
func TestRegressionIncompleteParentBriefRejected(t *testing.T) {
	h := newHarness(t)
	brief := execution.ParentBrief{
		SchemaVersion: "parent-brief/v1", BriefID: "minimal", RevisionID: "r1",
		ParentRunID: h.parentID, Lane: "acp-sched", TaskID: "ACP-SCHED-001",
		Mission: "minimal", DeadlineAt: seamNow.Add(time.Hour).Format(time.RFC3339Nano),
		CancellationPolicy: "cooperative",
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := execution.BriefDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	brief.BriefDigest = digest
	unsigned, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.ValidateParentBriefDigest(unsigned); !errors.Is(err, execution.ErrParentBriefIncomplete) {
		t.Fatalf("incomplete brief error=%v", err)
	}
	if _, _, err := h.store.AdmitParent(h.ctx, foreman.ParentRunSpec{
		ParentRunID: h.parentID, SubmittingAuthority: "scheduler:test", SubmissionIdempotencyKey: "minimal-parent", ParentBriefJSON: unsigned,
	}); !errors.Is(err, execution.ErrParentBriefIncomplete) {
		t.Fatalf("incomplete brief admitted: error=%v", err)
	}
	var runs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("incomplete brief left %d parent rows", runs)
	}
}

// TestRegressionParentAdmissionAtomicOnSecondStageFailure proves a
// second-write failure leaves no visible half-parent.
func TestRegressionParentAdmissionAtomicOnSecondStageFailure(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `CREATE TRIGGER fail_parent_plane BEFORE UPDATE OF parent_brief_json ON execution_runs
		WHEN NEW.parent_brief_json != '' BEGIN SELECT RAISE(ABORT, 'injected second-stage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.AdmitParent(h.ctx, foreman.ParentRunSpec{
		ParentRunID: h.parentID, SubmittingAuthority: "scheduler:test",
		SubmissionIdempotencyKey: "submit-" + h.parentID, ParentBriefJSON: h.briefJSON,
	}); err == nil {
		t.Fatal("admission unexpectedly survived the second-stage failure")
	}
	var runs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("second-stage failure left %d visible rows", runs)
	}
	// Removing the fault admits exactly one complete parent.
	if _, err := h.db.ExecContext(h.ctx, `DROP TRIGGER fail_parent_plane`); err != nil {
		t.Fatal(err)
	}
	rec := h.admit(0)
	if rec.ParentState != execution.ParentQueued || len(rec.BriefJSON) == 0 {
		t.Fatalf("repaired admission incomplete: %+v", rec)
	}
}

// TestRegressionEvidenceCannotOverwriteEstablishedAuthority proves later
// evidence cannot overwrite connector or independent-verifier authority that
// is already durably established.
func TestRegressionEvidenceCannotOverwriteEstablishedAuthority(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	receipt := h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	outcome, connector, independent := h.settleTriple()
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
		ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
		Outcome: outcome, Connector: connector, Independent: independent,
	}); err != nil {
		t.Fatal(err)
	}
	// A later report claiming different connector/verifier authority is
	// refused: the durable authority stands.
	changed := outcome
	changed.OutcomeRef = "outcome-2"
	changedConnector := connector
	changedConnector.SettlementReceiptRef = "connector-settlement-2"
	changedIndependent := independent
	changedIndependent.VerificationRef = "verification-2"
	if _, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
		ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
		Outcome: changed, Connector: changedConnector, Independent: changedIndependent,
	}); !errors.Is(err, foreman.ErrParentConflict) {
		t.Fatalf("report overwrote durable authority: error=%v", err)
	}
	// Settling the attempt with contradicting authority is refused too.
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: changed, Connector: changedConnector, Independent: changedIndependent,
	}}); !errors.Is(err, foreman.ErrAttemptConflict) {
		t.Fatalf("settlement overwrote durable authority: error=%v", err)
	}
	rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.TerminalOutcomeRef != outcome.OutcomeRef || rec.ConnectorSettlementRef != connector.SettlementReceiptRef ||
		rec.IndependentVerificationRef != independent.VerificationRef {
		t.Fatalf("durable authority changed: %+v", rec)
	}
}

type crashInvoker struct {
	mu    sync.Mutex
	calls int
	db    *sql.DB
	crash bool
}

func (c *crashInvoker) InvokeForeman(_ context.Context, inv execution.ForemanInvocation) (execution.ForemanReceipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.crash {
		c.crash = false
		_ = c.db.Close() // simulated crash after the external accept
	}
	return execution.ForemanReceipt{
		InvocationID: inv.InvocationID,
		ParentRunID:  inv.ParentRunID,
		AcceptedAt:   seamNow.Format(time.RFC3339Nano),
	}, nil
}

func (c *crashInvoker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type stubReconciler struct {
	status execution.InvocationStatus
	calls  int
}

func (s *stubReconciler) ReconcileInvocation(context.Context, string, string) (execution.InvocationResolution, error) {
	s.calls++
	return execution.InvocationResolution{Status: s.status, AcceptedAt: seamNow.Format(time.RFC3339Nano)}, nil
}

// TestRegressionCrashRestartAndConcurrentCallsOneInvocationEffect proves the
// foreman is invoked with one stable identity: a crash after the external
// accept is reconciled before any resend, and concurrent callers never both
// invoke.
func TestRegressionCrashRestartAndConcurrentCallsOneInvocationEffect(t *testing.T) {
	t.Run("crash then reconcile before resend", func(t *testing.T) {
		h := newHarness(t)
		h.admit(0)
		inv := &crashInvoker{db: h.db, crash: true}
		seam := foreman.NewSeamWithReconciler(h.durable, inv, &stubReconciler{status: execution.InvocationAccepted}, h.projector)
		if _, err := seam.InvokeForeman(h.ctx, h.parentID); err == nil {
			t.Fatal("expected persistence failure after the simulated crash")
		}
		if inv.count() != 1 {
			t.Fatalf("external calls=%d before restart", inv.count())
		}
		h.restart()
		seam = foreman.NewSeamWithReconciler(h.durable, inv, &stubReconciler{status: execution.InvocationAccepted}, h.projector)
		receipt, err := seam.InvokeForeman(h.ctx, h.parentID)
		if err != nil {
			t.Fatal(err)
		}
		if inv.count() != 1 {
			t.Fatalf("Hermes invoked %d times across crash/restart", inv.count())
		}
		rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.InvocationID != rec.ForemanInvocationID || rec.ForemanInvokedAt == "" {
			t.Fatalf("identity not stable: receipt=%+v parent=%+v", receipt, rec)
		}
	})

	t.Run("uncertain crossing without reconciler fails closed", func(t *testing.T) {
		h := newHarness(t)
		h.admit(0)
		inv := &crashInvoker{db: h.db, crash: true}
		seam := foreman.NewSeam(h.durable, inv, h.projector)
		if _, err := seam.InvokeForeman(h.ctx, h.parentID); err == nil {
			t.Fatal("expected persistence failure after the simulated crash")
		}
		h.restart()
		seam = foreman.NewSeam(h.durable, inv, h.projector)
		if _, err := seam.InvokeForeman(h.ctx, h.parentID); !errors.Is(err, execution.ErrInvocationUncertain) {
			t.Fatalf("resend without reconciliation error=%v", err)
		}
		if inv.count() != 1 {
			t.Fatalf("unreconciled resend invoked Hermes: %d", inv.count())
		}
	})

	t.Run("reconciled not-started resends the same identity", func(t *testing.T) {
		h := newHarness(t)
		h.admit(0)
		inv := &crashInvoker{db: h.db, crash: true}
		seam := foreman.NewSeamWithReconciler(h.durable, inv, &stubReconciler{status: execution.InvocationNotStarted}, h.projector)
		if _, err := seam.InvokeForeman(h.ctx, h.parentID); err == nil {
			t.Fatal("expected persistence failure after the simulated crash")
		}
		h.restart()
		inv.db = h.db
		seam = foreman.NewSeamWithReconciler(h.durable, inv, &stubReconciler{status: execution.InvocationNotStarted}, h.projector)
		receipt, err := seam.InvokeForeman(h.ctx, h.parentID)
		if err != nil {
			t.Fatal(err)
		}
		// At-least-once with a STABLE receiver-facing identity: the resend
		// presents the same invocation id the receiver can deduplicate.
		if inv.count() != 2 {
			t.Fatalf("resend calls=%d, want 2", inv.count())
		}
		rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.InvocationID != rec.ForemanInvocationID || rec.ForemanInvokedAt == "" {
			t.Fatalf("identity not stable across resend: receipt=%+v parent=%+v", receipt, rec)
		}
	})

	t.Run("concurrent callers never both invoke", func(t *testing.T) {
		h := newHarness(t)
		h.admit(0)
		inv := &barrierInvoker{entered: make(chan struct{}), release: make(chan struct{})}
		seam := foreman.NewSeam(h.durable, inv, h.projector)
		done := make(chan error, 1)
		go func() { _, err := seam.InvokeForeman(h.ctx, h.parentID); done <- err }()
		<-inv.entered
		// The in-flight crossing is not confirmed: a concurrent caller must
		// not invoke Hermes again.
		if _, err := seam.InvokeForeman(h.ctx, h.parentID); !errors.Is(err, execution.ErrInvocationUncertain) {
			t.Fatalf("concurrent invoke error=%v", err)
		}
		close(inv.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := inv.count(); got != 1 {
			t.Fatalf("Hermes invoked %d times by concurrent callers", got)
		}
	})
}

type barrierInvoker struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *barrierInvoker) InvokeForeman(_ context.Context, inv execution.ForemanInvocation) (execution.ForemanReceipt, error) {
	b.calls.Add(1)
	close(b.entered)
	<-b.release
	return execution.ForemanReceipt{
		InvocationID: inv.InvocationID,
		ParentRunID:  inv.ParentRunID,
		AcceptedAt:   seamNow.Format(time.RFC3339Nano),
	}, nil
}

func (b *barrierInvoker) count() int { return int(b.calls.Load()) }
