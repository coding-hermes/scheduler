package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// TickFilter is the server-side search/filter for the tick history
// (SCHED-GAP-1593). Empty fields are skipped. Query is a case-insensitive
// substring match against tick id AND project name; the other fields are
// exact matches.
type TickFilter struct {
	Project string
	Query   string
	Status  string
	Outcome string
}

// likeEscape escapes LIKE wildcards in user input so a filter value such as
// "100%" matches the literal string, not "100<anything>". Pairs with the
// ESCAPE '\' clause in the query.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// tickFilterConds builds the shared WHERE clause for ListTicksFiltered.
func tickFilterConds(f TickFilter) (string, []any) {
	conds := []string{}
	args := []any{}
	if f.Project != "" {
		conds = append(conds, "project_name = ?")
		args = append(args, f.Project)
	}
	if f.Query != "" {
		conds = append(conds, "(id LIKE ? ESCAPE '\\' OR project_name LIKE ? ESCAPE '\\')")
		pat := "%" + likeEscape(f.Query) + "%"
		args = append(args, pat, pat)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.Outcome != "" {
		conds = append(conds, "outcome = ?")
		args = append(args, f.Outcome)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListTicksFiltered returns ticks matching f, newest first, with offset
// pagination, plus the TOTAL number of matching rows (for page counts).
// limit <= 0 means unbounded (callers should bound it).
func ListTicksFiltered(ctx context.Context, db *sql.DB, f TickFilter, limit, offset int) ([]Tick, int, error) {
	where, args := tickFilterConds(f)

	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count filtered ticks: %w", err)
	}

	q := `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, COALESCE(cost_source,''), COALESCE(error,''), created_at, COALESCE(code_commits,0), COALESCE(board_commits,0), COALESCE(bump,0), COALESCE(worker_count,0), COALESCE(wave_recovery,0), COALESCE(slot_wait_ms,0), COALESCE(admit_reason,''), COALESCE(nudge_source,'')
FROM ticks` + where + ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
		if offset > 0 {
			q += ` OFFSET ?`
			args = append(args, offset)
		}
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list filtered ticks: %w", err)
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
			return nil, 0, fmt.Errorf("scan filtered tick row: %w", err)
		}
		t.Status = TickStatus(status)
		t.Outcome = TickOutcome(outcome)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate filtered tick rows: %w", err)
	}
	return out, total, nil
}

// GetTickWithTrace loads one tick plus its raw gateway_trace JSON
// (SCHED-GAP-119 per-POST record; carries the real agent session id the
// drill-down page resolves). ErrTickNotFound when the id is unknown.
func GetTickWithTrace(ctx context.Context, db *sql.DB, id string) (*Tick, string, error) {
	const q = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, COALESCE(cost_source,''), COALESCE(error,''), created_at, COALESCE(code_commits,0), COALESCE(board_commits,0), COALESCE(bump,0), COALESCE(worker_count,0), COALESCE(wave_recovery,0), COALESCE(slot_wait_ms,0), COALESCE(admit_reason,''), COALESCE(nudge_source,''), COALESCE(gateway_trace,'')
FROM ticks WHERE id = ?`
	var (
		t               Tick
		status, outcome string
		trace           string
	)
	err := db.QueryRowContext(ctx, q, id).Scan(
		&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.CostSource,
		&t.Error, &t.CreatedAt, &t.CodeCommits, &t.BoardCommits,
		&t.Bump, &t.WorkerCount, &t.WaveRecovery,
		&t.SlotWaitMs, &t.AdmitReason, &t.NudgeSource, &trace)
	if err == sql.ErrNoRows {
		return nil, "", fmt.Errorf("%w: %s", ErrTickNotFound, id)
	}
	if err != nil {
		return nil, "", fmt.Errorf("get tick %q with trace: %w", id, err)
	}
	t.Status = TickStatus(status)
	t.Outcome = TickOutcome(outcome)
	return &t, trace, nil
}

// DistinctTickProjects returns the project names present in the ticks
// table (alphabetical), for the tick-history filter dropdown.
func DistinctTickProjects(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT project_name FROM ticks ORDER BY project_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list distinct tick projects: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan distinct tick project: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate distinct tick projects: %w", err)
	}
	return out, nil
}

// ListEventsRecent returns up to limit newest events (newest first). The
// drill-down page scans this small tail in Go because the events table
// carries no tick-id column — per-tick selection is a best-effort match
// (see the dashboard's tick detail page for the honest-labelling contract).
func ListEventsRecent(ctx context.Context, db *sql.DB, limit int) ([]Event, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, severity, component, message, COALESCE(details,'{}'), created_at
FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var sev string
		if err := rows.Scan(&e.ID, &sev, &e.Component, &e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recent event row: %w", err)
		}
		e.Severity = EventSeverity(sev)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent event rows: %w", err)
	}
	return out, nil
}
