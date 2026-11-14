package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultDeferralRetention is the conservative operator default. A zero
// retention period is handled by the CLI as "keep forever"; it is never
// silently converted to a delete window.
const DefaultDeferralRetention = 14 * 24 * time.Hour

const defaultDeferralBatchSize = 1000

// DeferralRetentionStats is a read-only snapshot used by the retention command
// and its operator report. PayloadBytes counts only reason/detail text; the
// database file size is reported separately by the command.
type DeferralRetentionStats struct {
	Rows         int64
	PayloadBytes int64
	Oldest       string
	Newest       string
	OverdueRows  int64
}

// DeferralRetentionReport describes one bounded retention pass.
type DeferralRetentionReport struct {
	RowsBefore    int64
	RowsDeleted   int64
	RowsAfter     int64
	PayloadBefore int64
	PayloadAfter  int64
	OverdueBefore int64
	OverdueAfter  int64
	Retention     time.Duration
	BatchSize     int
	KeepForever   bool
}

// ValidateDeferralRetention rejects negative windows and invalid batch sizes.
// Zero is deliberately valid and means keep forever.
func ValidateDeferralRetention(retention time.Duration, batchSize int) error {
	if retention < 0 {
		return errors.New("deferral retention must be >= 0 (0 = keep forever)")
	}
	if batchSize <= 0 {
		return errors.New("deferral retention batch size must be > 0")
	}
	return nil
}

// DefaultDeferralBatch returns the bounded batch size used by operators that do
// not provide one. It is exposed for the CLI without duplicating the policy.
func DefaultDeferralBatch() int { return defaultDeferralBatchSize }

// DeferralRetentionSnapshot reads deferral counts and payload sizes without
// changing the database. When retention is non-zero, overdue rows are rows
// older than twice the configured window: this is the silent-regrowth guard.
func DeferralRetentionSnapshot(ctx context.Context, db *sql.DB, now time.Time, retention time.Duration) (DeferralRetentionStats, error) {
	if db == nil {
		return DeferralRetentionStats{}, errors.New("deferral retention snapshot: nil database")
	}
	if retention < 0 {
		return DeferralRetentionStats{}, errors.New("deferral retention snapshot: retention must be >= 0")
	}
	var stats DeferralRetentionStats
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(length(reason) + length(detail)), 0),
       COALESCE(MIN(created_at), ''),
       COALESCE(MAX(created_at), '')
FROM deferrals`).Scan(&stats.Rows, &stats.PayloadBytes, &stats.Oldest, &stats.Newest); err != nil {
		return DeferralRetentionStats{}, fmt.Errorf("snapshot deferrals: %w", err)
	}
	if retention > 0 {
		cutoff := now.UTC().Add(-2 * retention).Format(time.RFC3339)
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deferrals WHERE created_at < ?`, cutoff).Scan(&stats.OverdueRows); err != nil {
			return DeferralRetentionStats{}, fmt.Errorf("count overdue deferrals: %w", err)
		}
	}
	return stats, nil
}

// PruneDeferrals deletes at most batchSize rows older than cutoff. Each call is
// one short transaction, so callers can pause between batches and keep a live
// daemon responsive. The caller owns the loop and decides when to VACUUM.
func PruneDeferrals(ctx context.Context, db *sql.DB, cutoff time.Time, batchSize int) (int64, error) {
	if db == nil {
		return 0, errors.New("prune deferrals: nil database")
	}
	if batchSize <= 0 {
		return 0, errors.New("prune deferrals: batch size must be > 0")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin deferral prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
DELETE FROM deferrals
WHERE id IN (
    SELECT id FROM deferrals
    WHERE created_at < ?
    ORDER BY created_at ASC, id ASC
    LIMIT ?
)`, cutoff.UTC().Format(time.RFC3339), batchSize)
	if err != nil {
		return 0, fmt.Errorf("delete deferrals batch: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted deferrals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit deferral prune: %w", err)
	}
	return deleted, nil
}

// CopyDatabase creates a consistent SQLite copy without mutating the source.
// The destination must not already exist, preventing an accidental overwrite.
// VACUUM INTO requires a writable SQLite connection, but this helper sends no
// source DML: the only created file is the destination copy.
func CopyDatabase(ctx context.Context, source, destination string) error {
	if source == "" || destination == "" {
		return errors.New("copy database: source and destination are required")
	}
	if filepath.Clean(source) == filepath.Clean(destination) {
		return errors.New("copy database: source and destination must differ")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("copy database: destination %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("copy database: stat destination: %w", err)
	}
	if dir := filepath.Dir(destination); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("copy database: create destination directory: %w", err)
		}
	}
	// SQLite requires the connection issuing VACUUM INTO to be writable even
	// though the source file is not changed. No INSERT/UPDATE/DELETE is sent to
	// the source; the destination is the only file created by this operation.
	src, err := sql.Open("sqlite", source)
	if err != nil {
		return fmt.Errorf("copy database: open source: %w", err)
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if _, err := src.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("copy database: configure source: %w", err)
	}
	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("copy database with VACUUM INTO: %w", err)
	}
	return nil
}
