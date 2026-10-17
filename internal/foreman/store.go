// Package foreman is the dark, harness-neutral scheduler seam that admits
// exactly one parent execution run, invokes Hermes as foreman, and permits a
// child attempt only just in time.
//
// It is DARK: nothing in this package is wired into the live scheduling loop,
// HTTP mux, configuration or production defaults. Later Crier, Task Router and
// Bunker integrations compose against the closed interfaces here:
//
//   - execution.ForemanInvoker — the scheduler's only outward call: Hermes
//     receives the canonical parent brief. The scheduler never decomposes
//     steps, never calls Task Router, never chooses a harness and never sends
//     A2A through this seam.
//   - execution.ReadyStepIntent — the foreman's just-in-time route receipt,
//     candidate order, tool/connector compatibility and workspace lease
//     authority. Task Router and Crier evidence arrives inside it; the seam
//     only verifies it.
//   - execution.TransportAck — transport-only evidence that can never settle
//     an attempt or a parent.
//   - execution.DuckBrainProjector — projection interface; this slice records
//     outbox references only, with no raw secret and no network side effect.
//   - Store — the durable seam onto the landed execution schema (runs,
//     attempts, events, leases, projections, outbox). Planned and blocked
//     foreman steps never become rows through it.
package foreman

import (
	"context"

	"github.com/coding-hermes/scheduler/internal/execution"
)

// Bounds of the explicit bounded parent concurrency policy. The default is
// one active attempt per parent; anything else must be stated and bounded.
const (
	DefaultMaxActiveAttempts = 1
	MaxActiveAttemptsCap     = 16
)

// ParentRunSpec is the closed admission input for exactly one parent run.
// ParentBriefJSON is the canonical parent brief including its brief_digest.
type ParentRunSpec struct {
	ParentRunID              string
	SubmittingAuthority      string
	SubmissionIdempotencyKey string
	ParentBriefJSON          []byte
	// MaxActiveAttempts is the explicit bounded parallelism policy; 0 means
	// DefaultMaxActiveAttempts.
	MaxActiveAttempts int
}

// ParentRecord is the durable parent-run state.
type ParentRecord struct {
	ParentRunID                string
	ParentState                execution.ParentState
	State                      string // landed connector-plane state (schema v69 vocabulary)
	BriefID                    string
	BriefRevision              string
	BriefDigest                string
	BriefJSON                  []byte
	Lane                       string
	TaskID                     string
	TickID                     string
	SubmittingAuthority        string
	SubmissionIdempotencyKey   string
	CurrentAttemptGeneration   int
	MaxActiveAttempts          int
	DeadlineAt                 string
	BudgetJSON                 string
	CancellationPolicy         string
	ForemanInvocationID        string
	ForemanInvokedAt           string
	CancelRequestedAt          string
	CancelReason               string
	TerminalOutcomeRef         string
	ConnectorSettlementRef     string
	IndependentVerificationRef string
	QuiescenceReceiptRef       string
	SettledAt                  string
	CreatedAt                  string
	UpdatedAt                  string
}

// AttemptPreparation is the dispatch-preparation input that persists exactly
// one attempt after every fail-closed check has passed.
type AttemptPreparation struct {
	AttemptID string
	Intent    execution.ReadyStepIntent
}

// AttemptRecord is the durable just-in-time attempt state.
type AttemptRecord struct {
	AttemptID             string
	ParentRunID           string
	StepID                string
	StepGeneration        int
	Generation            int
	AttemptIdempotencyKey string
	State                 string
	BriefDigest           string
	RouteDigest           string
	CandidateRank         int
	ConnectorID           string
	ConnectorVersion      string
	SelectedModel         string
	SelectedProvider      string
	LeaseID               string
	LeaseEpoch            int
	ProfileID             string
	EnforcementReceiptRef string
	// Policy is the durable timeout/budget/cancellation policy admitted with
	// this attempt. It is caller-authoritative and part of the exact replay
	// contract: a changed policy on replay is a conflict, never a replay.
	Policy                     execution.AttemptPolicy
	StartDeadlineAt            string
	StartedAt                  string
	NativeSessionID            string
	TerminalOutcomeRef         string
	ConnectorSettlementRef     string
	IndependentVerificationRef string
	QuiescenceReceiptRef       string
	ReconciledNoStart          bool
	ProcessBacked              bool
	SubmittedAt                string
	SettledAt                  string
}

// Active reports whether the attempt is still live.
func (a AttemptRecord) Active() bool { return execution.IsActiveAttemptState(a.State) }

// Quiescent reports whether the generation has stopped AND either connector
// settlement landed or reconciliation proved it never started.
func (a AttemptRecord) Quiescent() bool {
	if !execution.IsTerminalAttemptState(a.State) {
		return false
	}
	return a.ConnectorSettlementRef != "" || a.ReconciledNoStart
}

// EvidenceRecord is one append-only attempt evidence row. Sequence 0
// allocates the next sequence for the attempt.
type EvidenceRecord struct {
	ParentRunID     string
	AttemptID       string
	Generation      int
	Sequence        int
	Kind            string
	ProposedState   string
	PayloadDigest   string
	Accepted        bool
	RejectionReason string
	EventID         int64
	CreatedAt       string
}

// UncertainRequest records a start timeout: the parent becomes uncertain and
// no retry or new generation is allowed until reconciliation resolves.
type UncertainRequest struct {
	ParentRunID string
	AttemptID   string
	Generation  int
	Reason      string
}

// ReconcileRequest is the enqueue-first reconciliation request.
type ReconcileRequest struct {
	ParentRunID string
	AttemptID   string
	Generation  int
	Reason      string
}

// Reconciliation result kinds. Closed.
const (
	ReconcileNoStart          = "no_start"
	ReconcileNativeIdentified = "native_identified"
)

// ReconcileResult is the proven reconciliation outcome.
type ReconcileResult struct {
	// Kind is one of ReconcileNoStart, ReconcileNativeIdentified.
	Kind string
	// EvidenceRef is the durable evidence reference for the proof.
	EvidenceRef string
	// NativeSessionID identifies the native attempt when Kind is
	// ReconcileNativeIdentified.
	NativeSessionID string
}

// ReconcileOutcome resolves an uncertain start. Next must be executing: a
// reconciliation result is a durable resolution receipt, never a settlement —
// settling follows the normal settlement path with full evidence.
type ReconcileOutcome struct {
	ParentRunID string
	AttemptID   string
	Generation  int
	Next        execution.ParentState
	Result      ReconcileResult
}

// AttemptSettlementRequest settles one attempt on complete settlement
// evidence.
type AttemptSettlementRequest struct {
	Settlement execution.AttemptSettlement
}

// ParentSettlementRequest settles the parent. Descendant facts are loaded
// from the store; any caller-supplied descendants are overwritten.
type ParentSettlementRequest struct {
	Settlement execution.ParentSettlement
}

// TransportAckRecord is transport-only evidence. It can never carry state.
type TransportAckRecord struct {
	ParentRunID string
	AttemptID   string
	Generation  int
	Ack         execution.TransportAck
	// ProposedState is always refused when non-empty: an ack is not
	// completion.
	ProposedState string
}

// CancelFanout is the parent-authoritative cancellation fan-out. Only
// currently active attempts are cancelled.
type CancelFanout struct {
	Parent            ParentRecord
	ActiveAttempts    []AttemptRecord
	CancelRequestedAt string
}

// InvocationIntent is the durable foreman-invocation identity. It is
// persisted BEFORE the external boundary is crossed; the receiver-facing
// identity never changes on replay.
type InvocationIntent struct {
	InvocationID string
	ParentRunID  string
	// Uncertain is true when an earlier crossing persisted the identity but
	// no confirmation landed: the crossing must be reconciled before any
	// resend.
	Uncertain bool
	// Receipt carries the confirmed receipt once the invocation completed.
	Receipt execution.ForemanReceipt
}

// Confirmed reports whether the invocation already completed durably.
func (i InvocationIntent) Confirmed() bool { return i.Receipt.InvocationID != "" }

// Store is the durable seam onto the landed execution schema. Implementations
// must be fail-closed and replay-safe: every mutation is idempotent by its
// derived identity and every refusal names a sentinel error from
// execution or this package.
type Store interface {
	// AdmitParentRun persists exactly one parent run, replaying an identical
	// submission.
	AdmitParentRun(ctx context.Context, spec ParentRunSpec) (ParentRecord, bool, error)
	// GetParentRun reads one parent run.
	GetParentRun(ctx context.Context, parentRunID string) (ParentRecord, error)
	// BeginForemanInvocation persists the stable invocation identity before
	// the external boundary is crossed. crossing is true only for the caller
	// that may invoke the foreman; an unconfirmed intent is an uncertain
	// crossing that must be reconciled before any resend.
	BeginForemanInvocation(ctx context.Context, parentRunID string) (InvocationIntent, bool, error)
	// ConfirmForemanInvocation records the receiver-accepted receipt under
	// the SAME stable identity and applies the queued → foreman_planning
	// edge. replay is true on an identical repeat.
	ConfirmForemanInvocation(ctx context.Context, parentRunID string, receipt execution.ForemanReceipt) (ParentRecord, bool, error)
	// PrepareStepAttempt persists exactly one attempt at dispatch
	// preparation after every fail-closed check. replay is true on an
	// identical repeat of the same derived idempotency key.
	PrepareStepAttempt(ctx context.Context, prep AttemptPreparation) (AttemptRecord, bool, error)
	// AppendEvidence appends immutable, idempotent attempt evidence.
	AppendEvidence(ctx context.Context, ev EvidenceRecord) (EvidenceRecord, bool, error)
	// MarkStartUncertain applies the executing → uncertain edge.
	MarkStartUncertain(ctx context.Context, req UncertainRequest) (ParentRecord, error)
	// BeginReconciliation enqueues one reconciliation first and applies the
	// uncertain → reconciling edge. replay is true on an identical repeat.
	BeginReconciliation(ctx context.Context, req ReconcileRequest) (ParentRecord, bool, error)
	// CompleteReconciliation records the proven result and applies the
	// reconciling → executing edge. It can never manufacture settled.
	CompleteReconciliation(ctx context.Context, req ReconcileOutcome) (ParentRecord, error)
	// SettleAttempt settles one attempt on complete settlement evidence.
	SettleAttempt(ctx context.Context, req AttemptSettlementRequest) (AttemptRecord, error)
	// RecordForemanReport applies the executing → verifying edge on the
	// foreman's verified parent result.
	RecordForemanReport(ctx context.Context, report execution.ForemanReport) (ParentRecord, error)
	// RequestCancellation applies the cancelling edge and returns only the
	// active attempts to fan out to.
	RequestCancellation(ctx context.Context, parentRunID, reason string) (CancelFanout, error)
	// SettleParent settles the parent on complete settlement evidence and
	// queues the DuckBrain projection reference.
	SettleParent(ctx context.Context, req ParentSettlementRequest) (ParentRecord, error)
}
