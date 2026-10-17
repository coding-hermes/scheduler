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
// trusted, and planned or blocked foreman steps have no write path at all.
type ParentRunStore struct {
	DB *sql.DB
}

var _ foreman.Store = (*ParentRunStore)(nil)

// NewParentRunStore returns the durable seam over db.
func NewParentRunStore(db *sql.DB) *ParentRunStore { return &ParentRunStore{DB: db} }

const attemptActiveStatesSQL = `('submitted','workspace_ready','dispatched','working','input_required','auth_required','connector_completed','verifying','blocked')`

const parentRunColumns = `run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,current_attempt_generation,parent_state,parent_state_updated_at,parent_brief_json,foreman_invocation_id,foreman_invoked_at,cancel_requested_at,cancel_reason,max_active_attempts,parent_deadline_at,parent_budget_json,cancellation_policy,terminal_outcome_ref,connector_settlement_ref,independent_verification_ref,quiescence_receipt_ref,parent_settled_at,created_at,updated_at`

const attemptColumns = `attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,native_session_id,workspace_lease_id,lease_epoch,submitted_at,started_at,settled_at,step_id,step_generation,enforcement_receipt_ref,start_deadline_at,terminal_outcome_ref,connector_settlement_ref,independent_verification_ref,reconciled_no_start`

// AdmitParentRun persists exactly one parent run on the landed
// CreateExecutionRun idempotency, then fills the parent-plane columns once.
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
	_, replay, err := CreateExecutionRun(ctx, s.DB, ExecutionRun{
		RunID:                    spec.ParentRunID,
		SubmittingAuthority:      spec.SubmittingAuthority,
		SubmissionIdempotencyKey: spec.SubmissionIdempotencyKey,
		BriefID:                  brief.BriefID,
		BriefRevision:            brief.RevisionID,
		BriefDigest:              brief.BriefDigest,
		TickID:                   brief.TickID,
		Lane:                     brief.Lane,
		TaskID:                   brief.TaskID,
	})
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	now := executionNow(ctx)
	// Fill the parent-plane columns exactly once; a crash between the landed
	// insert and this update is repaired by the same guarded statement.
	if _, err := s.DB.ExecContext(ctx, `UPDATE execution_runs SET parent_brief_json=?,max_active_attempts=?,parent_deadline_at=?,parent_budget_json=?,cancellation_policy=?,parent_state='queued',parent_state_updated_at=? WHERE run_id=? AND parent_brief_json=''`,
		spec.ParentBriefJSON, maxActive, brief.DeadlineAt, string(budgetJSON), brief.CancellationPolicy, now, spec.ParentRunID); err != nil {
		return foreman.ParentRecord{}, false, err
	}
	rec, err := s.GetParentRun(ctx, spec.ParentRunID)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if replay && (rec.MaxActiveAttempts != maxActive || rec.DeadlineAt != brief.DeadlineAt || rec.CancellationPolicy != brief.CancellationPolicy) {
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: submission %s/%s has different parent policy", foreman.ErrParentConflict, spec.SubmittingAuthority, spec.SubmissionIdempotencyKey)
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

// InvokeForeman records the foreman invocation and the queued →
// foreman_planning edge.
func (s *ParentRunStore) InvokeForeman(ctx context.Context, parentRunID string, receipt execution.ForemanReceipt) (foreman.ParentRecord, bool, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, parentRunID))
	if err != nil {
		return foreman.ParentRecord{}, false, err
	}
	if rec.ForemanInvocationID != "" {
		if rec.ForemanInvocationID == receipt.InvocationID {
			return rec, true, tx.commit()
		}
		return foreman.ParentRecord{}, false, fmt.Errorf("%w: parent already invoked as %q", foreman.ErrParentConflict, rec.ForemanInvocationID)
	}
	if err := execution.CanTransitionParent(rec.ParentState, execution.ParentForemanPlanning); err != nil {
		return foreman.ParentRecord{}, false, err
	}
	acceptedAt := receipt.AcceptedAt
	if acceptedAt == "" {
		acceptedAt = executionNow(ctx)
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE execution_runs SET foreman_invocation_id=?,foreman_invoked_at=?,parent_state='foreman_planning',parent_state_updated_at=? WHERE run_id=? AND parent_state='queued' AND foreman_invocation_id=''`,
		receipt.InvocationID, acceptedAt, executionNow(ctx), parentRunID)
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
// The complete fail-closed gate — parent executability, step generation
// freshness, route receipt digest and expiry, candidate order, connector
// compatibility and lease authority — runs inside this transaction before any
// row exists, and the attempt idempotency key is re-derived here rather than
// trusted from the caller.
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
	// Replay is answered from the derived key BEFORE the gate: an identical
	// preparation is idempotent even after the parent moved on.
	key := execution.DeriveAttemptIdempotencyKey(rec.ParentRunID, intent.StepID, intent.StepGeneration, intent.Route.ReceiptDigest, intent.Lease.LeaseEpoch)
	if _, ok, e := findAttemptByKey(ctx, tx.conn, key); e != nil {
		return foreman.AttemptRecord{}, false, e
	} else if ok {
		stored, e := scanAttemptRow(tx.conn.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_idempotency_key=?`, key))
		if e != nil {
			return foreman.AttemptRecord{}, false, e
		}
		if !sameStepAttempt(stored, prep, key) {
			return foreman.AttemptRecord{}, false, ErrExecutionConflict
		}
		return stored, true, tx.commit()
	}
	facts, err := admissionFacts(ctx, tx.conn, rec, intent)
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
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_id,run_id,generation,attempt_idempotency_key,state,selected_candidate_rank,requested_model,requested_provider,selected_model,selected_provider,selected_base_url,route_digest,route_receipt_json,connector_id,connector_version,bunker_profile_id,native_session_id,workspace_lease_id,lease_epoch,submitted_at,step_id,step_generation,enforcement_receipt_ref,start_deadline_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		prep.AttemptID, rec.ParentRunID, generation, key, execution.AttemptStateSubmitted,
		intent.Choice.CandidateRank, head.Model, head.Provider,
		chosen.Model, chosen.Provider, chosen.BaseURL,
		intent.Route.ReceiptDigest, string(routeJSON),
		intent.Choice.ConnectorID, intent.Choice.ConnectorVersion, intent.Lease.ProfileID,
		"", intent.Lease.LeaseID, intent.Lease.LeaseEpoch, nowStr,
		intent.StepID, intent.StepGeneration, intent.Lease.EnforcementReceiptRef, startDeadline)
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

func admissionFacts(ctx context.Context, conn *sql.Conn, rec foreman.ParentRecord, intent execution.ReadyStepIntent) (execution.AdmissionFacts, error) {
	facts := execution.AdmissionFacts{
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
	var epoch int
	var state, expires string
	err = conn.QueryRowContext(ctx, `SELECT lease_epoch,state,expires_at FROM workspace_leases WHERE lease_id=?`, intent.Lease.LeaseID).Scan(&epoch, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return facts, nil
	}
	if err != nil {
		return facts, err
	}
	facts.Lease = execution.LeaseFact{Present: true, State: state, Epoch: epoch, ExpiresAt: expires}
	return facts, nil
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

func sameStepAttempt(existing foreman.AttemptRecord, prep foreman.AttemptPreparation, key string) bool {
	intent := prep.Intent
	return existing.AttemptID == prep.AttemptID &&
		existing.AttemptIdempotencyKey == key &&
		existing.ParentRunID == intent.ParentRunID &&
		existing.StepID == intent.StepID &&
		existing.StepGeneration == intent.StepGeneration &&
		existing.RouteDigest == intent.Route.ReceiptDigest &&
		existing.ConnectorID == intent.Choice.ConnectorID &&
		existing.LeaseID == intent.Lease.LeaseID &&
		existing.LeaseEpoch == intent.Lease.LeaseEpoch
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
	var routeJSON, connectorVersion, profileID, enforcementRef, startDeadline string
	var startedAt, settledAt sql.NullString
	err := row.Scan(&a.AttemptID, &a.ParentRunID, &generation, &a.AttemptIdempotencyKey, &a.State,
		&rank, &requestedModel, &requestedProvider, &selectedModel, &selectedProvider, &selectedBaseURL,
		&a.RouteDigest, &routeJSON, &a.ConnectorID, &connectorVersion, &profileID, &a.NativeSessionID,
		&a.LeaseID, &leaseEpoch, &a.SubmittedAt, &startedAt, &settledAt,
		&a.StepID, &stepGeneration, &enforcementRef, &startDeadline,
		&a.TerminalOutcomeRef, &a.ConnectorSettlementRef, &a.IndependentVerificationRef, &noStart)
	if err != nil {
		return a, err
	}
	a.Generation = generation
	a.LeaseEpoch = leaseEpoch
	a.StepGeneration = stepGeneration
	a.EnforcementReceiptRef = enforcementRef
	a.StartDeadlineAt = startDeadline
	a.SettledAt = settledAt.String
	a.ReconciledNoStart = noStart == 1
	return a, nil
}

// AppendEvidence appends immutable attempt evidence, idempotent by
// (attempt, kind, payload digest) and append-only by sequence.
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
	// Idempotent enqueue: identical evidence already appended replays.
	var existing foreman.EvidenceRecord
	err := conn.QueryRowContext(ctx, `SELECT event_id,sequence,created_at FROM execution_events WHERE attempt_id=? AND kind=? AND payload_digest=?`, ev.AttemptID, ev.Kind, ev.PayloadDigest).Scan(&existing.EventID, &existing.Sequence, &existing.CreatedAt)
	if err == nil {
		existing.ParentRunID, existing.AttemptID = ev.ParentRunID, ev.AttemptID
		existing.Generation, existing.Kind, existing.PayloadDigest = ev.Generation, ev.Kind, ev.PayloadDigest
		existing.Accepted = true
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ev, false, err
	}
	sequence := ev.Sequence
	if sequence < 1 {
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM execution_events WHERE attempt_id=?`, ev.AttemptID).Scan(&sequence); err != nil {
			return ev, false, err
		}
	}
	appended, replay, err := appendExecutionEventTx(ctx, conn, ExecutionEvent{
		RunID:         ev.ParentRunID,
		AttemptID:     ev.AttemptID,
		Generation:    attemptGen,
		Sequence:      sequence,
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

// MarkStartUncertain applies executing → uncertain. A repeat while already
// uncertain is idempotent.
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
	if _, _, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
		ParentRunID:   req.ParentRunID,
		AttemptID:     req.AttemptID,
		Generation:    req.Generation,
		Kind:          "start_uncertain",
		PayloadDigest: execution.EvidenceDigest("start_uncertain", req.ParentRunID, req.AttemptID, fmt.Sprintf("%d", req.Generation), req.Reason),
	}); err != nil {
		return rec, err
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
	_, replay, err := appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
		ParentRunID:   req.ParentRunID,
		AttemptID:     req.AttemptID,
		Generation:    req.Generation,
		Kind:          "reconcile_enqueued",
		PayloadDigest: execution.EvidenceDigest("reconcile_enqueued", req.ParentRunID, req.AttemptID, fmt.Sprintf("%d", req.Generation), req.Reason),
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
// reconciling → executing|settled. A repeat of an identical outcome replays.
func (s *ParentRunStore) CompleteReconciliation(ctx context.Context, req foreman.ReconcileOutcome) (foreman.ParentRecord, error) {
	tx, err := beginExecutionTx(ctx, s.DB)
	if err != nil {
		return foreman.ParentRecord{}, err
	}
	defer tx.rollback()
	rec, err := scanParentRun(tx.conn.QueryRowContext(ctx, `SELECT `+parentRunColumns+` FROM execution_runs WHERE run_id=?`, req.ParentRunID))
	if err != nil {
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
		if _, err := tx.conn.ExecContext(ctx, `UPDATE execution_attempts SET state=?,settled_at=?,reconciled_no_start=1 WHERE attempt_id=? AND state IN `+attemptActiveStatesSQL,
			execution.AttemptStateExpired, executionNow(ctx), req.AttemptID); err != nil {
			return rec, err
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

// SettleAttempt settles one attempt only on complete settlement evidence.
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
		} {
			if got != "" && got != want {
				return attempt, fmt.Errorf("%w: attempt %s already settled", foreman.ErrAttemptConflict, settlement.AttemptID)
			}
		}
		if attempt.TerminalOutcomeRef != "" && attempt.ConnectorSettlementRef != "" && attempt.IndependentVerificationRef != "" {
			return attempt, tx.commit() // exact replay
		}
	} else {
		_, _, err = appendEvidenceTx(ctx, tx.conn, foreman.EvidenceRecord{
			ParentRunID:   settlement.ParentRunID,
			AttemptID:     settlement.AttemptID,
			Generation:    attempt.StepGeneration,
			Kind:          "attempt_settlement",
			PayloadDigest: execution.EvidenceDigest("attempt_settlement", settlement.ParentRunID, settlement.AttemptID, settlement.Outcome.OutcomeRef, settlement.Connector.SettlementReceiptRef, settlement.Independent.VerificationRef),
		})
		if err != nil {
			return attempt, err
		}
	}
	if _, err := tx.conn.ExecContext(ctx, `UPDATE execution_attempts SET terminal_outcome_ref=?,connector_settlement_ref=?,independent_verification_ref=?,state=?,settled_at=? WHERE attempt_id=?`,
		settlement.Outcome.OutcomeRef, settlement.Connector.SettlementReceiptRef, settlement.Independent.VerificationRef,
		settlement.Outcome.State, executionNow(ctx), settlement.AttemptID); err != nil {
		return attempt, err
	}
	if err := tx.commit(); err != nil {
		return attempt, err
	}
	return scanAttemptRow(s.DB.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE attempt_id=?`, settlement.AttemptID))
}

// RecordForemanReport applies executing → verifying on the foreman's verified
// parent result.
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

// SettleParent settles the parent only on complete settlement evidence and
// full descendant quiescence, then queues the DuckBrain projection as an
// outbox reference (no network side effect).
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
			rec.IndependentVerificationRef == settlement.Independent.VerificationRef {
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
	settlement.Descendants = settlement.Descendants[:0]
	for _, a := range attempts {
		settlement.Descendants = append(settlement.Descendants, execution.AttemptQuiescence{
			AttemptID:         a.AttemptID,
			Terminal:          execution.IsTerminalAttemptState(a.State),
			ConnectorSettled:  a.ConnectorSettlementRef != "",
			ReconciledNoStart: a.ReconciledNoStart,
		})
	}
	if err := execution.ValidateParentSettlement(settlement); err != nil {
		return rec, err
	}
	if rec.TerminalOutcomeRef != "" && rec.TerminalOutcomeRef != settlement.Outcome.OutcomeRef {
		return rec, fmt.Errorf("%w: reported outcome %q differs from settlement", foreman.ErrParentConflict, rec.TerminalOutcomeRef)
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
