package foreman_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

var seamNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type fakeInvoker struct {
	mu    sync.Mutex
	calls []execution.ParentBrief
	err   error
}

func (f *fakeInvoker) InvokeForeman(_ context.Context, brief execution.ParentBrief) (execution.ForemanReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, brief)
	if f.err != nil {
		return execution.ForemanReceipt{}, f.err
	}
	return execution.ForemanReceipt{
		InvocationID: "invocation-1",
		ParentRunID:  brief.ParentRunID,
		AcceptedAt:   seamNow.Format(time.RFC3339Nano),
	}, nil
}

func (f *fakeInvoker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeProjector struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeProjector) Project(context.Context, execution.DuckBrainProjection) (execution.DuckBrainReadback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return execution.DuckBrainReadback{}, nil
}

type harness struct {
	t               *testing.T
	ctx             context.Context
	dbPath          string
	db              *sql.DB
	store           *foreman.Seam
	durable         *database.ParentRunStore
	invoker         *fakeInvoker
	projector       *fakeProjector
	parentID        string
	briefJSON       []byte
	briefDigest     string
	stepBriefDigest string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler.db")
	h := &harness{t: t, parentID: "parent-1"}
	h.ctx = clock.WithClock(context.Background(), clock.NewFixed(seamNow))
	h.dbPath = dbPath
	h.open()
	t.Cleanup(func() {
		if h.db != nil {
			_ = h.db.Close()
		}
	})
	h.briefJSON, h.briefDigest = mustParentBrief(t, h.parentID)
	h.stepBriefDigest = mustStepBriefDigest(t)
	h.invoker = &fakeInvoker{}
	h.projector = &fakeProjector{}
	h.store = foreman.NewSeam(h.durable, h.invoker, h.projector)
	return h
}

func (h *harness) open() {
	h.t.Helper()
	db, err := database.InitDB(h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.db != nil {
		_ = h.db.Close()
	}
	h.db = db
	h.durable = database.NewParentRunStore(db)
	if h.invoker != nil {
		h.store = foreman.NewSeam(h.durable, h.invoker, h.projector)
	}
}

// restart simulates a daemon restart: the database is closed and reopened on
// the same file.
func (h *harness) restart() {
	h.t.Helper()
	h.open()
}

func (h *harness) admit(maxActive int) foreman.ParentRecord {
	h.t.Helper()
	rec, replay, err := h.store.AdmitParent(h.ctx, foreman.ParentRunSpec{
		ParentRunID:              h.parentID,
		SubmittingAuthority:      "scheduler:test",
		SubmissionIdempotencyKey: "submit-" + h.parentID,
		ParentBriefJSON:          h.briefJSON,
		MaxActiveAttempts:        maxActive,
	})
	if err != nil || replay {
		h.t.Fatalf("admit parent: replay=%v err=%v", replay, err)
	}
	return rec
}

func (h *harness) invoke() execution.ForemanReceipt {
	h.t.Helper()
	receipt, err := h.store.InvokeForeman(h.ctx, h.parentID)
	if err != nil {
		h.t.Fatalf("invoke foreman: %v", err)
	}
	return receipt
}

func (h *harness) acquireLease(leaseID string, epoch int, expected int) {
	h.t.Helper()
	err := database.AcquireWorkspaceLease(h.ctx, h.db, database.LeaseAcquire{
		WorkspaceLease: database.WorkspaceLease{
			LeaseID:    leaseID,
			LeaseEpoch: epoch,
			Identity:   database.WorkspaceIdentity{RepositoryURI: "https://example.invalid/repo.git", BaseRevision: "base", Branch: "work", HostID: "bunker-1", WorkspacePath: "/workspace/" + leaseID},
			OwnerRunID: h.parentID,
			ExpiresAt:  seamNow.Add(time.Hour).Format(time.RFC3339Nano),
		},
		ExpectedEpoch: expected,
	})
	if err != nil {
		h.t.Fatalf("acquire lease %s: %v", leaseID, err)
	}
}

func (h *harness) route(mutators ...func(*execution.RouteReceipt)) execution.RouteReceipt {
	h.t.Helper()
	r := execution.RouteReceipt{
		Candidates: []execution.RouteCandidate{
			{Rank: 1, Model: "mimo-v2.6-pro", Provider: "opencode-go", BaseURL: "https://router.invalid"},
			{Rank: 2, Model: "longcat-2.0", Provider: "opencode-go", BaseURL: "https://router.invalid"},
		},
		ResolvedAt: seamNow.Add(-time.Minute).Format(time.RFC3339Nano),
		ValidUntil: seamNow.Add(5 * time.Minute).Format(time.RFC3339Nano),
	}
	for _, m := range mutators {
		m(&r)
	}
	digest, err := execution.RouteReceiptDigest(r)
	if err != nil {
		h.t.Fatal(err)
	}
	r.ReceiptDigest = digest
	return r
}

func (h *harness) intent(stepID string, generation int, leaseID string, leaseEpoch int, route execution.RouteReceipt) execution.ReadyStepIntent {
	h.t.Helper()
	return execution.ReadyStepIntent{
		ParentRunID:          h.parentID,
		StepID:               stepID,
		StepGeneration:       generation,
		BriefDigest:          h.stepBriefDigest,
		ContextRefs:          []execution.ContextRef{{Namespace: "goals", KeyOrID: "AUTO-CODE-001", RetrievedAt: "2026-10-15T11:00:00Z", Authority: "goal"}},
		RequiredCapabilities: []string{"code_gen", "git"},
		Route:                route,
		Choice: execution.ToolChoice{
			CandidateRank:        1,
			ConnectorID:          "codex-app-server",
			ConnectorVersion:     "1.0",
			CompatibleModel:      route.Candidates[0].Model,
			CompatibleProvider:   route.Candidates[0].Provider,
			DeclaredCapabilities: []string{"code_gen", "git", "test"},
		},
		Lease: execution.WorkspaceLeaseAuthority{
			LeaseID:               leaseID,
			LeaseEpoch:            leaseEpoch,
			ProfileID:             "bunker-profile-1",
			EnforcementReceiptRef: "enforcement-receipt-1",
			VerifiedAt:            seamNow.Add(-time.Minute).Format(time.RFC3339Nano),
			ValidUntil:            seamNow.Add(time.Hour).Format(time.RFC3339Nano),
		},
		Policy: execution.AttemptPolicy{
			StartTimeoutS:      60,
			WallTimeoutS:       3600,
			BudgetUnits:        100,
			CancellationPolicy: "cooperative",
		},
	}
}

func (h *harness) prepare(attemptID string, intent execution.ReadyStepIntent) foreman.AttemptRecord {
	h.t.Helper()
	rec, replay, err := h.store.PrepareAttempt(h.ctx, attemptID, intent)
	if err != nil || replay {
		h.t.Fatalf("prepare attempt %s: replay=%v err=%v", attemptID, replay, err)
	}
	return rec
}

func (h *harness) attemptCount() int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_attempts`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) eventCount() int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM execution_events`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) settleTriple() (execution.TerminalOutcome, execution.ConnectorSettlement, execution.IndependentVerification) {
	return execution.TerminalOutcome{OutcomeRef: "outcome-1", State: execution.AttemptStateCompleted, CorrelationID: "corr-1"},
		execution.ConnectorSettlement{SettlementReceiptRef: "connector-settlement-1", QuiescenceReceiptRef: "quiescence-1"},
		execution.IndependentVerification{VerificationRef: "verification-1", VerifierID: "verifier-1", Verdict: execution.VerifierPass}
}

// mustStepBriefDigest returns a canonical execution-brief digest computed by
// the landed BriefDigest machinery — what a ready-step intent carries.
func mustStepBriefDigest(t *testing.T) string {
	t.Helper()
	digest, err := execution.BriefDigest([]byte(`{"schema_version":"1.0","brief_id":"step-brief-1","revision_id":"rev-1","task_id":"ACP-SCHED-001"}`))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func mustParentBrief(t *testing.T, parentRunID string) ([]byte, string) {
	t.Helper()
	brief := execution.ParentBrief{
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
		ContextRefs:   []execution.ContextRef{{Namespace: "goals", KeyOrID: "AUTO-CODE-001", RevisionOrDigest: "sha256:abc", RetrievedAt: "2026-10-15T11:00:00Z", Authority: "goal"}},
		Scope:         execution.Scope{AllowedPaths: []string{"internal/**"}, ForbiddenPaths: []string{"**/secrets/**"}},
		Constraints:   []execution.Constraint{{ID: "c1", Level: "hard", Statement: "no credentials in payloads"}},
		NonGoals:      []string{"no live wiring"},
		AcceptanceCriteria: []execution.AcceptanceCriterion{{
			ID: "ac1", Statement: "tests pass", Required: true,
			ProofRequirements: []execution.ProofRequirement{{ID: "p1", Kind: "test", Argv: []string{"go", "test", "./..."}, TimeoutS: 600, ExpectExit: 0}},
		}},
		DeadlineAt:         "2026-10-15T18:00:00Z",
		Budget:             execution.ParentBudget{MaxCostUnits: 100, MaxWallTimeoutS: 21600, MaxTokens: 1000000},
		CancellationPolicy: "cooperative: fan out to active attempts and wait for quiescence",
		Provenance:         execution.Provenance{ForemanRunID: "foreman-run-1", BoardRevision: "board-1"},
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := execution.BriefDigest(raw)
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
