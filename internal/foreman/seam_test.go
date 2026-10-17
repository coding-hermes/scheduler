package foreman_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

// TestOneParentOneForemanInvocationOneJustInTimeAttempt is the positive path
// of the slice: one parent run is admitted, Hermes is invoked exactly once
// with the canonical parent brief, planning produces no scheduler rows, and
// exactly one attempt is persisted at dispatch preparation — replaying the
// same preparation is idempotent.
func TestOneParentOneForemanInvocationOneJustInTimeAttempt(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("planning created %d attempt rows before dispatch preparation", got)
	}

	receipt := h.invoke()
	if receipt.InvocationID == "" || receipt.ParentRunID != h.parentID {
		t.Fatalf("receipt=%+v", receipt)
	}
	// Planned/blocked steps are foreman state: invoking the foreman creates no
	// scheduler attempt rows. The only write path is PrepareAttempt.
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("foreman planning created %d attempt rows", got)
	}

	intent := h.intent("step-1", 1, "lease-1", 1, h.route())
	attempt := h.prepare("attempt-1", intent)
	wantKey := execution.DeriveAttemptIdempotencyKey(h.parentID, "step-1", 1, intent.Route.ReceiptDigest, 1)
	if attempt.AttemptIdempotencyKey != wantKey {
		t.Fatalf("idempotency key=%q, want %q", attempt.AttemptIdempotencyKey, wantKey)
	}
	if attempt.State != execution.AttemptStateSubmitted || attempt.StepGeneration != 1 || attempt.Generation != 1 {
		t.Fatalf("attempt=%+v", attempt)
	}
	rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil || rec.ParentState != execution.ParentExecuting {
		t.Fatalf("parent=%+v err=%v", rec, err)
	}

	replay, replayed, err := h.store.PrepareAttempt(h.ctx, "attempt-1", intent)
	if err != nil || !replayed || replay.AttemptID != "attempt-1" {
		t.Fatalf("preparation replay: %+v replay=%v err=%v", replay, replayed, err)
	}
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("replay created rows: attempts=%d", got)
	}
	// The dispatch outbox row exists and names no secret-bearing payload.
	var outbox int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_outbox WHERE attempt_id='attempt-1' AND kind='execution.dispatch'`).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("dispatch outbox=%d err=%v", outbox, err)
	}
}

// TestRefuseAttemptBeforeReadyStepIntent covers both readings of the rule: no
// intent at all, and an attempt before the foreman was ever invoked.
func TestRefuseAttemptBeforeReadyStepIntent(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)

	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", execution.ReadyStepIntent{}); !errors.Is(err, execution.ErrNoReadyStepIntent) {
		t.Fatalf("empty intent error=%v", err)
	}
	intent := h.intent("step-1", 1, "lease-1", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", intent); !errors.Is(err, execution.ErrForemanNotInvoked) {
		t.Fatalf("attempt before foreman invocation error=%v", err)
	}
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("refused attempts left %d rows", got)
	}
}

func TestRefuseMissingExpiredTamperedRouteReceipt(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()

	t.Run("missing", func(t *testing.T) {
		intent := h.intent("step-1", 1, "lease-1", 1, h.route())
		intent.Route.ReceiptDigest = ""
		if _, _, err := h.store.PrepareAttempt(h.ctx, "a-missing", intent); !errors.Is(err, execution.ErrRouteReceiptMissing) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		route := h.route(func(r *execution.RouteReceipt) {
			r.ResolvedAt = seamNow.Add(-time.Hour).Format(time.RFC3339Nano)
			r.ValidUntil = seamNow.Add(-time.Minute).Format(time.RFC3339Nano)
		})
		if _, _, err := h.store.PrepareAttempt(h.ctx, "a-expired", h.intent("step-1", 1, "lease-1", 1, route)); !errors.Is(err, execution.ErrRouteReceiptExpired) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		intent := h.intent("step-1", 1, "lease-1", 1, h.route())
		intent.Route.Candidates[0].Model = "tampered-model"
		if _, _, err := h.store.PrepareAttempt(h.ctx, "a-tampered", intent); !errors.Is(err, execution.ErrRouteReceiptTampered) {
			t.Fatalf("error=%v", err)
		}
	})
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("refused attempts left %d rows", got)
	}
}

func TestRefuseCandidateOrderViolation(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()

	intent := h.intent("step-1", 1, "lease-1", 1, h.route())
	intent.Choice.CandidateRank = 2
	intent.Choice.CompatibleModel = "longcat-2.0"
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", intent); !errors.Is(err, execution.ErrCandidateOrder) {
		t.Fatalf("candidate order violation error=%v", err)
	}

	// Explicit exclusion evidence on candidate #1 makes candidate #2 legal.
	route := h.route(func(r *execution.RouteReceipt) {
		r.Candidates[0].ExclusionEvidence = "circuit-open:provider-health=fail"
	})
	allowed := h.intent("step-1", 1, "lease-1", 1, route)
	allowed.Choice.CandidateRank = 2
	allowed.Choice.CompatibleModel = "longcat-2.0"
	rec, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", allowed)
	if err != nil {
		t.Fatalf("excluded head candidate refused: %v", err)
	}
	if rec.State != execution.AttemptStateSubmitted {
		t.Fatalf("attempt=%+v", rec)
	}
}

func TestRefuseIncompatibleToolOrCapabilities(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()

	incompatible := h.intent("step-1", 1, "lease-1", 1, h.route())
	incompatible.Choice.CompatibleModel = "some-other-model"
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-incompatible", incompatible); !errors.Is(err, execution.ErrConnectorIncompatible) {
		t.Fatalf("incompatible tool error=%v", err)
	}

	missingCaps := h.intent("step-1", 1, "lease-1", 1, h.route())
	missingCaps.Choice.DeclaredCapabilities = []string{"code_gen"}
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-caps", missingCaps); !errors.Is(err, execution.ErrConnectorCapabilities) {
		t.Fatalf("missing capabilities error=%v", err)
	}

	noIdentity := h.intent("step-1", 1, "lease-1", 1, h.route())
	noIdentity.Choice.ConnectorID = ""
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-noidentity", noIdentity); !errors.Is(err, execution.ErrConnectorIncompatible) {
		t.Fatalf("missing connector identity error=%v", err)
	}
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("refused attempts left %d rows", got)
	}
}

func TestRefuseMissingOrStaleLeaseAuthority(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()

	missing := h.intent("step-1", 1, "lease-1", 1, h.route())
	missing.Lease.EnforcementReceiptRef = ""
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-missing-receipt", missing); !errors.Is(err, execution.ErrLeaseAuthorityMissing) {
		t.Fatalf("missing enforcement receipt error=%v", err)
	}

	noProfile := h.intent("step-1", 1, "lease-1", 1, h.route())
	noProfile.Lease.ProfileID = ""
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-noprofile", noProfile); !errors.Is(err, execution.ErrLeaseAuthorityMissing) {
		t.Fatalf("missing profile error=%v", err)
	}

	stale := h.intent("step-1", 1, "lease-1", 1, h.route())
	stale.Lease.ValidUntil = seamNow.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-stale", stale); !errors.Is(err, execution.ErrLeaseAuthorityStale) {
		t.Fatalf("stale authority error=%v", err)
	}

	unrecorded := h.intent("step-1", 1, "lease-9", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-unrecorded", unrecorded); !errors.Is(err, execution.ErrLeaseAuthorityMissing) {
		t.Fatalf("unrecorded lease error=%v", err)
	}

	drifted := h.intent("step-1", 1, "lease-1", 2, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-drift", drifted); !errors.Is(err, execution.ErrLeaseAuthorityStale) {
		t.Fatalf("lease epoch drift error=%v", err)
	}

	// A revoked lease is stale even though the authority record is fresh.
	if err := database.RevokeWorkspaceLease(h.ctx, h.db, "lease-1", 1, "test"); err != nil {
		t.Fatal(err)
	}
	revoked := h.intent("step-1", 1, "lease-1", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "a-revoked", revoked); !errors.Is(err, execution.ErrLeaseAuthorityStale) {
		t.Fatalf("revoked lease error=%v", err)
	}
	if got := h.attemptCount(); got != 0 {
		t.Fatalf("refused attempts left %d rows", got)
	}
}

func TestRefuseSecondActiveAttemptUnderDefaultConcurrency(t *testing.T) {
	h := newHarness(t)
	h.admit(0) // default max_active_attempts_per_parent = 1
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	second := h.intent("step-2", 1, "lease-1", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-2", second); !errors.Is(err, execution.ErrTooManyActiveAttempts) {
		t.Fatalf("second active attempt error=%v", err)
	}
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("refused attempt left rows: %d", got)
	}
}

// TestExplicitBoundedParallelismPolicy proves parallelism is only possible
// under an explicit bounded parent policy.
func TestExplicitBoundedParallelismPolicy(t *testing.T) {
	h := newHarness(t)
	h.admit(2)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	h.prepare("attempt-2", h.intent("step-2", 1, "lease-1", 1, h.route()))
	third := h.intent("step-3", 1, "lease-1", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-3", third); !errors.Is(err, execution.ErrTooManyActiveAttempts) {
		t.Fatalf("third active attempt error=%v", err)
	}
}

func TestRefuseRetryWhileStartUncertain(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	h.advance(61 * time.Second) // past the persisted start deadline
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil {
		t.Fatal(err)
	}
	retry := h.intent("step-1", 2, "lease-1", 1, h.route())
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-2", retry); !errors.Is(err, execution.ErrReconciliationRequired) {
		t.Fatalf("retry while uncertain error=%v", err)
	}
	if _, _, err := h.store.BeginReconciliation(h.ctx, foreman.ReconcileRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-2", retry); !errors.Is(err, execution.ErrReconciliationRequired) {
		t.Fatalf("retry while reconciling error=%v", err)
	}
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("uncertain retry created rows: %d", got)
	}
}

// TestUncertainReconciliationIdentifiesNativeAttempt proves the uncertain →
// reconciling → executing path once the native attempt is identified.
func TestUncertainReconciliationIdentifiesNativeAttempt(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	h.advance(61 * time.Second) // past the persisted start deadline
	if _, err := h.store.MarkStartUncertain(h.ctx, foreman.UncertainRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, replay, err := h.store.BeginReconciliation(h.ctx, foreman.ReconcileRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil || replay {
		t.Fatalf("begin reconciliation: replay=%v err=%v", replay, err)
	}
	// The native identification proof must itself be durable evidence.
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindNativeIdentification, "native-proof-1")
	eventsBefore := h.eventCount()
	rec, err := h.store.CompleteReconciliation(h.ctx, foreman.ReconcileOutcome{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Next:   execution.ParentExecuting,
		Result: foreman.ReconcileResult{Kind: foreman.ReconcileNativeIdentified, EvidenceRef: "native-proof-1", NativeSessionID: "native-session-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ParentState != execution.ParentExecuting {
		t.Fatalf("parent state=%q", rec.ParentState)
	}
	updated, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil || updated.ParentState != execution.ParentExecuting {
		t.Fatalf("parent=%+v err=%v", updated, err)
	}
	var nativeSession, state string
	if err := h.db.QueryRowContext(h.ctx, `SELECT native_session_id,state FROM execution_attempts WHERE attempt_id=?`, attempt.AttemptID).Scan(&nativeSession, &state); err != nil {
		t.Fatal(err)
	}
	if nativeSession != "native-session-1" || state != execution.AttemptStateWorking {
		t.Fatalf("native session=%q state=%q", nativeSession, state)
	}
	if got := h.eventCount(); got != eventsBefore+1 {
		t.Fatalf("reconciliation appended %d events", got-eventsBefore)
	}
}

// TestBoundedRetryAfterProvenNoStart proves the only retry path: the start is
// proven absent, the generation increments by exactly one, and the attempt
// idempotency key is derived — never an unrelated pending task.
func TestBoundedRetryAfterProvenNoStart(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	h.advance(61 * time.Second) // past the persisted start deadline
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
	// A no-start resolution requires durable no-start proof.
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindNoStartProof, "no-start-proof-1")
	rec, err := h.store.CompleteReconciliation(h.ctx, foreman.ReconcileOutcome{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Next:   execution.ParentExecuting,
		Result: foreman.ReconcileResult{Kind: foreman.ReconcileNoStart, EvidenceRef: "no-start-proof-1"},
	})
	if err != nil || rec.ParentState != execution.ParentExecuting {
		t.Fatalf("parent=%+v err=%v", rec, err)
	}

	retryRoute := h.route(func(r *execution.RouteReceipt) {
		r.ResolvedAt = seamNow.Format(time.RFC3339Nano)
	})
	retry := h.intent("step-1", 2, "lease-1", 1, retryRoute)
	retryAttempt := h.prepare("attempt-2", retry)
	wantKey := execution.DeriveAttemptIdempotencyKey(h.parentID, "step-1", 2, retryRoute.ReceiptDigest, 1)
	if retryAttempt.AttemptIdempotencyKey == attempt.AttemptIdempotencyKey || retryAttempt.AttemptIdempotencyKey != wantKey {
		t.Fatalf("retry key=%q want %q (first %q)", retryAttempt.AttemptIdempotencyKey, wantKey, attempt.AttemptIdempotencyKey)
	}

	// The retry generation is now active: no further generation is admissible
	// until it is quiescent, and generation gaps are never accepted.
	gap := h.intent("step-1", 4, "lease-1", 1, retryRoute)
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-3", gap); !errors.Is(err, execution.ErrStepGenerationActive) {
		t.Fatalf("generation ahead of active retry error=%v", err)
	}
	next := h.intent("step-1", 3, "lease-1", 1, retryRoute)
	if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-3", next); !errors.Is(err, execution.ErrStepGenerationActive) {
		t.Fatalf("retry before quiescence error=%v", err)
	}
	if got := h.attemptCount(); got != 2 {
		t.Fatalf("attempts=%d", got)
	}
}

// TestRefuseTransportAckAsCompletion proves an A2A/Crier ack is transport
// only: it can neither carry a state nor settle an attempt.
func TestRefuseTransportAckAsCompletion(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	ack := execution.TransportAck{AckID: "ack-1", MessageID: "msg-1", CorrelationID: "corr-1", ReceivedAt: seamNow.Format(time.RFC3339Nano)}
	if _, _, err := h.store.RecordTransportAck(h.ctx, foreman.TransportAckRecord{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Ack: ack, ProposedState: execution.AttemptStateCompleted,
	}); !errors.Is(err, execution.ErrTransportAckIsNotCompletion) {
		t.Fatalf("ack with completion state error=%v", err)
	}

	if _, _, err := h.store.RecordTransportAck(h.ctx, foreman.TransportAckRecord{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Ack: ack,
	}); err != nil {
		t.Fatalf("transport ack evidence: %v", err)
	}
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID:  h.parentID,
		AttemptID:    attempt.AttemptID,
		TransportAck: &ack,
	}}); !errors.Is(err, execution.ErrTransportAckIsNotCompletion) {
		t.Fatalf("ack as settlement error=%v", err)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID:  h.parentID,
		TransportAck: &ack,
	}}); err == nil {
		t.Fatal("ack as parent settlement unexpectedly accepted")
	}

	var state string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM execution_attempts WHERE attempt_id=?`, attempt.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if !execution.IsActiveAttemptState(state) {
		t.Fatalf("transport ack settled the attempt: state=%q", state)
	}
}

// TestRefuseTerminalOutcomeWithoutSettlementOrVerification proves settlement
// authority: the durable terminal outcome alone is not settlement.
func TestRefuseTerminalOutcomeWithoutSettlementOrVerification(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	outcome, connector, independent := h.settleTriple()

	noSettlement := connector
	noSettlement.SettlementReceiptRef = ""
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: noSettlement, Independent: independent,
	}}); !errors.Is(err, execution.ErrSettlementIncomplete) {
		t.Fatalf("attempt without connector settlement error=%v", err)
	}

	noVerification := independent
	noVerification.VerificationRef = ""
	if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: noVerification,
	}}); !errors.Is(err, execution.ErrSettlementIncomplete) {
		t.Fatalf("attempt without independent verification error=%v", err)
	}

	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Independent: independent,
	}}); !errors.Is(err, execution.ErrSettlementIncomplete) {
		t.Fatalf("parent without connector settlement error=%v", err)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector,
	}}); !errors.Is(err, execution.ErrSettlementIncomplete) {
		t.Fatalf("parent without independent verification error=%v", err)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: execution.TerminalOutcome{OutcomeRef: "outcome-1", State: execution.AttemptStateWorking},
		Connector: connector, Independent: independent,
	}}); !errors.Is(err, execution.ErrSettlementIncomplete) {
		t.Fatalf("parent with non-terminal outcome error=%v", err)
	}
}

// TestRefuseCancellationSettlementWithDescendants proves cancellation fans out
// only to active attempts and the parent cannot settle while descendants are
// live or their connectors are unsettled.
func TestRefuseCancellationSettlementWithDescendants(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))

	fanout, err := h.store.RequestCancellation(h.ctx, h.parentID, "operator abort")
	if err != nil {
		t.Fatal(err)
	}
	if fanout.Parent.ParentState != execution.ParentCancelling || len(fanout.ActiveAttempts) != 1 || fanout.ActiveAttempts[0].AttemptID != attempt.AttemptID {
		t.Fatalf("fanout=%+v", fanout)
	}

	outcome, connector, independent := h.settleTriple()
	outcome.State = execution.AttemptStateCanceled
	quiescent := execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
		QuiescenceReceiptRef: "quiescence-receipt-1",
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: quiescent}); !errors.Is(err, execution.ErrUnsettledDescendants) {
		t.Fatalf("settlement over live descendants error=%v", err)
	}

	// A connector-reported terminal outcome without a settlement receipt is
	// still an unsettled connector.
	if _, _, err := database.AppendExecutionEvent(h.ctx, h.db, database.ExecutionEvent{
		RunID: h.parentID, AttemptID: attempt.AttemptID, Generation: attempt.Generation, Sequence: 1,
		Kind: "connector_terminal", ProposedState: execution.AttemptStateFailed, PayloadDigest: execution.EvidenceDigest("connector_terminal", attempt.AttemptID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: quiescent}); !errors.Is(err, execution.ErrUnsettledDescendants) {
		t.Fatalf("settlement over unsettled connector error=%v", err)
	}

	// Cancellation settlement also demands quiescence evidence. Every
	// reference must resolve to durable records first.
	quiescenceless := quiescent
	quiescenceless.QuiescenceReceiptRef = ""
	attemptOutcome := execution.TerminalOutcome{OutcomeRef: "outcome-1", State: execution.AttemptStateFailed, CorrelationID: "corr-1"}
	h.recordSettlementEvidence(attempt.AttemptID, 1, attemptOutcome, connector, independent)
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindConnectorQuiescence, "quiescence-receipt-1")
	settledAttempt, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID,
		Outcome:   attemptOutcome,
		Connector: connector, Independent: independent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if settledAttempt.State != execution.AttemptStateFailed {
		t.Fatalf("attempt=%+v", settledAttempt)
	}
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: quiescenceless}); !errors.Is(err, execution.ErrQuiescenceEvidenceRequired) {
		t.Fatalf("cancellation settlement without quiescence evidence error=%v", err)
	}
	rec, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: quiescent})
	if err != nil || rec.ParentState != execution.ParentSettled {
		t.Fatalf("parent=%+v err=%v", rec, err)
	}
}

// TestVerifiedParentSettlement is the positive settlement path: durable
// terminal outcome + connector settlement + independent verification, every
// descendant quiescent, DuckBrain queued as an outbox reference only.
func TestVerifiedParentSettlement(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	receipt := h.invoke()
	attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
	outcome, connector, independent := h.settleTriple()
	h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)

	settled, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
	}})
	if err != nil || settled.State != execution.AttemptStateCompleted {
		t.Fatalf("attempt=%+v err=%v", settled, err)
	}

	reported, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
		ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
		Outcome: outcome, Connector: connector, Independent: independent,
	})
	if err != nil || reported.ParentState != execution.ParentVerifying {
		t.Fatalf("parent=%+v err=%v", reported, err)
	}

	rec, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ParentState != execution.ParentSettled || rec.TerminalOutcomeRef != outcome.OutcomeRef ||
		rec.ConnectorSettlementRef != connector.SettlementReceiptRef || rec.IndependentVerificationRef != independent.VerificationRef {
		t.Fatalf("settled parent=%+v", rec)
	}
	if rec.SettledAt == "" || rec.ForemanInvocationID != receipt.InvocationID {
		t.Fatalf("settled parent=%+v", rec)
	}

	// DuckBrain projection: an outbox reference only — the projector seam is
	// not invoked in this slice and nothing leaves the process.
	var projections int
	var projectionState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*),COALESCE(MAX(state),'') FROM execution_projections WHERE run_id=? AND projection='duckbrain'`, h.parentID).Scan(&projections, &projectionState); err != nil {
		t.Fatal(err)
	}
	if projections != 1 || projectionState != "pending" {
		t.Fatalf("duckbrain projection rows=%d state=%q", projections, projectionState)
	}
	if h.projector.calls != 0 {
		t.Fatalf("duckbrain projector invoked %d times in a dark slice", h.projector.calls)
	}

	// Settlement is idempotent; changed evidence conflicts.
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
	}}); err != nil {
		t.Fatalf("settlement replay: %v", err)
	}
	changed := outcome
	changed.OutcomeRef = "outcome-2"
	if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: changed, Connector: connector, Independent: independent,
	}}); !errors.Is(err, foreman.ErrParentConflict) {
		t.Fatalf("changed settlement error=%v", err)
	}
}

// TestRestartReplayIdempotency proves the seam survives a restart and every
// write replays against the existing SQLite store without duplicating rows.
func TestRestartReplayIdempotency(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	receipt := h.invoke()
	intent := h.intent("step-1", 1, "lease-1", 1, h.route())
	attempt := h.prepare("attempt-1", intent)
	h.advance(61 * time.Second) // past the persisted start deadline
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
	attemptsBefore, eventsBefore := h.attemptCount(), h.eventCount()
	invocationsBefore := h.invoker.count()

	h.restart()

	rec, err := h.durable.GetParentRun(h.ctx, h.parentID)
	if err != nil || rec.ParentState != execution.ParentReconciling {
		t.Fatalf("parent after restart=%+v err=%v", rec, err)
	}
	runs, _, _, err := database.RecoverExecutionState(h.ctx, h.db)
	if err != nil || len(runs) != 1 || runs[0].RunID != h.parentID {
		t.Fatalf("recovery runs=%v err=%v", runs, err)
	}

	if _, replay, err := h.store.AdmitParent(h.ctx, foreman.ParentRunSpec{
		ParentRunID:              h.parentID,
		SubmittingAuthority:      "scheduler:test",
		SubmissionIdempotencyKey: "submit-" + h.parentID,
		ParentBriefJSON:          h.briefJSON,
	}); err != nil || !replay {
		t.Fatalf("parent admission replay: replay=%v err=%v", replay, err)
	}
	if _, err := h.store.InvokeForeman(h.ctx, h.parentID); err != nil {
		t.Fatalf("foreman invocation replay: %v", err)
	}
	if got := h.invoker.count(); got != invocationsBefore {
		t.Fatalf("restart re-invoked the foreman: %d → %d", invocationsBefore, got)
	}
	if _, replay, err := h.store.PrepareAttempt(h.ctx, "attempt-1", intent); err != nil || !replay {
		t.Fatalf("attempt preparation replay: replay=%v err=%v", replay, err)
	}
	if _, replay, err := h.store.BeginReconciliation(h.ctx, foreman.ReconcileRequest{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1, Reason: "start deadline passed",
	}); err != nil || !replay {
		t.Fatalf("reconciliation enqueue replay: replay=%v err=%v", replay, err)
	}
	if got := h.attemptCount(); got != attemptsBefore {
		t.Fatalf("replay changed attempts: %d → %d", attemptsBefore, got)
	}
	if got := h.eventCount(); got != eventsBefore {
		t.Fatalf("replay changed events: %d → %d", eventsBefore, got)
	}

	// The replayed reconciliation still resolves to the identified native
	// attempt and then settles with complete, durably recorded evidence.
	h.recordEvidence(attempt.AttemptID, 1, execution.EvidenceKindNativeIdentification, "native-proof-1")
	rec, err = h.store.CompleteReconciliation(h.ctx, foreman.ReconcileOutcome{
		ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Generation: 1,
		Next:   execution.ParentExecuting,
		Result: foreman.ReconcileResult{Kind: foreman.ReconcileNativeIdentified, EvidenceRef: "native-proof-1", NativeSessionID: "native-session-1"},
	})
	if err != nil || rec.ParentState != execution.ParentExecuting {
		t.Fatalf("parent=%+v err=%v", rec, err)
	}
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
	final, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: execution.ParentSettlement{
		ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
	}})
	if err != nil || final.ParentState != execution.ParentSettled {
		t.Fatalf("parent=%+v err=%v", final, err)
	}
}

// TestConcurrentAttemptAdmissionSingleWinner is the focused race test for the
// store seam: under the default one-attempt policy, concurrent dispatch
// preparations for distinct ready steps must persist exactly one attempt.
func TestConcurrentAttemptAdmissionSingleWinner(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.acquireLease("lease-1", 1, 0)
	h.invoke()

	const contenders = 6
	var wg sync.WaitGroup
	errs := make([]error, contenders)
	replays := make([]bool, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			step := fmt.Sprintf("step-%d", i+1)
			_, replays[i], errs[i] = h.store.PrepareAttempt(h.ctx, fmt.Sprintf("attempt-%d", i+1), h.intent(step, 1, "lease-1", 1, h.route()))
		}(i)
	}
	wg.Wait()
	winners := 0
	for i := 0; i < contenders; i++ {
		switch {
		case errs[i] == nil && !replays[i]:
			winners++
		case errors.Is(errs[i], execution.ErrTooManyActiveAttempts):
		default:
			t.Fatalf("contender %d: replay=%v err=%v", i, replays[i], errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d, want exactly 1", winners)
	}
	if got := h.attemptCount(); got != 1 {
		t.Fatalf("attempts=%d, want 1", got)
	}
}

// TestParentBriefCarriesNoDecomposition proves the canonical parent brief the
// scheduler hands the foreman contains no step plan, no attempt and no tool
// choice: those belong to the foreman.
func TestParentBriefCarriesNoDecomposition(t *testing.T) {
	h := newHarness(t)
	h.admit(0)
	h.invoke()
	if len(h.invoker.calls) != 1 {
		t.Fatalf("foreman invocations=%d", len(h.invoker.calls))
	}
	brief := h.invoker.calls[0]
	if _, err := execution.ValidateParentBriefDigest(h.briefJSON); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"step_id", "attempt_id", "harness", "candidate", "a2a"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("parent brief carries %q: %s", forbidden, raw)
		}
	}
	if brief.ParentRunID != h.parentID || brief.Mission == "" || brief.CancellationPolicy == "" {
		t.Fatalf("brief=%+v", brief)
	}
}
