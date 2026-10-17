package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/execution"
)

// Errors returned by the durable execution store.
var (
	ErrExecutionConflict = errors.New("execution store idempotency conflict")
	ErrExecutionNotFound = errors.New("execution store row not found")
	ErrLeaseConflict     = errors.New("workspace lease compare-and-set conflict")
)

var executionStates = map[string]bool{
	"submitted": true, "workspace_ready": true, "dispatched": true, "working": true,
	"input_required": true, "auth_required": true, "connector_completed": true,
	"verifying": true, "blocked": true, "completed": true, "failed": true,
	"canceled": true, "rejected": true, "expired": true,
}

func executionNow(ctx context.Context) string {
	return clock.FromContext(ctx).Now().UTC().Format(time.RFC3339Nano)
}

// ExecutionRun is the scheduler-owned logical coding run. Connector completion
// is deliberately distinct from verification and durable completion.
type ExecutionRun struct {
	RunID, SubmittingAuthority, SubmissionIdempotencyKey   string
	BriefID, BriefRevision, BriefDigest                    string
	TickID, Lane, TaskID                                   string
	State, BlockedReason                                   string
	CurrentAttemptGeneration                               int
	VerificationState, VerdictID                           string
	ImplementationCommit, IntegrationCommit, BoardCommit   string
	RoutingOutcomeID, DuckBrainNamespace, DuckBrainKeyOrID string
	CreatedAt, UpdatedAt                                   string
	DurableCompletedAt                                     sql.NullString
}

// CreateExecutionRun creates a scheduler-controlled submitted parent run or
// returns the existing run for an identical submitting-authority/key/brief-
// digest replay. Callers may omit State or explicitly provide "submitted";
// no other initial state is accepted. A changed digest is an
// ErrExecutionConflict.
func CreateExecutionRun(ctx context.Context, db *sql.DB, r ExecutionRun) (ExecutionRun, bool, error) {
	if r.RunID == "" || r.SubmittingAuthority == "" || r.SubmissionIdempotencyKey == "" || r.BriefDigest == "" || r.Lane == "" || r.TaskID == "" {
		return ExecutionRun{}, false, errors.New("create execution run: required field is empty")
	}
	if r.State == "" {
		r.State = "submitted"
	}
	if r.State != "submitted" {
		return ExecutionRun{}, false, fmt.Errorf("create execution run: initial state must be submitted, got %q", r.State)
	}
	now := executionNow(ctx)
	_, err := db.ExecContext(ctx, `INSERT INTO execution_runs
		(run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.RunID, r.SubmittingAuthority, r.SubmissionIdempotencyKey, r.BriefID, r.BriefRevision, r.BriefDigest, r.TickID, r.Lane, r.TaskID, r.State, now, now)
	if err == nil {
		created, getErr := GetExecutionRun(ctx, db, r.RunID)
		return created, false, getErr
	}
	existing, getErr := getExecutionRunBySubmission(ctx, db, r.SubmittingAuthority, r.SubmissionIdempotencyKey)
	if getErr != nil {
		return ExecutionRun{}, false, fmt.Errorf("create execution run: %w", err)
	}
	if existing.BriefDigest != r.BriefDigest {
		return ExecutionRun{}, false, fmt.Errorf("%w: submission %s/%s has a different brief digest", ErrExecutionConflict, r.SubmittingAuthority, r.SubmissionIdempotencyKey)
	}
	return existing, true, nil
}

// GetExecutionRun reads one logical run.
func GetExecutionRun(ctx context.Context, db *sql.DB, runID string) (ExecutionRun, error) {
	return scanExecutionRun(db.QueryRowContext(ctx, executionRunSelect+` WHERE run_id=?`, runID))
}

func getExecutionRunBySubmission(ctx context.Context, db *sql.DB, authority, key string) (ExecutionRun, error) {
	return scanExecutionRun(db.QueryRowContext(ctx, executionRunSelect+` WHERE submitting_authority=? AND submission_idempotency_key=?`, authority, key))
}

const executionRunSelect = `SELECT run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,blocked_reason,current_attempt_generation,verification_state,verdict_id,implementation_commit,integration_commit,board_commit,routing_outcome_id,duckbrain_namespace,duckbrain_key_or_id,created_at,updated_at,durable_completed_at FROM execution_runs`

type rowScanner interface{ Scan(...any) error }

func scanExecutionRun(row rowScanner) (ExecutionRun, error) {
	var r ExecutionRun
	err := row.Scan(&r.RunID, &r.SubmittingAuthority, &r.SubmissionIdempotencyKey, &r.BriefID, &r.BriefRevision, &r.BriefDigest, &r.TickID, &r.Lane, &r.TaskID, &r.State, &r.BlockedReason, &r.CurrentAttemptGeneration, &r.VerificationState, &r.VerdictID, &r.ImplementationCommit, &r.IntegrationCommit, &r.BoardCommit, &r.RoutingOutcomeID, &r.DuckBrainNamespace, &r.DuckBrainKeyOrID, &r.CreatedAt, &r.UpdatedAt, &r.DurableCompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrExecutionNotFound
	}
	return r, err
}

// WorkspaceIdentity is the exact write-authority boundary.
type WorkspaceIdentity struct{ RepositoryURI, BaseRevision, Branch, HostID, WorkspacePath string }

// WorkspaceLease is one epoch-fenced workspace writer.
type WorkspaceLease struct {
	LeaseID                                 string
	Identity                                WorkspaceIdentity
	OwnerRunID, OwnerAttemptID              string
	LeaseEpoch                              int
	State, AcquiredAt, RenewedAt, ExpiresAt string
	RevokedAt                               sql.NullString
	RevocationReason                        string
}

// LeaseAcquire performs a compare-and-set from ExpectedEpoch to LeaseEpoch.
type LeaseAcquire struct {
	WorkspaceLease
	ExpectedEpoch int
}

// ExecutionAttempt records one generated attempt and its process incarnation.
type ExecutionAttempt struct {
	AttemptID, RunID                                                                    string
	Generation                                                                          int
	AttemptIdempotencyKey, State                                                        string
	SelectedCandidateRank                                                               int
	RequestedModel, RequestedProvider, SelectedModel, SelectedProvider, SelectedBaseURL string
	ServedModel, ServedProvider, ServedBaseURL, RouteDigest, RouteReceiptJSON           string
	ConnectorID, ConnectorVersion, BunkerProfileID, NativeSessionID                     string
	CrierMessageID, CrierContextID, WorkspaceLeaseID                                    string
	LeaseEpoch                                                                          int
	PID, PGID                                                                           sql.NullInt64
	CgroupOrContainerID                                                                 string
	ProcessStartedAt                                                                    sql.NullString
	SubmittedAt                                                                         string
}

// OutboxMessage is a durable dispatch command.
type OutboxMessage struct {
	OutboxID, RunID, AttemptID, Kind, IdempotencyKey, PayloadJSON, PayloadDigest string
	State                                                                        string
	AttemptCount                                                                 int
	AvailableAt, ClaimedBy                                                       string
	ClaimedAt                                                                    sql.NullString
	AckID, LastError, CreatedAt, UpdatedAt                                       string
}

// AttemptAdmission atomically installs a new generation, its exact workspace
// lease, and its dispatch outbox row. Nothing becomes network-visible here.
type AttemptAdmission struct {
	Attempt  ExecutionAttempt
	Lease    LeaseAcquire
	Dispatch OutboxMessage
}

// AdmitExecutionAttempt is the older raw admission path. It runs the same
// fail-closed parent authority gates as the parent-run seam before any row
// exists — the parent must be executable with the foreman invoked, the default
// one-active-attempt concurrency policy is enforced, and the workspace lease
// must be owned by this parent/attempt — so it can never bypass the
// parent/readiness/route/tool/lease/concurrency gates. Generation allocation,
// write authority, and dispatch persistence still commit together.
func AdmitExecutionAttempt(ctx context.Context, db *sql.DB, a AttemptAdmission) (ExecutionAttempt, bool, error) {
	if a.Attempt.AttemptID == "" || a.Attempt.RunID == "" || a.Attempt.AttemptIdempotencyKey == "" || a.Attempt.RouteDigest == "" || a.Attempt.ConnectorID == "" || a.Lease.LeaseID == "" || a.Dispatch.OutboxID == "" || a.Dispatch.IdempotencyKey == "" {
		return ExecutionAttempt{}, false, errors.New("admit execution attempt: required field is empty")
	}
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return ExecutionAttempt{}, false, err
	}
	defer tx.rollback()
	if existing, ok, e := findAttemptByKey(ctx, tx.conn, a.Attempt.AttemptIdempotencyKey); e != nil {
		return ExecutionAttempt{}, false, e
	} else if ok {
		lease, e := findWorkspaceLease(ctx, tx.conn, existing.WorkspaceLeaseID)
		if e != nil {
			return ExecutionAttempt{}, false, e
		}
		dispatch, e := findExecutionOutboxByAttempt(ctx, tx.conn, existing.AttemptID)
		if e != nil {
			return ExecutionAttempt{}, false, e
		}
		if !sameAttemptAdmission(existing, lease, dispatch, a) {
			return ExecutionAttempt{}, false, ErrExecutionConflict
		}
		return existing, true, tx.commit()
	}
	// A lease owned by another parent or attempt can never authorize work.
	if a.Lease.OwnerRunID != "" && a.Lease.OwnerRunID != a.Attempt.RunID {
		return ExecutionAttempt{}, false, fmt.Errorf("%w: lease %s is owned by parent %q", execution.ErrLeaseOwnership, a.Lease.LeaseID, a.Lease.OwnerRunID)
	}
	if a.Lease.OwnerAttemptID != "" && a.Lease.OwnerAttemptID != a.Attempt.AttemptID {
		return ExecutionAttempt{}, false, fmt.Errorf("%w: lease %s is owned by attempt %q", execution.ErrLeaseOwnership, a.Lease.LeaseID, a.Lease.OwnerAttemptID)
	}
	var current, maxActive, active int
	var parentState, invocationID string
	if err := tx.conn.QueryRowContext(ctx, `SELECT current_attempt_generation,parent_state,foreman_invocation_id,max_active_attempts FROM execution_runs WHERE run_id=?`, a.Attempt.RunID).Scan(&current, &parentState, &invocationID, &maxActive); errors.Is(err, sql.ErrNoRows) {
		return ExecutionAttempt{}, false, ErrExecutionNotFound
	} else if err != nil {
		return ExecutionAttempt{}, false, err
	}
	switch execution.ParentState(parentState) {
	case execution.ParentQueued:
		return ExecutionAttempt{}, false, fmt.Errorf("%w: legacy admission on a queued parent", execution.ErrForemanNotInvoked)
	case execution.ParentCancelling:
		return ExecutionAttempt{}, false, execution.ErrParentCancelling
	case execution.ParentSettled:
		return ExecutionAttempt{}, false, execution.ErrParentSettled
	case execution.ParentUncertain, execution.ParentReconciling:
		return ExecutionAttempt{}, false, execution.ErrReconciliationRequired
	case execution.ParentForemanPlanning, execution.ParentExecuting:
	default:
		return ExecutionAttempt{}, false, fmt.Errorf("%w: parent state %q", execution.ErrParentNotExecutable, parentState)
	}
	if invocationID == "" {
		return ExecutionAttempt{}, false, execution.ErrForemanNotInvoked
	}
	if err := tx.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE run_id=? AND state IN `+attemptActiveStatesSQL, a.Attempt.RunID).Scan(&active); err != nil {
		return ExecutionAttempt{}, false, err
	}
	if maxActive < 1 {
		maxActive = 1
	}
	if active >= maxActive {
		return ExecutionAttempt{}, false, fmt.Errorf("%w: %d active of %d allowed", execution.ErrTooManyActiveAttempts, active, maxActive)
	}
	a.Attempt.Generation = current + 1
	a.Attempt.State = "submitted"
	a.Attempt.WorkspaceLeaseID, a.Attempt.LeaseEpoch = a.Lease.LeaseID, a.Lease.LeaseEpoch
	if err := acquireLeaseTx(ctx, tx.conn, a.Lease, a.Attempt.RunID, a.Attempt.AttemptID); err != nil {
		return ExecutionAttempt{}, false, err
	}
	now := executionNow(ctx)
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,workspace_lease_id,lease_epoch,pid,pgid,cgroup_or_container_id,process_started_at,submitted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Attempt.AttemptID, a.Attempt.RunID, a.Attempt.Generation, a.Attempt.AttemptIdempotencyKey, a.Attempt.State, a.Attempt.SelectedCandidateRank, a.Attempt.RequestedModel, a.Attempt.RequestedProvider, a.Attempt.SelectedModel, a.Attempt.SelectedProvider, a.Attempt.SelectedBaseURL, a.Attempt.RouteDigest, a.Attempt.RouteReceiptJSON, a.Attempt.ConnectorID, a.Attempt.ConnectorVersion, a.Attempt.BunkerProfileID, a.Attempt.WorkspaceLeaseID, a.Attempt.LeaseEpoch, a.Attempt.PID, a.Attempt.PGID, a.Attempt.CgroupOrContainerID, a.Attempt.ProcessStartedAt, now)
	if err != nil {
		return ExecutionAttempt{}, false, fmt.Errorf("insert attempt: %w", err)
	}
	if a.Dispatch.Kind == "" {
		a.Dispatch.Kind = "execution.dispatch"
	}
	a.Dispatch.RunID, a.Dispatch.AttemptID = a.Attempt.RunID, a.Attempt.AttemptID
	if a.Dispatch.AvailableAt == "" {
		a.Dispatch.AvailableAt = now
	}
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO execution_outbox(outbox_id,run_id,attempt_id,kind,idempotency_key,payload_json,payload_digest,state,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?)`, a.Dispatch.OutboxID, a.Dispatch.RunID, a.Dispatch.AttemptID, a.Dispatch.Kind, a.Dispatch.IdempotencyKey, a.Dispatch.PayloadJSON, a.Dispatch.PayloadDigest, a.Dispatch.AvailableAt, now, now)
	if err != nil {
		return ExecutionAttempt{}, false, fmt.Errorf("insert dispatch outbox: %w", err)
	}
	_, err = tx.conn.ExecContext(ctx, `UPDATE execution_runs SET current_attempt_generation=?,state='submitted',updated_at=? WHERE run_id=?`, a.Attempt.Generation, now, a.Attempt.RunID)
	if err != nil {
		return ExecutionAttempt{}, false, err
	}
	if err := tx.commit(); err != nil {
		return ExecutionAttempt{}, false, err
	}
	a.Attempt.SubmittedAt = now
	return a.Attempt, false, nil
}

type executionTx struct {
	conn *sql.Conn
	done bool
}

func beginExecutionTx(ctx context.Context, db *sql.DB) (*executionTx, error) {
	c, e := db.Conn(ctx)
	if e != nil {
		return nil, e
	}
	if _, e = c.ExecContext(ctx, `BEGIN IMMEDIATE`); e != nil {
		_ = c.Close()
		return nil, e
	}
	return &executionTx{conn: c}, nil
}
func (t *executionTx) commit() error {
	_, e := t.conn.ExecContext(context.Background(), `COMMIT`)
	t.done = true
	ce := t.conn.Close()
	if e != nil {
		return e
	}
	return ce
}
func (t *executionTx) rollback() {
	if !t.done {
		_, _ = t.conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = t.conn.Close()
		t.done = true
	}
}

func findAttemptByKey(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (ExecutionAttempt, bool, error) {
	var a ExecutionAttempt
	err := q.QueryRowContext(ctx, `SELECT attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,workspace_lease_id,lease_epoch,submitted_at FROM execution_attempts WHERE attempt_idempotency_key=?`, key).Scan(&a.AttemptID, &a.RunID, &a.Generation, &a.AttemptIdempotencyKey, &a.State, &a.SelectedCandidateRank, &a.RequestedModel, &a.RequestedProvider, &a.SelectedModel, &a.SelectedProvider, &a.SelectedBaseURL, &a.RouteDigest, &a.RouteReceiptJSON, &a.ConnectorID, &a.ConnectorVersion, &a.BunkerProfileID, &a.WorkspaceLeaseID, &a.LeaseEpoch, &a.SubmittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

func findWorkspaceLease(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, leaseID string) (WorkspaceLease, error) {
	var l WorkspaceLease
	err := q.QueryRowContext(ctx, `SELECT lease_id,repository_uri,base_revision,branch,host_id,workspace_path,owner_run_id,owner_attempt_id,lease_epoch,state,acquired_at,renewed_at,expires_at,revoked_at,revocation_reason FROM workspace_leases WHERE lease_id=?`, leaseID).Scan(&l.LeaseID, &l.Identity.RepositoryURI, &l.Identity.BaseRevision, &l.Identity.Branch, &l.Identity.HostID, &l.Identity.WorkspacePath, &l.OwnerRunID, &l.OwnerAttemptID, &l.LeaseEpoch, &l.State, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.RevokedAt, &l.RevocationReason)
	return l, err
}

func findExecutionOutboxByAttempt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, attemptID string) (OutboxMessage, error) {
	var m OutboxMessage
	err := q.QueryRowContext(ctx, `SELECT outbox_id,run_id,attempt_id,kind,idempotency_key,payload_json,payload_digest,state,attempt_count,available_at,claimed_by,claimed_at,ack_id,last_error,created_at,updated_at FROM execution_outbox WHERE attempt_id=?`, attemptID).Scan(&m.OutboxID, &m.RunID, &m.AttemptID, &m.Kind, &m.IdempotencyKey, &m.PayloadJSON, &m.PayloadDigest, &m.State, &m.AttemptCount, &m.AvailableAt, &m.ClaimedBy, &m.ClaimedAt, &m.AckID, &m.LastError, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

func sameAttemptAdmission(existing ExecutionAttempt, lease WorkspaceLease, dispatch OutboxMessage, proposed AttemptAdmission) bool {
	kind := proposed.Dispatch.Kind
	if kind == "" {
		kind = "execution.dispatch"
	}
	return existing.AttemptID == proposed.Attempt.AttemptID &&
		existing.RunID == proposed.Attempt.RunID &&
		existing.AttemptIdempotencyKey == proposed.Attempt.AttemptIdempotencyKey &&
		existing.SelectedCandidateRank == proposed.Attempt.SelectedCandidateRank &&
		existing.RequestedModel == proposed.Attempt.RequestedModel &&
		existing.RequestedProvider == proposed.Attempt.RequestedProvider &&
		existing.SelectedModel == proposed.Attempt.SelectedModel &&
		existing.SelectedProvider == proposed.Attempt.SelectedProvider &&
		existing.SelectedBaseURL == proposed.Attempt.SelectedBaseURL &&
		existing.RouteDigest == proposed.Attempt.RouteDigest &&
		existing.RouteReceiptJSON == proposed.Attempt.RouteReceiptJSON &&
		existing.ConnectorID == proposed.Attempt.ConnectorID &&
		existing.ConnectorVersion == proposed.Attempt.ConnectorVersion &&
		existing.BunkerProfileID == proposed.Attempt.BunkerProfileID &&
		existing.WorkspaceLeaseID == proposed.Lease.LeaseID &&
		existing.LeaseEpoch == proposed.Lease.LeaseEpoch &&
		proposed.Lease.ExpectedEpoch == existing.LeaseEpoch-1 &&
		lease.Identity == proposed.Lease.Identity &&
		dispatch.OutboxID == proposed.Dispatch.OutboxID &&
		dispatch.RunID == proposed.Attempt.RunID &&
		dispatch.AttemptID == proposed.Attempt.AttemptID &&
		dispatch.Kind == kind &&
		dispatch.IdempotencyKey == proposed.Dispatch.IdempotencyKey &&
		dispatch.PayloadDigest == proposed.Dispatch.PayloadDigest &&
		dispatch.PayloadJSON == proposed.Dispatch.PayloadJSON
}

func acquireLeaseTx(ctx context.Context, q *sql.Conn, a LeaseAcquire, runID, attemptID string) error {
	if a.LeaseEpoch != a.ExpectedEpoch+1 || a.Identity.RepositoryURI == "" || a.Identity.BaseRevision == "" || a.Identity.Branch == "" || a.Identity.HostID == "" || a.Identity.WorkspacePath == "" {
		return ErrLeaseConflict
	}
	var max int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(lease_epoch),0) FROM workspace_leases WHERE repository_uri=? AND branch=? AND host_id=? AND workspace_path=?`, a.Identity.RepositoryURI, a.Identity.Branch, a.Identity.HostID, a.Identity.WorkspacePath).Scan(&max); err != nil {
		return err
	}
	if max != a.ExpectedEpoch {
		return ErrLeaseConflict
	}
	now := executionNow(ctx)
	if a.ExpiresAt == "" {
		return errors.New("acquire workspace lease: expires_at required")
	}
	_, err := q.ExecContext(ctx, `INSERT INTO workspace_leases(lease_id,repository_uri,base_revision,branch,host_id,workspace_path,owner_run_id,owner_attempt_id,lease_epoch,state,acquired_at,renewed_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,'active',?,?,?)`, a.LeaseID, a.Identity.RepositoryURI, a.Identity.BaseRevision, a.Identity.Branch, a.Identity.HostID, a.Identity.WorkspacePath, runID, attemptID, a.LeaseEpoch, now, now, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLeaseConflict, err)
	}
	return nil
}

// AcquireWorkspaceLease grants a writer only when the caller's expected epoch
// is still current for the exact workspace identity.
func AcquireWorkspaceLease(ctx context.Context, db *sql.DB, a LeaseAcquire) error {
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return err
	}
	defer tx.rollback()
	if err := acquireLeaseTx(ctx, tx.conn, a, a.OwnerRunID, a.OwnerAttemptID); err != nil {
		return err
	}
	return tx.commit()
}

// RenewWorkspaceLease renews only the exact active owner/identity/epoch.
func RenewWorkspaceLease(ctx context.Context, db *sql.DB, l WorkspaceLease, newExpiry string) error {
	res, err := db.ExecContext(ctx, `UPDATE workspace_leases SET renewed_at=?,expires_at=? WHERE lease_id=? AND owner_run_id=? AND owner_attempt_id=? AND lease_epoch=? AND repository_uri=? AND base_revision=? AND branch=? AND host_id=? AND workspace_path=? AND state='active'`, executionNow(ctx), newExpiry, l.LeaseID, l.OwnerRunID, l.OwnerAttemptID, l.LeaseEpoch, l.Identity.RepositoryURI, l.Identity.BaseRevision, l.Identity.Branch, l.Identity.HostID, l.Identity.WorkspacePath)
	return casResult(res, err)
}

// RevokeWorkspaceLease fences the exact active lease epoch.
func RevokeWorkspaceLease(ctx context.Context, db *sql.DB, leaseID string, epoch int, reason string) error {
	res, err := db.ExecContext(ctx, `UPDATE workspace_leases SET state='revoked',revoked_at=?,revocation_reason=? WHERE lease_id=? AND lease_epoch=? AND state='active'`, executionNow(ctx), reason, leaseID, epoch)
	return casResult(res, err)
}
func casResult(r sql.Result, e error) error {
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrLeaseConflict
	}
	return nil
}

// SetAttemptProcessIncarnation binds an admitted generation to the native
// process/container identity observed at launch.
func SetAttemptProcessIncarnation(ctx context.Context, db *sql.DB, attemptID string, generation int, pid, pgid int64, containerID string) error {
	res, err := db.ExecContext(ctx, `UPDATE execution_attempts SET pid=?,pgid=?,cgroup_or_container_id=?,process_started_at=?,started_at=?,state='working' WHERE attempt_id=? AND generation=? AND state IN ('submitted','workspace_ready','dispatched')`, pid, pgid, containerID, executionNow(ctx), executionNow(ctx), attemptID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrExecutionConflict
	}
	return nil
}

// ExecutionEvent is immutable evidence. Accepted says whether it was allowed
// to affect the current generation; rejected evidence remains queryable.
type ExecutionEvent struct {
	EventID                            int64
	RunID, AttemptID                   string
	Generation, Sequence               int
	Kind, ProposedState, PayloadDigest string
	Accepted                           bool
	RejectionReason, CreatedAt         string
}

// AppendExecutionEvent appends once by attempt/sequence and fences mutations
// by the parent's current generation. Crier acknowledgements and completed
// proposals are evidence-only because neither has completion authority.
func AppendExecutionEvent(ctx context.Context, db *sql.DB, e ExecutionEvent) (ExecutionEvent, bool, error) {
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return e, false, err
	}
	defer tx.rollback()
	e, replay, err := appendExecutionEventTx(ctx, tx.conn, e)
	if err != nil {
		return e, replay, err
	}
	return e, replay, tx.commit()
}

// appendExecutionEventTx is AppendExecutionEvent's transaction body so the
// parent-run seam can append evidence inside its own transactions. The rules
// are identical: the sequence is allocated from (or validated against) the
// durable per-attempt stream inside the transaction, an event appends once by
// attempt/sequence, state authority is fenced by the parent's current
// generation and by the closed monotonic attempt transitions, and transport
// acks and completion proposals never carry state authority.
func appendExecutionEventTx(ctx context.Context, conn *sql.Conn, e ExecutionEvent) (ExecutionEvent, bool, error) {
	if e.RunID == "" || e.AttemptID == "" || e.Generation < 1 || e.Kind == "" || e.PayloadDigest == "" {
		return e, false, errors.New("append execution event: invalid fields")
	}
	if e.ProposedState != "" && !executionStates[e.ProposedState] {
		return e, false, fmt.Errorf("invalid proposed state %q", e.ProposedState)
	}
	var old ExecutionEvent
	var accepted int
	err := conn.QueryRowContext(ctx, `SELECT event_id,run_id,generation,kind,proposed_state,payload_digest,accepted,rejection_reason,created_at FROM execution_events WHERE attempt_id=? AND sequence=?`, e.AttemptID, e.Sequence).Scan(&old.EventID, &old.RunID, &old.Generation, &old.Kind, &old.ProposedState, &old.PayloadDigest, &accepted, &old.RejectionReason, &old.CreatedAt)
	if err == nil {
		old.AttemptID = e.AttemptID
		old.Sequence = e.Sequence
		old.Accepted = accepted == 1
		if old.RunID != e.RunID || old.PayloadDigest != e.PayloadDigest || old.Kind != e.Kind || old.ProposedState != e.ProposedState || old.Generation != e.Generation {
			return e, false, ErrExecutionConflict
		}
		return old, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return e, false, err
	}
	var nextSeq int
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM execution_events WHERE attempt_id=?`, e.AttemptID).Scan(&nextSeq); err != nil {
		return e, false, err
	}
	if e.Sequence < 1 {
		e.Sequence = nextSeq
	} else if e.Sequence != nextSeq {
		// The caller cannot go backward, collide or skip the durable stream.
		return e, false, fmt.Errorf("%w: sequence %d, next is %d", execution.ErrEvidenceSequence, e.Sequence, nextSeq)
	}
	var current int
	var attemptRun, attemptState string
	var attemptGen int
	if err = conn.QueryRowContext(ctx, `SELECT r.current_attempt_generation,a.run_id,a.generation,a.state FROM execution_runs r JOIN execution_attempts a ON a.run_id=r.run_id WHERE r.run_id=? AND a.attempt_id=?`, e.RunID, e.AttemptID).Scan(&current, &attemptRun, &attemptGen, &attemptState); err != nil {
		return e, false, err
	}
	e.Accepted = true
	if current != e.Generation || attemptGen != e.Generation {
		e.Accepted = false
		e.RejectionReason = "stale_generation"
	}
	// State authority is denied with exactly one reason, in precedence order:
	// stale generation first, then ack/completion authority, then the closed
	// monotonic attempt transitions.
	if e.Accepted && e.Kind == "crier_ack" && e.ProposedState != "" {
		e.Accepted = false
		e.RejectionReason = "crier_ack_has_no_state_authority"
	} else if e.Accepted && e.Kind == "transport_ack" && e.ProposedState != "" {
		e.Accepted = false
		e.RejectionReason = "transport_ack_has_no_state_authority"
	} else if e.Accepted && e.ProposedState == "completed" {
		e.Accepted = false
		e.RejectionReason = "completion_requires_verification_and_projections"
	} else if e.Accepted && e.ProposedState != "" {
		// Closed monotonic attempt transitions: a terminal attempt never
		// resurrects and an active attempt never regresses.
		if execution.IsTerminalAttemptState(attemptState) {
			e.Accepted = false
			e.RejectionReason = "terminal_attempt_cannot_resurrect"
		} else if terr := execution.CanTransitionAttempt(attemptState, e.ProposedState); terr != nil {
			e.Accepted = false
			e.RejectionReason = "illegal_transition"
		}
	}
	now := executionNow(ctx)
	e.CreatedAt = now
	res, err := conn.ExecContext(ctx, `INSERT INTO execution_events(run_id,attempt_id,generation,sequence,kind,proposed_state,payload_digest,accepted,rejection_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, e.RunID, e.AttemptID, e.Generation, e.Sequence, e.Kind, e.ProposedState, e.PayloadDigest, e.Accepted, e.RejectionReason, now)
	if err != nil {
		return e, false, err
	}
	e.EventID, _ = res.LastInsertId()
	if e.Accepted && e.ProposedState != "" {
		if _, err = conn.ExecContext(ctx, `UPDATE execution_attempts SET state=? WHERE attempt_id=? AND generation=?`, e.ProposedState, e.AttemptID, e.Generation); err != nil {
			return e, false, err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE execution_runs SET state=?,updated_at=? WHERE run_id=? AND current_attempt_generation=?`, e.ProposedState, now, e.RunID, e.Generation); err != nil {
			return e, false, err
		}
	}
	return e, false, nil
}

// VerificationReceipt is deterministic verifier evidence keyed to the exact
// run, attempt, criterion, and commit.
type VerificationReceipt struct {
	ReceiptID, RunID, AttemptID, CriterionID, CommitSHA, TreeSHA, State, TrustedEnvironment, ArgvJSON string
	ExitCode                                                                                          sql.NullInt64
	ArtifactDigest, VerifierID, CreatedAt                                                             string
}

// RecordVerificationReceipt stores an identical replay once and conflicts on changed evidence.
func RecordVerificationReceipt(ctx context.Context, db *sql.DB, r VerificationReceipt) (bool, error) {
	if r.ReceiptID == "" || r.RunID == "" || r.AttemptID == "" || r.CriterionID == "" || r.CommitSHA == "" || r.VerifierID == "" {
		return false, errors.New("verification receipt: required field empty")
	}
	if r.State != "passed" && r.State != "failed" && r.State != "error" {
		return false, errors.New("verification receipt: invalid state")
	}
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return false, err
	}
	defer tx.rollback()
	var old VerificationReceipt
	err = tx.conn.QueryRowContext(ctx, `SELECT receipt_id,run_id,attempt_id,criterion_id,commit_sha,tree_sha,state,trusted_environment,argv_json,exit_code,artifact_digest,verifier_id,created_at FROM verification_receipts WHERE receipt_id=? OR (run_id=? AND attempt_id=? AND criterion_id=? AND commit_sha=?) LIMIT 1`, r.ReceiptID, r.RunID, r.AttemptID, r.CriterionID, r.CommitSHA).Scan(&old.ReceiptID, &old.RunID, &old.AttemptID, &old.CriterionID, &old.CommitSHA, &old.TreeSHA, &old.State, &old.TrustedEnvironment, &old.ArgvJSON, &old.ExitCode, &old.ArtifactDigest, &old.VerifierID, &old.CreatedAt)
	if err == nil {
		if !sameVerificationReceipt(old, r) {
			return false, ErrExecutionConflict
		}
		return true, tx.commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	r.CreatedAt = executionNow(ctx)
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO verification_receipts(receipt_id,run_id,attempt_id,criterion_id,commit_sha,tree_sha,state,trusted_environment,argv_json,exit_code,artifact_digest,verifier_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ReceiptID, r.RunID, r.AttemptID, r.CriterionID, r.CommitSHA, r.TreeSHA, r.State, r.TrustedEnvironment, r.ArgvJSON, r.ExitCode, r.ArtifactDigest, r.VerifierID, r.CreatedAt)
	if err != nil {
		return false, err
	}
	return false, tx.commit()
}

func sameVerificationReceipt(a, b VerificationReceipt) bool {
	return a.ReceiptID == b.ReceiptID && a.RunID == b.RunID && a.AttemptID == b.AttemptID &&
		a.CriterionID == b.CriterionID && a.CommitSHA == b.CommitSHA && a.TreeSHA == b.TreeSHA &&
		a.State == b.State && a.TrustedEnvironment == b.TrustedEnvironment && a.ArgvJSON == b.ArgvJSON &&
		a.ExitCode == b.ExitCode && a.ArtifactDigest == b.ArtifactDigest && a.VerifierID == b.VerifierID
}

// ExecutionProjection is a replay-safe external closeout step.
type ExecutionProjection struct {
	RunID, Projection, State, IdempotencyKey, PayloadDigest, RemoteID string
	Attempts                                                          int
	LastError, UpdatedAt                                              string
}

// PutExecutionProjection creates or advances a projection. Its boolean is true
// only for an exact replay; creation and a valid state advance both return
// false. Reusing a projection with a changed key or payload digest conflicts.
func PutExecutionProjection(ctx context.Context, db *sql.DB, p ExecutionProjection) (bool, error) {
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return false, err
	}
	defer tx.rollback()
	replay, err := putExecutionProjectionTx(ctx, tx.conn, p)
	if err != nil {
		return replay, err
	}
	return replay, tx.commit()
}

// putExecutionProjectionTx is PutExecutionProjection's transaction body so the
// parent-run seam can queue its DuckBrain outbox reference atomically with
// parent settlement.
func putExecutionProjectionTx(ctx context.Context, conn *sql.Conn, p ExecutionProjection) (bool, error) {
	if p.RunID == "" || p.Projection == "" || p.IdempotencyKey == "" || p.PayloadDigest == "" {
		return false, errors.New("projection: required field empty")
	}
	var old ExecutionProjection
	err := conn.QueryRowContext(ctx, `SELECT run_id,projection,state,idempotency_key,payload_digest,remote_id,attempts,last_error,updated_at FROM execution_projections WHERE run_id=? AND projection=?`, p.RunID, p.Projection).Scan(&old.RunID, &old.Projection, &old.State, &old.IdempotencyKey, &old.PayloadDigest, &old.RemoteID, &old.Attempts, &old.LastError, &old.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.UpdatedAt = executionNow(ctx)
		_, err = conn.ExecContext(ctx, `INSERT INTO execution_projections(run_id,projection,state,idempotency_key,payload_digest,remote_id,attempts,last_error,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, p.RunID, p.Projection, p.State, p.IdempotencyKey, p.PayloadDigest, p.RemoteID, p.Attempts, p.LastError, p.UpdatedAt)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old.IdempotencyKey != p.IdempotencyKey || old.PayloadDigest != p.PayloadDigest {
		return false, ErrExecutionConflict
	}
	if old.State == p.State && old.RemoteID == p.RemoteID && old.Attempts == p.Attempts && old.LastError == p.LastError {
		return true, nil
	}
	p.UpdatedAt = executionNow(ctx)
	_, err = conn.ExecContext(ctx, `UPDATE execution_projections SET state=?,remote_id=?,attempts=?,last_error=?,updated_at=? WHERE run_id=? AND projection=?`, p.State, p.RemoteID, p.Attempts, p.LastError, p.UpdatedAt, p.RunID, p.Projection)
	if err != nil {
		return false, err
	}
	return false, nil
}

// ClaimExecutionOutbox atomically claims the oldest due dispatch row.
func ClaimExecutionOutbox(ctx context.Context, db *sql.DB, worker string) (OutboxMessage, error) {
	if worker == "" {
		return OutboxMessage{}, errors.New("claim execution outbox: worker required")
	}
	tx, err := beginExecutionTx(ctx, db)
	if err != nil {
		return OutboxMessage{}, err
	}
	defer tx.rollback()
	now := executionNow(ctx)
	var m OutboxMessage
	err = tx.conn.QueryRowContext(ctx, `SELECT outbox_id,run_id,attempt_id,kind,idempotency_key,payload_json,payload_digest,state,attempt_count,available_at,claimed_by,claimed_at,ack_id,last_error,created_at,updated_at FROM execution_outbox WHERE state='pending' AND available_at<=? ORDER BY created_at,outbox_id LIMIT 1`, now).Scan(&m.OutboxID, &m.RunID, &m.AttemptID, &m.Kind, &m.IdempotencyKey, &m.PayloadJSON, &m.PayloadDigest, &m.State, &m.AttemptCount, &m.AvailableAt, &m.ClaimedBy, &m.ClaimedAt, &m.AckID, &m.LastError, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return m, err
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_outbox SET state='claimed',claimed_by=?,claimed_at=?,attempt_count=attempt_count+1,updated_at=? WHERE outbox_id=? AND state='pending'`, worker, now, now, m.OutboxID)
	if err = outboxCAS(res, err); err != nil {
		return m, err
	}
	if err = tx.commit(); err != nil {
		return m, err
	}
	m.State = "claimed"
	m.ClaimedBy = worker
	m.AttemptCount++
	m.ClaimedAt = sql.NullString{String: now, Valid: true}
	return m, nil
}

// AckExecutionOutbox durably records transport acknowledgement without granting completion authority.
func AckExecutionOutbox(ctx context.Context, db *sql.DB, id, worker, ack string) error {
	res, err := db.ExecContext(ctx, `UPDATE execution_outbox SET state='acked',ack_id=?,updated_at=? WHERE outbox_id=? AND state='claimed' AND claimed_by=?`, ack, executionNow(ctx), id, worker)
	return outboxCAS(res, err)
}

// RetryExecutionOutbox releases a claim for a later idempotent delivery attempt.
func RetryExecutionOutbox(ctx context.Context, db *sql.DB, id, worker, availableAt, lastError string) error {
	res, err := db.ExecContext(ctx, `UPDATE execution_outbox SET state='pending',available_at=?,claimed_by='',claimed_at=NULL,last_error=?,updated_at=? WHERE outbox_id=? AND state='claimed' AND claimed_by=?`, availableAt, lastError, executionNow(ctx), id, worker)
	return outboxCAS(res, err)
}

// ReconcileExecutionOutboxUndeliverable is the durable reconciliation path for
// a dispatch that can no longer be delivered: pending/claimed rows move to the
// terminal `dead` state with the reconciliation reason recorded on the row.
// The row is never deleted — it stays as durable proof of the explicit
// reconciled-undeliverable resolution that parent settlement accepts in place
// of an acknowledgement. An identical repeat replays; reconciling an already
// acknowledged (or differently reconciled) dispatch conflicts.
func ReconcileExecutionOutboxUndeliverable(ctx context.Context, db *sql.DB, id, reason string) error {
	if reason == "" {
		return errors.New("reconcile execution outbox: reason required")
	}
	res, err := db.ExecContext(ctx, `UPDATE execution_outbox SET state='dead',last_error=?,updated_at=? WHERE outbox_id=? AND state IN ('pending','claimed')`, reason, executionNow(ctx), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var state, lastError string
	err = db.QueryRowContext(ctx, `SELECT state,last_error FROM execution_outbox WHERE outbox_id=?`, id).Scan(&state, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecutionNotFound
	}
	if err != nil {
		return err
	}
	if state == "dead" && lastError == reason {
		return nil // identical replay
	}
	return ErrExecutionConflict
}
func outboxCAS(r sql.Result, e error) error {
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrExecutionConflict
	}
	return nil
}

// RecoverExecutionState returns all nonterminal runs, active leases, and
// pending/claimed dispatches for restart reconciliation. In particular, it
// never resets or reissues claimed rows: their ambiguous outcome must be
// reconciled explicitly by AckExecutionOutbox or RetryExecutionOutbox.
func RecoverExecutionState(ctx context.Context, db *sql.DB) ([]ExecutionRun, []WorkspaceLease, []OutboxMessage, error) {
	rows, err := db.QueryContext(ctx, executionRunSelect+` WHERE state NOT IN ('completed','failed','canceled','rejected','expired') ORDER BY created_at,run_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	var runs []ExecutionRun
	for rows.Next() {
		r, e := scanExecutionRun(rows)
		if e != nil {
			rows.Close()
			return nil, nil, nil, e
		}
		runs = append(runs, r)
	}
	if err = rows.Close(); err != nil {
		return nil, nil, nil, err
	}
	lr, err := db.QueryContext(ctx, `SELECT lease_id,repository_uri,base_revision,branch,host_id,workspace_path,owner_run_id,owner_attempt_id,lease_epoch,state,acquired_at,renewed_at,expires_at,revoked_at,revocation_reason FROM workspace_leases WHERE state='active' ORDER BY acquired_at,lease_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	var leases []WorkspaceLease
	for lr.Next() {
		var l WorkspaceLease
		if e := lr.Scan(&l.LeaseID, &l.Identity.RepositoryURI, &l.Identity.BaseRevision, &l.Identity.Branch, &l.Identity.HostID, &l.Identity.WorkspacePath, &l.OwnerRunID, &l.OwnerAttemptID, &l.LeaseEpoch, &l.State, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.RevokedAt, &l.RevocationReason); e != nil {
			lr.Close()
			return nil, nil, nil, e
		}
		leases = append(leases, l)
	}
	lr.Close()
	or, err := db.QueryContext(ctx, `SELECT outbox_id,run_id,attempt_id,kind,idempotency_key,payload_json,payload_digest,state,attempt_count,available_at,claimed_by,claimed_at,ack_id,last_error,created_at,updated_at FROM execution_outbox WHERE state IN ('pending','claimed') ORDER BY created_at,outbox_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	var out []OutboxMessage
	for or.Next() {
		var m OutboxMessage
		if e := or.Scan(&m.OutboxID, &m.RunID, &m.AttemptID, &m.Kind, &m.IdempotencyKey, &m.PayloadJSON, &m.PayloadDigest, &m.State, &m.AttemptCount, &m.AvailableAt, &m.ClaimedBy, &m.ClaimedAt, &m.AckID, &m.LastError, &m.CreatedAt, &m.UpdatedAt); e != nil {
			or.Close()
			return nil, nil, nil, e
		}
		out = append(out, m)
	}
	or.Close()
	return runs, leases, out, nil
}
