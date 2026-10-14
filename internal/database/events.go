package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LogEvent inserts an event row. CreatedAt is set automatically if empty.
//
// After the INSERT commits, the row is published to the subscribers of db
// (CTL-002) so live consumers such as GET /api/v1/events/stream see it
// immediately. Publishing happens only on success, and the event carries its
// committed ID — the ID the SSE stream replays from on reconnect.
func LogEvent(ctx context.Context, db *sql.DB, e *Event) error {
	if e.CreatedAt == "" {
		e.CreatedAt = nowUTC(ctx)
	}
	// REMOTE-003 (§1): every event the daemon writes carries its
	// scheduler_id — the writer-attribution join key for federation.
	sid := stampSchedulerID()
	const q = `INSERT INTO events (severity, component, message, details, created_at, scheduler_id)
VALUES (?,?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		string(e.Severity), e.Component,
		e.Message, e.Details, e.CreatedAt, sid)
	if err != nil {
		return fmt.Errorf("log event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("event last insert id: %w", err)
	}
	e.ID = id
	publishEvent(db, *e)
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

	return scanEventRows(rows)
}

// scanEventRows drains an event SELECT into Event values. The column order is
// the one every query in this file selects — id, severity, component, message,
// details, created_at — and the scan is shared so a new query cannot silently
// drift from the others (SCHED-GAP-1737 added the third caller).
func scanEventRows(rows *sql.Rows) ([]Event, error) {
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

// ListEventsTailFiltered returns up to limit of the NEWEST events matching
// severity and component, in chronological order (oldest first) — the bounded
// catch-up window of the SSE stream (SCHED-GAP-1737). Either filter may be
// empty to skip it; both empty means "newest limit events of any kind".
//
// The window is applied AFTER the filter, on purpose: it mirrors
// GET /api/v1/events?severity=X&limit=n (which also returns the newest n
// MATCHING rows), so a client that filters the stream sees its own events even
// when unrelated severities are being written faster than the window.
//
// Empty filters DELEGATE to ListEventsAfterID(ctx, db, 0, limit) so the
// unfiltered window is byte-identical to the pre-filter behaviour of the
// stream — the new path cannot drift from the old one. limit <= 0 is
// unbounded.
func ListEventsTailFiltered(ctx context.Context, db *sql.DB, severity, component string, limit int) ([]Event, error) {
	if severity == "" && component == "" {
		return ListEventsAfterID(ctx, db, 0, limit)
	}

	q := `SELECT id, severity, component, message, COALESCE(details,'{}'), created_at FROM events WHERE 1=1`
	args := []any{}
	if severity != "" {
		q += ` AND severity = ?`
		args = append(args, severity)
	}
	if component != "" {
		q += ` AND component = ?`
		args = append(args, component)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events tail: %w", err)
	}
	defer rows.Close()

	out, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	// Newest-first fetch → chronological output, matching ListEventsAfterID's
	// documented ordering so a stream consumer appends forward in one ordering.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// MaxEventID returns the highest event id in the log, or 0 when the log is
// empty. The SSE stream uses it as the starting cursor of a tail-seeded client
// (SCHED-GAP-1737): a client that connects without a Last-Event-ID is joining
// "now", so its cursor is the log head rather than the id of the newest frame
// its filter happened to emit — which keeps the cursor definition (an id
// watermark) the same on every path, and keeps the handler from holding cursor
// 0 while the log is non-empty.
//
// COALESCE keeps an empty log a plain 0 rather than a NULL scan error; the
// lookup is a MAX over the primary key, so it is an index read, not a scan.
func MaxEventID(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("max event id: %w", err)
	}
	return id, nil
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

	out, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	if afterID <= 0 {
		// Newest-first fetch → chronological output (see doc comment).
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}
