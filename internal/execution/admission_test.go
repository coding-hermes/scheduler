package execution

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var gateNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// stepBriefDigest is a canonical execution-brief digest produced by the
// landed BriefDigest machinery — the shape a ready-step intent must carry.
var stepBriefDigest = mustTestDigest(`{"schema_version":"1.0","brief_id":"step-brief-1","revision_id":"rev-1","task_id":"ACP-SCHED-001"}`)

func mustTestDigest(raw string) string {
	d, err := BriefDigest([]byte(raw))
	if err != nil {
		panic(err)
	}
	return d
}

func mustRouteReceipt(t *testing.T, mutators ...func(*RouteReceipt)) RouteReceipt {
	t.Helper()
	r := RouteReceipt{
		Candidates: []RouteCandidate{
			{Rank: 1, Model: "mimo-v2.6-pro", Provider: "opencode-go", BaseURL: "https://router.invalid"},
			{Rank: 2, Model: "longcat-2.0", Provider: "opencode-go", BaseURL: "https://router.invalid"},
		},
		ResolvedAt: gateNow.Add(-time.Minute).Format(time.RFC3339Nano),
		ValidUntil: gateNow.Add(5 * time.Minute).Format(time.RFC3339Nano),
	}
	for _, m := range mutators {
		m(&r)
	}
	digest, err := RouteReceiptDigest(r)
	if err != nil {
		t.Fatal(err)
	}
	r.ReceiptDigest = digest
	return r
}

func readyIntent(parentRunID, briefDigest string, route RouteReceipt) ReadyStepIntent {
	candidates := make([]RouteCandidate, len(route.Candidates))
	copy(candidates, route.Candidates)
	route.Candidates = candidates
	return ReadyStepIntent{
		ParentRunID:          parentRunID,
		StepID:               "step-1",
		StepGeneration:       1,
		BriefDigest:          briefDigest,
		ContextRefs:          []ContextRef{{Namespace: "goals", KeyOrID: "goal-1", RetrievedAt: "2026-10-15T11:00:00Z", Authority: "goal"}},
		RequiredCapabilities: []string{"code_gen", "git"},
		Route:                route,
		Choice: ToolChoice{
			CandidateRank:        1,
			ConnectorID:          "codex-app-server",
			ConnectorVersion:     "1.0",
			CompatibleModel:      "mimo-v2.6-pro",
			CompatibleProvider:   "opencode-go",
			DeclaredCapabilities: []string{"code_gen", "git", "test"},
		},
		Lease: WorkspaceLeaseAuthority{
			LeaseID:               "lease-1",
			LeaseEpoch:            1,
			ProfileID:             "bunker-profile-1",
			EnforcementReceiptRef: "enforcement-receipt-1",
			VerifiedAt:            gateNow.Add(-time.Minute).Format(time.RFC3339Nano),
			ValidUntil:            gateNow.Add(time.Hour).Format(time.RFC3339Nano),
			RepositoryURI:         "https://example.invalid/repo.git",
			BaseRevision:          "base",
			Branch:                "work",
			HostID:                "bunker-1",
			WorkspacePath:         "/workspace/lease-1",
		},
		Policy: AttemptPolicy{
			StartTimeoutS:      60,
			WallTimeoutS:       3600,
			BudgetUnits:        100,
			CancellationPolicy: "cooperative",
		},
	}
}

func healthyFacts() AdmissionFacts {
	return AdmissionFacts{
		AttemptID:         "attempt-1",
		ParentState:       ParentExecuting,
		MaxActiveAttempts: 0, // default one
		Lease: LeaseFact{
			Present:        true,
			State:          "active",
			Epoch:          1,
			ExpiresAt:      gateNow.Add(time.Hour).Format(time.RFC3339Nano),
			OwnerRunID:     "parent-1",
			OwnerAttemptID: "attempt-1",
			Identity: WorkspaceIdentityFact{
				RepositoryURI: "https://example.invalid/repo.git",
				BaseRevision:  "base",
				Branch:        "work",
				HostID:        "bunker-1",
				WorkspacePath: "/workspace/lease-1",
			},
		},
	}
}

func TestEvaluateAttemptAdmissionAcceptsReadyStep(t *testing.T) {
	intent := readyIntent("parent-1", stepBriefDigest, mustRouteReceipt(t))
	if err := EvaluateAttemptAdmission(gateNow, intent, healthyFacts()); err != nil {
		t.Fatalf("ready step refused: %v", err)
	}
	// The first generation of a second step is equally admissible.
	intent.StepID = "step-2"
	if err := EvaluateAttemptAdmission(gateNow, intent, healthyFacts()); err != nil {
		t.Fatalf("second step refused: %v", err)
	}
}

// TestCandidateBeyondHeadRequiresExclusionEvidence proves the "unless": the
// head candidate must be chosen unless every earlier candidate carries
// explicit exclusion evidence.
func TestCandidateBeyondHeadRequiresExclusionEvidence(t *testing.T) {
	route := mustRouteReceipt(t, func(r *RouteReceipt) {
		r.Candidates[0].ExclusionEvidence = "quota-exhausted:provider-quota=0"
	})
	intent := readyIntent("parent-1", stepBriefDigest, route)
	intent.Choice.CandidateRank = 2
	intent.Choice.CompatibleModel = "longcat-2.0"
	intent.Choice.CompatibleProvider = "opencode-go"
	if err := EvaluateAttemptAdmission(gateNow, intent, healthyFacts()); err != nil {
		t.Fatalf("excluded head candidate refused: %v", err)
	}
}

func TestDeriveAttemptIdempotencyKeyIsDeterministic(t *testing.T) {
	key := DeriveAttemptIdempotencyKey("parent-1", "step-1", 1, "sha256:route", 1)
	if key == "" || key == DeriveAttemptIdempotencyKey("parent-1", "step-1", 1, "sha256:route", 2) {
		t.Fatal("key must bind the lease epoch")
	}
	for _, other := range []string{
		DeriveAttemptIdempotencyKey("parent-2", "step-1", 1, "sha256:route", 1),
		DeriveAttemptIdempotencyKey("parent-1", "step-2", 1, "sha256:route", 1),
		DeriveAttemptIdempotencyKey("parent-1", "step-1", 2, "sha256:route", 1),
		DeriveAttemptIdempotencyKey("parent-1", "step-1", 1, "sha256:other", 1),
	} {
		if other == key {
			t.Fatal("key must bind parent/step/generation/route digest/lease epoch")
		}
	}
	if key != DeriveAttemptIdempotencyKey("parent-1", "step-1", 1, "sha256:route", 1) {
		t.Fatal("key derivation is not deterministic")
	}
}

// TestAttemptAdmissionRefusals proves every fail-closed refusal of the
// just-in-time gate, one sentinel error per rule.
func TestAttemptAdmissionRefusals(t *testing.T) {
	goodRoute := mustRouteReceipt(t)
	cases := []struct {
		name string
		now  time.Time
		mut  func(*ReadyStepIntent, *AdmissionFacts)
		want error
	}{
		{
			name: "attempt before foreman ready-step intent",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { *i = ReadyStepIntent{} },
			want: ErrNoReadyStepIntent,
		},
		{
			name: "parent never reached the foreman",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { f.ParentState = ParentQueued },
			want: ErrForemanNotInvoked,
		},
		{
			name: "parent cancelling",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { f.ParentState = ParentCancelling },
			want: ErrParentCancelling,
		},
		{
			name: "parent settled",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { f.ParentState = ParentSettled },
			want: ErrParentSettled,
		},
		{
			name: "retry while start is uncertain",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { f.ParentState = ParentUncertain },
			want: ErrReconciliationRequired,
		},
		{
			name: "retry while reconciliation is open",
			mut:  func(i *ReadyStepIntent, f *AdmissionFacts) { f.ParentState = ParentReconciling },
			want: ErrReconciliationRequired,
		},
		{
			name: "step generation already active or settled",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				f.StepGenerations = []StepGenerationFact{{Generation: 1, Quiescent: true}}
			},
			want: ErrStepGenerationConflict,
		},
		{
			name: "prior step generation still active",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.StepGeneration = 2
				f.StepGenerations = []StepGenerationFact{{Generation: 1, Active: true}}
			},
			want: ErrStepGenerationActive,
		},
		{
			name: "prior step generation not quiescent",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.StepGeneration = 2
				f.StepGenerations = []StepGenerationFact{{Generation: 1}}
			},
			want: ErrStepGenerationUnsettled,
		},
		{
			name: "generation gap is not a bounded retry",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.StepGeneration = 3
				f.StepGenerations = []StepGenerationFact{{Generation: 1, Quiescent: true}}
			},
			want: ErrGenerationNotIncremented,
		},
		{
			name: "missing route receipt",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Route.ReceiptDigest = ""
			},
			want: ErrRouteReceiptMissing,
		},
		{
			name: "tampered route receipt",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Route.Candidates[0].Model = "tampered-model"
			},
			want: ErrRouteReceiptTampered,
		},
		{
			name: "expired route receipt",
			now:  gateNow.Add(10 * time.Minute),
			want: ErrRouteReceiptExpired,
		},
		{
			name: "candidate order violation",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Choice.CandidateRank = 2
				i.Choice.CompatibleModel = "longcat-2.0"
			},
			want: ErrCandidateOrder,
		},
		{
			name: "chosen candidate rank out of range",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Choice.CandidateRank = 7
			},
			want: ErrCandidateOrder,
		},
		{
			name: "incompatible tool",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Choice.CompatibleModel = "other-model"
			},
			want: ErrConnectorIncompatible,
		},
		{
			name: "missing connector capabilities",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Choice.DeclaredCapabilities = []string{"code_gen"}
			},
			want: ErrConnectorCapabilities,
		},
		{
			name: "missing lease authority",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.Lease.EnforcementReceiptRef = ""
			},
			want: ErrLeaseAuthorityMissing,
		},
		{
			name: "stale lease authority",
			now:  gateNow.Add(2 * time.Hour),
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				// Keep the route receipt fresh so only the lease is stale.
				i.Route = mustRouteReceipt(t, func(r *RouteReceipt) {
					r.ResolvedAt = gateNow.Format(time.RFC3339Nano)
					r.ValidUntil = gateNow.Add(24 * time.Hour).Format(time.RFC3339Nano)
				})
				i.Lease.ValidUntil = gateNow.Add(time.Hour).Format(time.RFC3339Nano)
			},
			want: ErrLeaseAuthorityStale,
		},
		{
			name: "lease not recorded durably",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				f.Lease.Present = false
			},
			want: ErrLeaseAuthorityMissing,
		},
		{
			name: "lease revoked",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				f.Lease.State = "revoked"
			},
			want: ErrLeaseAuthorityStale,
		},
		{
			name: "lease epoch drift",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				f.Lease.Epoch = 2
			},
			want: ErrLeaseAuthorityStale,
		},
		{
			name: "second active attempt under default concurrency",
			mut: func(i *ReadyStepIntent, f *AdmissionFacts) {
				i.StepID = "step-2"
				f.ActiveAttempts = 1
			},
			want: ErrTooManyActiveAttempts,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := gateNow
			if tc.now != (time.Time{}) {
				now = tc.now
			}
			intent := readyIntent("parent-1", stepBriefDigest, goodRoute)
			facts := healthyFacts()
			if tc.mut != nil {
				tc.mut(&intent, &facts)
			}
			err := EvaluateAttemptAdmission(now, intent, facts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestExplicitBoundedParallelismAllowsSecondAttempt proves parallelism is an
// explicit bounded parent policy, never the default.
func TestExplicitBoundedParallelismAllowsSecondAttempt(t *testing.T) {
	intent := readyIntent("parent-1", stepBriefDigest, mustRouteReceipt(t))
	intent.StepID = "step-2"
	facts := healthyFacts()
	facts.ActiveAttempts = 1
	facts.MaxActiveAttempts = 2
	if err := EvaluateAttemptAdmission(gateNow, intent, facts); err != nil {
		t.Fatalf("explicit bounded policy refused: %v", err)
	}
	facts.MaxActiveAttempts = 1
	if err := EvaluateAttemptAdmission(gateNow, intent, facts); !errors.Is(err, ErrTooManyActiveAttempts) {
		t.Fatalf("explicit single-attempt policy error=%v", err)
	}
}

func TestReadyStepIntentStructuralRefusals(t *testing.T) {
	base := readyIntent("parent-1", stepBriefDigest, mustRouteReceipt(t))
	cases := []struct {
		name string
		mut  func(*ReadyStepIntent)
		want error
	}{
		{"missing step identity", func(i *ReadyStepIntent) { i.StepID = "" }, ErrReadyStepIntent},
		{"missing brief digest", func(i *ReadyStepIntent) { i.BriefDigest = "" }, ErrReadyStepIntent},
		{"zero generation", func(i *ReadyStepIntent) { i.StepGeneration = 0 }, ErrReadyStepIntent},
		{"missing policy", func(i *ReadyStepIntent) { i.Policy.CancellationPolicy = "" }, ErrReadyStepIntent},
		{"missing lease profile", func(i *ReadyStepIntent) { i.Lease.ProfileID = "" }, ErrLeaseAuthorityMissing},
		{"missing route candidates", func(i *ReadyStepIntent) {
			i.Route.Candidates = nil
			i.Route.ReceiptDigest = "sha256:orphan"
		}, ErrRouteReceiptInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := base
			tc.mut(&intent)
			if err := ValidateReadyStepIntent(intent); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestRouteReceiptDigestTamperDetection(t *testing.T) {
	r := mustRouteReceipt(t)
	if err := VerifyRouteReceiptDigest(r); err != nil {
		t.Fatal(err)
	}
	r.ResolvedAt = gateNow.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if err := VerifyRouteReceiptDigest(r); !errors.Is(err, ErrRouteReceiptTampered) {
		t.Fatalf("error=%v, want tampered", err)
	}
	r2 := mustRouteReceipt(t, func(r *RouteReceipt) {
		r.Candidates[1].ExclusionEvidence = "health-circuit-open"
	})
	if err := VerifyRouteReceiptDigest(r2); err != nil {
		t.Fatalf("recomputed receipt refused: %v", err)
	}
	if r2.ReceiptDigest == r.ReceiptDigest {
		t.Fatal("digest must cover exclusion evidence")
	}
}

func TestEvidenceDigestIsStableAndDiscriminating(t *testing.T) {
	a := EvidenceDigest("reconcile_enqueued", "parent-1", "attempt-1", "1", "reason")
	if a != EvidenceDigest("reconcile_enqueued", "parent-1", "attempt-1", "1", "reason") {
		t.Fatal("evidence digest must be stable")
	}
	if a == EvidenceDigest("reconcile_enqueued", "parent-1", "attempt-1", "1", "other") {
		t.Fatal("evidence digest must discriminate reasons")
	}
	if got := CapabilitySet([]string{"git", "code_gen", "git", ""}); fmt.Sprint(got) != "[code_gen git]" {
		t.Fatalf("capability set=%v", got)
	}
}
