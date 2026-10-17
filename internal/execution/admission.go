package execution

import (
	"fmt"
	"time"
)

// AttemptState classification mirrors the landed execution_attempts vocabulary
// (schema v69). Active states are live work; every other recorded state is
// terminal.
const (
	AttemptStateSubmitted          = "submitted"
	AttemptStateWorkspaceReady     = "workspace_ready"
	AttemptStateDispatched         = "dispatched"
	AttemptStateWorking            = "working"
	AttemptStateInputRequired      = "input_required"
	AttemptStateAuthRequired       = "auth_required"
	AttemptStateConnectorCompleted = "connector_completed"
	AttemptStateVerifying          = "verifying"
	AttemptStateBlocked            = "blocked"
	AttemptStateCompleted          = "completed"
	AttemptStateFailed             = "failed"
	AttemptStateCanceled           = "canceled"
	AttemptStateRejected           = "rejected"
	AttemptStateExpired            = "expired"
)

// IsActiveAttemptState reports whether an attempt in state is live.
func IsActiveAttemptState(state string) bool {
	switch state {
	case AttemptStateSubmitted, AttemptStateWorkspaceReady, AttemptStateDispatched,
		AttemptStateWorking, AttemptStateInputRequired, AttemptStateAuthRequired,
		AttemptStateConnectorCompleted, AttemptStateVerifying, AttemptStateBlocked:
		return true
	}
	return false
}

// IsTerminalAttemptState reports whether an attempt in state has stopped.
func IsTerminalAttemptState(state string) bool {
	switch state {
	case AttemptStateCompleted, AttemptStateFailed, AttemptStateCanceled,
		AttemptStateRejected, AttemptStateExpired:
		return true
	}
	return false
}

// StepGenerationFact is one recorded generation of one step.
type StepGenerationFact struct {
	Generation int
	// Active is true while the attempt row is live.
	Active bool
	// Quiescent is true once the generation is terminal AND either connector
	// settlement landed or reconciliation proved the attempt never started.
	Quiescent bool
}

// LeaseFact is the durable workspace-lease side of the authority check.
type LeaseFact struct {
	Present   bool
	State     string
	Epoch     int
	ExpiresAt string // RFC3339Nano
}

// AdmissionFacts are the durable facts the admission gate consults. The store
// gathers them inside the admitting transaction so the checks cannot race the
// insert.
type AdmissionFacts struct {
	ParentState       ParentState
	MaxActiveAttempts int
	ActiveAttempts    int
	StepGenerations   []StepGenerationFact
	Lease             LeaseFact
}

// EvaluateAttemptAdmission is the single fail-closed just-in-time gate. An
// attempt may be persisted only when every check passes; the first refusal
// wins and names exactly one sentinel error.
//
// Order matches the authority model: parent executability, step generation
// freshness, route receipt currency, candidate order, tool/connector
// compatibility, workspace lease authority, then concurrency.
func EvaluateAttemptAdmission(now time.Time, intent ReadyStepIntent, facts AdmissionFacts) error {
	if err := ValidateReadyStepIntent(intent); err != nil {
		return err
	}
	switch facts.ParentState {
	case ParentQueued:
		return ErrForemanNotInvoked
	case ParentCancelling:
		return ErrParentCancelling
	case ParentSettled:
		return ErrParentSettled
	case ParentUncertain, ParentReconciling:
		return ErrReconciliationRequired
	case ParentForemanPlanning, ParentExecuting:
	default:
		return fmt.Errorf("%w: parent state %q", ErrParentNotExecutable, facts.ParentState)
	}
	if err := evaluateStepGeneration(intent, facts.StepGenerations); err != nil {
		return err
	}
	if err := evaluateRoute(now, intent.Route); err != nil {
		return err
	}
	if err := evaluateCandidateOrder(intent); err != nil {
		return err
	}
	if err := evaluateConnector(intent); err != nil {
		return err
	}
	if err := evaluateLease(now, intent.Lease, facts.Lease); err != nil {
		return err
	}
	maxActive := facts.MaxActiveAttempts
	if maxActive < 1 {
		maxActive = 1
	}
	if facts.ActiveAttempts >= maxActive {
		return fmt.Errorf("%w: %d active of %d allowed", ErrTooManyActiveAttempts, facts.ActiveAttempts, maxActive)
	}
	return nil
}

func evaluateStepGeneration(intent ReadyStepIntent, prior []StepGenerationFact) error {
	maxPrior := 0
	priorFact := StepGenerationFact{}
	for _, g := range prior {
		if g.Generation == intent.StepGeneration {
			return fmt.Errorf("%w: (step %s, generation %d)", ErrStepGenerationConflict, intent.StepID, intent.StepGeneration)
		}
		if g.Generation > maxPrior {
			maxPrior, priorFact = g.Generation, g
		}
	}
	if maxPrior == 0 {
		if intent.StepGeneration != 1 {
			return fmt.Errorf("%w: first generation must be 1, got %d", ErrGenerationNotIncremented, intent.StepGeneration)
		}
		return nil
	}
	if priorFact.Active {
		return fmt.Errorf("%w: generation %d", ErrStepGenerationActive, maxPrior)
	}
	if !priorFact.Quiescent {
		return fmt.Errorf("%w: generation %d", ErrStepGenerationUnsettled, maxPrior)
	}
	if intent.StepGeneration != maxPrior+1 {
		return fmt.Errorf("%w: %d follows %d", ErrGenerationNotIncremented, intent.StepGeneration, maxPrior)
	}
	return nil
}

func evaluateRoute(now time.Time, r RouteReceipt) error {
	if err := VerifyRouteReceiptDigest(r); err != nil {
		return err
	}
	resolved, _ := time.Parse(time.RFC3339Nano, r.ResolvedAt)
	validUntil, _ := time.Parse(time.RFC3339Nano, r.ValidUntil)
	if validUntil.Before(resolved) || validUntil.Equal(resolved) {
		return fmt.Errorf("%w: valid_until must follow resolved_at", ErrRouteReceiptInvalid)
	}
	if now.Before(resolved) {
		return fmt.Errorf("%w: resolved_at is in the future", ErrRouteReceiptInvalid)
	}
	if !now.Before(validUntil) {
		return fmt.Errorf("%w: valid_until %s", ErrRouteReceiptExpired, r.ValidUntil)
	}
	return nil
}

func evaluateCandidateOrder(intent ReadyStepIntent) error {
	rank := intent.Choice.CandidateRank
	if rank < 1 || rank > len(intent.Route.Candidates) {
		return fmt.Errorf("%w: chosen rank %d outside 1..%d", ErrCandidateOrder, rank, len(intent.Route.Candidates))
	}
	for _, earlier := range intent.Route.Candidates[:rank-1] {
		if earlier.ExclusionEvidence == "" {
			return fmt.Errorf("%w: candidate %d skipped without exclusion evidence", ErrCandidateOrder, earlier.Rank)
		}
	}
	return nil
}

func evaluateConnector(intent ReadyStepIntent) error {
	chosen := intent.Route.Candidates[intent.Choice.CandidateRank-1]
	if intent.Choice.ConnectorID == "" || intent.Choice.ConnectorVersion == "" {
		return fmt.Errorf("%w: connector identity incomplete", ErrConnectorIncompatible)
	}
	if intent.Choice.CompatibleModel != chosen.Model || intent.Choice.CompatibleProvider != chosen.Provider {
		return fmt.Errorf("%w: connector declares %s/%s, candidate is %s/%s",
			ErrConnectorIncompatible,
			intent.Choice.CompatibleModel, intent.Choice.CompatibleProvider,
			chosen.Model, chosen.Provider)
	}
	declared := map[string]bool{}
	for _, c := range CapabilitySet(intent.Choice.DeclaredCapabilities) {
		declared[c] = true
	}
	for _, need := range CapabilitySet(intent.RequiredCapabilities) {
		if !declared[need] {
			return fmt.Errorf("%w: %s", ErrConnectorCapabilities, need)
		}
	}
	return nil
}

func evaluateLease(now time.Time, authority WorkspaceLeaseAuthority, fact LeaseFact) error {
	if authority.LeaseID == "" || authority.LeaseEpoch < 1 || authority.ProfileID == "" || authority.EnforcementReceiptRef == "" {
		return ErrLeaseAuthorityMissing
	}
	validUntil, _ := time.Parse(time.RFC3339Nano, authority.ValidUntil)
	if !now.Before(validUntil) {
		return fmt.Errorf("%w: lease authority valid_until %s", ErrLeaseAuthorityStale, authority.ValidUntil)
	}
	if !fact.Present {
		return fmt.Errorf("%w: lease %s not recorded", ErrLeaseAuthorityMissing, authority.LeaseID)
	}
	if fact.State != "active" {
		return fmt.Errorf("%w: lease %s state %q", ErrLeaseAuthorityStale, authority.LeaseID, fact.State)
	}
	if fact.Epoch != authority.LeaseEpoch {
		return fmt.Errorf("%w: lease epoch %d, authority claims %d", ErrLeaseAuthorityStale, fact.Epoch, authority.LeaseEpoch)
	}
	expires, _ := time.Parse(time.RFC3339Nano, fact.ExpiresAt)
	if !now.Before(expires) {
		return fmt.Errorf("%w: lease expires_at %s", ErrLeaseAuthorityStale, fact.ExpiresAt)
	}
	return nil
}
