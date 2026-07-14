package database

import (
	"context"
	"database/sql"
	"fmt"
)

// InsertNamespaceTick writes a single namespace_tick row. CreatedAt is set
// automatically if empty.
func InsertNamespaceTick(ctx context.Context, db *sql.DB, nt *NamespaceTick) error {
	if nt.CreatedAt == "" {
		nt.CreatedAt = nowUTC()
	}
	const q = `INSERT INTO namespace_ticks
(tick_group, namespace_id, allocated, used, borrowed, lent, job_count, created_at)
VALUES (?,?,?,?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		nt.TickGroup, nt.NamespaceID, nt.Allocated, nt.Used,
		nt.Borrowed, nt.Lent, nt.JobCount, nt.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert namespace_tick: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("namespace_tick last insert id: %w", err)
	}
	nt.ID = id
	return nil
}

// ListNamespaceTicks returns the most recent namespace_ticks for a given
// namespace, newest first. limit caps the result count; pass 0 for unbounded.
func ListNamespaceTicks(ctx context.Context, db *sql.DB, namespaceID string, limit int) ([]NamespaceTick, error) {
	q := `SELECT id, tick_group, namespace_id, allocated, used, borrowed, lent, job_count, created_at
FROM namespace_ticks WHERE namespace_id = ?
ORDER BY created_at DESC`
	args := []any{namespaceID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list namespace ticks: %w", err)
	}
	defer rows.Close()

	var out []NamespaceTick
	for rows.Next() {
		var nt NamespaceTick
		if err := rows.Scan(
			&nt.ID, &nt.TickGroup, &nt.NamespaceID, &nt.Allocated,
			&nt.Used, &nt.Borrowed, &nt.Lent, &nt.JobCount, &nt.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace_tick row: %w", err)
		}
		out = append(out, nt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate namespace_tick rows: %w", err)
	}
	return out, nil
}

// ListNamespaceTicksByGroup returns all namespace_ticks for a given tick_group.
// Used to reconstruct what happened in a specific evaluation cycle.
func ListNamespaceTicksByGroup(ctx context.Context, db *sql.DB, tickGroup string) ([]NamespaceTick, error) {
	const q = `SELECT id, tick_group, namespace_id, allocated, used, borrowed, lent, job_count, created_at
FROM namespace_ticks WHERE tick_group = ?
ORDER BY namespace_id ASC`

	rows, err := db.QueryContext(ctx, q, tickGroup)
	if err != nil {
		return nil, fmt.Errorf("list namespace ticks by group %q: %w", tickGroup, err)
	}
	defer rows.Close()

	var out []NamespaceTick
	for rows.Next() {
		var nt NamespaceTick
		if err := rows.Scan(
			&nt.ID, &nt.TickGroup, &nt.NamespaceID, &nt.Allocated,
			&nt.Used, &nt.Borrowed, &nt.Lent, &nt.JobCount, &nt.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace_tick row: %w", err)
		}
		out = append(out, nt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate namespace_tick rows: %w", err)
	}
	return out, nil
}
