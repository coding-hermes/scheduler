package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTickNotFound is returned when a tick lookup or transition targets an
// id that does not exist in the ticks table.
var ErrTickNotFound = errors.New("tick not found")

// CreateTick inserts a new tick row with status='queued'. The caller is
// expected to set Tick.ID (via NextTickID) and Tick.ProjectName; CreatedAt
// is set automatically if empty.
func CreateTick(ctx context.Context, db *sql.DB, t *Tick) error {
	if t.CreatedAt == "" {
		t.CreatedAt = nowUTC()
	}
	if t.Status == "" {
		t.Status = StatusQueued
	}
	const q = `INSERT INTO ticks
(id, project_name, session_id, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, error, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, q,
		t.ID, t.ProjectName, nullableString(t.SessionID), string(t.Status),
		nullableString(string(t.Outcome)),
		nullableString(t.SpawnedAt), nullableString(t.CompletedAt),
		t.ExitCode, t.Commits, t.FilesChanged, t.TokensIn, t.TokensOut,
		t.CostUSD, t.Urgency, t.WeightUsed, nullableString(t.Error), t.CreatedAt)
	if err != nil {
		return fmt.Errorf("create tick %q: %w", t.ID, err)
	}
	return nil
}

// UpdateTickStatus transitions a tick to the given status and records the
// session id. When transitioning to 'running', SpawnedAt is stamped.
func UpdateTickStatus(ctx context.Context, db *sql.DB, id string, status TickStatus, sessionID string) error {
	q := `UPDATE ticks SET status = ?, session_id = ?`
	args := []any{string(status), nullableString(sessionID)}
	if status == StatusRunning {
		q += `, spawned_at = ?`
		args = append(args, nowUTC())
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
	args := []any{string(status), string(outcome), nowUTC(), exitCode, nullableString(errMsg), id}

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
	const q = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, COALESCE(error,''), created_at
FROM ticks WHERE id = ?`
	var t Tick
	var status, outcome string
	err := db.QueryRowContext(ctx, q, id).Scan(
		&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed,
		&t.Error, &t.CreatedAt)
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
	q := `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, COALESCE(error,''), created_at
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
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed,
			&t.Error, &t.CreatedAt); err != nil {
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
	const baseQuery = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, COALESCE(error,''), created_at
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
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed,
			&t.Error, &t.CreatedAt); err != nil {
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
func NextTickID(projectName string) string {
	now := time.Now().UTC()
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
