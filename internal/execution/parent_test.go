package execution

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestParentLifecycleClosedEdges pins the closed parent lifecycle exactly as
// the 2026-10-16 order flow defines it. Any edge drift is a design change and
// must be deliberate.
func TestParentLifecycleClosedEdges(t *testing.T) {
	legal := map[[2]ParentState]bool{
		{ParentQueued, ParentForemanPlanning}:     true,
		{ParentQueued, ParentCancelling}:          true,
		{ParentForemanPlanning, ParentExecuting}:  true,
		{ParentForemanPlanning, ParentCancelling}: true,
		{ParentExecuting, ParentVerifying}:        true,
		{ParentExecuting, ParentCancelling}:       true,
		{ParentExecuting, ParentUncertain}:        true,
		{ParentVerifying, ParentSettled}:          true,
		{ParentVerifying, ParentCancelling}:       true,
		{ParentUncertain, ParentReconciling}:      true,
		{ParentUncertain, ParentCancelling}:       true,
		{ParentReconciling, ParentExecuting}:      true,
		{ParentReconciling, ParentSettled}:        true,
		{ParentReconciling, ParentCancelling}:     true,
		{ParentCancelling, ParentSettled}:         true,
	}
	states := []ParentState{
		ParentQueued, ParentForemanPlanning, ParentExecuting, ParentVerifying,
		ParentSettled, ParentCancelling, ParentUncertain, ParentReconciling,
	}
	for _, from := range states {
		for _, to := range states {
			err := CanTransitionParent(from, to)
			if legal[[2]ParentState{from, to}] {
				if err != nil {
					t.Errorf("%q → %q refused: %v", from, to, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%q → %q unexpectedly legal", from, to)
			}
		}
	}
	if err := CanTransitionParent(ParentState("bogus"), ParentExecuting); !errors.Is(err, ErrParentTransitionIllegal) {
		t.Fatalf("unknown source error=%v", err)
	}
	if err := CanTransitionParent(ParentExecuting, ParentState("bogus")); !errors.Is(err, ErrParentTransitionIllegal) {
		t.Fatalf("unknown target error=%v", err)
	}
}

func TestParentStateClassification(t *testing.T) {
	for _, s := range []ParentState{ParentQueued, ParentForemanPlanning, ParentExecuting, ParentVerifying, ParentCancelling, ParentUncertain, ParentReconciling} {
		if s.Terminal() {
			t.Errorf("%q must not be terminal", s)
		}
	}
	if !ParentSettled.Terminal() {
		t.Error("settled must be terminal")
	}
	for _, s := range []ParentState{ParentForemanPlanning, ParentExecuting} {
		if !s.ExecutableForAttempt() {
			t.Errorf("%q must be executable for attempts", s)
		}
	}
	for _, s := range []ParentState{ParentQueued, ParentVerifying, ParentSettled, ParentCancelling, ParentUncertain, ParentReconciling} {
		if s.ExecutableForAttempt() {
			t.Errorf("%q must refuse attempt admission", s)
		}
	}
	if ParentState("nope").Valid() {
		t.Error("unknown state must not be valid")
	}
}

func mustParentBriefJSON(t *testing.T, parentRunID string) ([]byte, string) {
	t.Helper()
	brief := ParentBrief{
		SchemaVersion: "parent-brief/v1",
		BriefID:       "brief-1",
		RevisionID:    "rev-1",
		IssuedAt:      "2026-10-15T11:00:00Z",
		IssuedBy:      "scheduler:test",
		ParentRunID:   parentRunID,
		Project:       "coding-hermes-scheduler",
		Lane:          "acp-sched",
		TaskID:        "ACP-SCHED-001",
		TickID:        "tick-1",
		Mission:       "land the parent-run seam",
		ContextRefs:   []ContextRef{{Namespace: "goals", KeyOrID: "AUTO-CODE-001", RevisionOrDigest: "sha256:abc", RetrievedAt: "2026-10-15T11:00:00Z", Authority: "goal"}},
		Scope:         Scope{AllowedPaths: []string{"internal/**"}, ForbiddenPaths: []string{"**/secrets/**"}},
		Constraints:   []Constraint{{ID: "c1", Level: "hard", Statement: "no credentials in payloads"}},
		NonGoals:      []string{"no live wiring"},
		AcceptanceCriteria: []AcceptanceCriterion{{
			ID: "ac1", Statement: "tests pass", Required: true,
			ProofRequirements: []ProofRequirement{{ID: "p1", Kind: "test", Argv: []string{"go", "test", "./..."}, TimeoutS: 600, ExpectExit: 0}},
		}},
		DeadlineAt:         "2026-10-15T18:00:00Z",
		Budget:             ParentBudget{MaxCostUnits: 100, MaxWallTimeoutS: 21600, MaxTokens: 1000000},
		CancellationPolicy: "cooperative: fan out to active attempts and wait for quiescence",
		Provenance:         Provenance{ForemanRunID: "foreman-run-1", BoardRevision: "board-1"},
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := BriefDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	brief.BriefDigest = digest
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	return data, digest
}

func TestValidateParentBriefDigest(t *testing.T) {
	data, digest := mustParentBriefJSON(t, "parent-1")
	brief, err := ValidateParentBriefDigest(data)
	if err != nil {
		t.Fatal(err)
	}
	if brief.BriefDigest != digest || brief.ParentRunID != "parent-1" {
		t.Fatalf("brief=%+v", brief)
	}

	tampered := make([]byte, len(data))
	copy(tampered, data)
	tampered[len(tampered)-3] ^= 0x21 // one hex character of brief_digest
	if _, err := ValidateParentBriefDigest(tampered); err == nil {
		t.Fatal("tampered brief digest unexpectedly validated")
	}

	incomplete := ParentBrief{SchemaVersion: "parent-brief/v1", ParentRunID: "parent-1"}
	raw, _ := json.Marshal(incomplete)
	d, err := BriefDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	incomplete.BriefDigest = d
	raw, _ = json.Marshal(incomplete)
	if _, err := ValidateParentBriefDigest(raw); !errors.Is(err, ErrParentBriefIncomplete) {
		t.Fatalf("incomplete brief error=%v", err)
	}
}
