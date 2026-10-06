package scheduler

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1706 — tick_workers janitor.
//
// A tick's own terminal state is authoritative: once the ticks row is
// completed/failed/timeout, its running worker rows can never finish — the
// ingest path that would flip them to 'done' only runs for live ticks, and
// the reapers' abandonTickWorkers only covers ticks reaped THROUGH
// cleanDanglingOnStartup / reapZombies / reapStaleQueuedRows. A tick that
// went terminal any other way (Stop-drain abort, a completion whose wave
// manifest was never ingested, legacy rows predating SCHED-GAP-114) leaks
// its tick_workers rows in state='running' forever — the fleet DB held ~259
// such rows, the oldest 13 days — and anything waiting on 'running' waits
// forever. This janitor is the backstop: it flips those rows to 'abandoned'
// (UPDATE, never DELETE, so tick attribution stays reconstructable) and
// leaves every other row untouched.
//
// CALLED FROM: Loop.Run (loop.go), once per boot, after
// cleanDanglingOnStartup and reapStaleQueuedRows and BEFORE the resume nudge
// (resumeOrphansAtStartup) — so the resume scan, the in-flight dedup, and
// the namespace-cap admission count never see a leaked running worker row
// from this process forward. Runtime reaps keep abandoning their own workers
// via reapWaveAbandoned; this is the repair for rows that leaked before
// those steps existed.
//
// NOT CLASSIFIED (deliberately untouched):
//   - worker rows already in 'done' or 'abandoned' (any non-running state) —
//     a row never returns from done/abandoned;
//   - running worker rows of a LIVE tick — queued/running/deferred ticks
//     with a fresh updated_at are mid-flight work, and even a live tick that
//     has exceeded its deadline keeps its rows until the age branch below
//     fires, because backstopMaxAge is derived FROM the running ticks'
//     effective deadlines (+grace): a row older than that cutoff belongs to
//     no tick that could still be alive.
const (
	// orphanedWorkerRowsSQL selects the ids of leaking rows: state='running'
	// whose tick is already terminal, or whose updated_at is older than the
	// per-tick wave cap (the SCHED-GAP-217 backstop: MAX effective tick
	// deadline across in-flight running ticks + backstopGrace, floored at
	// 90m — wave-enabled namespaces resolve up to the 4h ceiling, so this
	// single cutoff can never precede any live tick's own deadline).
	// julianday() on both sides for the age comparison: updated_at values
	// are written in mixed formats (Go RFC3339 by CreateTickWorker, SQLite
	// datetime('now') by abandonTickWorkers/UpdateTickWorker), and a raw
	// string comparison would be wrong across the two (same reasoning as
	// staleQueuedRowsSQL). COALESCE covers a legacy NULL updated_at even
	// though the column is NOT NULL by schema — the comparison must always
	// have a usable clock and never fail open on a legacy row shape.
	orphanedWorkerRowsSQL = `
SELECT w.id
FROM tick_workers w
JOIN ticks t ON t.id = w.tick_id
WHERE w.state = 'running'
  AND (t.status IN ('completed','failed','timeout')
       OR julianday(COALESCE(w.updated_at, w.created_at)) < julianday(?))`

	// abandonOrphanedWorkersSQL flips the collected rows to 'abandoned' and
	// stamps updated_at — byte-identical to abandonTickWorkers' UPDATE so
	// both paths leave the same row shape behind. 'abandoned' is the
	// outcome of the tick dying, not of the worker failing (S12 §10.3).
	abandonOrphanedWorkersSQL = `UPDATE tick_workers SET state = ?, updated_at = datetime('now')
 WHERE id IN (`
)

// reapOrphanedWorkerRows runs the SCHED-GAP-1706 janitor pass and returns
// the number of worker rows flipped 'running' → 'abandoned'.
//
// Discipline: collect ids first and close rows BEFORE the UPDATE — SQLite
// allows a single writer, and an UPDATE issued while the SELECT still holds
// the pool's only connection blocks forever (the same contract as
// staleGatewayTicks, abandonTickWorkers, reapStaleQueuedRows).
//
// Best-effort by contract: errors are logged here and the pass returns 0;
// the boot sequence never aborts because the janitor could not run.
func (l *Loop) reapOrphanedWorkerRows() int {
	ctx := context.Background()
	cutoff := l.clock().Now().Add(-l.backstopMaxAge())

	// Rows are consumed and CLOSED before any UPDATE: SQLite single-writer
	// discipline (see reapOrphanedWorkerRows doc comment).
	rows, err := l.db.QueryContext(ctx, orphanedWorkerRowsSQL, cutoff.Format(time.RFC3339))
	if err != nil {
		log.Printf("WORKER-JANITOR: stale worker-row query failed: %v", err)
		return 0
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("WORKER-JANITOR: stale worker-row iteration failed: %v", err)
		return 0
	}
	if len(ids) == 0 {
		return 0
	}

	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, database.TickWorkerStateAbandoned)
	args = append(args, ids...)
	res, err := l.db.ExecContext(ctx,
		abandonOrphanedWorkersSQL+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		log.Printf("WORKER-JANITOR: abandoning %d stale worker rows failed: %v", len(ids), err)
		return 0
	}
	abandoned := len(ids)
	if n, err := res.RowsAffected(); err == nil {
		abandoned = int(n)
	}
	if abandoned > 0 {
		log.Printf("WORKER-JANITOR: abandoned %d stale tick_workers row(s) — tick terminal or older than the wave cap (SCHED-GAP-1706, attribution rows kept)", abandoned)
	}
	return abandoned
}
