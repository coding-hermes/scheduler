package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §8 (SCHED-GAP-114): reaper abandonment + wave recovery trigger ────
//
// Two halves, both deliberately narrow (spec §8.4): the scheduler keeps the
// tick row truthful and the recovery trigger observable, and NEVER destroys
// the artifacts (W6 — no git worktree call, no os.RemoveAll on manifest or
// worktree data; the reaper has no workdir authority and no merge authority;
// deciding whether a half-finished branch is good belongs to the NEXT
// foreman, which has a gate runner and merge authority).
//
//  1. REAPER ABANDONMENT — both reaper paths (cleanDanglingOnStartup,
//     reapZombies) keep their tick semantics unchanged (status='timeout',
//     outcome unset, completed_at per GAP-045, orphan stamps per
//     SCHED-GAP-091) and gain ONE shared step, abandonTickWorkers: the
//     reaped tick's tick_workers rows flip state='running' → 'abandoned'
//     (§10.3). The reaper never touches worker PROCESSES — tick_workers has
//     no pid column (§9.1) and the workers are grandchildren of a dead
//     foreman session anyway; there is nothing to double-kill.
//
//  2. RECOVERY TRIGGER — at spawn time (before prompt assembly, serial ticks
//     included), a bounded readdir of <workdir>/.coding-hermes/waves/*.json
//     with tolerant parse finds manifests whose own tick row is TERMINAL and
//     whose finished_at is empty (waveManifestUnfinished). Such a project's
//     next tick is stamped wave_recovery=1 (§9.1: "set at spawn so the row
//     itself records that the tick was a recovery tick") and its prompt gets
//     the recover-before-dispatch preamble. One recovery tick at a time —
//     recovery is dispatched like any other tick (one slot), never as an
//     extra parallel spawn (§8.2 item 6).

// waveScanMaxManifests bounds the recovery scan's readdir (same class as
// countBoardRows, adaptive_cooldown.go): at most this many *.json entries
// are considered, sorted by name for determinism. A pathological waves/
// directory cannot drag the spawn path (spec §14: < 5ms per project).
const waveScanMaxManifests = 64

// wavePreambleMaxManifests bounds how many unfinished manifests the recovery
// preamble lists. The scan stays tolerant up to waveScanMaxManifests; the
// PROMPT truncates beyond this so a stale-manifest pileup cannot bloat the
// foreman prompt unboundedly (truncation is noted in the block).
const wavePreambleMaxManifests = 8

// abandonTickWorkers is the reapers' ONE shared wave step (S12 §8.2 item 3,
// §10.3): after a tick row has been reaped terminal, its tick_workers rows
// in state 'running' flip to 'abandoned'. Rows already 'done' stay 'done' —
// a row never returns from done/abandoned, and 'abandoned' is the outcome of
// the tick dying, not of the worker failing. worker_count on the tick row is
// deliberately NOT touched (§8.2 item 7: a reaped wave does not
// double-count progress — the count already persisted, adaptive cooldown
// reads the tick as a no-progress timeout exactly like a serial one).
//
// Discipline: collect ids first and close rows BEFORE the UPDATE — SQLite
// allows a single writer, and an UPDATE issued while the SELECT still holds
// the pool's only connection blocks forever (the same reason the reapers
// collect-then-update, tick_process.go).
//
// Returns (waveSize, abandoned): the tick's total worker rows and how many
// flipped. Both are 0 for a serial tick (no rows) — callers log the REAPER
// line only when waveSize > 0 so serial reaps stay quiet. Errors are logged
// here and returned for test visibility; the reaper never aborts because a
// worker row failed to flip.
func abandonTickWorkers(ctx context.Context, db *sql.DB, tickID string) (waveSize, abandoned int, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, state FROM tick_workers WHERE tick_id = ?`, tickID)
	if err != nil {
		return 0, 0, fmt.Errorf("wave abandon %s: select tick_workers: %w", tickID, err)
	}
	var ids []any
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("wave abandon %s: scan tick_workers: %w", tickID, err)
		}
		waveSize++
		if state == database.TickWorkerStateRunning {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return waveSize, 0, fmt.Errorf("wave abandon %s: iterate tick_workers: %w", tickID, err)
	}
	if len(ids) == 0 {
		return waveSize, 0, nil
	}
	placeholders := make([]string, len(ids))
	for i := range ids {
		placeholders[i] = "?"
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, database.TickWorkerStateAbandoned)
	args = append(args, ids...)
	res, err := db.ExecContext(ctx,
		`UPDATE tick_workers SET state = ?, updated_at = datetime('now')
		 WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return waveSize, 0, fmt.Errorf("wave abandon %s: update tick_workers: %w", tickID, err)
	}
	if n, err := res.RowsAffected(); err == nil {
		abandoned = int(n)
	}
	return waveSize, abandoned, nil
}

// reapWaveAbandoned runs the shared wave step for one just-reaped tick and
// emits the grep-able REAPER line (tick id, wave size, abandoned count) when
// the tick carried a wave. Both reaper paths call this immediately after a
// successful timeoutReapSQL — nowhere else.
func (l *Loop) reapWaveAbandoned(ctx context.Context, tickID string) {
	waveSize, abandoned, err := abandonTickWorkers(ctx, l.db, tickID)
	if err != nil {
		log.Printf("REAPER: tick %s wave abandonment incomplete: %v", tickID, err)
		return
	}
	if waveSize > 0 {
		log.Printf("REAPER: tick %s wave abandoned — wave_size=%d abandoned=%d (worktrees preserved, W6)",
			tickID, waveSize, abandoned)
	}
}

// unfinishedWaveManifests is the recovery trigger's scan (S12 §8.2 item 5):
// a bounded, tolerant readdir of <workdir>/.coding-hermes/waves/*.json that
// returns every manifest which (a) parses cleanly under the §9.3 rules
// (bounded size, tolerant JSON, tick_id matching its filename) AND
// (b) has finished_at == "" (the foreman died before closing its wave)
// AND (c) whose own tick row is TERMINAL (completed/failed/timeout — a
// manifest whose tick is still queued/running is a LIVE wave, not a
// recovery candidate, and a manifest whose tick row is gone cannot be
// verified and is conservatively skipped).
//
// Every failure mode degrades to "not a candidate": missing waves dir,
// unreadable dir, malformed/oversize manifest, unknown tick row. The scan
// can never fail a spawn (§9.3 fail-safe doctrine).
func unfinishedWaveManifests(ctx context.Context, db *sql.DB, workdir string) []*WaveManifest {
	dir := filepath.Join(workdir, ".coding-hermes", "waves")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no waves dir (serial project) or unreadable — nothing to do
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
		if len(names) >= waveScanMaxManifests {
			break
		}
	}
	sort.Strings(names) // deterministic scan + preamble

	var out []*WaveManifest
	for _, name := range names {
		path := filepath.Join(dir, name)
		if !waveManifestUnfinished(path) {
			continue
		}
		if !tickRowTerminal(ctx, db, waveTickIDFromPath(path)) {
			continue
		}
		// Re-parse for the preamble payload. waveManifestUnfinished just
		// proved the manifest parses and matches its filename, so the only
		// error path here is a file that changed between the two reads —
		// tolerate it by skipping (tolerant-parse doctrine).
		m, err := parseWaveManifest(path, waveTickIDFromPath(path))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// tickRowTerminal reports whether the tick row with the given id exists and
// is in a terminal state (completed/failed/timeout). queued/running rows and
// missing rows return false.
func tickRowTerminal(ctx context.Context, db *sql.DB, tickID string) bool {
	var status string
	err := db.QueryRowContext(ctx, `SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
	if err != nil {
		return false // missing row or query fault — not a verifiable candidate
	}
	switch status {
	case string(TickCompleted), string(TickFailed), string(TickTimeout):
		return true
	default:
		return false // queued / running — a live wave, never a recovery trigger
	}
}

// markTickWaveRecovery stamps wave_recovery=1 on the tick row (S12 §9.1:
// the row itself records that the tick ran the wave-recovery phase). Called
// from the spawn path the moment the scan finds recovery work, so dashboards
// and /api/v1/ticks can count recovery ticks even if the tick later fails.
func markTickWaveRecovery(ctx context.Context, db *sql.DB, tickID string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE ticks SET wave_recovery = 1 WHERE id = ?`, tickID)
	if err != nil {
		return fmt.Errorf("mark wave_recovery %s: %w", tickID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("mark wave_recovery %s: no such tick row", tickID)
	}
	return nil
}

// waveRecoveryPreamble renders the recover-before-dispatch preamble (S12
// §8.2 item 5): a short fenced block listing the unfinished worker set from
// the manifest(s) and instructing the foreman to recover that work FIRST —
// re-run gates on each preserved branch, merge green branches serially or
// dispatch a fixup worker in a fresh worktree, close the board truth, then
// (and only then) proceed with the new tick's normal work / compose a new
// wave. PURE function of the manifests: no db, no fs — unit-testable and
// deterministic. Returns "" for an empty manifest set (clean project → the
// prompt stays byte-identical to the pre-114 builder).
func waveRecoveryPreamble(manifests []*WaveManifest) string {
	if len(manifests) == 0 {
		return ""
	}
	listed := manifests
	truncated := 0
	if len(listed) > wavePreambleMaxManifests {
		truncated = len(listed) - wavePreambleMaxManifests
		listed = listed[:wavePreambleMaxManifests]
	}
	var b strings.Builder
	b.WriteString("```text\n")
	b.WriteString("=== WAVE RECOVERY — RECOVER BEFORE DISPATCH ===\n")
	b.WriteString("A previous wave tick ended before its wave was finished. Its branches and\n")
	b.WriteString("worktrees are preserved as evidence. Recover this work FIRST, before any new task:\n")
	b.WriteString("1. For each worker below: re-run the gates on its branch tip.\n")
	b.WriteString("   - green → merge into main (serially, one merge at a time, gates re-run on the merged tree).\n")
	b.WriteString("   - red   → dispatch a fixup worker in a FRESH worktree (never hand-resolve).\n")
	b.WriteString("2. Board truth: every affected task complete, or failed WITH a reason.\n")
	b.WriteString("3. Remove worktrees of merged branches; delete merged branches; preserve unmerged\n")
	b.WriteString("   branches as evidence. Close the recovered manifest(s): set finished_at when done.\n")
	b.WriteString("Only AFTER recovery is complete, proceed with this tick's normal work — compose a\n")
	b.WriteString("new wave only if this tick's WAVE_BUDGET allows one.\n")
	for _, m := range listed {
		fmt.Fprintf(&b, "Unfinished wave tick=%s started=%s (%d worker(s)):\n", m.TickID, m.StartedAt, len(m.Workers))
		for _, w := range m.Workers {
			fmt.Fprintf(&b, "- %s branch=%s worktree=%s sha=%s judge=%s merge=%s\n",
				w.TaskID, w.Branch, w.Worktree, w.CommitSHA, w.Judge, w.Merge)
		}
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "(%d additional unfinished manifest(s) not listed — close or recover them too.)\n", truncated)
	}
	b.WriteString("=== END WAVE RECOVERY ===\n")
	b.WriteString("```")
	return b.String()
}
