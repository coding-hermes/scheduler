// Package connector defines the harness-neutral boundary between the scheduler
// execution plane and native coding-harness sidecars.
package connector

import (
	"context"
	"errors"
	"time"
)

// Capability declares a native lifecycle operation that a connector can prove.
type Capability string

const (
	CapabilityEvents       Capability = "events"
	CapabilitySteer        Capability = "steer"
	CapabilityCancel       Capability = "cancel"
	CapabilityAttachResume Capability = "attach_resume"
	CapabilityQuiescence   Capability = "quiescence"
)

// Correlation binds every connector exchange to scheduler-owned durable identity.
type Correlation struct {
	RunID                  string    `json:"run_id"`
	Generation             int       `json:"generation"`
	AttemptKey             string    `json:"attempt_key"`
	AttemptID              string    `json:"attempt_id"`
	RouteDigest            string    `json:"route_digest"`
	WorkspaceLeaseID       string    `json:"workspace_lease_id"`
	WorkspaceLeaseEpoch    int       `json:"workspace_lease_epoch"`
	ConnectorProfileID     string    `json:"connector_profile_id"`
	ConnectorProfileDigest string    `json:"connector_profile_digest"`
	Requested              Candidate `json:"requested_candidate"`
	NativeSessionID        string    `json:"native_session_id,omitempty"`
	NativeTurnID           string    `json:"native_turn_id,omitempty"`
}

// Candidate is a requested model/provider pair, kept distinct from actual service.
type Candidate struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// ActualIdentity is native evidence of the model/provider that served the attempt.
type ActualIdentity struct {
	Model          string    `json:"model"`
	Provider       string    `json:"provider"`
	EvidenceSource string    `json:"evidence_source"`
	ObservedAt     time.Time `json:"observed_at"`
}

// Capabilities explicitly records supported optional lifecycle operations.
type Capabilities struct{ Events, Steer, Cancel, AttachResume, Quiescence bool }

// Description identifies and describes a connector implementation.
type Description struct {
	ID, Version, NativeProtocol string
	Capabilities                Capabilities
}

// ExecutionBrief contains only execution-relevant authored intent, never hidden reasoning.
type ExecutionBrief struct {
	Goal        string   `json:"goal"`
	Rationale   string   `json:"rationale,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	Acceptance  []string `json:"acceptance,omitempty"`
	Proof       []string `json:"proof,omitempty"`
}

// PreflightRequest asks whether the correlated candidate can be started.
type PreflightRequest struct{ Correlation Correlation }

// PreflightReceipt records native readiness without exposing authentication material.
type PreflightReceipt struct {
	Correlation                          Correlation
	Ready, Authenticated, ModelAvailable bool
	ModelsChecked                        int
}

// StartRequest starts a native attempt in the exact leased workspace.
type StartRequest struct {
	Correlation   Correlation
	WorkspacePath string
	Brief         ExecutionBrief
}

// StartResult records native IDs and initial actual identity; it is not completion authority.
type StartResult struct {
	Correlation Correlation
	Actual      ActualIdentity
}

// EventKind is a bounded connector event classification.
type EventKind string

const (
	EventStarted        EventKind = "started"
	EventActualIdentity EventKind = "actual_identity"
	EventRerouted       EventKind = "model_rerouted"
	EventTerminal       EventKind = "terminal"
	EventNativeState    EventKind = "native_state"
)

// Event is an ordered, monotonically sequenced native observation.
type Event struct {
	Correlation Correlation
	Sequence    uint64
	At          time.Time
	Kind        EventKind
	Status      TerminalStatus
	Actual      *ActualIdentity
	Detail      string
}

// EventStream is a pull-based bounded event stream. Next blocks until an event, error, or cancellation.
type EventStream interface {
	Next(context.Context) (Event, error)
}

// TerminalStatus is native terminal evidence, not independently verified completion.
type TerminalStatus string

const (
	TerminalNone        TerminalStatus = ""
	TerminalCompleted   TerminalStatus = "completed"
	TerminalInterrupted TerminalStatus = "interrupted"
	TerminalFailed      TerminalStatus = "failed"
)

// State is the connector's current native observation.
type State struct {
	Correlation  Correlation
	Actual       ActualIdentity
	Active       bool
	Terminal     TerminalStatus
	ThreadStatus string
	Settled      bool
}

// SettlementReceipt proves matching terminal observation followed by native quiescence.
type SettlementReceipt struct {
	Correlation                              Correlation
	Terminal                                 TerminalStatus
	ThreadStatus                             string
	TerminalObservedAt, QuiescenceObservedAt time.Time
}

// CancellationReceipt proves interruption and supervisor-observed zero descendants.
type CancellationReceipt struct {
	Correlation             Correlation
	Terminal                TerminalStatus
	ZeroDescendants         bool
	InterruptedAt, ProvedAt time.Time
}

// SteerRequest supplies execution-relevant additional direction to an active turn.
type SteerRequest struct {
	Correlation Correlation
	Input       string
}

// SteerReceipt records acceptance by the native harness.
type SteerReceipt struct {
	Correlation Correlation
	Accepted    bool
}

// CancelRequest requests native interruption.
type CancelRequest struct{ Correlation Correlation }

// CollectRequest requests durable references after native execution.
type CollectRequest struct{ Correlation Correlation }

// ArtifactReference points to out-of-band evidence; Data is intentionally absent.
type ArtifactReference struct {
	URI, MediaType, SHA256 string
	Size                   int64
}

// ArtifactBundle contains references and digests only.
type ArtifactBundle struct {
	Correlation    Correlation
	References     []ArtifactReference
	ManifestSHA256 string
}

// DisposeRequest closes native resources without authorizing workspace deletion.
type DisposeRequest struct{ Correlation Correlation }

// ErrorKind classifies connector failures without embedding sensitive payloads.
type ErrorKind string

const (
	ErrorInvalid           ErrorKind = "invalid"
	ErrorUnsupported       ErrorKind = "unsupported"
	ErrorProtocol          ErrorKind = "protocol"
	ErrorTransport         ErrorKind = "transport"
	ErrorTimeout           ErrorKind = "timeout"
	ErrorNotSettled        ErrorKind = "not_settled"
	ErrorDescendantsRemain ErrorKind = "descendants_remain"
)

// Error is a sanitized connector failure.
type Error struct {
	Kind               ErrorKind
	Operation, Message string
	Err                error
}

func (e *Error) Error() string { return "connector " + e.Operation + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

// IsKind reports whether err has the requested connector taxonomy.
func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// Connector is the harness-neutral native lifecycle. Its results are evidence;
// independent verification and durable completion authority remain outside it.
type Connector interface {
	Describe(context.Context) (Description, error)
	Preflight(context.Context, PreflightRequest) (PreflightReceipt, error)
	Start(context.Context, StartRequest) (StartResult, error)
	Events(context.Context, Correlation) (EventStream, error)
	Get(context.Context, Correlation) (State, error)
	WaitSettled(context.Context, Correlation) (SettlementReceipt, error)
	Steer(context.Context, SteerRequest) (SteerReceipt, error)
	Cancel(context.Context, CancelRequest) (CancellationReceipt, error)
	Collect(context.Context, CollectRequest) (ArtifactBundle, error)
	Dispose(context.Context, DisposeRequest) error
}
