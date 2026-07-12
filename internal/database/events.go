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
	const q = `INSERT INTO events (timestamp, level, project_name, message, detail, created_at)
VALUES (?,?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		e.Timestamp, string(e.Level), nullableString(e.ProjectName),
		e.Message, nullableString(e.Detail), e.CreatedAt)
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
// level and projectName may be empty to skip the respective filter. limit
// caps the result count (use 0 for unbounded); offset skips leading rows.
// Results are newest-first.
func ListEvents(ctx context.Context, db *sql.DB, level string, projectName string, limit, offset int) ([]Event, error) {
	conds := []string{}
	args := []any{}
	if level != "" {
		conds = append(conds, "level = ?")
		args = append(args, level)
	}
	if projectName != "" {
		conds = append(conds, "project_name = ?")
		args = append(args, projectName)
	}

	q := `SELECT id, timestamp, level, COALESCE(project_name,''), message, COALESCE(detail,''), created_at FROM events`
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
		var levelStr string
		if err := rows.Scan(&e.ID, &e.Timestamp, &levelStr, &e.ProjectName,
			&e.Message, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		e.Level = EventLevel(levelStr)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event rows: %w", err)
	}
	return out, nil
}
