package execution

import "errors"

// Sentinel errors for the parent lifecycle, the just-in-time attempt gate and
// settlement authority. Every fail-closed refusal maps to exactly one of
// these, so callers can distinguish "retry later" from "foreman bug" without
// parsing prose.
var (
	// Parent lifecycle.
	ErrParentTransitionIllegal = errors.New("illegal parent lifecycle transition")
	ErrParentNotExecutable     = errors.New("parent is not executable for attempt admission")
	ErrParentCancelling        = errors.New("parent is cancelling")
	ErrParentSettled           = errors.New("parent is settled")
	ErrParentBriefIncomplete   = errors.New("parent brief is incomplete")

	// Ready-step intent.
	ErrNoReadyStepIntent     = errors.New("no foreman ready-step intent")
	ErrReadyStepIntent       = errors.New("invalid foreman ready-step intent")
	ErrRouteReceiptMissing   = errors.New("task router route receipt missing")
	ErrRouteReceiptInvalid   = errors.New("task router route receipt invalid")
	ErrRouteReceiptTampered  = errors.New("task router route receipt digest mismatch")
	ErrRouteReceiptExpired   = errors.New("task router route receipt expired")
	ErrCandidateOrder        = errors.New("route candidate order violation")
	ErrConnectorIncompatible = errors.New("tool/connector incompatible with chosen candidate")
	ErrConnectorCapabilities = errors.New("tool/connector missing required capabilities")
	ErrLeaseAuthorityMissing = errors.New("workspace lease/profile authority missing")
	ErrLeaseAuthorityStale   = errors.New("workspace lease/profile authority stale")
	ErrLeaseOwnership        = errors.New("workspace lease is owned by another parent or attempt")

	// Attempt admission.
	ErrForemanNotInvoked        = errors.New("foreman was never invoked on this parent")
	ErrStepGenerationConflict   = errors.New("step generation already active or settled")
	ErrStepGenerationActive     = errors.New("prior step generation still active")
	ErrStepGenerationUnsettled  = errors.New("prior step generation not quiescent")
	ErrGenerationNotIncremented = errors.New("step generation must increment by exactly one")
	ErrTooManyActiveAttempts    = errors.New("max active attempts per parent reached")
	ErrReconciliationRequired   = errors.New("reconciliation required before a new attempt generation")
	ErrDependencyNotReady       = errors.New("dependency is not a verified successful terminal result")

	// Evidence and transition authority.
	ErrEvidenceSequence      = errors.New("evidence sequence violates the durable append-only stream")
	ErrAttemptTransition     = errors.New("illegal attempt state transition")
	ErrAttemptTerminal       = errors.New("attempt is terminal and cannot accept state authority")
	ErrEvidenceNotAccepted   = errors.New("evidence was not accepted by the durable stream")
	ErrEvidenceNotDurable    = errors.New("evidence reference does not resolve to a durable record")
	ErrVerificationAuthority = errors.New("independent verification authority contradicts durable receipts")

	// Foreman invocation seam.
	ErrInvocationUncertain = errors.New("foreman invocation crossing is uncertain; reconcile before resend")
	ErrInvocationIdentity  = errors.New("foreman invocation identity is not stable")

	// Start uncertainty and reconciliation.
	ErrStartDeadlineNotElapsed = errors.New("persisted start deadline has not elapsed")
	ErrAttemptNotStartPending  = errors.New("attempt is not awaiting native start evidence")
	ErrNoStartContradiction    = errors.New("no-start proof contradicts recorded process authority")
	ErrReconciliationSettled   = errors.New("reconciliation cannot manufacture settled; settle through the normal settlement path")

	// Settlement authority.
	ErrTransportAckIsNotCompletion = errors.New("transport ack is not completion evidence")
	ErrSettlementIncomplete        = errors.New("settlement requires durable terminal outcome, connector settlement and independent verification")
	ErrUnsettledDescendants        = errors.New("unsettled descendant attempts block parent settlement")
	ErrQuiescenceEvidenceRequired  = errors.New("quiescence evidence required for cancellation settlement")
	ErrCancellationMonotonic       = errors.New("cancellation is monotonic: success cannot follow a cancellation request")
)
