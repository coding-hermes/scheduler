package execution

import (
	"encoding/json"
	"fmt"
	"time"
)

// ParentState is the closed scheduler parent-run lifecycle from the
// 2026-10-16 Hermes foreman order flow:
//
//	queued → foreman_planning → executing → verifying → settled
//	                          ↘ cancelling → settled
//	                          ↘ uncertain → reconciling → executing|settled
//
// Only one parent row is pending per admitted run. Planned and blocked
// foreman steps are never parent or attempt state.
type ParentState string

// The closed parent lifecycle vocabulary. No other value is a legal
// ParentState.
const (
	ParentQueued          ParentState = "queued"
	ParentForemanPlanning ParentState = "foreman_planning"
	ParentExecuting       ParentState = "executing"
	ParentVerifying       ParentState = "verifying"
	ParentSettled         ParentState = "settled"
	ParentCancelling      ParentState = "cancelling"
	ParentUncertain       ParentState = "uncertain"
	ParentReconciling     ParentState = "reconciling"
)

// parentEdges is the closed legal-edge table. Cancelling is enterable from
// every non-terminal state because cancellation is parent-authoritative;
// uncertain is enterable only from executing because a start timeout can only
// occur while a dispatched attempt is live.
var parentEdges = map[ParentState][]ParentState{
	ParentQueued:          {ParentForemanPlanning, ParentCancelling},
	ParentForemanPlanning: {ParentExecuting, ParentCancelling},
	ParentExecuting:       {ParentVerifying, ParentCancelling, ParentUncertain},
	ParentVerifying:       {ParentSettled, ParentCancelling},
	ParentUncertain:       {ParentReconciling, ParentCancelling},
	ParentReconciling:     {ParentExecuting, ParentSettled, ParentCancelling},
	ParentCancelling:      {ParentSettled},
	ParentSettled:         nil,
}

// Valid reports whether s is a member of the closed parent vocabulary.
func (s ParentState) Valid() bool {
	_, ok := parentEdges[s]
	return ok
}

// Terminal reports whether s admits no further transition.
func (s ParentState) Terminal() bool {
	return s == ParentSettled
}

// ExecutableForAttempt reports whether a just-in-time child attempt may be
// admitted in s. Exactly the working states qualify: the foreman has the
// parent (foreman_planning) or is running steps (executing). Queued means the
// foreman was never invoked; cancelling, settled, uncertain and reconciling
// refuse new generations until their own resolutions land.
func (s ParentState) ExecutableForAttempt() bool {
	return s == ParentForemanPlanning || s == ParentExecuting
}

// CanTransitionParent reports whether from → to is a legal parent lifecycle
// edge. Every other pair, self-transitions included, is refused.
func CanTransitionParent(from, to ParentState) error {
	if !from.Valid() {
		return fmt.Errorf("%w: unknown source state %q", ErrParentTransitionIllegal, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: unknown target state %q", ErrParentTransitionIllegal, to)
	}
	if from == to {
		return fmt.Errorf("%w: self transition %q", ErrParentTransitionIllegal, from)
	}
	for _, edge := range parentEdges[from] {
		if edge == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %q → %q", ErrParentTransitionIllegal, from, to)
}

// ParentBudget is the parent deadline/budget ceiling in named integer units.
// Money-like quantities are never floats here.
type ParentBudget struct {
	MaxCostUnits    int64 `json:"max_cost_units"`
	MaxWallTimeoutS int64 `json:"max_wall_timeout_s"`
	MaxTokens       int64 `json:"max_tokens"`
}

// ParentBrief is the canonical scheduler → Hermes parent brief. It carries no
// step decomposition, no speculative child attempts, and no credentials.
type ParentBrief struct {
	SchemaVersion      string                `json:"schema_version"`
	BriefID            string                `json:"brief_id"`
	RevisionID         string                `json:"revision_id"`
	IssuedAt           string                `json:"issued_at"`
	IssuedBy           string                `json:"issued_by"`
	ParentRunID        string                `json:"parent_run_id"`
	Project            string                `json:"project"`
	Lane               string                `json:"lane"`
	TaskID             string                `json:"task_id"`
	TickID             string                `json:"tick_id"`
	Mission            string                `json:"mission"`
	ContextRefs        []ContextRef          `json:"context_refs"`
	Scope              Scope                 `json:"scope"`
	Constraints        []Constraint          `json:"constraints"`
	NonGoals           []string              `json:"non_goals"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
	DeadlineAt         string                `json:"deadline_at"`
	Budget             ParentBudget          `json:"budget"`
	CancellationPolicy string                `json:"cancellation_policy"`
	Provenance         Provenance            `json:"provenance"`
	BriefDigest        string                `json:"brief_digest"`
}

// DecodeParentBrief decodes one canonical parent brief with the same strict
// I-JSON rules as DecodeBrief: no duplicate or unknown members, NFC
// normalization, integers only, no trailing data.
func DecodeParentBrief(data []byte) (*ParentBrief, error) {
	v, err := parseAndNormalize(data)
	if err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized parent brief: %w", err)
	}
	var brief ParentBrief
	if err := strictDecode(normalized, &brief); err != nil {
		return nil, fmt.Errorf("decode parent brief: %w", err)
	}
	return &brief, nil
}

// ValidateParentBriefDigest compares the supplied brief_digest against the
// canonical digest of the parent brief bytes, then checks the closed required
// fields. Agent prose is never consulted. Completeness is fail-closed: scope,
// context, constraints, non-goals, acceptance criteria, budget and provenance
// must all carry real content, not merely hashes and IDs.
func ValidateParentBriefDigest(data []byte) (*ParentBrief, error) {
	if err := ValidateBriefDigest(data); err != nil {
		return nil, fmt.Errorf("parent brief digest: %w", err)
	}
	brief, err := DecodeParentBrief(data)
	if err != nil {
		return nil, err
	}
	if err := ValidateParentBrief(*brief); err != nil {
		return nil, err
	}
	return brief, nil
}

// ValidateParentBrief checks the closed completeness rules of one decoded
// parent brief.
func ValidateParentBrief(brief ParentBrief) error {
	if brief.SchemaVersion == "" || brief.BriefID == "" || brief.RevisionID == "" ||
		brief.ParentRunID == "" || brief.Lane == "" || brief.TaskID == "" ||
		brief.Mission == "" || brief.DeadlineAt == "" || brief.CancellationPolicy == "" {
		return ErrParentBriefIncomplete
	}
	if brief.IssuedAt == "" || brief.IssuedBy == "" || brief.Project == "" {
		return fmt.Errorf("%w: issuance/project identity missing", ErrParentBriefIncomplete)
	}
	if _, err := time.Parse(time.RFC3339Nano, brief.IssuedAt); err != nil {
		return fmt.Errorf("%w: issued_at: %v", ErrParentBriefIncomplete, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, brief.DeadlineAt); err != nil {
		return fmt.Errorf("%w: deadline_at: %v", ErrParentBriefIncomplete, err)
	}
	if len(brief.ContextRefs) == 0 {
		return fmt.Errorf("%w: context references missing", ErrParentBriefIncomplete)
	}
	for _, ref := range brief.ContextRefs {
		if ref.Namespace == "" || ref.KeyOrID == "" || ref.RetrievedAt == "" || ref.Authority == "" {
			return fmt.Errorf("%w: context reference incomplete", ErrParentBriefIncomplete)
		}
	}
	if len(brief.Scope.AllowedPaths) == 0 && len(brief.Scope.ForbiddenPaths) == 0 {
		return fmt.Errorf("%w: scope is empty", ErrParentBriefIncomplete)
	}
	if len(brief.Constraints) == 0 {
		return fmt.Errorf("%w: constraints missing", ErrParentBriefIncomplete)
	}
	for _, c := range brief.Constraints {
		if c.ID == "" || c.Level == "" || c.Statement == "" {
			return fmt.Errorf("%w: constraint incomplete", ErrParentBriefIncomplete)
		}
	}
	if len(brief.NonGoals) == 0 {
		return fmt.Errorf("%w: non-goals missing", ErrParentBriefIncomplete)
	}
	for _, ng := range brief.NonGoals {
		if ng == "" {
			return fmt.Errorf("%w: empty non-goal", ErrParentBriefIncomplete)
		}
	}
	if len(brief.AcceptanceCriteria) == 0 {
		return fmt.Errorf("%w: acceptance criteria missing", ErrParentBriefIncomplete)
	}
	for _, ac := range brief.AcceptanceCriteria {
		if ac.ID == "" || ac.Statement == "" {
			return fmt.Errorf("%w: acceptance criterion incomplete", ErrParentBriefIncomplete)
		}
		if ac.Required && len(ac.ProofRequirements) == 0 {
			return fmt.Errorf("%w: acceptance criterion %s has no proof requirements", ErrParentBriefIncomplete, ac.ID)
		}
		for _, p := range ac.ProofRequirements {
			if p.ID == "" || p.Kind == "" || len(p.Argv) == 0 || p.TimeoutS < 1 {
				return fmt.Errorf("%w: proof requirement %s incomplete", ErrParentBriefIncomplete, p.ID)
			}
		}
	}
	if brief.Budget.MaxCostUnits < 1 || brief.Budget.MaxWallTimeoutS < 1 || brief.Budget.MaxTokens < 0 {
		return fmt.Errorf("%w: budget has no substance", ErrParentBriefIncomplete)
	}
	if brief.Provenance.ForemanRunID == "" || brief.Provenance.BoardRevision == "" {
		return fmt.Errorf("%w: provenance missing", ErrParentBriefIncomplete)
	}
	return nil
}
