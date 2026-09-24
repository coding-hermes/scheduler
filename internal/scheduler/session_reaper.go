package scheduler

import (
	"context"
	"database/sql"
	"log"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ReapStaleHermesSessions runs one SCHED-GAP-089 reap pass against the
// agent state database and logs the outcome. It is a thin, reusable seam
// over database.ReapStaleHermesSessions — the real schema (source TEXT +
// epoch REALs) lives there, along with the safety model:
//
//   - cfg is the caller's EXPLICIT decision. The zero value is a dry-run;
//     an Apply pass only happens when the operator asked for one.
//   - db must be an OPEN state.db handle (database.OpenHermesStateDB —
//     mode=rw, never creates, busy-timeout against the live agent).
//   - A wrong-shaped database fails closed (database.ErrHermesSessionsShape);
//     the error is returned, never swallowed.
//
// Nothing here runs on a timer: the daemon does not schedule this function
// (no bespoke cron — SCHED-GAP-089 round 2 deliberately leaves cadence to
// the operator, e.g. a one-shot `--reap-sessions` run). state.db is the live
// agent's own database and the scheduler has no liveness handshake with the
// gateway, so automated writes stay out of scope until that changes.
func ReapStaleHermesSessions(ctx context.Context, db *sql.DB, cfg database.HermesReaperConfig) (database.Result, error) {
	res, err := database.ReapStaleHermesSessions(ctx, db, cfg)
	if err != nil {
		return database.Result{}, err
	}
	if cfg.Apply {
		log.Printf("SESSION-REAPER: closed %d stale api_server session(s) (candidates %d, stalest idle %s, threshold %s)",
			res.Reaped, res.Candidates, res.OldestIdle, cfg.Normalized().StaleAfter)
	} else {
		log.Printf("SESSION-REAPER (dry-run): %d stale api_server session(s) would be closed (stalest idle %s, threshold %s)",
			res.Candidates, res.OldestIdle, cfg.Normalized().StaleAfter)
	}
	return res, nil
}
