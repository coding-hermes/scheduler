package scheduler

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ReapZombieSessions closes api_server sessions in the local SQLite database
// that are older than threshold and still have ended_at IS NULL (zombie
// sessions that inflate metrics/billing — SCHED-GAP-089). A threshold <= 0
// selects database.DefaultZombieReapThreshold (24h). The reaped row count is
// logged on success. The operation is idempotent (a second run reaps zero
// rows) and performs no external HTTP calls — all work goes through the
// existing local database layer.
func ReapZombieSessions(ctx context.Context, db *sql.DB, threshold time.Duration) (int, error) {
	if threshold <= 0 {
		threshold = database.DefaultZombieReapThreshold
	}
	reaped, err := database.ReapZombieSessions(ctx, db, threshold)
	if err != nil {
		return 0, err
	}
	log.Printf("ZOMBIE-REAPER: reaped %d zombie session(s) (threshold %s)", reaped, threshold)
	return reaped, nil
}
