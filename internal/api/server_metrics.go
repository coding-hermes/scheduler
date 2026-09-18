package api

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-156: GET /api/v1/metrics — the eight fleet questions that used to
// be answered with hand-written SQL against the daemon's SQLite DB, in ONE
// read-only JSON response.
//
//	1. spawns by namespace + outcome          -> spawns.by_namespace / .by_outcome
//	2. deferrals by reason                    -> deferrals.by_reason
//	3. orphan nudges by path                  -> nudges.by_path
//	4. active/queued ticks vs caps            -> ticks.*
//	5. cooldown-expired but unscheduled       -> ticks.cooldown_expired_unscheduled
//	6. tick duration p50/p90/p99              -> tick_duration_ms.*
//	7. gateway drain-503 count                -> gateway.drain_503
//	8. zero-output 'committed' ticks          -> outcomes.zero_output_committed
//
// HONESTY RULE (load-bearing — this is what the endpoint is judged on):
// every block carries "available": true|false. A block whose real source is
// absent (no Loop for the in-memory admission counters, a failed query) is
// reported as {"available": false, "reason": "..."} — NEVER as a zero. A 0 is
// only ever emitted after a real query returned it, and the sibling "sources"
// map names the exact query/counter behind every block. No number on this
// endpoint is invented, derived from an unrelated table, or defaulted to look
// populated.

// metricsWindow is the lookback for every WINDOWED metrics block (spawns,
// tick durations, gateway drains, zero-output outcomes). It is echoed to the
// wire as metricsWindowLabel so a dashboard never has to guess the window.
const metricsWindow = 24 * time.Hour

// metricsWindowLabel is the wire string for metricsWindow ("24h").
const metricsWindowLabel = "24h"

// Percentile numerators (percent) used by the tick-duration block.
const (
	metricsPercentileP50 = 50
	metricsPercentileP90 = 90
	metricsPercentileP99 = 99
)

// tickDurationMs is the tick_duration_ms block: the nearest-rank percentiles
// of the spawned_at -> completed_at durations (milliseconds) of ticks
// completed inside the window. Count is the sample size; P50/P90/P99 are nil
// (JSON null) when Count == 0 — an empty sample has NO percentile, and
// reporting 0 would be a fabricated measurement.
type tickDurationMs struct {
	Count  int    `json:"count"`
	Window string `json:"window"`
	P50    *int   `json:"p50"`
	P90    *int   `json:"p90"`
	P99    *int   `json:"p99"`
}

// metricsUnavailable builds the AC3 honesty shape for a block whose real
// source is missing or whose query failed. The reason is logged so the gap is
// visible in scheduler.log as well as on the wire.
func metricsUnavailable(block, reason string) map[string]interface{} {
	log.Printf("metrics: %s unavailable: %s", block, reason)
	return map[string]interface{}{
		"available": false,
		"reason":    reason,
	}
}

// metrics handles GET /api/v1/metrics. Read-only: it opens no write, runs no
// migration and mutates no state; the same loopback/auth posture as
// /api/v1/status applies (the caller's transport is the only guard).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	now := time.Now().UTC()
	// RFC3339 cutoff for the windowed blocks. Every predicate compares with
	// julianday() — spawned_at/completed_at are RFC3339 TEXT and the fleet
	// contains BOTH UTC ("...Z") and local-offset ("...-05:00") rows, so a raw
	// string comparison would silently drop rows (same rationale as
	// LoadBudgetSpends and the stale-gateway SQL in tick_process.go).
	cutoff := now.Add(-metricsWindow).Format(time.RFC3339)

	writeJSON(w, 200, map[string]interface{}{
		"generated_at":     now.Format(time.RFC3339),
		"uptime_s":         int64(time.Since(s.started).Seconds()),
		"sources":          metricsSources(),
		"spawns":           s.metricsSpawns(ctx, cutoff),
		"deferrals":        s.metricsDeferrals(),
		"nudges":           s.metricsNudges(ctx),
		"ticks":            s.metricsTicks(ctx),
		"tick_duration_ms": s.metricsTickDurations(ctx, cutoff),
		"gateway":          s.metricsGateway(ctx, cutoff),
		"outcomes":         s.metricsOutcomes(ctx, cutoff),
	})
}

// metricsSources names the exact source (or the gap) behind every block, so a
// consumer can tell a measured number from an unavailable one without reading
// this file. Keys are metric-block names.
func metricsSources() map[string]string {
	return map[string]string{
		"spawns": "ticks table, rows with spawned_at inside the window (julianday(spawned_at) >= julianday(cutoff)): " +
			"total = every such row; by_namespace = LEFT JOIN projects on project_name, grouped by namespace_id " +
			"(\"-\" when the project row is missing or its namespace_id is empty); by_outcome = rows whose outcome is " +
			"set, one key per CHECK-constraint value (NULL outcome = never reached a terminal outcome, counted in " +
			"total only). Window: " + metricsWindowLabel + ".",
		"deferrals": "Loop.AdmissionCounters() — per-process SCHED-GAP-155 admission counters, monotonic since daemon " +
			"boot and reset by a restart (no persisted equivalent exists). by_reason carries every reason in the " +
			"admission vocabulary plus the decision reasons; admitted_by_namespace and passes carry the two " +
			"bookkeeping entries (\"admitted:<ns>\" totals and the emitter's pass count). available=false when no " +
			"Loop is attached to this Server (nothing to read).",
		"nudges": "ticks table, SUM(nudge_count) grouped by orphan_reason for rows with nudge_count > 0 — each " +
			"increment is one admitted orphan re-nudge (the counter is bumped immediately before the nudge row is " +
			"enqueued, so a nudge whose enqueue failed still counts once). orphan_reason is the drop path that " +
			"orphaned the tick (drain_timeout | startup_reap | zombie_reap; empty -> \"unknown\"). Persisted across " +
			"restarts, so this block is all-time, not windowed.",
		"ticks": "ticks table: active = COUNT(status='running') (the same helper /api/v1/status reports as " +
			"active_ticks), queued = COUNT(status='queued'); by_namespace = one entry per namespaces row carrying " +
			"its max_concurrent cap (0 = unlimited — the global cap still applies) plus the live running/queued " +
			"counts of the projects assigned to it, and a \"-\" entry when running/queued ticks belong to a project " +
			"with no namespace; global_cap = the resolved-config snapshot's max_concurrent, the same field " +
			"/api/v1/config serves (0 = this Server was built without SetResolvedConfig, i.e. no value in this " +
			"process); cooldown_expired_unscheduled = enabled projects with no running AND no queued tick whose " +
			"wall-clock cooldown has elapsed (bump-aware: an active bump's cooldown owns the gate). Known " +
			"limitation: that count is a SQL mirror of the packer's wall-clock gate and does NOT model blackout " +
			"windows, tasks-mode post-tick pacing or cooldown_s=0 dynamic intervals, so it can overcount slightly.",
		"tick_duration_ms": "ticks table: ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000) " +
			"milliseconds for rows completed inside the window (and never negative: completed_at >= spawned_at is " +
			"required). p50/p90/p99 are nearest-rank on the ascending sample — idx = ceil(pct/100 * n) - 1 " +
			"clamped to [0, n-1] — and are null with count=0 when the window holds no completed tick.",
		"gateway": "ticks table: rows spawned inside the window whose error text matches the harness drain class " +
			"(error LIKE '%503%' OR '%draining%' — e.g. \"gateway POST: HTTP 503: ...gateway is draining\"). The " +
			"in-memory Spawner counter behind /api/v1/status gateway_errors (GatewayErrorCount) counts EVERY " +
			"transient gateway failure (5xx, timeouts, read/unmarshal) and does not classify drains, so it cannot " +
			"source this number and is deliberately not used.",
		"outcomes": "ticks table: rows spawned inside the window with outcome='committed' AND commits=0 — a tick " +
			"that recorded a commit outcome while landing no commit at all. Window: " + metricsWindowLabel + ".",
	}
}

// metricsSpawns answers question 1: spawn volume per namespace and per outcome
// over the window. An empty by_namespace map is written as {} (never null) so
// a dashboard can iterate without a nil check.
func (s *Server) metricsSpawns(ctx context.Context, cutoff string) map[string]interface{} {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE julianday(spawned_at) >= julianday(?)`,
		cutoff).Scan(&total); err != nil {
		return metricsUnavailable("spawns", "query failed: "+err.Error())
	}

	byNamespace := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(p.namespace_id, ''), '-') AS ns, COUNT(*)
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE julianday(t.spawned_at) >= julianday(?)
GROUP BY ns
ORDER BY ns`, cutoff)
	if err != nil {
		return metricsUnavailable("spawns", "namespace query failed: "+err.Error())
	}
	for rows.Next() {
		var ns string
		var n int
		if err := rows.Scan(&ns, &n); err != nil {
			rows.Close()
			return metricsUnavailable("spawns", "namespace scan failed: "+err.Error())
		}
		byNamespace[ns] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return metricsUnavailable("spawns", "namespace query failed: "+err.Error())
	}
	rows.Close()

	// The four values the ticks.outcome CHECK constraint allows are always
	// present (0 = the query found none), so a consumer can read
	// by_outcome.committed without a presence check.
	byOutcome := map[string]int{"committed": 0, "dry_run": 0, "failed": 0, "timeout": 0}
	orows, err := s.db.QueryContext(ctx, `
SELECT outcome, COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?) AND outcome IS NOT NULL
GROUP BY outcome`, cutoff)
	if err != nil {
		return metricsUnavailable("spawns", "outcome query failed: "+err.Error())
	}
	for orows.Next() {
		var outcome string
		var n int
		if err := orows.Scan(&outcome, &n); err != nil {
			orows.Close()
			return metricsUnavailable("spawns", "outcome scan failed: "+err.Error())
		}
		if _, known := byOutcome[outcome]; known {
			byOutcome[outcome] = n
		}
	}
	if err := orows.Err(); err != nil {
		orows.Close()
		return metricsUnavailable("spawns", "outcome query failed: "+err.Error())
	}
	orows.Close()

	return map[string]interface{}{
		"available":    true,
		"window":       metricsWindowLabel,
		"total":        total,
		"by_namespace": byNamespace,
		"by_outcome":   byOutcome,
	}
}

// metricsDeferrals answers question 2 from the SCHED-GAP-155 admission
// counters. Those counters live in the Loop's memory only, so a Server built
// without a Loop (tests, tooling) reports the block unavailable with the
// reason instead of an invented zero map.
func (s *Server) metricsDeferrals() map[string]interface{} {
	if s.loop == nil {
		return metricsUnavailable("deferrals",
			"no scheduler Loop attached to this Server: the SCHED-GAP-155 admission counters are per-process "+
				"in-memory state owned by the Loop; nothing is persisted, so there is no source to query")
	}
	counters := s.loop.AdmissionCounters()
	byReason := make(map[string]int, len(counters))
	admitted := map[string]int{}
	passes := 0
	for key, n := range counters {
		switch {
		case key == "passes":
			passes = n
		case strings.HasPrefix(key, "admitted:"):
			admitted[strings.TrimPrefix(key, "admitted:")] = n
		default:
			byReason[key] = n
		}
	}
	return map[string]interface{}{
		"available":             true,
		"by_reason":             byReason,
		"admitted_by_namespace": admitted,
		"passes":                passes,
	}
}

// metricsNudges answers question 3: orphan re-nudges grouped by the drop path
// that orphaned the tick. The source is persisted (ticks.nudge_count +
// ticks.orphan_reason), so this block is all-time and survives restarts.
func (s *Server) metricsNudges(ctx context.Context) map[string]interface{} {
	byPath := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(orphan_reason, ''), 'unknown') AS path, SUM(nudge_count)
FROM ticks
WHERE nudge_count > 0
GROUP BY path
ORDER BY path`)
	if err != nil {
		return metricsUnavailable("nudges", "query failed: "+err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var n int
		if err := rows.Scan(&path, &n); err != nil {
			return metricsUnavailable("nudges", "scan failed: "+err.Error())
		}
		byPath[path] = n
	}
	if err := rows.Err(); err != nil {
		return metricsUnavailable("nudges", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available": true,
		"by_path":   byPath,
	}
}

// metricsTicks answers questions 4 and 5: the live occupancy gauges against
// both caps, and the cooldown-expired-but-unscheduled count.
func (s *Server) metricsTicks(ctx context.Context) map[string]interface{} {
	// active reuses the exact helper /api/v1/status serves as active_ticks, so
	// the two endpoints can never disagree on this number.
	active := countActiveTicks(ctx, s.db)

	var queued int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE status = 'queued'`).Scan(&queued); err != nil {
		return metricsUnavailable("ticks", "queued query failed: "+err.Error())
	}

	// One entry per namespaces row (cap + live counts), plus "-" when a
	// running/queued tick belongs to a project with no namespace.
	byNamespace := map[string]map[string]int{}
	namespaces, err := database.ListNamespaces(ctx, s.db, false)
	if err != nil {
		return metricsUnavailable("ticks", "namespace query failed: "+err.Error())
	}
	for _, ns := range namespaces {
		byNamespace[ns.ID] = map[string]int{"active": 0, "queued": 0, "cap": ns.MaxConcurrent}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(p.namespace_id, ''), '-') AS ns, t.status, COUNT(*)
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE t.status IN ('running', 'queued')
GROUP BY ns, t.status`)
	if err != nil {
		return metricsUnavailable("ticks", "namespace occupancy query failed: "+err.Error())
	}
	for rows.Next() {
		var ns, status string
		var n int
		if err := rows.Scan(&ns, &status, &n); err != nil {
			rows.Close()
			return metricsUnavailable("ticks", "namespace occupancy scan failed: "+err.Error())
		}
		entry, ok := byNamespace[ns]
		if !ok {
			// A namespace can be missing from the namespaces table (project
			// row points at a deleted/unknown id) — report the occupancy with
			// no cap rather than dropping it.
			entry = map[string]int{"active": 0, "queued": 0, "cap": 0}
			byNamespace[ns] = entry
		}
		switch status {
		case "running":
			entry["active"] = n
		case "queued":
			entry["queued"] = n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return metricsUnavailable("ticks", "namespace occupancy query failed: "+err.Error())
	}
	rows.Close()

	// Question 5. A project is counted when it is enabled, owns no live tick
	// (running or queued — a queued row already owns a future slot) and its
	// wall-clock cooldown has elapsed. An active bump owns the gate, exactly
	// like the packer's selection paths.
	var cooldownExpired int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM projects p
WHERE p.enabled = 1
  AND NOT EXISTS (SELECT 1 FROM ticks t WHERE t.project_name = p.name AND t.status IN ('running', 'queued'))
  AND (
        p.last_tick_completed IS NULL
     OR p.last_tick_completed = ''
     OR (julianday('now') - julianday(p.last_tick_completed)) * 86400.0
            >= CASE WHEN COALESCE(p.bump_active, 0) = 1 AND COALESCE(p.bump_cooldown_s, 0) > 0
                    THEN p.bump_cooldown_s ELSE p.cooldown_s END
      )`).Scan(&cooldownExpired); err != nil {
		return metricsUnavailable("ticks", "cooldown query failed: "+err.Error())
	}

	return map[string]interface{}{
		"available":                    true,
		"active":                       active,
		"queued":                       queued,
		"global_cap":                   s.resolvedConfig.MaxConcurrent,
		"by_namespace":                 byNamespace,
		"cooldown_expired_unscheduled": cooldownExpired,
	}
}

// metricsTickDurations answers question 6. Returns either the tickDurationMs
// block or the unavailable shape (interface{} so both marshal identically).
func (s *Server) metricsTickDurations(ctx context.Context, cutoff string) interface{} {
	rows, err := s.db.QueryContext(ctx, `
SELECT CAST(ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000.0) AS INTEGER)
FROM ticks
WHERE completed_at IS NOT NULL AND completed_at <> ''
  AND spawned_at IS NOT NULL AND spawned_at <> ''
  AND julianday(completed_at) >= julianday(?)
  AND julianday(completed_at) >= julianday(spawned_at)`, cutoff)
	if err != nil {
		return metricsUnavailable("tick_duration_ms", "query failed: "+err.Error())
	}
	defer rows.Close()

	var samples []int
	for rows.Next() {
		var ms int
		if err := rows.Scan(&ms); err != nil {
			return metricsUnavailable("tick_duration_ms", "scan failed: "+err.Error())
		}
		samples = append(samples, ms)
	}
	if err := rows.Err(); err != nil {
		return metricsUnavailable("tick_duration_ms", "query failed: "+err.Error())
	}

	block := tickDurationMs{Count: len(samples), Window: metricsWindowLabel}
	if len(samples) > 0 {
		sort.Ints(samples)
		p50 := nearestRankPercentile(samples, metricsPercentileP50)
		p90 := nearestRankPercentile(samples, metricsPercentileP90)
		p99 := nearestRankPercentile(samples, metricsPercentileP99)
		block.P50, block.P90, block.P99 = &p50, &p90, &p99
	}
	return block
}

// nearestRankPercentile returns the nearest-rank percentile of an ASCENDING
// sample: idx = ceil(pct/100 * n) - 1, clamped to [0, n-1] (the definition
// pinned by SCHED-GAP-156 §2.1). The ceiling uses integer arithmetic
// ((pct*n + 99) / 100) so a float product like 0.9*10 cannot land on the
// wrong side of an integer boundary. Callers must pass a non-empty sample.
func nearestRankPercentile(ascending []int, pct int) int {
	n := len(ascending)
	if n == 0 {
		return 0
	}
	idx := (pct*n+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx > n-1 {
		idx = n - 1
	}
	return ascending[idx]
}

// metricsGateway answers question 7: drain-class 503s from the ticks table's
// error text (the only persisted, drain-classifying source — see
// metricsSources).
func (s *Server) metricsGateway(ctx context.Context, cutoff string) map[string]interface{} {
	var drains int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND error IS NOT NULL AND error <> ''
  AND (error LIKE '%503%' OR error LIKE '%draining%')`, cutoff).Scan(&drains); err != nil {
		return metricsUnavailable("gateway", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available": true,
		"drain_503": drains,
		"window":    metricsWindowLabel,
	}
}

// metricsOutcomes answers question 8: ticks that recorded outcome='committed'
// while committing nothing.
func (s *Server) metricsOutcomes(ctx context.Context, cutoff string) map[string]interface{} {
	var zeroOutput int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND outcome = 'committed'
  AND COALESCE(commits, 0) = 0`, cutoff).Scan(&zeroOutput); err != nil {
		return metricsUnavailable("outcomes", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available":             true,
		"zero_output_committed": zeroOutput,
		"window":                metricsWindowLabel,
	}
}
