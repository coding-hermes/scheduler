package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DefaultZombieReapThreshold is the default age past which an api_server
// session with ended_at IS NULL is considered a zombie and closed by
// ReapZombieSessions (SCHED-GAP-089). Zero/negative thresholds passed to
// ReapZombieSessions fall back to this value.
const DefaultZombieReapThreshold = 24 * time.Hour

// tableExists reports whether a table with the given name exists in the
// database (checked against sqlite_master).
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check %q table existence: %w", name, err)
	}
	return n > 0, nil
}

// ReapZombieSessions closes api_server sessions older than threshold that
// still have ended_at IS NULL. Timestamps are stored as RFC3339 UTC text, so
// the cutoff is computed in Go and passed as a bound parameter — no
// datetime()/strftime TEXT literals are ever compared against the columns.
//
// The UPDATE backfills ended_at from updated_at (falling back to created_at
// when updated_at is NULL), matching migration v23's backfill semantics, so a
// second run matches zero rows: the operation is idempotent. If the sessions
// table does not exist (this DB layer's schema has no sessions table), the
// function is a no-op returning (0, nil) so daemon startup is never blocked.
func ReapZombieSessions(ctx context.Context, db *sql.DB, threshold time.Duration) (int, error) {
	if threshold <= 0 {
		threshold = DefaultZombieReapThreshold
	}

	exists, err := tableExists(ctx, db, "sessions")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	cutoff := time.Now().UTC().Add(-threshold).Format(time.RFC3339)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin zombie session reap tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE sessions
   SET ended_at = COALESCE(updated_at, created_at)
 WHERE ended_at IS NULL
   AND platform = 'api_server'
   AND COALESCE(updated_at, created_at) < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reap zombie sessions: %w", err)
	}
	reaped, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reap zombie sessions rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit zombie session reap: %w", err)
	}
	return int(reaped), nil
}
