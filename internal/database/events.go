package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LogEvent inserts an event row. CreatedAt is set automatically if empty.
func LogEvent(ctx context.Context, db *sql.DB, e *Event) error {
	if e.CreatedAt == "" {
		e.CreatedAt = nowUTC()
	}
	const q = `INSERT INTO events (severity, component, message, details, created_at)
VALUES (?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		string(e.Severity), e.Component,
		e.Message, e.Details, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("log event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("event last insert id: %w", err)
	}
	e.ID = id
	return nil
}

// ListEvents queries the event log with optional filters and pagination.
// severity and component may be empty to skip the respective filter. limit
// caps the result count (use 0 for unbounded); offset skips leading rows.
// Results are newest-first.
func ListEvents(ctx context.Context, db *sql.DB, severity string, component string, limit, offset int) ([]Event, error) {
	conds := []string{}
	args := []any{}
	if severity != "" {
		conds = append(conds, "severity = ?")
		args = append(args, severity)
	}
	if component != "" {
		conds = append(conds, "component = ?")
		args = append(args, component)
	}

	q := `SELECT id, severity, component, message, COALESCE(details,'{}'), created_at FROM events`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			q += " OFFSET ?"
			args = append(args, offset)
		}
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var sevStr string
		if err := rows.Scan(&e.ID, &sevStr, &e.Component,
			&e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		e.Severity = EventSeverity(sevStr)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event rows: %w", err)
	}
	return out, nil
}
