package foreman

import (
	"context"
	"errors"
	"fmt"

	"github.com/coding-hermes/scheduler/internal/execution"
)

// Durable seam sentinel errors. Implementations wrap these so callers can
// classify refusals without parsing prose.
var (
	ErrParentNotFound  = errors.New("parent run not found")
	ErrParentConflict  = errors.New("parent run conflict")
	ErrAttemptNotFound = errors.New("attempt not found")
	ErrAttemptConflict = errors.New("attempt conflict")
	ErrSeamInvoked     = errors.New("seam precondition violated")
)

// Seam is the explicit dark composition seam: one parent run in, one foreman
// invocation under a stable durable identity, just-in-time attempts behind the
// closed fail-closed gate, and settlement only on complete evidence.
//
// It holds no routing, harness or A2A capability by construction.
type Seam struct {
	store      Store
	foreman    execution.ForemanInvoker
	reconciler execution.InvocationReconciler
	duckbrain  execution.DuckBrainProjector
}

// NewSeam composes the dark seam. The projector is recorded as an outbox
// reference only in this slice and is never invoked here. Without an
// invocation reconciler an uncertain crossing fails closed instead of
// re-invoking.
func NewSeam(store Store, invoker execution.ForemanInvoker, projector execution.DuckBrainProjector) *Seam {
	return &Seam{store: store, foreman: invoker, duckbrain: projector}
}

// NewSeamWithReconciler composes the dark seam with the explicit model of the
// crash window: after an uncertain external crossing the scheduler reconciles
// the stable invocation identity before any resend.
func NewSeamWithReconciler(store Store, invoker execution.ForemanInvoker, reconciler execution.InvocationReconciler, projector execution.DuckBrainProjector) *Seam {
	return &Seam{store: store, foreman: invoker, reconciler: reconciler, duckbrain: projector}
}

// AdmitParent admits exactly one parent execution run. The canonical parent
// brief digest is verified fail-closed; a replay of the same submission is
// idempotent.
func (s *Seam) AdmitParent(ctx context.Context, spec ParentRunSpec) (ParentRecord, bool, error) {
	if spec.ParentRunID == "" || spec.SubmittingAuthority == "" || spec.SubmissionIdempotencyKey == "" {
		return ParentRecord{}, false, fmt.Errorf("%w: parent admission identity incomplete", ErrSeamInvoked)
	}
	brief, err := execution.ValidateParentBriefDigest(spec.ParentBriefJSON)
	if err != nil {
		return ParentRecord{}, false, err
	}
	if brief.ParentRunID != spec.ParentRunID {
		return ParentRecord{}, false, fmt.Errorf("%w: brief names parent %q", ErrSeamInvoked, brief.ParentRunID)
	}
	if spec.MaxActiveAttempts == 0 {
		spec.MaxActiveAttempts = DefaultMaxActiveAttempts
	}
	if spec.MaxActiveAttempts < 1 || spec.MaxActiveAttempts > MaxActiveAttemptsCap {
		return ParentRecord{}, false, fmt.Errorf("%w: max_active_attempts %d outside 1..%d", ErrSeamInvoked, spec.MaxActiveAttempts, MaxActiveAttemptsCap)
	}
	return s.store.AdmitParentRun(ctx, spec)
}

// InvokeForeman invokes Hermes as the foreman with the canonical parent brief
// under one stable, durably persisted invocation identity and applies
// queued → foreman_planning. The identity is persisted BEFORE the external
// boundary is crossed; after a crash the identity is reconciled before any
// resend, and concurrent callers never both invoke. This is at-least-once
// delivery with a stable receiver-facing identity — never a claim of
// distributed exactly-once.
func (s *Seam) InvokeForeman(ctx context.Context, parentRunID string) (execution.ForemanReceipt, error) {
	intent, crossing, err := s.store.BeginForemanInvocation(ctx, parentRunID)
	if err != nil {
		return execution.ForemanReceipt{}, err
	}
	if intent.Confirmed() {
		return intent.Receipt, nil
	}
	if intent.Uncertain {
		if s.reconciler == nil {
			return execution.ForemanReceipt{}, fmt.Errorf("%w: invocation %s", execution.ErrInvocationUncertain, intent.InvocationID)
		}
		resolution, err := s.reconciler.ReconcileInvocation(ctx, intent.InvocationID, parentRunID)
		if err != nil {
			return execution.ForemanReceipt{}, err
		}
		switch resolution.Status {
		case execution.InvocationAccepted:
			receipt := execution.ForemanReceipt{
				InvocationID: intent.InvocationID,
				ParentRunID:  parentRunID,
				AcceptedAt:   resolution.AcceptedAt,
			}
			if _, _, err := s.store.ConfirmForemanInvocation(ctx, parentRunID, receipt); err != nil {
				return execution.ForemanReceipt{}, err
			}
			return receipt, nil
		case execution.InvocationNotStarted:
			// The receiver never saw it: a resend with the SAME identity is
			// safe because the receiver deduplicates on it.
			crossing = true
		default:
			return execution.ForemanReceipt{}, fmt.Errorf("%w: invocation %s", execution.ErrInvocationUncertain, intent.InvocationID)
		}
	}
	if !crossing {
		// A concurrent caller must not invoke Hermes while another crossing
		// is in flight.
		return execution.ForemanReceipt{}, fmt.Errorf("%w: invocation %s in flight", execution.ErrInvocationUncertain, intent.InvocationID)
	}
	rec, err := s.store.GetParentRun(ctx, parentRunID)
	if err != nil {
		return execution.ForemanReceipt{}, err
	}
	brief, err := execution.ValidateParentBriefDigest(rec.BriefJSON)
	if err != nil {
		return execution.ForemanReceipt{}, err
	}
	if s.foreman == nil {
		return execution.ForemanReceipt{}, fmt.Errorf("%w: no ForemanInvoker configured", ErrSeamInvoked)
	}
	receipt, err := s.foreman.InvokeForeman(ctx, execution.ForemanInvocation{
		InvocationID: intent.InvocationID,
		ParentRunID:  parentRunID,
		Brief:        *brief,
	})
	if err != nil {
		return execution.ForemanReceipt{}, err
	}
	if receipt.InvocationID != intent.InvocationID || receipt.ParentRunID != parentRunID {
		return execution.ForemanReceipt{}, fmt.Errorf("%w: receipt %s for parent %s", execution.ErrInvocationIdentity, receipt.InvocationID, receipt.ParentRunID)
	}
	if _, _, err := s.store.ConfirmForemanInvocation(ctx, parentRunID, receipt); err != nil {
		return execution.ForemanReceipt{}, err
	}
	return receipt, nil
}

// PrepareAttempt persists exactly one child attempt just in time. It refuses
// anything but a complete ready-step intent; the store re-checks every gate
// durably before the row exists.
func (s *Seam) PrepareAttempt(ctx context.Context, attemptID string, intent execution.ReadyStepIntent) (AttemptRecord, bool, error) {
	if attemptID == "" {
		return AttemptRecord{}, false, fmt.Errorf("%w: attempt id required", ErrSeamInvoked)
	}
	if err := execution.ValidateReadyStepIntent(intent); err != nil {
		return AttemptRecord{}, false, err
	}
	return s.store.PrepareStepAttempt(ctx, AttemptPreparation{AttemptID: attemptID, Intent: intent})
}

// MarkStartUncertain applies the start-timeout rule: the parent becomes
// uncertain and no retry or new generation is allowed until reconciliation
// proves no start or identifies the native attempt.
func (s *Seam) MarkStartUncertain(ctx context.Context, req UncertainRequest) (ParentRecord, error) {
	if req.ParentRunID == "" || req.AttemptID == "" || req.Generation < 1 || req.Reason == "" {
		return ParentRecord{}, fmt.Errorf("%w: uncertain request incomplete", ErrSeamInvoked)
	}
	return s.store.MarkStartUncertain(ctx, req)
}

// BeginReconciliation enqueues one reconciliation durably first (idempotent)
// and applies uncertain → reconciling.
func (s *Seam) BeginReconciliation(ctx context.Context, req ReconcileRequest) (ParentRecord, bool, error) {
	if req.ParentRunID == "" || req.AttemptID == "" || req.Generation < 1 || req.Reason == "" {
		return ParentRecord{}, false, fmt.Errorf("%w: reconcile request incomplete", ErrSeamInvoked)
	}
	return s.store.BeginReconciliation(ctx, req)
}

// CompleteReconciliation records the proven reconciliation result and applies
// reconciling → executing. A proven no-start is what permits a bounded retry
// with a new generation; reconciliation can never manufacture settled — that
// is the normal settlement path with full evidence.
func (s *Seam) CompleteReconciliation(ctx context.Context, req ReconcileOutcome) (ParentRecord, error) {
	if req.ParentRunID == "" || req.AttemptID == "" || req.Generation < 1 {
		return ParentRecord{}, fmt.Errorf("%w: reconcile outcome incomplete", ErrSeamInvoked)
	}
	if req.Next == execution.ParentSettled {
		return ParentRecord{}, execution.ErrReconciliationSettled
	}
	if req.Next != execution.ParentExecuting {
		return ParentRecord{}, fmt.Errorf("%w: reconciliation next state %q", execution.ErrParentTransitionIllegal, req.Next)
	}
	switch req.Result.Kind {
	case ReconcileNoStart:
		if req.Result.EvidenceRef == "" {
			return ParentRecord{}, fmt.Errorf("%w: no-start proof reference missing", ErrSeamInvoked)
		}
	case ReconcileNativeIdentified:
		if req.Result.EvidenceRef == "" || req.Result.NativeSessionID == "" {
			return ParentRecord{}, fmt.Errorf("%w: native identification reference missing", ErrSeamInvoked)
		}
	default:
		return ParentRecord{}, fmt.Errorf("%w: unknown reconciliation result %q", ErrSeamInvoked, req.Result.Kind)
	}
	return s.store.CompleteReconciliation(ctx, req)
}

// RecordTransportAck records transport-only evidence. A transport ack is
// never completion: presenting a state with it is refused.
func (s *Seam) RecordTransportAck(ctx context.Context, req TransportAckRecord) (EvidenceRecord, bool, error) {
	if req.ProposedState != "" {
		return EvidenceRecord{}, false, execution.ErrTransportAckIsNotCompletion
	}
	if req.ParentRunID == "" || req.AttemptID == "" || req.Generation < 1 || req.Ack.AckID == "" {
		return EvidenceRecord{}, false, fmt.Errorf("%w: transport ack incomplete", ErrSeamInvoked)
	}
	ev := EvidenceRecord{
		ParentRunID: req.ParentRunID,
		AttemptID:   req.AttemptID,
		Generation:  req.Generation,
		Kind:        "transport_ack",
		PayloadDigest: execution.EvidenceDigest(
			"transport_ack", req.ParentRunID, req.AttemptID,
			fmt.Sprintf("%d", req.Generation), req.Ack.AckID, req.Ack.MessageID, req.Ack.CorrelationID,
		),
	}
	return s.store.AppendEvidence(ctx, ev)
}

// SettleAttempt settles one attempt on complete settlement evidence: a
// durable terminal outcome, a connector settlement and an independent
// verification reference. A transport ack alone is refused.
func (s *Seam) SettleAttempt(ctx context.Context, req AttemptSettlementRequest) (AttemptRecord, error) {
	if err := execution.ValidateAttemptSettlement(req.Settlement); err != nil {
		return AttemptRecord{}, err
	}
	return s.store.SettleAttempt(ctx, req)
}

// RecordForemanReport admits the foreman's verified parent result and applies
// executing → verifying.
func (s *Seam) RecordForemanReport(ctx context.Context, report execution.ForemanReport) (ParentRecord, error) {
	if err := execution.ValidateForemanReport(report); err != nil {
		return ParentRecord{}, err
	}
	return s.store.RecordForemanReport(ctx, report)
}

// RequestCancellation is parent-authoritative: it fans out only to currently
// active attempts and the parent settles only after settlement/quiescence
// evidence.
func (s *Seam) RequestCancellation(ctx context.Context, parentRunID, reason string) (CancelFanout, error) {
	if parentRunID == "" || reason == "" {
		return CancelFanout{}, fmt.Errorf("%w: cancellation request incomplete", ErrSeamInvoked)
	}
	return s.store.RequestCancellation(ctx, parentRunID, reason)
}

// SettleParent settles the parent on complete settlement evidence — durable
// terminal outcome, connector settlement and independent verification — after
// every descendant has settled or been proven never started. The DuckBrain
// projection is queued as an outbox reference only.
func (s *Seam) SettleParent(ctx context.Context, req ParentSettlementRequest) (ParentRecord, error) {
	rec, err := s.store.GetParentRun(ctx, req.Settlement.ParentRunID)
	if err != nil {
		return ParentRecord{}, err
	}
	pre := req.Settlement
	pre.FromParentState = rec.ParentState
	pre.Descendants = nil // descendant quiescence is the store's fact to load
	if err := execution.ValidateParentSettlement(pre); err != nil {
		return ParentRecord{}, err
	}
	return s.store.SettleParent(ctx, req)
}

// DuckBrainProjector exposes the projection seam for later integration. This
// slice never calls it.
func (s *Seam) DuckBrainProjector() execution.DuckBrainProjector { return s.duckbrain }
