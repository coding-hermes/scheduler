package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ErrTickNotFound is returned when a tick lookup or transition targets an
// id that does not exist in the ticks table.
var ErrTickNotFound = errors.New("tick not found")

// CreateTick inserts a new tick row with status='queued'. The caller is
// expected to set Tick.ID (via NextTickID) and Tick.ProjectName; CreatedAt
// is set automatically if empty.
func CreateTick(ctx context.Context, db *sql.DB, t *Tick) error {
	if t.CreatedAt == "" {
		t.CreatedAt = nowUTC(ctx)
	}
	if t.Status == "" {
		t.Status = StatusQueued
	}
	const q = `INSERT INTO ticks
(id, project_name, session_id, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, cost_source, urgency, weight_used, error, created_at, bump, worker_count, wave_recovery)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, q,
		t.ID, t.ProjectName, nullableString(t.SessionID), string(t.Status),
		nullableString(string(t.Outcome)),
		nullableString(t.SpawnedAt), nullableString(t.CompletedAt),
		t.ExitCode, t.Commits, t.FilesChanged, t.TokensIn, t.TokensOut,
		t.CostUSD, t.CostSource, t.Urgency, t.WeightUsed, nullableString(t.Error), t.CreatedAt,
		t.Bump, t.WorkerCount, t.WaveRecovery)
	if err != nil {
		return fmt.Errorf("create tick %q: %w", t.ID, err)
	}
	return nil
}

// RecordTickAdmission persists the SCHED-GAP-157 admission-lifecycle stamp on
// an existing tick row: how long the tick waited for a slot (slotWait, already
// measured by the caller through internal/clock), the admission decision
// that let it in (admitReason, one of the scheduler's SCHED-GAP-155 vocabulary
// strings), and the SCHED-GAP-1597 selection facts (urgency, weightUsed — the
// packer's computed urgency and effective weight at the admit boundary; 0/0 for
// a tick no packer selected, which is the honest value for manual and resume
// spawns). nudgeSource ("startup" | "manual" | "board_wake") is stored only
// when non-empty, so a packer tick never overwrites the nudge-source stamp its
// own enqueue path wrote.
//
// SCHED-GAP-1597: urgency/weight_used/slot_wait_ms/admit_reason previously read
// as never-written over 7 days of production rows because the packer path
// stamped BEFORE the row existed (the UPDATE matched 0 rows and was silently
// lost). The caller must enqueue the row FIRST; this writer reports
// ErrTickNotFound when it does not.
//
// Best-effort by contract: the stamp is observability, never a gate — an
// unknown id or a write error is returned and logged by the caller, and can
// never change the tick's scheduling outcome.
func RecordTickAdmission(ctx context.Context, db *sql.DB, id string, slotWait time.Duration, admitReason, nudgeSource string, urgency float64, weightUsed int) error {
	q := `UPDATE ticks SET slot_wait_ms = ?, admit_reason = ?, urgency = ?, weight_used = ?`
	args := []any{slotWait.Milliseconds(), admitReason, urgency, weightUsed}
	if nudgeSource != "" {
		q += `, nudge_source = ?`
		args = append(args, nudgeSource)
	}
	q += ` WHERE id = ?`
	args = append(args, id)

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("record tick admission %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for tick %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	return nil
}

// Deferral is one recorded SCHED-GAP-157 pass-over: a candidate project that
// an evaluation pass decided NOT to admit, with the reason from the
// SCHED-GAP-155 vocabulary. "why was this lane skipped in window Y" is one
// query over this table instead of log-line order across a rotated file.
type Deferral struct {
	ID          int64  `json:"id"` // AUTOINCREMENT PK
	ProjectName string `json:"project_name"`
	Reason      string `json:"reason"`
	PassID      int64  `json:"pass_id"`
	Detail      string `json:"detail"`
	CreatedAt   string `json:"created_at"`
}

// RecordDeferral writes one pass-over record. Best-effort: observability, not
// admission state — callers log a failure and move on. created_at comes from
// the context clock (SCHED-GAP-169) when one is installed.
func RecordDeferral(ctx context.Context, db *sql.DB, projectName, reason string, passID int64, detail string) error {
	if projectName == "" {
		return errors.New("RecordDeferral: projectName must not be empty")
	}
	if reason == "" {
		return errors.New("RecordDeferral: reason must not be empty")
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO deferrals (project_name, reason, pass_id, detail, created_at)
VALUES (?, ?, ?, ?, ?)
`, projectName, reason, passID, detail, nowUTC(ctx))
	if err != nil {
		return fmt.Errorf("record deferral for %q: %w", projectName, err)
	}
	return nil
}

// ListDeferrals returns deferral records newest first, optionally filtered by
// project (empty = all), with offset pagination; limit 0 = unbounded.
func ListDeferrals(ctx context.Context, db *sql.DB, projectName string, limit, offset int) ([]Deferral, error) {
	q := `SELECT id, project_name, reason, pass_id, detail, created_at FROM deferrals`
	args := []any{}
	if projectName != "" {
		q += ` WHERE project_name = ?`
		args = append(args, projectName)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	if offset > 0 {
		q += ` OFFSET ?`
		args = append(args, offset)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list deferrals: %w", err)
	}
	defer rows.Close()
	var out []Deferral
	for rows.Next() {
		var d Deferral
		if err := rows.Scan(&d.ID, &d.ProjectName, &d.Reason, &d.PassID, &d.Detail, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan deferral row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateTickStatus transitions a tick to the given status and records the
// session id. When transitioning to 'running', SpawnedAt is stamped.
func UpdateTickStatus(ctx context.Context, db *sql.DB, id string, status TickStatus, sessionID string) error {
	q := `UPDATE ticks SET status = ?, session_id = ?`
	args := []any{string(status), nullableString(sessionID)}
	if status == StatusRunning {
		q += `, spawned_at = ?`
		args = append(args, nowUTC(ctx))
	}
	q += ` WHERE id = ?`
	args = append(args, id)

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update tick status %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for tick %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	return nil
}

// CompleteTick finalizes a tick: sets status to 'completed' or 'failed',
// records the outcome, stamps CompletedAt, and persists the exit code and
// error string (if any).
func CompleteTick(ctx context.Context, db *sql.DB, id string, outcome TickOutcome, exitCode int, errMsg string) error {
	status := StatusCompleted
	if outcome == OutcomeFailed || outcome == OutcomeTimeout {
		status = StatusFailed
		if outcome == OutcomeTimeout {
			status = StatusTimeout
		}
	}
	q := `UPDATE ticks
SET status = ?, outcome = ?, completed_at = ?, exit_code = ?, error = ?
WHERE id = ?`
	args := []any{string(status), string(outcome), nowUTC(ctx), exitCode, nullableString(errMsg), id}

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("complete tick %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for tick %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	return nil
}

// RecordTickMetrics persists the post-run metrics (commits, files, tokens,
// cost, urgency, weight) for a completed tick.
//
// SCHED-GAP-1597: in production the urgency/weight_used columns are stamped at
// the admit boundary by RecordTickAdmission (the packer's selection-time
// values); this writer's urgency/weight arguments are for the test/seeding
// path only and would overwrite an admission stamp if used on a live row.
func RecordTickMetrics(ctx context.Context, db *sql.DB, id string, commits, filesChanged, weightUsed int, tokensIn, tokensOut int64, costUSD, urgency float64) error {
	q := `UPDATE ticks
SET commits = ?, files_changed = ?, tokens_in = ?, tokens_out = ?, cost_usd = ?, urgency = ?, weight_used = ?
WHERE id = ?`
	res, err := db.ExecContext(ctx, q, commits, filesChanged, tokensIn, tokensOut, costUSD, urgency, weightUsed, id)
	if err != nil {
		return fmt.Errorf("record tick metrics %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for tick %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	return nil
}

// GetTick loads a single tick by id.
func GetTick(ctx context.Context, db *sql.DB, id string) (*Tick, error) {
	const q = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, COALESCE(cost_source,''), COALESCE(error,''), created_at, COALESCE(code_commits,0), COALESCE(board_commits,0), COALESCE(bump,0), COALESCE(worker_count,0), COALESCE(wave_recovery,0), COALESCE(slot_wait_ms,0), COALESCE(admit_reason,''), COALESCE(nudge_source,'')
FROM ticks WHERE id = ?`
	var t Tick
	var status, outcome string
	err := db.QueryRowContext(ctx, q, id).Scan(
		&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.CostSource,
		&t.Error, &t.CreatedAt, &t.CodeCommits, &t.BoardCommits,
		&t.Bump, &t.WorkerCount, &t.WaveRecovery,
		&t.SlotWaitMs, &t.AdmitReason, &t.NudgeSource)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get tick %q: %w", id, err)
	}
	t.Status = TickStatus(status)
	t.Outcome = TickOutcome(outcome)
	return &t, nil
}

// ListTicks returns the most recent ticks for a project, newest first.
// If projectName is empty, ticks across all projects are returned.
// limit caps the result count; pass 0 for an unbounded query (the caller
// should usually bound it).
func ListTicks(ctx context.Context, db *sql.DB, projectName string, limit int) ([]Tick, error) {
	q := `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, COALESCE(cost_source,''), COALESCE(error,''), created_at, COALESCE(code_commits,0), COALESCE(board_commits,0), COALESCE(bump,0), COALESCE(worker_count,0), COALESCE(wave_recovery,0), COALESCE(slot_wait_ms,0), COALESCE(admit_reason,''), COALESCE(nudge_source,'')
FROM ticks`
	args := []any{}
	if projectName != "" {
		q += " WHERE project_name = ?"
		args = append(args, projectName)
	}
	q += " ORDER BY created_at DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list ticks: %w", err)
	}
	defer rows.Close()

	var out []Tick
	for rows.Next() {
		var t Tick
		var status, outcome string
		if err := rows.Scan(
			&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
			&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.CostSource,
			&t.Error, &t.CreatedAt, &t.CodeCommits, &t.BoardCommits,
			&t.Bump, &t.WorkerCount, &t.WaveRecovery,
			&t.SlotWaitMs, &t.AdmitReason, &t.NudgeSource); err != nil {
			return nil, fmt.Errorf("scan tick row: %w", err)
		}
		t.Status = TickStatus(status)
		t.Outcome = TickOutcome(outcome)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tick rows: %w", err)
	}
	return out, nil
}

// ListAllTicks returns ticks across all projects, newest first, with offset
// pagination. limit caps the result count; pass 0 for an unbounded query.
func ListAllTicks(ctx context.Context, db *sql.DB, limit, offset int) ([]Tick, error) {
	const baseQuery = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, COALESCE(cost_source,''), COALESCE(error,''), created_at, COALESCE(code_commits,0), COALESCE(board_commits,0), COALESCE(bump,0), COALESCE(worker_count,0), COALESCE(wave_recovery,0), COALESCE(slot_wait_ms,0), COALESCE(admit_reason,''), COALESCE(nudge_source,'')
FROM ticks ORDER BY created_at DESC, id DESC`

	q := baseQuery
	args := []any{}
	if offset < 0 {
		offset = 0
	}
	if limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	} else if offset > 0 {
		q += " LIMIT -1 OFFSET ?"
		args = append(args, offset)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list all ticks: %w", err)
	}
	defer rows.Close()

	var out []Tick
	for rows.Next() {
		var t Tick
		var status, outcome string
		if err := rows.Scan(
			&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
			&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.CostSource,
			&t.Error, &t.CreatedAt, &t.CodeCommits, &t.BoardCommits,
			&t.Bump, &t.WorkerCount, &t.WaveRecovery,
			&t.SlotWaitMs, &t.AdmitReason, &t.NudgeSource); err != nil {
			return nil, fmt.Errorf("scan all tick row: %w", err)
		}
		t.Status = TickStatus(status)
		t.Outcome = TickOutcome(outcome)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate all tick rows: %w", err)
	}
	return out, nil
}

// PruneOldTicks deletes all but the keep most recent ticks for the given
// project (ranked by created_at descending). If keep <= 0, all ticks for
// the project are deleted.
func PruneOldTicks(ctx context.Context, db *sql.DB, projectName string, keep int) error {
	if projectName == "" {
		return errors.New("PruneOldTicks: projectName must not be empty")
	}
	q := `DELETE FROM ticks
WHERE project_name = ? AND id NOT IN (
    SELECT id FROM ticks WHERE project_name = ?
    ORDER BY created_at DESC LIMIT ?
)`
	_, err := db.ExecContext(ctx, q, projectName, projectName, keep)
	if err != nil {
		return fmt.Errorf("prune ticks for %q (keep %d): %w", projectName, keep, err)
	}
	return nil
}

// NextTickID generates a tick id in the format:
//
//	<project>-<YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>
//
// The timestamp is UTC. Two ticks created in the same second for the same
// project will collide — callers are expected to serialize spawning per
// project (enforced by the cooldown).
// The clock comes from the context (SCHED-GAP-169), exactly like nowUTC, so
// a simulated run generates tick ids on the simulated timeline instead of
// stamping real wall-clock ids into a virtual world.
func NextTickID(ctx context.Context, projectName string) string {
	now := clock.FromContext(ctx).Now().UTC()
	return fmt.Sprintf("%s-%04d-%02d-%02d-%02d-%02d-%02d",
		projectName, now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), now.Second())
}

// nullableString returns nil for an empty string so the column stores NULL
// rather than the empty string. This keeps optional fields cleanly
// distinguishable from present-but-empty values.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
