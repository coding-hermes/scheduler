package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

var executionTestNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func executionTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, clock.WithClock(context.Background(), clock.NewFixed(executionTestNow))
}

func createTestRun(t *testing.T, ctx context.Context, db *sql.DB, id string) ExecutionRun {
	t.Helper()
	r, replay, err := CreateExecutionRun(ctx, db, ExecutionRun{
		RunID: id, SubmittingAuthority: "foreman:test", SubmissionIdempotencyKey: "submit-" + id,
		BriefID: "brief", BriefRevision: "r1", BriefDigest: "digest-" + id,
		Lane: "test", TaskID: "TASK-1",
	})
	if err != nil || replay {
		t.Fatalf("create run: replay=%v err=%v", replay, err)
	}
	return r
}

func admission(runID, attemptID, key, leaseID string, epoch, expected int) AttemptAdmission {
	return AttemptAdmission{
		Attempt: ExecutionAttempt{
			AttemptID: attemptID, RunID: runID, AttemptIdempotencyKey: key,
			RouteDigest: "route-" + attemptID, RouteReceiptJSON: `{}`,
			ConnectorID: "codex", ConnectorVersion: "1.0", SelectedCandidateRank: 1,
		},
		Lease: LeaseAcquire{WorkspaceLease: WorkspaceLease{
			LeaseID: leaseID, LeaseEpoch: epoch,
			Identity:  WorkspaceIdentity{RepositoryURI: "https://example.invalid/repo.git", BaseRevision: "base", Branch: "work", HostID: "bunker-1", WorkspacePath: "/workspace/task"},
			ExpiresAt: executionTestNow.Add(time.Hour).Format(time.RFC3339Nano),
		}, ExpectedEpoch: expected},
		Dispatch: OutboxMessage{OutboxID: "out-" + attemptID, IdempotencyKey: "dispatch-" + attemptID, PayloadJSON: `{}`, PayloadDigest: "payload-" + attemptID},
	}
}

func TestExecutionMigrationFreshDatabase(t *testing.T) {
	db, ctx := executionTestDB(t)
	v, err := MigrationVersion(ctx, db)
	if err != nil || v != 69 {
		t.Fatalf("migration version=%d err=%v", v, err)
	}
	for _, table := range []string{"execution_runs", "execution_attempts", "execution_events", "workspace_leases", "verification_receipts", "execution_projections", "execution_outbox"} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&got); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
	var desc string
	if err := db.QueryRowContext(ctx, `SELECT desc FROM migrations WHERE version=69`).Scan(&desc); err != nil || desc == "" {
		t.Fatalf("migration ledger: desc=%q err=%v", desc, err)
	}
}

func TestExecutionRunReplayAndConflict(t *testing.T) {
	db, ctx := executionTestDB(t)
	original := createTestRun(t, ctx, db, "run-1")
	replayed, replay, err := CreateExecutionRun(ctx, db, ExecutionRun{
		RunID: "ignored", SubmittingAuthority: original.SubmittingAuthority,
		SubmissionIdempotencyKey: original.SubmissionIdempotencyKey,
		BriefDigest:              original.BriefDigest, Lane: original.Lane, TaskID: original.TaskID,
	})
	if err != nil || !replay || replayed.RunID != original.RunID {
		t.Fatalf("identical replay: got=%+v replay=%v err=%v", replayed, replay, err)
	}
	_, _, err = CreateExecutionRun(ctx, db, ExecutionRun{
		RunID: "other", SubmittingAuthority: original.SubmittingAuthority,
		SubmissionIdempotencyKey: original.SubmissionIdempotencyKey,
		BriefDigest:              "changed", Lane: original.Lane, TaskID: original.TaskID,
	})
	if !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("changed digest error=%v", err)
	}
	_, _, err = CreateExecutionRun(ctx, db, ExecutionRun{
		RunID: "terminal", SubmittingAuthority: "foreman:test", SubmissionIdempotencyKey: "terminal-key",
		BriefDigest: "terminal-digest", Lane: "test", TaskID: "TASK-2", State: "completed",
	})
	if err == nil {
		t.Fatal("caller-supplied terminal state unexpectedly created a run")
	}
	if _, err = GetExecutionRun(ctx, db, "terminal"); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("terminal run lookup error=%v, want not found", err)
	}
}

func TestAttemptReplayRequiresIdenticalAdmission(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	original := admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0)
	original.Attempt.RequestedModel = "requested-model"
	original.Attempt.RequestedProvider = "requested-provider"
	original.Attempt.SelectedModel = "selected-model"
	original.Attempt.SelectedProvider = "selected-provider"
	original.Attempt.SelectedBaseURL = "https://provider.invalid"
	original.Attempt.BunkerProfileID = "bunker-profile"
	if _, replay, err := AdmitExecutionAttempt(ctx, db, original); err != nil || replay {
		t.Fatalf("initial admission replay=%v err=%v", replay, err)
	}

	tests := []struct {
		name   string
		change func(*AttemptAdmission)
	}{
		{"attempt ID", func(a *AttemptAdmission) { a.Attempt.AttemptID = "changed-attempt" }},
		{"connector ID", func(a *AttemptAdmission) { a.Attempt.ConnectorID = "changed-connector" }},
		{"connector version", func(a *AttemptAdmission) { a.Attempt.ConnectorVersion = "2.0" }},
		{"candidate rank", func(a *AttemptAdmission) { a.Attempt.SelectedCandidateRank++ }},
		{"requested model", func(a *AttemptAdmission) { a.Attempt.RequestedModel = "changed" }},
		{"requested provider", func(a *AttemptAdmission) { a.Attempt.RequestedProvider = "changed" }},
		{"selected model", func(a *AttemptAdmission) { a.Attempt.SelectedModel = "changed" }},
		{"selected provider", func(a *AttemptAdmission) { a.Attempt.SelectedProvider = "changed" }},
		{"selected base URL", func(a *AttemptAdmission) { a.Attempt.SelectedBaseURL = "https://changed.invalid" }},
		{"route digest", func(a *AttemptAdmission) { a.Attempt.RouteDigest = "changed" }},
		{"route receipt", func(a *AttemptAdmission) { a.Attempt.RouteReceiptJSON = `{"changed":true}` }},
		{"bunker profile", func(a *AttemptAdmission) { a.Attempt.BunkerProfileID = "changed" }},
		{"lease ID", func(a *AttemptAdmission) { a.Lease.LeaseID = "changed-lease" }},
		{"lease epoch", func(a *AttemptAdmission) { a.Lease.LeaseEpoch++ }},
		{"lease expected epoch", func(a *AttemptAdmission) { a.Lease.ExpectedEpoch++ }},
		{"lease repository", func(a *AttemptAdmission) { a.Lease.Identity.RepositoryURI = "https://changed.invalid/repo.git" }},
		{"lease base revision", func(a *AttemptAdmission) { a.Lease.Identity.BaseRevision = "changed" }},
		{"lease branch", func(a *AttemptAdmission) { a.Lease.Identity.Branch = "changed" }},
		{"lease host", func(a *AttemptAdmission) { a.Lease.Identity.HostID = "changed" }},
		{"lease path", func(a *AttemptAdmission) { a.Lease.Identity.WorkspacePath = "/changed" }},
		{"outbox ID", func(a *AttemptAdmission) { a.Dispatch.OutboxID = "changed-outbox" }},
		{"outbox key", func(a *AttemptAdmission) { a.Dispatch.IdempotencyKey = "changed-key" }},
		{"outbox kind", func(a *AttemptAdmission) { a.Dispatch.Kind = "changed-kind" }},
		{"outbox payload digest", func(a *AttemptAdmission) { a.Dispatch.PayloadDigest = "changed" }},
		{"outbox payload bytes", func(a *AttemptAdmission) { a.Dispatch.PayloadJSON = "{ }" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := original
			tt.change(&changed)
			if _, _, err := AdmitExecutionAttempt(ctx, db, changed); !errors.Is(err, ErrExecutionConflict) {
				t.Fatalf("error=%v, want ErrExecutionConflict", err)
			}
		})
	}
	var attempts, leases, outbox int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_leases`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || leases != 1 || outbox != 1 {
		t.Fatalf("replay conflicts mutated rows: attempts=%d leases=%d outbox=%d", attempts, leases, outbox)
	}
	stored, ok, err := findAttemptByKey(ctx, db, original.Attempt.AttemptIdempotencyKey)
	if err != nil || !ok || stored.AttemptID != original.Attempt.AttemptID || stored.ConnectorVersion != original.Attempt.ConnectorVersion {
		t.Fatalf("stored attempt changed: %+v ok=%v err=%v", stored, ok, err)
	}
	lease, err := findWorkspaceLease(ctx, db, original.Lease.LeaseID)
	if err != nil || lease.LeaseEpoch != original.Lease.LeaseEpoch || lease.Identity != original.Lease.Identity {
		t.Fatalf("stored lease changed: %+v err=%v", lease, err)
	}
	dispatch, err := findExecutionOutboxByAttempt(ctx, db, original.Attempt.AttemptID)
	if err != nil || dispatch.OutboxID != original.Dispatch.OutboxID || dispatch.IdempotencyKey != original.Dispatch.IdempotencyKey || dispatch.PayloadJSON != original.Dispatch.PayloadJSON || dispatch.PayloadDigest != original.Dispatch.PayloadDigest {
		t.Fatalf("stored outbox changed: %+v err=%v", dispatch, err)
	}
}

func TestAttemptGenerationLeaseCASAndAtomicRollback(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a1, replay, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil || replay || a1.Generation != 1 {
		t.Fatalf("attempt 1: %+v replay=%v err=%v", a1, replay, err)
	}
	got, replay, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil || !replay || got.AttemptID != "attempt-1" {
		t.Fatalf("attempt replay: %+v replay=%v err=%v", got, replay, err)
	}
	if _, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-blocked", "akey-blocked", "lease-blocked", 2, 1)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("second active writer error=%v", err)
	}
	if err := RenewWorkspaceLease(ctx, db, WorkspaceLease{LeaseID: "lease-1", OwnerRunID: "run-1", OwnerAttemptID: "attempt-1", LeaseEpoch: 1, Identity: admission("run-1", "attempt-1", "", "", 1, 0).Lease.Identity}, executionTestNow.Add(2*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("renew exact lease: %v", err)
	}
	if err := RevokeWorkspaceLease(ctx, db, "lease-1", 2, "wrong epoch"); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong revoke=%v", err)
	}
	if err := RevokeWorkspaceLease(ctx, db, "lease-1", 1, "retry"); err != nil {
		t.Fatal(err)
	}
	a2, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-2", "akey-2", "lease-2", 2, 1))
	if err != nil || a2.Generation != 2 {
		t.Fatalf("attempt 2: %+v err=%v", a2, err)
	}
	if err := RevokeWorkspaceLease(ctx, db, "lease-2", 2, "atomic rollback test"); err != nil {
		t.Fatal(err)
	}
	bad := admission("run-1", "attempt-3", "akey-3", "lease-3", 3, 2)
	bad.Dispatch.Kind = "not-a-dispatch"
	if _, _, err := AdmitExecutionAttempt(ctx, db, bad); err == nil {
		t.Fatal("invalid outbox kind unexpectedly committed")
	}
	for table, target := range map[string][2]string{
		"execution_attempts": {"attempt_id", "attempt-3"},
		"workspace_leases":   {"lease_id", "lease-3"},
	} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+target[0]+`=?`, target[1]).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rollback %s count=%d err=%v", table, n, err)
		}
	}
}

func TestEventFencingAndCompletionAuthority(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a1, _, _ := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err := RevokeWorkspaceLease(ctx, db, "lease-1", 1, "retry"); err != nil {
		t.Fatal(err)
	}
	a2, _, _ := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-2", "akey-2", "lease-2", 2, 1))
	stale, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{RunID: "run-1", AttemptID: a1.AttemptID, Generation: 1, Sequence: 1, Kind: "progress", ProposedState: "working", PayloadDigest: "stale"})
	if err != nil || stale.Accepted || stale.RejectionReason != "stale_generation" {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
	completed, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{RunID: "run-1", AttemptID: a2.AttemptID, Generation: 2, Sequence: 1, Kind: "connector_terminal", ProposedState: "connector_completed", PayloadDigest: "terminal"})
	if err != nil || !completed.Accepted {
		t.Fatalf("connector completion=%+v err=%v", completed, err)
	}
	ack, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{RunID: "run-1", AttemptID: a2.AttemptID, Generation: 2, Sequence: 2, Kind: "crier_ack", ProposedState: "completed", PayloadDigest: "ack"})
	if err != nil || ack.Accepted || ack.RejectionReason != "crier_ack_has_no_state_authority" {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	r, err := GetExecutionRun(ctx, db, "run-1")
	if err != nil || r.State != "connector_completed" || r.VerificationState != "pending" || r.BoardCommit != "" || r.DurableCompletedAt.Valid {
		t.Fatalf("authority escaped: %+v err=%v", r, err)
	}
	replay, wasReplay, err := AppendExecutionEvent(ctx, db, ExecutionEvent{RunID: "run-1", AttemptID: a2.AttemptID, Generation: 2, Sequence: 1, Kind: "connector_terminal", ProposedState: "connector_completed", PayloadDigest: "terminal"})
	if err != nil || !wasReplay || replay.EventID != completed.EventID {
		t.Fatalf("event replay=%+v replay=%v err=%v", replay, wasReplay, err)
	}
}

func TestExecutionEventReplayRequiresAuthorityIdentity(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	original := ExecutionEvent{RunID: "run-1", AttemptID: a.AttemptID, Generation: 1, Sequence: 1, Kind: "progress", ProposedState: "working", PayloadDigest: "payload"}
	created, _, err := AppendExecutionEvent(ctx, db, original)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*ExecutionEvent)
	}{
		{"run ID", func(e *ExecutionEvent) { e.RunID = "changed-run" }},
		{"generation", func(e *ExecutionEvent) { e.Generation = 2 }},
		{"kind", func(e *ExecutionEvent) { e.Kind = "changed-kind" }},
		{"proposed state", func(e *ExecutionEvent) { e.ProposedState = "blocked" }},
		{"payload digest", func(e *ExecutionEvent) { e.PayloadDigest = "changed" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := original
			tt.change(&changed)
			if _, _, err := AppendExecutionEvent(ctx, db, changed); !errors.Is(err, ErrExecutionConflict) {
				t.Fatalf("error=%v, want conflict", err)
			}
		})
	}
	var count int
	var state string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(proposed_state) FROM execution_events WHERE attempt_id=? AND sequence=1`, a.AttemptID).Scan(&count, &state); err != nil {
		t.Fatal(err)
	}
	if count != 1 || state != original.ProposedState || created.EventID == 0 {
		t.Fatalf("event changed after conflicts: count=%d state=%q event=%+v", count, state, created)
	}
}

func TestOutboxRetryRecoveryProjectionAndReceipts(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ClaimExecutionOutbox(ctx, db, "dispatcher-1")
	if err != nil || m.IdempotencyKey != "dispatch-attempt-1" || m.AttemptCount != 1 {
		t.Fatalf("claim=%+v err=%v", m, err)
	}
	next := executionTestNow.Add(-time.Minute).Format(time.RFC3339Nano)
	if err := RetryExecutionOutbox(ctx, db, m.OutboxID, "dispatcher-1", next, "ambiguous response"); err != nil {
		t.Fatal(err)
	}
	m2, err := ClaimExecutionOutbox(ctx, db, "dispatcher-2")
	if err != nil || m2.IdempotencyKey != m.IdempotencyKey || m2.AttemptCount != 2 {
		t.Fatalf("retry claim=%+v err=%v", m2, err)
	}
	if err := SetAttemptProcessIncarnation(ctx, db, a.AttemptID, a.Generation, 123, 120, "container-1"); err != nil {
		t.Fatal(err)
	}
	runs, leases, outbox, err := RecoverExecutionState(ctx, db)
	if err != nil || len(runs) != 1 || len(leases) != 1 || len(outbox) != 1 || outbox[0].State != "claimed" {
		t.Fatalf("recovery runs=%d leases=%d outbox=%+v err=%v", len(runs), len(leases), outbox, err)
	}
	var state, claimedBy string
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT state,claimed_by,attempt_count FROM execution_outbox WHERE outbox_id=?`, m2.OutboxID).Scan(&state, &claimedBy, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "claimed" || claimedBy != "dispatcher-2" || attempts != 2 {
		t.Fatalf("recovery reset/reissued claimed row: state=%q worker=%q attempts=%d", state, claimedBy, attempts)
	}
	if err := AckExecutionOutbox(ctx, db, m2.OutboxID, "dispatcher-2", "transport-ack"); err != nil {
		t.Fatal(err)
	}
	p := ExecutionProjection{RunID: "run-1", Projection: "duckbrain", State: "pending", IdempotencyKey: "proj-1", PayloadDigest: "projection-digest"}
	if replay, err := PutExecutionProjection(ctx, db, p); err != nil || replay {
		t.Fatalf("projection create replay=%v err=%v", replay, err)
	}
	if replay, err := PutExecutionProjection(ctx, db, p); err != nil || !replay {
		t.Fatalf("exact projection replay=%v err=%v", replay, err)
	}
	p.State = "succeeded"
	p.RemoteID = "memory-1"
	p.Attempts = 1
	if replay, err := PutExecutionProjection(ctx, db, p); err != nil || replay {
		t.Fatalf("projection advance replay=%v err=%v", replay, err)
	}
	changedKey := p
	changedKey.IdempotencyKey = "changed-key"
	if _, err := PutExecutionProjection(ctx, db, changedKey); !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("projection key conflict=%v", err)
	}
	p.PayloadDigest = "changed"
	if _, err := PutExecutionProjection(ctx, db, p); !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("projection conflict=%v", err)
	}
	receipt := VerificationReceipt{ReceiptID: "vr-1", RunID: "run-1", AttemptID: a.AttemptID, CriterionID: "criterion-1", CommitSHA: "commit-1", State: "passed", TrustedEnvironment: "verifier", ArgvJSON: `["go","test"]`, ArtifactDigest: "proof-1", VerifierID: "verifier-v1"}
	if replay, err := RecordVerificationReceipt(ctx, db, receipt); err != nil || replay {
		t.Fatalf("receipt replay=%v err=%v", replay, err)
	}
	if replay, err := RecordVerificationReceipt(ctx, db, receipt); err != nil || !replay {
		t.Fatalf("receipt second replay=%v err=%v", replay, err)
	}
}

func TestVerificationReceiptReplayRequiresIdenticalEvidence(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	original := VerificationReceipt{
		ReceiptID: "vr-1", RunID: "run-1", AttemptID: a.AttemptID, CriterionID: "criterion-1",
		CommitSHA: "commit-1", TreeSHA: "tree-1", State: "passed", TrustedEnvironment: "trusted",
		ArgvJSON: `["go","test"]`, ExitCode: sql.NullInt64{Int64: 0, Valid: true},
		ArtifactDigest: "artifact-1", VerifierID: "verifier-1",
	}
	if replay, err := RecordVerificationReceipt(ctx, db, original); err != nil || replay {
		t.Fatalf("create replay=%v err=%v", replay, err)
	}
	if replay, err := RecordVerificationReceipt(ctx, db, original); err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	tests := []struct {
		name   string
		change func(*VerificationReceipt)
	}{
		{"receipt ID", func(r *VerificationReceipt) { r.ReceiptID = "changed" }},
		{"run ID", func(r *VerificationReceipt) { r.RunID = "changed" }},
		{"attempt ID", func(r *VerificationReceipt) { r.AttemptID = "changed" }},
		{"criterion ID", func(r *VerificationReceipt) { r.CriterionID = "changed" }},
		{"commit", func(r *VerificationReceipt) { r.CommitSHA = "changed" }},
		{"tree", func(r *VerificationReceipt) { r.TreeSHA = "changed" }},
		{"state", func(r *VerificationReceipt) { r.State = "failed" }},
		{"trusted environment", func(r *VerificationReceipt) { r.TrustedEnvironment = "changed" }},
		{"argv", func(r *VerificationReceipt) { r.ArgvJSON = `["changed"]` }},
		{"exit code", func(r *VerificationReceipt) { r.ExitCode.Int64 = 1 }},
		{"exit code nullness", func(r *VerificationReceipt) { r.ExitCode.Valid = false }},
		{"artifact digest", func(r *VerificationReceipt) { r.ArtifactDigest = "changed" }},
		{"verifier ID", func(r *VerificationReceipt) { r.VerifierID = "changed" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := original
			tt.change(&changed)
			if _, err := RecordVerificationReceipt(ctx, db, changed); !errors.Is(err, ErrExecutionConflict) {
				t.Fatalf("error=%v, want conflict", err)
			}
		})
	}
	var count int
	var tree, artifact string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(tree_sha),MAX(artifact_digest) FROM verification_receipts`).Scan(&count, &tree, &artifact); err != nil {
		t.Fatal(err)
	}
	if count != 1 || tree != original.TreeSHA || artifact != original.ArtifactDigest {
		t.Fatalf("receipt changed after conflicts: count=%d tree=%q artifact=%q", count, tree, artifact)
	}
}

func TestClaimExecutionOutboxRequiresWorkerAndSingleWinner(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	if _, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimExecutionOutbox(ctx, db, ""); err == nil {
		t.Fatal("empty worker unexpectedly claimed outbox")
	}
	first, err := ClaimExecutionOutbox(ctx, db, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimExecutionOutbox(ctx, db, "worker-2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("competing claim error=%v, want no rows", err)
	}
	var state, worker string
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT state,claimed_by,attempt_count FROM execution_outbox WHERE outbox_id=?`, first.OutboxID).Scan(&state, &worker, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "claimed" || worker != "worker-1" || attempts != 1 {
		t.Fatalf("competing claim changed winner: state=%q worker=%q attempts=%d", state, worker, attempts)
	}
}
