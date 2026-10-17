package execution

import "context"

// ForemanReceipt is the durable record that Hermes accepted one parent brief.
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
// Implementations must tolerate an at-least-once invocation across scheduler
// restarts: the parent run ID is the correlation identity.
type ForemanInvoker interface {
	InvokeForeman(ctx context.Context, brief ParentBrief) (ForemanReceipt, error)
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
