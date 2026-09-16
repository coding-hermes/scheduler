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

// ListEventsAfterID returns events strictly newer than afterID (id > afterID)
// for incremental consumers such as the MCP events_list tool — the id > ?
// predicate lives in SQL so a reader can cheaply poll the tail of the log.
// Unlike ListEvents there are no severity/component/offset parameters
// (callers post-filter in Go) and no pagination beyond limit.
//
// Ordering semantics (deliberate choice, documented here): afterID > 0
// streams the tail chronologically (ORDER BY id ASC). afterID <= 0 means "no
// cursor" and returns the newest limit rows — fetched newest-first exactly
// like ListEvents, then reversed, so BOTH modes share one output ordering
// (oldest→newest) and a consumer appending forward sees a stable stream.
// limit <= 0 is unbounded.
func ListEventsAfterID(ctx context.Context, db *sql.DB, afterID int64, limit int) ([]Event, error) {
	q := `SELECT id, severity, component, message, COALESCE(details,'{}'), created_at FROM events`
	args := []any{}
	if afterID > 0 {
		q += ` WHERE id > ?`
		args = append(args, afterID)
		q += ` ORDER BY id ASC`
	} else {
		q += ` ORDER BY id DESC`
	}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events after id: %w", err)
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
	if afterID <= 0 {
		// Newest-first fetch → chronological output (see doc comment).
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}
