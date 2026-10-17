package execution

import "context"

// ForemanInvocation is the scheduler's outward call identity: a stable
// invocation id, persisted durably before the external boundary is crossed,
// plus the canonical parent brief. The receiver-facing identity never changes
// on replay — a resend after an uncertain crossing presents the same
// invocation id so the receiver can deduplicate.
type ForemanInvocation struct {
	InvocationID string      `json:"invocation_id"`
	ParentRunID  string      `json:"parent_run_id"`
	Brief        ParentBrief `json:"brief"`
}

// ForemanReceipt is the durable record that Hermes accepted one parent brief
// under one stable invocation identity.
type ForemanReceipt struct {
	InvocationID string `json:"invocation_id"`
	ParentRunID  string `json:"parent_run_id"`
	AcceptedAt   string `json:"accepted_at"`
}

// ForemanInvoker invokes Hermes as the foreman exactly once per parent run
// with the canonical parent brief. The scheduler never decomposes steps, never
// calls Task Router, never chooses a harness and never sends A2A through this
// seam — those are the foreman's.
//
// The contract is at-least-once with a STABLE invocation identity, not
// impossible distributed exactly-once: the scheduler persists the invocation
// id before calling, reconciles an uncertain crossing before any resend, and
// the receiver deduplicates on the invocation id. Implementations must echo
// the InvocationID they were given.
type ForemanInvoker interface {
	InvokeForeman(ctx context.Context, inv ForemanInvocation) (ForemanReceipt, error)
}

// InvocationStatus is the reconciled truth of one uncertain external
// crossing. Closed.
type InvocationStatus string

const (
	// InvocationNotStarted means the receiver never accepted the invocation:
	// a resend with the same identity is safe (the receiver deduplicates).
	InvocationNotStarted InvocationStatus = "not_started"
	// InvocationAccepted means the receiver accepted the invocation: the
	// recorded receipt is confirmed and nothing is re-sent.
	InvocationAccepted InvocationStatus = "accepted"
)

// InvocationResolution is the outcome of reconciling one uncertain crossing.
type InvocationResolution struct {
	Status     InvocationStatus
	AcceptedAt string
}

// InvocationReconciler resolves an uncertain external crossing before any
// resend. It is the explicit model of the crash window: after the stable
// invocation identity is persisted, the scheduler never blindly re-invokes —
// it asks what actually happened at the receiver first.
type InvocationReconciler interface {
	ReconcileInvocation(ctx context.Context, invocationID, parentRunID string) (InvocationResolution, error)
}

// ForemanReport is the one settled parent result Hermes reports back: the
// durable terminal outcome plus connector settlement and the independent
// verification reference.
type ForemanReport struct {
	ParentRunID  string                  `json:"parent_run_id"`
	InvocationID string                  `json:"invocation_id"`
	Outcome      TerminalOutcome         `json:"outcome"`
	Connector    ConnectorSettlement     `json:"connector"`
	Independent  IndependentVerification `json:"independent"`
}

// ValidateForemanReport validates the reported parent result before the
// scheduler admits it into verifying.
func ValidateForemanReport(r ForemanReport) error {
	if r.ParentRunID == "" || r.InvocationID == "" {
		return ErrSettlementIncomplete
	}
	return validateSettlementTriple(r.Outcome, r.Connector, r.Independent, nil)
}

// DuckBrainProjection is the durable projection reference handed to
// DuckBrain. It carries identities and digests only — never raw secrets.
type DuckBrainProjection struct {
	ParentRunID    string `json:"parent_run_id"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadDigest  string `json:"payload_digest"`
	OutcomeRef     string `json:"outcome_ref"`
}

// DuckBrainReadback is the projection read-back identity.
type DuckBrainReadback struct {
	ProjectionID   string `json:"projection_id"`
	ReadbackDigest string `json:"readback_digest"`
}

// DuckBrainProjector is the projection seam. This slice only records outbox
// references; nothing here performs a network side effect or carries a raw
// secret.
type DuckBrainProjector interface {
	Project(ctx context.Context, projection DuckBrainProjection) (DuckBrainReadback, error)
}
