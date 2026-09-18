package database

import (
	"context"
	"database/sql"
	"fmt"
)

// SpoolEntry is one queued DuckBrain memory write that failed to post and
// awaits replay once the HTTP endpoint is reachable again.
type SpoolEntry struct {
	ID        int64
	MemKey    string
	Domain    string
	Content   string
	Attempts  int
	LastError string
	CreatedAt string
}

// SpoolMemory inserts a failed DuckBrain write into sync_spool for replay.
// This is the fallback path: a write is NEVER dropped silently — if the
// HTTP post fails, it lands here and is replayed on the next sync cycle
// once DuckBrain is reachable (Bane 2026-08-01).
func SpoolMemory(ctx context.Context, db *sql.DB, memKey, domain, content string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO sync_spool (mem_key, domain, content, attempts, created_at)
VALUES (?,?,?,0,?)`, memKey, domain, content, nowUTC(ctx))
	if err != nil {
		return 0, fmt.Errorf("spool memory %q: %w", memKey, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("spool last insert id: %w", err)
	}
	return id, nil
}

// ListSpooledMemories returns spooled writes oldest-first (FIFO replay order).
func ListSpooledMemories(ctx context.Context, db *sql.DB, limit int) ([]SpoolEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.QueryContext(ctx, `SELECT id, mem_key, domain, content, attempts, COALESCE(last_error,''), created_at
FROM sync_spool ORDER BY created_at ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync_spool: %w", err)
	}
	defer rows.Close()

	var out []SpoolEntry
	for rows.Next() {
		var e SpoolEntry
		if err := rows.Scan(&e.ID, &e.MemKey, &e.Domain, &e.Content, &e.Attempts, &e.LastError, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan spool row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountSpooledMemories returns the number of pending spooled writes.
func CountSpooledMemories(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_spool`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sync_spool: %w", err)
	}
	return n, nil
}

// RecordSpoolAttempt bumps the attempt counter and stores the last error.
func RecordSpoolAttempt(ctx context.Context, db *sql.DB, id int64, lastErr string) error {
	_, err := db.ExecContext(ctx, `UPDATE sync_spool SET attempts = attempts + 1, last_error = ? WHERE id = ?`, lastErr, id)
	if err != nil {
		return fmt.Errorf("record spool attempt %d: %w", id, err)
	}
	return nil
}

// DeleteSpooledMemory removes a successfully replayed spool entry.
func DeleteSpooledMemory(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sync_spool WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete spool entry %d: %w", id, err)
	}
	return nil
}

// PruneSpooledMemories removes spool entries that failed too many times
// (attempts >= maxAttempts) so a permanently dead DuckBrain can't grow the
// table unbounded. Returns the number pruned.
func PruneSpooledMemories(ctx context.Context, db *sql.DB, maxAttempts int) (int64, error) {
	if maxAttempts <= 0 {
		maxAttempts = 50
	}
	res, err := db.ExecContext(ctx, `DELETE FROM sync_spool WHERE attempts >= ?`, maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("prune sync_spool: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune rows affected: %w", err)
	}
	return n, nil
}
