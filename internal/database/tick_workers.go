package database

import (
	"context"
	"database/sql"
	"fmt"
)

// tick_workerColumns is the canonical tick_workers column list shared by
// the INSERT and SELECT paths so the two can never drift apart.
const tickWorkerColumns = `id, tick_id, task_id, branch, worktree, commit_sha, judge, merge, state, cost_usd, tokens_in, tokens_out, created_at, updated_at`

// CreateTickWorker inserts one wave-worker attribution row (S12 §9.1,
// SCHED-GAP-109). All values are bound parameters — manifest data is never
// interpolated into SQL (S12 §13). Defaults are applied for empty enum
// fields (judge=unknown, merge=pending, state=running) and CreatedAt /
// UpdatedAt are stamped when unset, so a caller can pass the minimal
// {TickID, TaskID, Branch} triple. Validate() runs first so an invalid
// vocabulary value fails with a field-named error rather than the raw
// CHECK constraint.
func CreateTickWorker(ctx context.Context, db *sql.DB, w *TickWorker) (int64, error) {
	if w.Judge == "" {
		w.Judge = TickWorkerJudgeUnknown
	}
	if w.Merge == "" {
		w.Merge = TickWorkerMergePending
	}
	if w.State == "" {
		w.State = TickWorkerStateRunning
	}
	if err := w.Validate(); err != nil {
		return 0, err
	}
	if w.CreatedAt == "" {
		w.CreatedAt = nowUTC(ctx)
	}
	if w.UpdatedAt == "" {
		w.UpdatedAt = w.CreatedAt
	}
	const q = `INSERT INTO tick_workers
(` + tickWorkerColumns + `)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		nil, // id: AUTOINCREMENT
		w.TickID, w.TaskID, w.Branch, w.Worktree, w.CommitSHA,
		w.Judge, w.Merge, w.State, w.CostUSD, w.TokensIn, w.TokensOut,
		w.CreatedAt, w.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("create tick worker %q/%q: %w", w.TickID, w.TaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create tick worker %q/%q: last insert id: %w", w.TickID, w.TaskID, err)
	}
	w.ID = id
	return id, nil
}

// UpdateTickWorker updates the mutable worker fields (branch tip, verdicts,
// state, attribution numbers) by id and stamps updated_at with SQLite's
// datetime('now'). The tick_id / task_id / created_at identity columns are
// never rewritten.
func UpdateTickWorker(ctx context.Context, db *sql.DB, w *TickWorker) error {
	if err := w.Validate(); err != nil {
		return err
	}
	const q = `UPDATE tick_workers
SET branch = ?, commit_sha = ?, judge = ?, merge = ?, state = ?, cost_usd = ?, tokens_in = ?, tokens_out = ?, updated_at = datetime('now')
WHERE id = ?`
	res, err := db.ExecContext(ctx, q,
		w.Branch, w.CommitSHA, w.Judge, w.Merge, w.State,
		w.CostUSD, w.TokensIn, w.TokensOut, w.ID)
	if err != nil {
		return fmt.Errorf("update tick worker %d: %w", w.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update tick worker %d: rows affected: %w", w.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("update tick worker %d: no such row", w.ID)
	}
	return nil
}

// ListTickWorkersByTick returns every worker row attributed to the given
// tick, ordered by id (dispatch order). The result is always a non-nil
// (possibly empty) slice so callers can range and JSON-encode it directly.
func ListTickWorkersByTick(ctx context.Context, db *sql.DB, tickID string) ([]TickWorker, error) {
	const q = `SELECT ` + tickWorkerColumns + `
FROM tick_workers WHERE tick_id = ? ORDER BY id ASC`
	rows, err := db.QueryContext(ctx, q, tickID)
	if err != nil {
		return nil, fmt.Errorf("list tick workers for %q: %w", tickID, err)
	}
	defer rows.Close()

	out := make([]TickWorker, 0)
	for rows.Next() {
		var w TickWorker
		if err := rows.Scan(
			&w.ID, &w.TickID, &w.TaskID, &w.Branch, &w.Worktree, &w.CommitSHA,
			&w.Judge, &w.Merge, &w.State, &w.CostUSD, &w.TokensIn, &w.TokensOut,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan tick worker row: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tick workers for %q: %w", tickID, err)
	}
	return out, nil
}

// SetTickWorkerCount persists the number of worker sessions the foreman
// dispatched inside tickID (S12 §5.2). It is written by manifest ingestion
// (SCHED-GAP-110) in the same transaction as the tick_workers rows, so the
// count and the rows can never disagree.
func SetTickWorkerCount(ctx context.Context, db *sql.DB, tickID string, n int) error {
	if n < 0 {
		return fmt.Errorf("SetTickWorkerCount: negative count %d for tick %q", n, tickID)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE ticks SET worker_count = ? WHERE id = ?`, n, tickID)
	if err != nil {
		return fmt.Errorf("set worker_count for tick %q: %w", tickID, err)
	}
	nr, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set worker_count for tick %q: rows affected: %w", tickID, err)
	}
	if nr == 0 {
		return fmt.Errorf("%w: %s", ErrTickNotFound, tickID)
	}
	return nil
}

// CountRunningWorkersByNamespace returns the number of worker sessions
// currently in flight across a namespace's running ticks (the live wave
// depth — S12 §6.2). It joins ticks to projects on project_name so a tick
// only counts toward the namespace its project is assigned to; ticks for
// unassigned or other-namespace projects are excluded. Serial ticks
// contribute their worker_count = 0 and are invisible here, matching the
// pre-v27 world exactly.
func CountRunningWorkersByNamespace(ctx context.Context, db *sql.DB, namespaceID string) (int, error) {
	const q = `SELECT COALESCE(SUM(worker_count),0)
FROM ticks t JOIN projects p ON p.name = t.project_name
WHERE t.status='running' AND p.namespace_id = ?`
	var n int
	if err := db.QueryRowContext(ctx, q, namespaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count running workers for namespace %q: %w", namespaceID, err)
	}
	return n, nil
}

// RunningWave is one in-flight wave tick for the status surfaces (S12 §9.4 /
// §9.5). StartedAt is spawned_at, falling back to created_at for rows that
// never got a spawn stamp.
type RunningWave struct {
	TickID      string
	Project     string
	NamespaceID string
	WorkerCount int
	StartedAt   string
}

// ListRunningWaves returns every running tick with worker_count > 0, joined
// to projects for the namespace (SCHED-GAP-112). One indexed query over
// running ticks (idx_ticks_status) — O(running ticks), no per-project loop —
// so the /api/v1/status wave block stays inside its <2ms budget (S12 §14).
// A tick whose project row was purged still surfaces (LEFT JOIN) with an
// empty namespace. The result is always non-nil so callers can JSON-encode
// it directly as [] rather than null.
func ListRunningWaves(ctx context.Context, db *sql.DB) ([]RunningWave, error) {
	const q = `SELECT t.id, t.project_name, COALESCE(p.namespace_id,''),
COALESCE(t.worker_count,0), COALESCE(NULLIF(t.spawned_at,''), t.created_at)
FROM ticks t LEFT JOIN projects p ON p.name = t.project_name
WHERE t.status='running' AND t.worker_count > 0
ORDER BY t.spawned_at, t.id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list running waves: %w", err)
	}
	defer rows.Close()

	out := make([]RunningWave, 0)
	for rows.Next() {
		var w RunningWave
		if err := rows.Scan(&w.TickID, &w.Project, &w.NamespaceID,
			&w.WorkerCount, &w.StartedAt); err != nil {
			return nil, fmt.Errorf("scan running wave row: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate running waves: %w", err)
	}
	return out, nil
}

// WaveWorkersCapConfigured reports whether at least one namespace sets a
// positive wave_workers_cap (S12 §6.2 item 3) — the
// status.wave_workers_cap_configured flag of S12 §9.4. The namespaces table
// is a handful of rows, so this EXISTS probe is effectively free.
func WaveWorkersCapConfigured(ctx context.Context, db *sql.DB) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM namespaces WHERE wave_workers_cap > 0)`
	var ok bool
	if err := db.QueryRowContext(ctx, q).Scan(&ok); err != nil {
		return false, fmt.Errorf("wave workers cap configured: %w", err)
	}
	return ok, nil
}

// RunningWaveCosts sums the attributed per-worker cost (tick_workers.cost_usd)
// over each RUNNING wave tick (SCHED-GAP-115, S12 §11). Returns the per-tick
// map (tick id → attributed USD) and the fleet total — wave_cost_total, the
// cost twin of wave_depth_total. Attribution is written at completion
// (attributeTickWorkers), so a freshly-spawned wave reads 0 until its rows
// exist; the query is an honest read of what the replica holds, never a
// fabricated estimate (W4: attribution only, never additive).
//
// One indexed join over running ticks (idx_tick_workers_tick) — the same
// O(running ticks) class as ListRunningWaves.
func RunningWaveCosts(ctx context.Context, db *sql.DB) (map[string]float64, float64, error) {
	const q = `SELECT t.id, COALESCE(SUM(w.cost_usd), 0)
FROM ticks t JOIN tick_workers w ON w.tick_id = t.id
WHERE t.status='running' AND t.worker_count > 0
GROUP BY t.id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("running wave costs: %w", err)
	}
	defer rows.Close()

	perTick := make(map[string]float64)
	total := 0.0
	for rows.Next() {
		var id string
		var cost float64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, 0, fmt.Errorf("scan running wave cost: %w", err)
		}
		perTick[id] = cost
		total += cost
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate running wave costs: %w", err)
	}
	return perTick, total, nil
}
