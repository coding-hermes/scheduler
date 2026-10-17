package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

// ParentRunStore is the durable half of the dark parent-run → Hermes foreman
// seam (ACP-SCHED-001). It implements foreman.Store entirely on the landed
// execution schema — execution_runs, execution_attempts, execution_events,
// workspace_leases, execution_projections and execution_outbox are the single
// run/attempt/event store; nothing here is parallel to it.
//
// Every admission is fail-closed: the just-in-time gate runs inside the
// admitting transaction, the attempt idempotency key is re-derived rather than
// trusted, planned or blocked foreman steps have no write path at all, and
// every settlement/reconciliation reference must resolve to a durable record
// bound to this parent/attempt/generation.
type ParentRunStore struct {
	DB *sql.DB
}

var _ foreman.Store = (*ParentRunStore)(nil)

// NewParentRunStore returns the durable seam over db.
func NewParentRunStore(db *sql.DB) *ParentRunStore { return &ParentRunStore{DB: db} }

const attemptActiveStatesSQL = `('submitted','workspace_ready','dispatched','working','input_required','auth_required','connector_completed','verifying','blocked')`

const parentRunColumns = `run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,current_attempt_generation,parent_state,parent_state_updated_at,parent_brief_json,foreman_invocation_id,foreman_invoked_at,cancel_requested_at,cancel_reason,max_active_attempts,parent_deadline_at,parent_budget_json,cancellation_policy,terminal_outcome_ref,connector_settlement_ref,independent_verification_ref,quiescence_receipt_ref,parent_settled_at,created_at,updated_at`

const attemptColumns = `attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,native_session_id,workspace_lease_id,lease_epoch,submitted_at,started_at,settled_at,step_id,step_generation,enforcement_receipt_ref,brief_digest,start_deadline_at,terminal_outcome_ref,connector_settlement_ref,independent_verification_ref,quiescence_receipt_ref,reconciled_no_start,pid,pgid,cgroup_or_container_id,process_started_at`

// AdmitParentRun persists exactly one parent run on the landed
// CreateExecutionRun idempotency, then fills the parent-plane columns in the
// SAME transaction: a second-write failure or crash leaves no visible
// half-parent. A pre-seam (legacy) row is never adopted into a parent.
func (s *ParentRunStore) AdmitParentRun(ctx context.Context, spec foreman.ParentRunSpec) (foreman.ParentRecord, bool, error) {
	brief, err := execution.ValidateParentBriefDigest(spec.ParentBriefJSON)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if brief.ParentRunID != spec.ParentRunID {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: brief names parent %q", foreman.ErrParentConflict, brief.ParentRunID)
	}
	maxActive := spec.MaxActiveAttempts
	if maxActive == 0 {
		maxActive = foreman.DefaultMaxActiveAttempts
	}
	if maxActive < 1 || maxActive > foreman.MaxActiveAttemptsCap {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: max_active_attempts %d outside 1..%d", foreman.ErrParentConflict, maxActive, foreman.MaxActiveAttemptsCap)
	}
	budgetJSON, err := json.Marshal(brief.Budget)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	defer tx.rollback()
	now := executionNow(ctx)
	var existingBrief, existingState, existingParentState, existingAuthority, existingKey string
	err = tx.conn.QueryRowContext(ctx, `SELECT parent_brief_json,state,parent_state,submitting_authority,submission_idempotency_key FROM execution_runs WHERE run_id=?`, spec.ParentRunID).Scan(&existingBrief, &existingState, &existingParentState, &existingAuthority, &existingKey)
	replay := false
	switch {
	case err == nil:
		// The row already exists. Only an identical seam parent replays; a
		// pre-seam (legacy) row is never adopted into a parent, and a bare row
		// only repairs an interrupted admission of THIS submission.
		if existingBrief == "" {
			halfParent := existingState == "submitted" && existingParentState == "queued" &&
				existingAuthority == spec.SubmittingAuthority &&
				existingKey == spec.SubmissionIdempotencyKey
			if !halfParent {
				return foreman.ParentRecord{}, false, fmt.Errorf("%w: run %s is a pre-seam row and cannot be admitted as a parent", foreman.ErrParentConflict, spec.ParentRunID)
			}
		} else {
			replay = true
		}
	case errors.Is(err, sql.ErrNoRows):
		// First stage: the base execution row.
		if _, err := tx.conn.ExecContext(ctx, `INSERT INTO execution_runs
			(run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,'submitted',?,?)`,
			spec.ParentRunID, spec.SubmittingAuthority, spec.SubmissionIdempotencyKey,
			brief.BriefID, brief.RevisionID, brief.BriefDigest, brief.TickID, brief.Lane, brief.TaskID, now, now); err != nil {
			// The submission identity may already exist under another run id:
			// that is an idempotent replay, not a second parent.
			alt, lerr := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE submitting_authority=? AND submission_idempotency_key=?`, spec.SubmittingAuthority, spec.SubmissionIdempotencyKey))
			if lerr != nil {
				return foreman.ParentRecord{}, false, fmt.Errorf("insert parent run: %w", err)
			}
			spec.ParentRunID = alt.ParentRunID
			replay = true
		}
	default:
		return foreman.ParentRecord{}, false, err
	}
	// Second stage: every parent/canonical-brief/policy field, same
	// transaction. The guarded UPDATE fills a half-written row exactly once
	// and is a no-op on replay.
	if _, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_brief_json=?,max_active_attempts=?,parent_deadline_at=?,parent_budget_json=?,cancellation_policy=?,parent_state='queued',parent_state_updated_at=? WHERE run_id=? AND parent_brief_json=''`,
		spec.ParentBriefJSON, maxActive, brief.DeadlineAt, string(budgetJSON), brief.CancellationPolicy, now, spec.ParentRunID); err != nil {
		return foreman.ParentRecord{}, false, fmt.Errorf("fill parent plane: %w", err)
	}
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, spec.ParentRunID))
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if replay {
		if rec.BriefDigest != brief.BriefDigest {
			return foreman.ParentRecord{}, false, fmt.Errorf("%w: submission %s/%s has a different brief digest", foreman.ErrParentConflict, spec.SubmittingAuthority, spec.SubmissionIdempotencyKey)
		}
		if rec.MaxActiveAttempts != maxActive || rec.DeadlineAt != brief.DeadlineAt || rec.CancellationPolicy != brief.CancellationPolicy {
			return foreman.ParentRecord{}, false, fmt.Errorf("%w: submission %s/%s has different parent policy", foreman.ErrParentConflict, spec.SubmittingAuthority, spec.SubmissionIdempotencyKey)
		}
	}
	if err := tx.commit(); err != nil {
		return foreman.ParentRecord{}, false, err
	}
	return rec, replay, nil
}

// GetParentRun reads one parent run.
func (s *ParentRunStore) GetParentRun(ctx context.Context, parentRunID string) (foreman.ParentRecord, error) {
	return scanParentRun(s.DB.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, parentRunID))
}

func scanParentRun(row *sql.Row) (foreman.ParentRecord, error) {
	var r foreman.ParentRecord
	var parentState string
	var invokedAt, cancelAt, settledAt sql.NullString
	var parentStateAt string
	err := row.Scan(&r.ParentRunID, &r.SubmittingAuthority, &r.SubmissionIdempotencyKey,
		&r.BriefID, &r.BriefRevision, &r.BriefDigest, &r.TickID, &r.Lane, &r.TaskID,
		&r.State, &r.CurrentAttemptGeneration, &parentState, &parentStateAt,
		&r.BriefJSON, &r.ForemanInvocationID, &invokedAt, &cancelAt, &r.CancelReason,
		&r.MaxActiveAttempts, &r.DeadlineAt, &r.BudgetJSON, &r.CancellationPolicy,
		&r.TerminalOutcomeRef, &r.ConnectorSettlementRef, &r.IndependentVerificationRef,
		&r.QuiescenceReceiptRef, &settledAt, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, foreman.ErrParentNotFound
	}
	if err != nil {
		return r, err
	}
	r.ParentState = execution.ParentState(parentState)
	if !r.ParentState.Valid() {
		return r, fmt.Errorf("%w: unknown parent state %q", foreman.ErrParentConflict, parentState)
	}
	r.ForemanInvokedAt = invokedAt.String
	r.CancelRequestedAt = cancelAt.String
	r.SettledAt = settledAt.String
	return r, nil
}

// BeginForemanInvocation persists the stable invocation identity BEFORE the
// external boundary is crossed. Only the caller that gets crossing=true may
// invoke the foreman; an already confirmed invocation replays its receipt and
// an unconfirmed intent is an uncertain crossing that must be reconciled
// before any resend.
func (s *ParentRunStore) BeginForemanInvocation(ctx context.Context, parentRunID string) (foreman.InvocationIntent, bool, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.InvocationIntent{}, false, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, parentRunID))
	if err != nil {
		return foreman.InvocationIntent{}, false, err
	}
	intent := foreman.InvocationIntent{InvocationID: rec.ForemanInvocationID, ParentRunID: parentRunID}
	switch {
	case rec.ForemanInvocationID != "" && rec.ForemanInvokedAt != "":
		intent.Receipt = execution.ForemanReceipt{
			InvocationID: rec.ForemanInvocationID,
			ParentRunID:  parentRunID,
			AcceptedAt:   rec.ForemanInvokedAt,
		}
		return intent, false, tx.commit()
	case rec.ForemanInvocationID != "":
		// The identity is durable but the crossing is unconfirmed.
		intent.Uncertain = true
		return intent, false, tx.commit()
	}
	if rec.ParentState != execution.ParentQueued {
		return foreman.InvocationIntent{}, false, fmt.Errorf("%w: cannot invoke foreman in state %q", execution.ErrParentTransitionIllegal, rec.ParentState)
	}
	brief, err := execution.ValidateParentBriefDigest(rec.BriefJSON)
	if err != nil {
		return foreman.InvocationIntent{}, false, err
	}
	invocationID := execution.DeriveForemanInvocationID(parentRunID, brief.BriefDigest)
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET foreman_invocation_id=? WHERE run_id=? AND foreman_invocation_id='' AND parent_state='queued'`,
		invocationID, parentRunID)
	if err != nil {
		return foreman.InvocationIntent{}, false, err
	}
	if err := casResult(res, err); err != nil {
		return foreman.InvocationIntent{}, false, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return foreman.InvocationIntent{}, false, err
	}
	intent.InvocationID = invocationID
	return intent, true, nil
}

// ConfirmForemanInvocation records the receiver-accepted receipt under the
// SAME stable identity and applies the queued → foreman_planning edge. A
// repeat of the identical confirmed invocation replays.
func (s *ParentRunStore) ConfirmForemanInvocation(ctx context.Context, parentRunID string, receipt execution.ForemanReceipt) (foreman.ParentRecord, bool, error) {
	if receipt.InvocationID == "" || receipt.ParentRunID != parentRunID {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: receipt identity incomplete", execution.ErrInvocationIdentity)
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, parentRunID))
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if rec.ForemanInvocationID == "" {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: no invocation intent persisted for %s", execution.ErrInvocationIdentity, parentRunID)
	}
	if rec.ForemanInvocationID != receipt.InvocationID {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: intent %s, receipt %s", execution.ErrInvocationIdentity, rec.ForemanInvocationID, receipt.InvocationID)
	}
	if rec.ForemanInvokedAt != "" {
		if rec.ForemanInvokedAt == receipt.AcceptedAt || receipt.AcceptedAt == "" {
			return rec, true, tx.commit()
		}
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: parent already invoked at %s", foreman.ErrParentConflict, rec.ForemanInvokedAt)
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentForemanPlanning); err != nil {
		return foreman.ParentRecord{}, false, err
	}
	acceptedAt := receipt.AcceptedAt
	if acceptedAt == "" {
		acceptedAt = executionNow(ctx)
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET foreman_invoked_at=?,parent_state='foreman_planning',parent_state_updated_at=? WHERE run_id=? AND parent_state='queued' AND foreman_invocation_id=? AND foreman_invoked_at IS NULL`,
		acceptedAt, executionNow(ctx), parentRunID, receipt.InvocationID)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if err := casResult(res, err); err != nil {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return foreman.ParentRecord{}, false, err
	}
	rec, err = s.GetParentRun(ctx, parentRunID)
	return rec, false, err
}

// PrepareStepAttempt persists exactly one attempt at dispatch preparation.
// The complete fail-closed gate — parent executability, dependency readiness,
// step generation freshness, route receipt digest and expiry, candidate
// order, connector compatibility and lease ownership/authority — runs inside
// this transaction before any row exists, and the attempt idempotency key is
// re-derived here rather than trusted from the caller. A replay of the same
// (parent, step, generation) with ANY changed replay-contract field is an
// idempotency conflict, never a second attempt.
func (s *ParentRunStore) PrepareStepAttempt(ctx context.Context, prep foreman.AttemptPreparation) (foreman.AttemptRecord, bool, error) {
	intent := prep.Intent
	if prep.AttemptID == "" || intent.ParentRunID == "" {
		return foreman.AttemptRecord{}, false, fmt.Errorf("%w: attempt preparation identity incomplete", foreman.ErrSeamInvoked)
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, intent.ParentRunID))
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	key := execution.DeriveAttemptIdempotencyKey(rec.ParentRunID, intent.StepID, intent.StepGeneration, intent.Route.ReceiptDigest, intent.Lease.LeaseEpoch)
	// Replay is answered from the durable (parent, step, generation) identity
	// BEFORE the gate: an identical preparation is idempotent even after the
	// parent moved on, and a changed replay contract is a conflict.
	if stored, ok, e := findStepAttempt(ctx, tx.conn, rec.ParentRunID, intent.StepID, intent.StepGeneration); e != nil {
		return foreman.AttemptRecord{}, false, e
	} else if ok {
		if !sameStepAttempt(stored, prep, key) {
			return foreman.AttemptRecord{}, false, ErrExecutionConflict
		}
		return stored, true, tx.commit()
	}
	if _, ok, e := findAttemptByKey(ctx, tx.conn, key); e != nil {
		return foreman.AttemptRecord{}, false, e
	} else if ok {
		return foreman.AttemptRecord{}, false, ErrExecutionConflict
	}
	facts, err := admissionFacts(ctx, tx.conn, rec, intent, prep.AttemptID)
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	now := clock.FromContext(ctx).Now().UTC()
	nowStr := executionNow(ctx)
	if err := execution.EvaluateAttemptAdmission(now, intent, facts); err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	generation := rec.CurrentAttemptGeneration + 1
	startDeadline := now.Add(time.Duration(intent.Policy.StartTimeoutS) * time.Second).Format(time.RFC3339Nano)
	routeJSON, err := json.Marshal(intent.Route)
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	head := intent.Route.Candidates[0]
	chosen := intent.Route.Candidates[intent.Choice.CandidateRank-1]
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,native_session_id,workspace_lease_id,lease_epoch,submitted_at,step_id,step_generation,enforcement_receipt_ref,brief_digest,start_deadline_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		prep.AttemptID, rec.ParentRunID, generation, key, execution.AttemptStateSubmitted,
		intent.Choice.CandidateRank, head.Model, head.Provider,
		chosen.Model, chosen.Provider, chosen.BaseURL,
		intent.Route.ReceiptDigest, string(routeJSON),
		intent.Choice.ConnectorID, intent.Choice.ConnectorVersion, intent.Lease.ProfileID,
		"", intent.Lease.LeaseID, intent.Lease.LeaseEpoch, nowStr,
		intent.StepID, intent.StepGeneration, intent.Lease.EnforcementReceiptRef, intent.BriefDigest, startDeadline)
	if err != nil {
		return foreman.AttemptRecord{}, false, fmt.Errorf("insert step attempt: %w", err)
	}
	payload, err := dispatchPayload(rec.ParentRunID, prep.AttemptID, generation, key, intent)
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	payloadDigest := sha256.Sum256(payload)
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO execution_outbox(outbox_id,run_id,attempt_id,kind,idempotency_key,payload_json,payload_digest,state,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?,?)`,
		"out-"+prep.AttemptID, rec.ParentRunID, prep.AttemptID, "execution.dispatch",
		"dispatch-"+key, string(payload), "sha256:"+hex.EncodeToString(payloadDigest[:]), nowStr, nowStr, nowStr)
	if err != nil {
		return foreman.AttemptRecord{}, false, fmt.Errorf("insert dispatch outbox: %w", err)
	}
	newParentState := rec.ParentState
	if newParentState == execution.ParentForemanPlanning {
		newParentState = execution.ParentExecuting
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET current_attempt_generation=?,state='submitted',updated_at=?,parent_state=?,parent_state_updated_at=? WHERE run_id=? AND parent_state=?`,
		generation, nowStr, newParentState, nowStr, rec.ParentRunID, rec.ParentState)
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	if err := casResult(res, err); err != nil {
		return foreman.AttemptRecord{}, false, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	attempt, err := scanAttemptRow(s.DB.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, prep.AttemptID))
	return attempt, false, err
}

func findStepAttempt(ctx context.Context, conn *sql.Conn, runID, stepID string, stepGeneration int) (foreman.AttemptRecord, bool, error) {
	var attemptID string
	err := conn.QueryRowContext(ctx, `SELECT attempt_id FROM execution_attempts WHERE run_id=? AND step_id=? AND step_generation=?`, runID, stepID, stepGeneration).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return foreman.AttemptRecord{}, false, nil
	}
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	stored, err := scanAttemptRow(conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, attemptID))
	if err != nil {
		return foreman.AttemptRecord{}, false, err
	}
	return stored, true, nil
}

func admissionFacts(ctx context.Context, conn *sql.Conn, rec foreman.ParentRecord, intent execution.ReadyStepIntent, attemptID string) (execution.AdmissionFacts, error) {
	facts := execution.AdmissionFacts{
		AttemptID:         attemptID,
		ParentState:       rec.ParentState,
		MaxActiveAttempts: rec.MaxActiveAttempts,
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE run_id=? AND state IN `+attemptActiveStatesSQL, rec.ParentRunID).Scan(&facts.ActiveAttempts); err != nil {
		return facts, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT step_generation,state,connector_settlement_ref,reconciled_no_start FROM execution_attempts WHERE run_id=? AND step_id=? ORDER BY step_generation`, rec.ParentRunID, intent.StepID)
	if err != nil {
		return facts, err
	}
	defer rows.Close()
	for rows.Next() {
		var f execution.StepGenerationFact
		var state, settlementRef string
		var noStart int
		if err := rows.Scan(&f.Generation, &state, &settlementRef, &noStart); err != nil {
			return facts, err
		}
		f.Active = execution.IsActiveAttemptState(state)
		f.Quiescent = execution.IsTerminalAttemptState(state) && (settlementRef != "" || noStart == 1)
		facts.StepGenerations = append(facts.StepGenerations, f)
	}
	if err := rows.Close(); err != nil {
		return facts, err
	}
	for _, dep := range intent.Dependencies {
		fact, err := dependencyFact(ctx, conn, rec.ParentRunID, dep.StepID)
		if err != nil {
			return facts, err
		}
		facts.Dependencies = append(facts.Dependencies, fact)
	}
	var epoch int
	var state, expires, ownerRun, ownerAttempt string
	var id execution.WorkspaceIdentityFact
	err = conn.QueryRowContext(ctx, `SELECT lease_epoch,state,expires_at,owner_run_id,owner_attempt_id,repository_uri,base_revision,branch,host_id,workspace_path FROM workspace_leases WHERE lease_id=?`, intent.Lease.LeaseID).Scan(&epoch, &state, &expires, &ownerRun, &ownerAttempt, &id.RepositoryURI, &id.BaseRevision, &id.Branch, &id.HostID, &id.WorkspacePath)
	if errors.Is(err, sql.ErrNoRows) {
		return facts, nil
	}
	if err != nil {
		return facts, err
	}
	facts.Lease = execution.LeaseFact{
		Present: true, State: state, Epoch: epoch, ExpiresAt: expires,
		OwnerRunID: ownerRun, OwnerAttemptID: ownerAttempt, Identity: id,
	}
	return facts, nil
}

// dependencyFact loads the durable readiness fact of one prerequisite step:
// its latest generation must have stopped quiescently, and passing independent
// verification is read from the durable verification receipts — never from a
// caller's claim.
func dependencyFact(ctx context.Context, conn *sql.Conn, runID, stepID string) (execution.DependencyFact, error) {
	fact := execution.DependencyFact{StepID: stepID}
	var state, settlementRef, verificationRef string
	var noStart int
	err := conn.QueryRowContext(ctx, `SELECT state,connector_settlement_ref,independent_verification_ref,reconciled_no_start FROM execution_attempts WHERE run_id=? AND step_id=? ORDER BY step_generation DESC LIMIT 1`, runID, stepID).Scan(&state, &settlementRef, &verificationRef, &noStart)
	if errors.Is(err, sql.ErrNoRows) {
		return fact, nil
	}
	if err != nil {
		return fact, err
	}
	fact.State = state
	fact.Present = execution.IsTerminalAttemptState(state) && (settlementRef != "" || noStart == 1)
	if verificationRef != "" {
		var receiptState string
		if err := conn.QueryRowContext(ctx, `SELECT state FROM verification_receipts WHERE receipt_id=? AND run_id=?`, verificationRef, runID).Scan(&receiptState); err == nil {
			fact.VerifiedPass = execution.VerifyIndependentVerdict(receiptState, execution.VerifierPass)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fact, err
		}
	}
	return fact, nil
}

func dispatchPayload(parentRunID, attemptID string, generation int, key string, intent execution.ReadyStepIntent) ([]byte, error) {
	// The landed execution.dispatch envelope (schemas/execution/v1) — the
	// outbox row is a real dispatch command, not a parallel shape.
	body := map[string]any{
		"schema_version":  "1.0",
		"method":          "execution.dispatch",
		"run_id":          parentRunID,
		"attempt_id":      attemptID,
		"generation":      generation,
		"assignment_ref":  fmt.Sprintf("urn:hermes:assignment:%s/%s/%d", parentRunID, intent.StepID, intent.StepGeneration),
		"brief_digest":    intent.BriefDigest,
		"lease_epoch":     intent.Lease.LeaseEpoch,
		"correlation_id":  key,
		"idempotency_key": key,
		"reply_to":        "urn:hermes:parent-run:" + parentRunID,
	}
	return json.Marshal(body)
}

// sameStepAttempt is the exact replay contract: every route/tool/connector/
// profile/lease/brief field must match the stored attempt, or the preparation
// conflicts instead of silently producing a second attempt.
func sameStepAttempt(existing foreman.AttemptRecord, prep foreman.AttemptPreparation, key string) bool {
	intent := prep.Intent
	chosen := execution.RouteCandidate{}
	if rank := intent.Choice.CandidateRank; rank >= 1 && rank <= len(intent.Route.Candidates) {
		chosen = intent.Route.Candidates[rank-1]
	}
	return existing.AttemptID == prep.AttemptID &&
		existing.AttemptIdempotencyKey == key &&
		existing.ParentRunID == intent.ParentRunID &&
		existing.StepID == intent.StepID &&
		existing.StepGeneration == intent.StepGeneration &&
		existing.BriefDigest == intent.BriefDigest &&
		existing.RouteDigest == intent.Route.ReceiptDigest &&
		existing.CandidateRank == intent.Choice.CandidateRank &&
		existing.ConnectorID == intent.Choice.ConnectorID &&
		existing.ConnectorVersion == intent.Choice.ConnectorVersion &&
		existing.SelectedModel == chosen.Model &&
		existing.SelectedProvider == chosen.Provider &&
		existing.LeaseID == intent.Lease.LeaseID &&
		existing.LeaseEpoch == intent.Lease.LeaseEpoch &&
		existing.ProfileID == intent.Lease.ProfileID &&
		existing.EnforcementReceiptRef == intent.Lease.EnforcementReceiptRef
}

func scanAttemptRow(row *sql.Row) (foreman.AttemptRecord, error) {
	a, err := scanAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, foreman.ErrAttemptNotFound
	}
	return a, err
}

func scanAttempt(row interface{ Scan(...any) error }) (foreman.AttemptRecord, error) {
	var a foreman.AttemptRecord
	var rank, noStart, leaseEpoch, stepGeneration, generation int
	var requestedModel, requestedProvider, selectedModel, selectedProvider, selectedBaseURL string
	var routeJSON, connectorVersion, profileID, enforcementRef, briefDigest, startDeadline string
	var quiescenceRef, cgroup string
	var startedAt, settledAt, processStartedAt sql.NullString
	var pid, pgid sql.NullInt64
	err := row.Scan(&a.AttemptID, &a.ParentRunID, &generation, &a.AttemptIdempotencyKey, &a.State,
		&rank, &requestedModel, &requestedProvider, &selectedModel, &selectedProvider, &selectedBaseURL,
		&a.RouteDigest, &routeJSON, &a.ConnectorID, &connectorVersion, &profileID, &a.NativeSessionID,
		&a.LeaseID, &leaseEpoch, &a.SubmittedAt, &startedAt, &settledAt,
		&a.StepID, &stepGeneration, &enforcementRef, &briefDigest, &startDeadline,
		&a.TerminalOutcomeRef, &a.ConnectorSettlementRef, &a.IndependentVerificationRef, &quiescenceRef, &noStart,
		&pid, &pgid, &cgroup, &processStartedAt)
	if err != nil {
		return a, err
	}
	a.Generation = generation
	a.CandidateRank = rank
	a.ConnectorVersion = connectorVersion
	a.SelectedModel = selectedModel
	a.SelectedProvider = selectedProvider
	a.LeaseEpoch = leaseEpoch
	a.StepGeneration = stepGeneration
	a.ProfileID = profileID
	a.EnforcementReceiptRef = enforcementRef
	a.BriefDigest = briefDigest
	a.StartDeadlineAt = startDeadline
	a.StartedAt = startedAt.String
	a.SettledAt = settledAt.String
	a.QuiescenceReceiptRef = quiescenceRef
	a.ReconciledNoStart = noStart == 1
	a.ProcessBacked = pid.Valid || pgid.Valid || cgroup != "" || processStartedAt.Valid || startedAt.Valid
	return a, nil
}

// AppendEvidence appends immutable attempt evidence, idempotent by
// (attempt, kind, payload digest) and append-only by sequence against the
// durable stream. Replay preserves the original accepted/rejected verdict.
func (s *ParentRunStore) AppendEvidence(ctx context.Context, ev foreman.EvidenceRecord) (foreman.EvidenceRecord, bool, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return ev, false, err
	}
	defer tx.rollback()
	out, replay, err := appendEvidenceTx(ctx, tx.conn, ev)
	if err != nil {
		return out, replay, err
	}
	return out, replay, tx.commit()
}

func appendEvidenceTx(ctx context.Context, conn *sql.Conn, ev foreman.EvidenceRecord) (foreman.EvidenceRecord, bool, error) {
	if ev.ParentRunID == "" || ev.AttemptID == "" || ev.Generation < 1 || ev.Kind == "" || ev.PayloadDigest == "" {
		return ev, false, errors.New("append evidence: invalid fields")
	}
	var attemptRun, state string
	var attemptStepGen, attemptGen int
	if err := conn.QueryRowContext(ctx, `SELECT run_id,generation,step_generation,state FROM execution_attempts WHERE attempt_id=?`, ev.AttemptID).Scan(&attemptRun, &attemptGen, &attemptStepGen, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ev, false, foreman.ErrAttemptNotFound
		}
		return ev, false, err
	}
	if attemptRun != ev.ParentRunID || attemptStepGen != ev.Generation {
		return ev, false, fmt.Errorf("%w: evidence generation %d does not match attempt %s", foreman.ErrAttemptConflict, ev.Generation, ev.AttemptID)
	}
	// Idempotent enqueue: identical evidence already appended replays with
	// its ORIGINAL accepted/rejected verdict — rejected stale evidence never
	// replays as accepted.
	var existing foreman.EvidenceRecord
	var accepted int
	err := conn.QueryRowContext(ctx, `SELECT event_id,run_id,sequence,proposed_state,accepted,rejection_reason,created_at FROM execution_events WHERE attempt_id=? AND kind=? AND payload_digest=?`, ev.AttemptID, ev.Kind, ev.PayloadDigest).Scan(
		&existing.EventID, &existing.ParentRunID, &existing.Sequence, &existing.ProposedState, &accepted, &existing.RejectionReason, &existing.CreatedAt)
	if err == nil {
		existing.AttemptID, existing.Generation = ev.AttemptID, ev.Generation
		existing.Kind, existing.PayloadDigest = ev.Kind, ev.PayloadDigest
		existing.Accepted = accepted == 1
		// Compare every authoritative field before treating this as a replay.
		if existing.ParentRunID != ev.ParentRunID || existing.ProposedState != ev.ProposedState {
			return ev, false, ErrExecutionConflict
		}
		if ev.Sequence > 0 && ev.Sequence != existing.Sequence {
			return ev, false, fmt.Errorf("%w: evidence replay with sequence %d, stored %d", execution.ErrEvidenceSequence, ev.Sequence, existing.Sequence)
		}
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ev, false, err
	}
	appended, replay, err := appendExecutionEventTx(ctx, conn, ExecutionEvent{
		RunID:         ev.ParentRunID,
		AttemptID:     ev.AttemptID,
		Generation:    attemptGen,
		Sequence:      ev.Sequence,
		Kind:          ev.Kind,
		ProposedState: ev.ProposedState,
		PayloadDigest: ev.PayloadDigest,
	})
	if err != nil {
		return ev, false, err
	}
	out := ev
	out.Sequence = appended.Sequence
	out.Accepted = appended.Accepted
	out.RejectionReason = appended.RejectionReason
	out.EventID = appended.EventID
	out.CreatedAt = appended.CreatedAt
	return out, replay, nil
}

// resolveEvidenceRef requires the named reference to resolve to an accepted
// durable execution_events record of the given kind, bound to this
// parent/attempt. Nonempty invented strings are never evidence.
func resolveEvidenceRef(ctx context.Context, conn *sql.Conn, runID, attemptID, kind, ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty %s reference", execution.ErrEvidenceNotDurable, kind)
	}
	q := `SELECT COUNT(*) FROM execution_events WHERE run_id=? AND kind=? AND payload_digest=? AND accepted=1`
	args := []any{runID, kind, ref}
	if attemptID != "" {
		q += ` AND attempt_id=?`
		args = append(args, attemptID)
	}
	var n int
	if err := conn.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s %q does not resolve to a durable record of parent %s", execution.ErrEvidenceNotDurable, kind, ref, runID)
	}
	return nil
}

// resolveVerificationRef requires the independent verification reference to
// resolve to a durable verification_receipts row bound to this run (and
// attempt, when one is named), with the verifier identity and result matching
// the claimed verdict.
func resolveVerificationRef(ctx context.Context, conn *sql.Conn, runID, attemptID, ref, verdict, verifierID string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty verification reference", execution.ErrEvidenceNotDurable)
	}
	var state, receiptRun, receiptAttempt, receiptVerifier string
	err := conn.QueryRowContext(ctx, `SELECT state,run_id,attempt_id,verifier_id FROM verification_receipts WHERE receipt_id=?`, ref).Scan(&state, &receiptRun, &receiptAttempt, &receiptVerifier)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: verification %q does not resolve to a durable receipt", execution.ErrEvidenceNotDurable, ref)
	}
	if err != nil {
		return err
	}
	if receiptRun != runID || (attemptID != "" && receiptAttempt != attemptID) {
		return fmt.Errorf("%w: verification %q is bound to run %s/attempt %s", execution.ErrEvidenceNotDurable, ref, receiptRun, receiptAttempt)
	}
	if verifierID != "" && receiptVerifier != verifierID {
		return fmt.Errorf("%w: verification %q verifier %s", execution.ErrVerificationAuthority, ref, receiptVerifier)
	}
	if !execution.VerifyIndependentVerdict(state, verdict) {
		return fmt.Errorf("%w: verification %q state %s contradicts verdict %s", execution.ErrVerificationAuthority, ref, state, verdict)
	}
	return nil
}

// MarkStartUncertain applies executing → uncertain only on the current active
// attempt with an elapsed persisted start deadline and accepted durable
// evidence. Stale, completed, foreign-generation, premature or rejected
// evidence cannot move the parent to uncertain.
func (s *ParentRunStore) MarkStartUncertain(ctx context.Context, req foreman.UncertainRequest) (foreman.ParentRecord, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, req.ParentRunID))
	if err != nil {
		return rec, err
	}
	if rec.ParentState == execution.ParentUncertain {
		return rec, tx.commit()
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentUncertain); err != nil {
		return rec, err
	}
	attempt, err := scanAttemptRow(tx.conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, req.AttemptID))
	if err != nil {
		return rec, err
	}
	if attempt.ParentRunID != req.ParentRunID {
		return rec, fmt.Errorf("%w: attempt %s is not under parent %s", foreman.ErrAttemptConflict, req.AttemptID, req.ParentRunID)
	}
	if attempt.StepGeneration != req.Generation {
		return rec, fmt.Errorf("%w: evidence generation %d does not match attempt %s", foreman.ErrAttemptConflict, req.Generation, req.AttemptID)
	}
	if !execution.IsActiveAttemptState(attempt.State) {
		return rec, fmt.Errorf("%w: attempt %s is %s", foreman.ErrAttemptConflict, req.AttemptID, attempt.State)
	}
	if attempt.Generation != rec.CurrentAttemptGeneration {
		return rec, fmt.Errorf("%w: attempt %s generation %d is not current", foreman.ErrAttemptConflict, req.AttemptID, attempt.Generation)
	}
	if !execution.IsPreStartAttemptState(attempt.State) || attempt.StartedAt != "" {
		return rec, fmt.Errorf("%w: attempt %s already started", execution.ErrAttemptNotStartPending, req.AttemptID)
	}
	if attempt.StartDeadlineAt == "" {
		return rec, fmt.Errorf("%w: attempt %s has no persisted start deadline", foreman.ErrAttemptConflict, req.AttemptID)
	}
	now := clock.FromContext(ctx).Now().UTC()
	deadline, derr := time.Parse(time.RFC3339Nano, attempt.StartDeadlineAt)
	if derr != nil {
		return rec, fmt.Errorf("%w: start deadline %q", foreman.ErrAttemptConflict, attempt.StartDeadlineAt)
	}
	if now.Before(deadline) {
		return rec, fmt.Errorf("%w: deadline %s", execution.ErrStartDeadlineNotElapsed, attempt.StartDeadlineAt)
	}
	ev, _, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
		ParentRunID:   req.ParentRunID,
		AttemptID:     req.AttemptID,
		Generation:    req.Generation,
		Kind:          execution.EvidenceKindStartUncertain,
		PayloadDigest: execution.EvidenceDigest(execution.EvidenceKindStartUncertain, req.ParentRunID, req.AttemptID, fmt.Sprintf("%d", req.Generation), req.Reason),
	})
	if err != nil {
		return rec, err
	}
	if !ev.Accepted {
		return rec, fmt.Errorf("%w: %s", execution.ErrEvidenceNotAccepted, ev.RejectionReason)
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state='uncertain',parent_state_updated_at=? WHERE run_id=? AND parent_state=?`,
		executionNow(ctx), req.ParentRunID, rec.ParentState)
	if err != nil {
		return rec, err
	}
	if err := casResult(res, err); err != nil {
		return rec, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return rec, err
	}
	return s.GetParentRun(ctx, req.ParentRunID)
}

// BeginReconciliation is enqueue-first: the durable reconciliation request is
// appended to the append-only event log before the uncertain → reconciling
// edge. Identical requests replay.
func (s *ParentRunStore) BeginReconciliation(ctx context.Context, req foreman.ReconcileRequest) (foreman.ParentRecord, bool, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, req.ParentRunID))
	if err != nil {
		return rec, false, err
	}
	attempt, err := scanAttemptRow(tx.conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, req.AttemptID))
	if err != nil {
		return rec, false, err
	}
	if attempt.ParentRunID != req.ParentRunID || attempt.StepGeneration != req.Generation {
		return rec, false, fmt.Errorf("%w: attempt %s is not generation %d of parent %s", foreman.ErrAttemptConflict, req.AttemptID, req.Generation, req.ParentRunID)
	}
	_, replay, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
		ParentRunID:   req.ParentRunID,
		AttemptID:     req.AttemptID,
		Generation:    req.Generation,
		Kind:          execution.EvidenceKindReconcileEnqueued,
		PayloadDigest: execution.EvidenceDigest(execution.EvidenceKindReconcileEnqueued, req.ParentRunID, req.AttemptID, fmt.Sprintf("%d", req.Generation), req.Reason),
	})
	if err != nil {
		return rec, false, err
	}
	if rec.ParentState == execution.ParentReconciling {
		return rec, true, tx.commit()
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentReconciling); err != nil {
		return rec, false, err
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state='reconciling',parent_state_updated_at=? WHERE run_id=? AND parent_state='uncertain'`,
		executionNow(ctx), req.ParentRunID)
	if err != nil {
		return rec, false, err
	}
	if err := casResult(res, err); err != nil {
		return rec, false, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return rec, false, err
	}
	rec, err = s.GetParentRun(ctx, req.ParentRunID)
	return rec, replay, err
}

// CompleteReconciliation records the proven result append-only and applies
// reconciling → executing. It can never manufacture `settled`: settlement
// follows the normal transition/settlement path with full evidence. A no-start
// resolution requires durable no-start proof and the absence of process
// authority; a retry then needs a new generation and route.
func (s *ParentRunStore) CompleteReconciliation(ctx context.Context, req foreman.ReconcileOutcome) (foreman.ParentRecord, error) {
	if req.Next == execution.ParentSettled {
		return foreman.ParentRecord{}, execution.ErrReconciliationSettled
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, req.ParentRunID))
	if err != nil {
		return rec, err
	}
	attempt, err := scanAttemptRow(tx.conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, req.AttemptID))
	if err != nil {
		return rec, err
	}
	if attempt.ParentRunID != req.ParentRunID || attempt.StepGeneration != req.Generation {
		return rec, fmt.Errorf("%w: attempt %s is not generation %d of parent %s", foreman.ErrAttemptConflict, req.AttemptID, req.Generation, req.ParentRunID)
	}
	proofKind := execution.EvidenceKindNoStartProof
	if req.Result.Kind == foreman.ReconcileNativeIdentified {
		proofKind = execution.EvidenceKindNativeIdentification
	}
	if err := resolveEvidenceRef(ctx, tx.conn, req.ParentRunID, req.AttemptID, proofKind, req.Result.EvidenceRef); err != nil {
		return rec, err
	}
	_, replay, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
		ParentRunID:   req.ParentRunID,
		AttemptID:     req.AttemptID,
		Generation:    req.Generation,
		Kind:          "reconcile_" + req.Result.Kind,
		PayloadDigest: execution.EvidenceDigest("reconcile_"+req.Result.Kind, req.ParentRunID, req.AttemptID, fmt.Sprintf("%d", req.Generation), req.Result.EvidenceRef, req.Result.NativeSessionID),
	})
	if err != nil {
		return rec, err
	}
	if replay {
		return rec, tx.commit()
	}
	switch req.Result.Kind {
	case foreman.ReconcileNoStart:
		if attempt.ProcessBacked {
			return rec, fmt.Errorf("%w: attempt %s records process authority", execution.ErrNoStartContradiction, req.AttemptID)
		}
		res, err := tx.conn.ExecContext(ctx, `UPDATE execution_attempts SET state=?,settled_at=?,reconciled_no_start=1 WHERE attempt_id=? AND state IN `+attemptActiveStatesSQL,
			execution.AttemptStateExpired, executionNow(ctx), req.AttemptID)
		if err != nil {
			return rec, err
		}
		if err := casResult(res, err); err != nil {
			return rec, fmt.Errorf("%w: %v", foreman.ErrAttemptConflict, err)
		}
	case foreman.ReconcileNativeIdentified:
		res, err := tx.conn.ExecContext(ctx, `UPDATE execution_attempts SET native_session_id=?,state=? WHERE attempt_id=? AND state IN ('submitted','workspace_ready','dispatched')`,
			req.Result.NativeSessionID, execution.AttemptStateWorking, req.AttemptID)
		if err != nil {
			return rec, err
		}
		if err := casResult(res, err); err != nil {
			return rec, fmt.Errorf("%w: %v", foreman.ErrAttemptConflict, err)
		}
	}
	if err := execution.CanTransitionParent(rec.ParentState, req.Next); err != nil {
		return rec, err
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state=?,parent_state_updated_at=? WHERE run_id=? AND parent_state=?`,
		req.Next, executionNow(ctx), req.ParentRunID, rec.ParentState)
	if err != nil {
		return rec, err
	}
	if err := casResult(res, err); err != nil {
		return rec, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return rec, err
	}
	return s.GetParentRun(ctx, req.ParentRunID)
}

// SettleAttempt settles one attempt only on complete settlement evidence that
// resolves to durable records bound to this parent/attempt/generation.
// Cancellation is monotonic: after a cancellation request a success result
// cannot settle.
func (s *ParentRunStore) SettleAttempt(ctx context.Context, req foreman.AttemptSettlementRequest) (foreman.AttemptRecord, error) {
	settlement := req.Settlement
	if err := execution.ValidateAttemptSettlement(settlement); err != nil {
		return foreman.AttemptRecord{}, err
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.AttemptRecord{}, err
	}
	defer tx.rollback()
	attempt, err := scanAttemptRow(tx.conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, settlement.AttemptID))
	if err != nil {
		return attempt, err
	}
	if attempt.ParentRunID != settlement.ParentRunID {
		return attempt, fmt.Errorf("%w: attempt %s is not under parent %s", foreman.ErrAttemptConflict, settlement.AttemptID, settlement.ParentRunID)
	}
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, settlement.ParentRunID))
	if err != nil {
		return attempt, err
	}
	if (rec.CancelRequestedAt != "" || rec.ParentState == execution.ParentCancelling) && settlement.Outcome.State == execution.AttemptStateCompleted {
		return attempt, fmt.Errorf("%w: cancelled parent cannot settle attempt %s completed", execution.ErrCancellationMonotonic, settlement.AttemptID)
	}
	if execution.IsTerminalAttemptState(attempt.State) {
		// A connector terminal event may land before its settlement receipt.
		// Completing the evidence is allowed; contradicting it is not.
		if attempt.State != settlement.Outcome.State {
			return attempt, fmt.Errorf("%w: attempt %s is %s, settlement claims %s", foreman.ErrAttemptConflict, settlement.AttemptID, attempt.State, settlement.Outcome.State)
		}
		for got, want := range map[string]string{
			attempt.TerminalOutcomeRef:         settlement.Outcome.OutcomeRef,
			attempt.ConnectorSettlementRef:     settlement.Connector.SettlementReceiptRef,
			attempt.IndependentVerificationRef: settlement.Independent.VerificationRef,
			attempt.QuiescenceReceiptRef:       settlement.Connector.QuiescenceReceiptRef,
		} {
			if got != "" && got != want {
				return attempt, fmt.Errorf("%w: attempt %s already settled", foreman.ErrAttemptConflict, settlement.AttemptID)
			}
		}
		if attempt.TerminalOutcomeRef != "" && attempt.ConnectorSettlementRef != "" && attempt.IndependentVerificationRef != "" {
			return attempt, tx.commit() // exact replay
		}
	}
	// Every reference must resolve to durable evidence bound to this
	// parent/attempt/generation.
	if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, settlement.AttemptID, execution.EvidenceKindTerminalOutcome, settlement.Outcome.OutcomeRef); err != nil {
		return attempt, err
	}
	if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, settlement.AttemptID, execution.EvidenceKindConnectorSettlement, settlement.Connector.SettlementReceiptRef); err != nil {
		return attempt, err
	}
	if settlement.Connector.QuiescenceReceiptRef != "" {
		if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, settlement.AttemptID, execution.EvidenceKindConnectorQuiescence, settlement.Connector.QuiescenceReceiptRef); err != nil {
			return attempt, err
		}
	}
	if attempt.ProcessBacked && settlement.Connector.QuiescenceReceiptRef == "" {
		return attempt, fmt.Errorf("%w: attempt %s ran a process", execution.ErrQuiescenceEvidenceRequired, settlement.AttemptID)
	}
	if err := resolveVerificationRef(ctx, tx.conn, settlement.ParentRunID, settlement.AttemptID,
		settlement.Independent.VerificationRef, settlement.Independent.Verdict, settlement.Independent.VerifierID); err != nil {
		return attempt, err
	}
	if !execution.IsTerminalAttemptState(attempt.State) {
		if err := execution.CanTransitionAttempt(attempt.State, settlement.Outcome.State); err != nil {
			return attempt, err
		}
		if _, _, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
			ParentRunID:   settlement.ParentRunID,
			AttemptID:     settlement.AttemptID,
			Generation:    attempt.StepGeneration,
			Kind:          execution.EvidenceKindAttemptSettlement,
			PayloadDigest: execution.EvidenceDigest(execution.EvidenceKindAttemptSettlement, settlement.ParentRunID, settlement.AttemptID, settlement.Outcome.OutcomeRef, settlement.Connector.SettlementReceiptRef, settlement.Independent.VerificationRef),
		}); err != nil {
			return attempt, err
		}
	}
	if _, err := tx.conn.ExecContext(ctx, `UPDATE execution_attempts SET terminal_outcome_ref=?,connector_settlement_ref=?,independent_verification_ref=?,quiescence_receipt_ref=?,state=?,settled_at=? WHERE attempt_id=?`,
		settlement.Outcome.OutcomeRef, settlement.Connector.SettlementReceiptRef, settlement.Independent.VerificationRef,
		settlement.Connector.QuiescenceReceiptRef, settlement.Outcome.State, executionNow(ctx), settlement.AttemptID); err != nil {
		return attempt, err
	}
	if err := tx.commit(); err != nil {
		return attempt, err
	}
	return scanAttemptRow(s.DB.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, settlement.AttemptID))
}

// RecordForemanReport applies executing → verifying on the foreman's verified
// parent result. It can never overwrite connector or independent-verifier
// authority already durably established, and its references must resolve to
// durable records bound to this parent.
func (s *ParentRunStore) RecordForemanReport(ctx context.Context, report execution.ForemanReport) (foreman.ParentRecord, error) {
	if err := execution.ValidateForemanReport(report); err != nil {
		return foreman.ParentRecord{}, err
	}
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, report.ParentRunID))
	if err != nil {
		return rec, err
	}
	if rec.ForemanInvocationID == "" || rec.ForemanInvocationID != report.InvocationID {
		return rec, fmt.Errorf("%w: report is not from the invoked foreman", foreman.ErrParentConflict)
	}
	for got, want := range map[string]string{
		rec.TerminalOutcomeRef:         report.Outcome.OutcomeRef,
		rec.ConnectorSettlementRef:     report.Connector.SettlementReceiptRef,
		rec.IndependentVerificationRef: report.Independent.VerificationRef,
	} {
		if got != "" && got != want {
			return rec, fmt.Errorf("%w: durable authority %q cannot be overwritten by %q", foreman.ErrParentConflict, got, want)
		}
	}
	if rec.CancelRequestedAt != "" && report.Outcome.State == execution.AttemptStateCompleted {
		return rec, fmt.Errorf("%w: cancelled parent cannot report completed", execution.ErrCancellationMonotonic)
	}
	if err := resolveEvidenceRef(ctx, tx.conn, report.ParentRunID, "", execution.EvidenceKindTerminalOutcome, report.Outcome.OutcomeRef); err != nil {
		return rec, err
	}
	if err := resolveEvidenceRef(ctx, tx.conn, report.ParentRunID, "", execution.EvidenceKindConnectorSettlement, report.Connector.SettlementReceiptRef); err != nil {
		return rec, err
	}
	if report.Connector.QuiescenceReceiptRef != "" {
		if err := resolveEvidenceRef(ctx, tx.conn, report.ParentRunID, "", execution.EvidenceKindConnectorQuiescence, report.Connector.QuiescenceReceiptRef); err != nil {
			return rec, err
		}
	}
	if err := resolveVerificationRef(ctx, tx.conn, report.ParentRunID, "",
		report.Independent.VerificationRef, report.Independent.Verdict, report.Independent.VerifierID); err != nil {
		return rec, err
	}
	if rec.ParentState == execution.ParentVerifying {
		if rec.TerminalOutcomeRef == report.Outcome.OutcomeRef && rec.ConnectorSettlementRef == report.Connector.SettlementReceiptRef {
			return rec, tx.commit()
		}
		return rec, fmt.Errorf("%w: parent already verifying a different result", foreman.ErrParentConflict)
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentVerifying); err != nil {
		return rec, err
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state='verifying',parent_state_updated_at=?,terminal_outcome_ref=?,connector_settlement_ref=?,independent_verification_ref=? WHERE run_id=? AND parent_state=?`,
		executionNow(ctx), report.Outcome.OutcomeRef, report.Connector.SettlementReceiptRef, report.Independent.VerificationRef,
		report.ParentRunID, rec.ParentState)
	if err != nil {
		return rec, err
	}
	if err := casResult(res, err); err != nil {
		return rec, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	if err := tx.commit(); err != nil {
		return rec, err
	}
	return s.GetParentRun(ctx, report.ParentRunID)
}

// RequestCancellation is parent-authoritative: it fans out only to currently
// active attempts.
func (s *ParentRunStore) RequestCancellation(ctx context.Context, parentRunID, reason string) (foreman.CancelFanout, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.CancelFanout{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, parentRunID))
	if err != nil {
		return foreman.CancelFanout{}, err
	}
	if rec.ParentState != execution.ParentCancelling {
		if err := execution.CanTransitionParent(rec.ParentState, execution.ParentCancelling); err != nil {
			return foreman.CancelFanout{}, err
		}
		res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state='cancelling',cancel_requested_at=?,cancel_reason=?,parent_state_updated_at=? WHERE run_id=? AND parent_state=?`,
			executionNow(ctx), reason, executionNow(ctx), parentRunID, rec.ParentState)
		if err != nil {
			return foreman.CancelFanout{}, err
		}
		if err := casResult(res, err); err != nil {
			return foreman.CancelFanout{}, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
		}
	}
	active, err := activeAttempts(ctx, tx.conn, parentRunID)
	if err != nil {
		return foreman.CancelFanout{}, err
	}
	if err := tx.commit(); err != nil {
		return foreman.CancelFanout{}, err
	}
	rec, err = s.GetParentRun(ctx, parentRunID)
	if err != nil {
		return foreman.CancelFanout{}, err
	}
	return foreman.CancelFanout{Parent: rec, ActiveAttempts: active, CancelRequestedAt: rec.CancelRequestedAt}, nil
}

func activeAttempts(ctx context.Context, conn *sql.Conn, parentRunID string) ([]foreman.AttemptRecord, error) {
	rows, err := conn.QueryContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE run_id=? AND state IN `+attemptActiveStatesSQL+` ORDER BY generation`, parentRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttemptRows(rows)
}

func scanAttemptRows(rows *sql.Rows) ([]foreman.AttemptRecord, error) {
	var out []foreman.AttemptRecord
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SettleParent settles the parent only on complete settlement evidence that
// resolves to durable records bound to this parent, and full descendant
// quiescence, then queues the DuckBrain projection as an outbox reference (no
// network side effect). Parent connector state, timestamps, attempt state and
// settlement/projection state commit in the same transaction.
func (s *ParentRunStore) SettleParent(ctx context.Context, req foreman.ParentSettlementRequest) (foreman.ParentRecord, error) {
	settlement := req.Settlement
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, settlement.ParentRunID))
	if err != nil {
		return rec, err
	}
	if rec.ParentState == execution.ParentSettled {
		if rec.TerminalOutcomeRef == settlement.Outcome.OutcomeRef &&
			rec.ConnectorSettlementRef == settlement.Connector.SettlementReceiptRef &&
			rec.IndependentVerificationRef == settlement.Independent.VerificationRef &&
			rec.QuiescenceReceiptRef == settlement.QuiescenceReceiptRef {
			return rec, tx.commit()
		}
		return rec, fmt.Errorf("%w: parent already settled", foreman.ErrParentConflict)
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentSettled); err != nil {
		return rec, err
	}
	attempts, err := scanAttemptRowsForParent(ctx, tx.conn, settlement.ParentRunID)
	if err != nil {
		return rec, err
	}
	settlement.FromParentState = rec.ParentState
	settlement.CancelRequested = rec.CancelRequestedAt != ""
	settlement.Descendants = settlement.Descendants[:0]
	for _, a := range attempts {
		settlement.Descendants = append(settlement.Descendants, execution.AttemptQuiescence{
			AttemptID:         a.AttemptID,
			Terminal:          execution.IsTerminalAttemptState(a.State),
			ConnectorSettled:  a.ConnectorSettlementRef != "",
			ReconciledNoStart: a.ReconciledNoStart,
			ProcessBacked:     a.ProcessBacked,
			QuiescenceRef:     a.QuiescenceReceiptRef,
		})
	}
	if err := execution.ValidateParentSettlement(settlement); err != nil {
		return rec, err
	}
	// Every settlement reference must resolve to durable authority bound to
	// this parent.
	if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, "", execution.EvidenceKindTerminalOutcome, settlement.Outcome.OutcomeRef); err != nil {
		return rec, err
	}
	if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, "", execution.EvidenceKindConnectorSettlement, settlement.Connector.SettlementReceiptRef); err != nil {
		return rec, err
	}
	if settlement.QuiescenceReceiptRef != "" {
		if err := resolveEvidenceRef(ctx, tx.conn, settlement.ParentRunID, "", execution.EvidenceKindConnectorQuiescence, settlement.QuiescenceReceiptRef); err != nil {
			return rec, err
		}
	}
	if err := resolveVerificationRef(ctx, tx.conn, settlement.ParentRunID, "",
		settlement.Independent.VerificationRef, settlement.Independent.Verdict, settlement.Independent.VerifierID); err != nil {
		return rec, err
	}
	for got, want := range map[string]string{
		rec.TerminalOutcomeRef:         settlement.Outcome.OutcomeRef,
		rec.ConnectorSettlementRef:     settlement.Connector.SettlementReceiptRef,
		rec.IndependentVerificationRef: settlement.Independent.VerificationRef,
		rec.QuiescenceReceiptRef:       settlement.QuiescenceReceiptRef,
	} {
		if got != "" && got != want {
			return rec, fmt.Errorf("%w: durable authority %q cannot be overwritten by %q", foreman.ErrParentConflict, got, want)
		}
	}
	now := executionNow(ctx)
	verificationState := "failed"
	if settlement.Independent.Verdict == execution.VerifierPass {
		verificationState = "passed"
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET parent_state='settled',parent_state_updated_at=?,parent_settled_at=?,terminal_outcome_ref=?,connector_settlement_ref=?,independent_verification_ref=?,quiescence_receipt_ref=?,state=?,verification_state=?,verdict_id=?,durable_completed_at=?,updated_at=? WHERE run_id=? AND parent_state=?`,
		now, now, settlement.Outcome.OutcomeRef, settlement.Connector.SettlementReceiptRef,
		settlement.Independent.VerificationRef, settlement.QuiescenceReceiptRef,
		settlement.Outcome.State, verificationState, settlement.Independent.VerificationRef, now, now,
		settlement.ParentRunID, rec.ParentState)
	if err != nil {
		return rec, err
	}
	if err := casResult(res, err); err != nil {
		return rec, fmt.Errorf("%w: %v", foreman.ErrParentConflict, err)
	}
	// DuckBrain projection: an outbox reference only in this slice.
	if _, err := putExecutionProjectionTx(ctx, tx.conn, ExecutionProjection{
		RunID:          settlement.ParentRunID,
		Projection:     "duckbrain",
		State:          "pending",
		IdempotencyKey: "duckbrain:" + settlement.ParentRunID + ":" + settlement.Outcome.OutcomeRef,
		PayloadDigest:  execution.EvidenceDigest("duckbrain", settlement.ParentRunID, settlement.Outcome.OutcomeRef, settlement.Independent.VerificationRef),
	}); err != nil {
		return rec, err
	}
	if err := tx.commit(); err != nil {
		return rec, err
	}
	return s.GetParentRun(ctx, settlement.ParentRunID)
}

func scanAttemptRowsForParent(ctx context.Context, conn *sql.Conn, parentRunID string) ([]foreman.AttemptRecord, error) {
	rows, err := conn.QueryContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE run_id=? ORDER BY generation`, parentRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttemptRows(rows)
}
