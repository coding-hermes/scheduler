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

// IsPreStartAttemptState reports whether an attempt is still awaiting native
// start evidence — the only window in which a start timeout can make the
// parent uncertain.
func IsPreStartAttemptState(state string) bool {
	switch state {
	case AttemptStateSubmitted, AttemptStateWorkspaceReady, AttemptStateDispatched:
		return true
	}
	return false
}

// attemptProgressRank is the closed monotonic ordering of the active attempt
// vocabulary. A proposal may move an active attempt forward (rank to >= from)
// or stop it in a terminal state; nothing ever moves backward and nothing
// leaves a terminal state.
var attemptProgressRank = map[string]int{
	AttemptStateSubmitted:          1,
	AttemptStateWorkspaceReady:     2,
	AttemptStateDispatched:         3,
	AttemptStateWorking:            4,
	AttemptStateInputRequired:      4,
	AttemptStateAuthRequired:       4,
	AttemptStateBlocked:            4,
	AttemptStateConnectorCompleted: 5,
	AttemptStateVerifying:          6,
}

// CanTransitionAttempt reports whether from → to is a legal attempt edge under
// the closed monotonic rules: active attempts only move forward or stop, and
// terminal attempts never regress or resurrect.
func CanTransitionAttempt(from, to string) error {
	if !IsActiveAttemptState(from) && !IsTerminalAttemptState(from) {
		return fmt.Errorf("%w: unknown source state %q", ErrAttemptTransition, from)
	}
	if !IsActiveAttemptState(to) && !IsTerminalAttemptState(to) {
		return fmt.Errorf("%w: unknown target state %q", ErrAttemptTransition, to)
	}
	if IsTerminalAttemptState(from) {
		return fmt.Errorf("%w: %q → %q", ErrAttemptTerminal, from, to)
	}
	if IsTerminalAttemptState(to) {
		return nil
	}
	if from == to {
		return nil
	}
	if attemptProgressRank[to] < attemptProgressRank[from] {
		return fmt.Errorf("%w: %q → %q", ErrAttemptTransition, from, to)
	}
	return nil
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

// WorkspaceIdentityFact is the durable workspace identity a lease is bound to.
type WorkspaceIdentityFact struct {
	RepositoryURI string
	BaseRevision  string
	Branch        string
	HostID        string
	WorkspacePath string
}

// LeaseFact is the durable workspace-lease side of the authority check:
// ownership, workspace identity, epoch, state and expiry.
type LeaseFact struct {
	Present    bool
	State      string
	Epoch      int
	ExpiresAt  string // RFC3339Nano
	OwnerRunID string
	// OwnerAttemptID is empty while the lease is unbound to a specific
	// attempt; otherwise it names the one attempt that may write.
	OwnerAttemptID string
	Identity       WorkspaceIdentityFact
}

// DependencyFact is the durable readiness fact of one prerequisite step: its
// latest generation must have stopped quiescently in the required outcome and,
// when policy demands it, carried a passing independent verification.
type DependencyFact struct {
	StepID string
	// Present is true when a quiescent terminal attempt exists for the step.
	Present bool
	// State is the terminal state of the prerequisite's latest generation.
	State string
	// VerifiedPass is true when durable independent verification passed.
	VerifiedPass bool
}

// AdmissionFacts are the durable facts the admission gate consults. The store
// gathers them inside the admitting transaction so the checks cannot race the
// insert.
type AdmissionFacts struct {
	// AttemptID is the proposed attempt identity; lease ownership is checked
	// against it.
	AttemptID         string
	ParentState       ParentState
	MaxActiveAttempts int
	ActiveAttempts    int
	StepGenerations   []StepGenerationFact
	Lease             LeaseFact
	Dependencies      []DependencyFact
}

// Closed dependency policy outcomes. The default (empty) is the strictest:
// a successful terminal result plus a passing independent verification.
const (
	// DependencyOutcomeCompletedVerified requires the prerequisite to be
	// terminal `completed` with durable passing independent verification.
	DependencyOutcomeCompletedVerified = "completed_verified"
	// DependencyOutcomeCompleted is an explicit policy that waives the
	// independent-verification requirement but still demands success.
	DependencyOutcomeCompleted = "completed"
	// DependencyOutcomeTerminal is an explicit policy that admits any
	// terminal outcome of the prerequisite.
	DependencyOutcomeTerminal = "terminal"
)

// ValidateDependencyOutcome reports whether name is one of the closed
// dependency policy outcomes.
func ValidateDependencyOutcome(name string) bool {
	switch name {
	case "", DependencyOutcomeCompletedVerified, DependencyOutcomeCompleted, DependencyOutcomeTerminal:
		return true
	}
	return false
}

// EvaluateAttemptAdmission is the single fail-closed just-in-time gate. An
// attempt may be persisted only when every check passes; the first refusal
// wins and names exactly one sentinel error.
//
// Order matches the authority model: parent executability, dependency
// readiness, step generation freshness, route receipt currency, candidate
// order, tool/connector compatibility, workspace lease authority, then
// concurrency.
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
	if err := evaluateDependencies(intent.Dependencies, facts.Dependencies); err != nil {
		return err
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
	if err := evaluateLease(now, intent, facts); err != nil {
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

func evaluateDependencies(required []StepDependency, facts []DependencyFact) error {
	byStep := map[string]DependencyFact{}
	for _, f := range facts {
		byStep[f.StepID] = f
	}
	for _, dep := range required {
		policy := dep.RequiredOutcome
		if policy == "" {
			policy = DependencyOutcomeCompletedVerified
		}
		fact, ok := byStep[dep.StepID]
		if !ok || !fact.Present {
			return fmt.Errorf("%w: prerequisite %s has no quiescent terminal result", ErrDependencyNotReady, dep.StepID)
		}
		switch policy {
		case DependencyOutcomeCompletedVerified:
			if fact.State != AttemptStateCompleted || !fact.VerifiedPass {
				return fmt.Errorf("%w: prerequisite %s is %s (verified=%v)", ErrDependencyNotReady, dep.StepID, fact.State, fact.VerifiedPass)
			}
		case DependencyOutcomeCompleted:
			if fact.State != AttemptStateCompleted {
				return fmt.Errorf("%w: prerequisite %s is %s", ErrDependencyNotReady, dep.StepID, fact.State)
			}
		case DependencyOutcomeTerminal:
			if !IsTerminalAttemptState(fact.State) {
				return fmt.Errorf("%w: prerequisite %s is %s", ErrDependencyNotReady, dep.StepID, fact.State)
			}
		default:
			return fmt.Errorf("%w: unknown dependency policy %q", ErrDependencyNotReady, policy)
		}
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

func evaluateLease(now time.Time, intent ReadyStepIntent, facts AdmissionFacts) error {
	authority := intent.Lease
	fact := facts.Lease
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
	if fact.OwnerRunID != intent.ParentRunID {
		return fmt.Errorf("%w: lease %s is owned by parent %q", ErrLeaseOwnership, authority.LeaseID, fact.OwnerRunID)
	}
	if fact.OwnerAttemptID != "" && fact.OwnerAttemptID != facts.AttemptID {
		return fmt.Errorf("%w: lease %s is owned by attempt %q", ErrLeaseOwnership, authority.LeaseID, fact.OwnerAttemptID)
	}
	if fact.Identity != (WorkspaceIdentityFact{
		RepositoryURI: authority.RepositoryURI,
		BaseRevision:  authority.BaseRevision,
		Branch:        authority.Branch,
		HostID:        authority.HostID,
		WorkspacePath: authority.WorkspacePath,
	}) {
		return fmt.Errorf("%w: lease %s workspace identity mismatch", ErrLeaseOwnership, authority.LeaseID)
	}
	return nil
}
