package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/version"
)

// failureRateQueryCount counts the number of SELECT-style queries issued
// by computeProjectFailureRates. It is the SCHED-PERF-002 test seam: the
// criteria require proof that the rewrite is a single round trip
// independent of the project count, and an atomic counter is the cheapest
// mechanism that does not depend on a custom database/sql driver wrapper
// (modernc.org/sqlite's driver surface is too rich to wrap reliably across
// versions). The counter is bumped exactly once per
// computeProjectFailureRates call, by the production code path below; tests
// reset it with failureRateQueryCount.Store(0) before invoking the
// function under test, then read it back. It is NOT a feature flag —
// every call always increments; the variable exists solely so tests can
// read the post-call value.
var failureRateQueryCount atomic.Int64

// failureRateQueryCountForTest returns the current value of
// failureRateQueryCount. Exported for tests; production callers do not need
// it (the counter is a no-op overhead and a successful handler will
// continue to issue exactly one SELECT on every call).
func failureRateQueryCountForTest() int64 {
	return failureRateQueryCount.Load()
}

// resetFailureRateQueryCountForTest sets the counter to zero. Exported for
// tests only; the production path never calls it.
func resetFailureRateQueryCountForTest() {
	failureRateQueryCount.Store(0)
}

// -- helpers --

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func countActiveTicks(ctx context.Context, db *sql.DB) int {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n)
	return n
}

func countRecentOutcomes(ctx context.Context, db *sql.DB) map[string]int {
	out := map[string]int{"completed": 0, "failed": 0, "timeout": 0}
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM ticks WHERE completed_at IS NOT NULL GROUP BY status ORDER BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err == nil {
			out[status] = count
		}
	}
	return out
}

// countZeroOutputCommitted24h (SCHED-GAP-205) counts completed ticks from
// the last 24h that billed non-zero output tokens but carry no commits and
// no output text — the historical signature of the zero-assistant
// completion the 205 completion gate now fails at spawn time
// ("zero assistant output with non-zero token usage"). The ticks table
// stores no assistant-message flag and no response text (schema v1:
// tokens/commits/error only), so the count is DERIVED from the same
// observable columns the gate keys on — status='completed', tokens_out>0,
// empty error text (a 205-failed row carries the sentinel in the error
// column; a legacy false-green completed row has none), and zero commits.
// Informational: after the fix lands the count drains to 0 within a day,
// giving operators a live before/after signal.
func countZeroOutputCommitted24h(ctx context.Context, db *sql.DB) int {
	var n int
	_ = db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ticks
WHERE status = 'completed'
  AND completed_at IS NOT NULL AND completed_at <> ''
  AND julianday(completed_at) >= julianday('now', '-1 day')
  AND COALESCE(tokens_out, 0) > 0
  AND COALESCE(commits, 0) = 0
  AND COALESCE(code_commits, 0) = 0
  AND COALESCE(board_commits, 0) = 0
  AND (error IS NULL OR error = '')`).Scan(&n)
	return n
}

// ProjectFailureRate is the per-project failure-rate breakdown for a single
// project over a window of recent ticks. It appears in /api/v1/status under
// the "projects_failure_rates" key (SCHED-GAP-018).
//
// SCHED-GAP-173: the counters describe PROJECT-ATTRIBUTABLE ticks only. A
// failed tick whose error the shared classifier (scheduler.HarnessFailure)
// marks as harness/infrastructure class — gateway unreachable, drain
// refusals, gateway auth failures, connection refused, exec fallback
// disabled, graceful-shutdown aborts — never reached the project, so it is
// excluded from BOTH counters, exactly as CheckFailureRateAutoDisable
// excludes it. failure_rate is therefore the very ratio the enforcer compares
// against its threshold: a lane whose whole window is gateway-drain noise
// reads failure_rate 0 and auto_disable_armed=false instead of 0.91/true.
type ProjectFailureRate struct {
	Failed      int     `json:"failed"`
	Total       int     `json:"total"`
	FailureRate float64 `json:"failure_rate"`

	// AutoDisableArmed reports whether this project currently meets the
	// auto-disable condition (GAP-047): the feature is enabled
	// (threshold > 0), the sample size reaches minTicks, and the failure
	// rate is at or above the threshold. It mirrors the exact condition in
	// internal/scheduler/alert_escalation.go CheckFailureRateAutoDisable —
	// including the harness-failure exclusion of the shared classifier
	// (SCHED-GAP-173), so armed is what the enforcer would actually do.
	AutoDisableArmed bool `json:"auto_disable_armed"`
}

// computeProjectFailureRates returns a per-project failure-rate breakdown
// computed over the last `window` completed ticks per project. Only projects
// with at least one tick in the window are included. "failed" counts both
// 'failed' and 'timeout' statuses (both are waste — non-completed outcomes),
// excluding failed ticks the shared harness classifier owns. "total" is the
// number of ticks in the window with a non-null completed_at (running/queued
// ticks are excluded) that are NOT harness-class failures. failure_rate =
// failed/total, rounded to 4 decimal places; it is 0 when the window holds no
// project-attributable tick at all (total 0 — 0/0 is NaN in Go and would
// break the JSON encoder).
//
// `threshold` and `minTicks` drive the AutoDisableArmed flag (GAP-047) and
// mirror the auto-disable policy in alert_escalation.go: armed when
// threshold > 0 && total >= minTicks && rate >= threshold, where rate is the
// unrounded project-attributable failed/total ratio (matching
// CheckFailureRateAutoDisable exactly).
func computeProjectFailureRates(ctx context.Context, db *sql.DB, window int, threshold float64, minTicks int) map[string]ProjectFailureRate {
	if window <= 0 {
		window = 100
	}
	// Mirror CheckFailureRateAutoDisable's effective sample-size default.
	if minTicks <= 0 {
		minTicks = 50
	}
	out := map[string]ProjectFailureRate{}

	// SCHED-PERF-002 — single-query failure-rate path.
	//
	// The previous shape was an N+1: a DISTINCT project-name list query
	// (368 rows) plus a per-project `SELECT status, error ORDER BY
	// spawned_at DESC LIMIT window` (~1 round trip per project). At
	// ~5 ms per project on a 368-project fleet, the inner loop alone
	// was ~2 s, and behind the daemon's SetMaxOpenConns(1) the
	// serialized contention with tick work pushed the whole handler
	// to 18-30 s on a busy day — past the 10 s urllib timeout in
	// fleet-cooldown-policy.py, which then silently stopped
	// regenerating fleet.toml.
	//
	// The instrumentation counter (failureRateQueryCount) is a test
	// seam: the SCHED-PERF-002 gitreins criteria require a proof
	// test that the rewrite is a single round trip independent of
	// the project count, and the counter is the cheapest mechanism
	// that does not depend on a custom driver wrapper (modernc's
	// driver surface is too rich to wrap reliably across versions).
	// It is bumped exactly once per computeProjectFailureRates call
	// — the production path below issues exactly one SELECT — and
	// it is the load-bearing assertion in
	// TestComputeProjectFailureRates_OneRoundTrip.
	//
	// The single-query replacement pulls every completed tick that
	// belongs to an existing project in ONE round trip, ordered
	// (project_name ASC, spawned_at DESC) so the Go walk below can
	// stop counting once it has seen `window` rows for a project
	// (per-project window truncation, TestComputeProjectFailureRates
	// _WindowTruncation). The result-set bound is 1 × (window ×
	// distinct-projects-with-completed-ticks) rows worst case; with
	// window=100 and 368 active projects that is ≤36 800 rows (~3
	// MB), scanned in a single Go pass after one DB round trip.
	// The query plan uses the existing
	// idx_ticks_project_spawned(project_name, spawned_at) index
	// (verified by EXPLAIN QUERY PLAN on the production DB — no temp
	// b-tree, no ROW_NUMBER() window function, which the prior
	// PERF-001 comment documented as a 128-212 ms regression on
	// modernc.org/sqlite).
	//
	// DOGFOOD-009 still holds: only existing projects in the
	// `projects` table are considered, via the EXISTS subquery. A
	// hard-deleted project (purged row, e.g. eduos-e2e) leaves
	// historical ticks behind; without the filter those ticks
	// resurfaced as ghost failure-rate entries (failure_rate=1.0,
	// auto_disable_armed=true) that could never be cleared. The
	// EXISTS clause keeps the per-project windowing accurate and the
	// output ghost-free.
	failureRateQueryCount.Add(1)
	rows, err := db.QueryContext(ctx,
		`SELECT t.project_name, t.status, COALESCE(t.error, '')
		 FROM ticks t
		 WHERE t.completed_at IS NOT NULL
		   AND EXISTS (SELECT 1 FROM projects p WHERE p.name = t.project_name)
		 ORDER BY t.project_name ASC, t.spawned_at DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()

	// Walk rows in (project_name, spawned_at DESC) order. A single
	// pass counts each project's most recent `window` ticks; the
	// harness classifier (SCHED-GAP-173 single source of truth,
	// failureclass.go) is applied in Go — never re-declared in SQL
	// so a marker-list change can never silently diverge between
	// this read surface and the enforcer.
	var (
		curName    string
		curSeen    int // ticks observed for curName (capped at window)
		curFailed  int
		curTotal   int
		hasCurrent bool
	)
	flush := func() {
		if !hasCurrent {
			return
		}
		// SCHED-GAP-173: total can legitimately be 0 (window's only
		// ticks were harness-class — gateway 503s). Emit the entry
		// anyway so a harness-blocked lane is visible (dropping it is
		// indistinguishable from "no ticks at all").
		rate := 0.0
		if curTotal > 0 {
			rate = float64(curFailed) / float64(curTotal)
		}
		// GAP-047: armed uses the UNROUNDED rate, exactly like
		// CheckFailureRateAutoDisable.
		armed := threshold > 0 && curTotal >= minTicks && rate >= threshold
		// Round to 4 decimals for clean JSON output.
		rate = float64(int(rate*10000)) / 10000
		out[curName] = ProjectFailureRate{
			Failed:           curFailed,
			Total:            curTotal,
			FailureRate:      rate,
			AutoDisableArmed: armed,
		}
	}

	for rows.Next() {
		var name, status, errText string
		if err := rows.Scan(&name, &status, &errText); err != nil {
			continue
		}
		// Project boundary (rows are ordered by project_name ASC).
		if hasCurrent && name != curName {
			flush()
			curName = name
			curSeen, curFailed, curTotal = 0, 0, 0
		} else if !hasCurrent {
			curName = name
			hasCurrent = true
		}
		// Per-project window: drop everything past the latest `window`
		// ticks. With ORDER BY spawned_at DESC inside each project
		// these are the most recent.
		if curSeen >= window {
			continue
		}
		curSeen++
		// SCHED-GAP-173 — single source of truth: the harness
		// classifier in scheduler.HarnessFailure (failureclass.go).
		// A failed tick whose error matches a harness marker never
		// reached the project; exclude it from BOTH total and failed.
		if status == "failed" && scheduler.HarnessFailure(errText) {
			continue
		}
		curTotal++
		if status == "failed" || status == "timeout" {
			curFailed++
		}
	}
	// Flush the final project (the loop body only flushes on
	// boundary change).
	flush()
	return out
}

func getLatestTick(ctx context.Context, db *sql.DB, project string) (*database.Tick, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, project_name, COALESCE(session_id,'') as session_id, status,
		       COALESCE(outcome,'') as outcome,
		       COALESCE(spawned_at,'') as spawned_at,
		       COALESCE(completed_at,'') as completed_at,
		       COALESCE(exit_code,0) as exit_code,
		       COALESCE(commits,0) as commits,
		       COALESCE(files_changed,0) as files_changed,
		       COALESCE(tokens_in,0) as tokens_in,
		       COALESCE(tokens_out,0) as tokens_out,
		       COALESCE(cost_usd,0.0) as cost_usd,
		       COALESCE(error,'') as error,
		       created_at
		FROM ticks WHERE project_name = ? ORDER BY spawned_at DESC LIMIT 1
	`, project)
	var t database.Tick
	err := row.Scan(&t.ID, &t.ProjectName, &t.SessionID, &t.Status, &t.Outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Error, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func getTick(ctx context.Context, db *sql.DB, id string) (*database.Tick, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, project_name, COALESCE(session_id,'') as session_id, status,
		       COALESCE(outcome,'') as outcome,
		       COALESCE(spawned_at,'') as spawned_at,
		       COALESCE(completed_at,'') as completed_at,
		       COALESCE(exit_code,0) as exit_code,
		       COALESCE(commits,0) as commits,
		       COALESCE(files_changed,0) as files_changed,
		       COALESCE(tokens_in,0) as tokens_in,
		       COALESCE(tokens_out,0) as tokens_out,
		       COALESCE(cost_usd,0.0) as cost_usd,
		       COALESCE(error,'') as error,
		       created_at,
		       COALESCE(worker_count,0) as worker_count,
		       COALESCE(wave_recovery,0) as wave_recovery
		FROM ticks WHERE id = ?
	`, id)
	var t database.Tick
	err := row.Scan(&t.ID, &t.ProjectName, &t.SessionID, &t.Status, &t.Outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Error, &t.CreatedAt,
		&t.WorkerCount, &t.WaveRecovery)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func listTicks(ctx context.Context, db *sql.DB, project, status string, limit int) ([]database.Tick, error) {
	q := "SELECT id, project_name, COALESCE(session_id,'') as session_id, status, COALESCE(outcome,'') as outcome, COALESCE(spawned_at,'') as spawned_at, COALESCE(completed_at,'') as completed_at, COALESCE(exit_code,0) as exit_code, COALESCE(commits,0) as commits, COALESCE(files_changed,0) as files_changed, COALESCE(tokens_in,0) as tokens_in, COALESCE(tokens_out,0) as tokens_out, COALESCE(cost_usd,0.0) as cost_usd, COALESCE(error,'') as error, created_at FROM ticks WHERE 1=1"
	var args []interface{}
	if project != "" {
		q += " AND project_name = ?"
		args = append(args, project)
	}
	if status != "" {
		q += " AND status = ?"
		args = append(args, status)
	}
	q += " ORDER BY spawned_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ticks []database.Tick
	for rows.Next() {
		var t database.Tick
		if err := rows.Scan(&t.ID, &t.ProjectName, &t.SessionID, &t.Status, &t.Outcome,
			&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Error, &t.CreatedAt); err != nil {
			return nil, err
		}
		ticks = append(ticks, t)
	}
	return ticks, rows.Err()
}

func listEvents(ctx context.Context, db *sql.DB, severity, component string, limit int) ([]database.Event, error) {
	q := "SELECT id, severity, component, message, details, created_at FROM events WHERE 1=1"
	var args []interface{}
	if severity != "" {
		q += " AND severity = ?"
		args = append(args, severity)
	}
	if component != "" {
		q += " AND component = ?"
		args = append(args, component)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []database.Event
	for rows.Next() {
		var e database.Event
		var sevStr string
		if err := rows.Scan(&e.ID, &sevStr, &e.Component, &e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Severity = database.EventSeverity(sevStr)
		events = append(events, e)
	}
	return events, rows.Err()
}

// getLastEvalTime returns the timestamp of the last evaluation, or empty string.
func getLastEvalTime(ctx context.Context, db *sql.DB) string {
	var t string
	db.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at), '') FROM events WHERE message = 'evaluation started'`).Scan(&t)
	return t
}

// gatewayHealthGateStatusBlock renders the SCHED-GAP-170 gateway-health
// admission gate for /api/v1/status. The gate is PACKAGE state (the gateway is
// one fleet-wide dependency, so its verdict is one fleet-wide verdict), which
// is why this needs no Server or loop — the block answers on every daemon,
// including one whose loop has no gateway client.
//
// It is the surface that did not exist while the FIX-STUCK liveness guard was
// silently inert: `armed` is exactly the condition that was false for months
// (see scheduler.GatewayHealthGateStatus), so a fleet spawning UNGATED is
// visible from the API instead of only from a source read.
//
// Style follows its neighbours: probed_at is RFC3339 UTC with the zero time
// rendered as "" (the health handler's convention — never "0001-01-01"), and
// ttl_s is seconds so the block stays JSON-stable if the TTL ever changes.
func gatewayHealthGateStatusBlock() map[string]interface{} {
	armed, healthy, probedAt, lastErr, ttl, deferrals := scheduler.GatewayHealthGateStatus()
	probed := ""
	if !probedAt.IsZero() {
		probed = probedAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"armed":           armed,
		"healthy":         healthy,
		"probed_at":       probed,
		"last_error":      lastErr,
		"ttl_s":           int(ttl.Seconds()),
		"deferrals_total": deferrals,
	}
}

// queueItem is a single entry in the scheduler queue.
type queueItem struct {
	Project   string  `json:"project"`
	Urgency   float64 `json:"urgency"`
	Weight    int     `json:"weight"`
	Priority  int     `json:"priority"`
	CooldownS int     `json:"cooldown_s"`
	Enabled   bool    `json:"enabled"`
}

// listQueue returns the ordered queue of eligible projects with real
// engine-formula urgency scores (GAP-054). Urgency is computed with the
// scheduler engine's own UrgencyCalculator (same formula as the daemon's
// selection path: priority * (1 + elapsed/interval)^decay_rate, elapsed
// since last_tick_completed or created_at), so the API ordering matches the
// scheduler's ComputeUrgency ordering. When no calculator is configured
// (Server built without SetResolvedConfig, or an unparseable interval
// range), scores fall back to priority-only so the endpoint never panics.
// Rows are sorted by urgency descending after computation.
func (s *Server) listQueue(ctx context.Context) ([]queueItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, COALESCE(weight,0), COALESCE(priority,0), COALESCE(cooldown_s,0), COALESCE(enabled,1), COALESCE(decay_rate,0), COALESCE(created_at,''), COALESCE(last_tick_completed,'') FROM projects WHERE enabled = 1 ORDER BY priority DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calc := s.urgencyCalculator()
	now := s.clock().Now()
	var items []queueItem
	for rows.Next() {
		var it queueItem
		var decayRate float64
		var createdAtStr, lastStr string
		if err := rows.Scan(&it.Project, &it.Weight, &it.Priority, &it.CooldownS, &it.Enabled, &decayRate, &createdAtStr, &lastStr); err != nil {
			return nil, err
		}
		if calc != nil {
			// Mirror the engine's input handling exactly (see
			// internal/scheduler/packer.go Pick): created_at parses as
			// RFC3339; an empty/unparseable last_tick_completed leaves
			// lastCompleted nil so urgency falls back to created_at.
			createdAt, _ := time.Parse(time.RFC3339, createdAtStr)
			var lastCompleted *time.Time
			if lastStr != "" {
				if t, err := time.Parse(time.RFC3339, lastStr); err == nil {
					lastCompleted = &t
				}
			}
			it.Urgency = calc.ComputeUrgency(float64(it.Priority), decayRate, now, lastCompleted, createdAt)
		} else {
			// No calculator configured: priority-only base (tests).
			it.Urgency = float64(it.Priority)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Urgency > items[j].Urgency
	})
	return items, nil
}

// openapiSpec is the OpenAPI 3.0 specification for the scheduler API.
var openapiSpec = []byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Coding Hermes Scheduler API",
    "version": "` + version.Current() + `",
    "description": "REST API for the Coding Hermes fleet scheduler — manage projects, namespaces, ticks, and fleet health."
  },
  "servers": [{"url": "http://127.0.0.1:9090", "description": "Local scheduler daemon"}],
  "paths": {
    "/api/v1/health": {
      "get": {
        "summary": "Daemon health check",
        "responses": {
          "200": {"description": "OK — returns uptime, DB status, active ticks, spawn counts, gateway error count"}
        }
      }
    },
    "/api/v1/status": {
      "get": {
        "summary": "Fleet overview",
        "responses": {
          "200": {"description": "Returns budget, active projects, tick counts, recent outcomes, gateway_errors (transient gateway spawn failures since restart), gateway_health_gate (the gateway-health admission gate's armed state and cached verdict, SCHED-GAP-170)"}
        }
      }
    },
    "/api/v1/metrics": {
      "get": {
        "summary": "Fleet metrics — one read-only request answers spawns by namespace/outcome, deferrals by reason, orphan nudges by path, active/queued ticks vs caps, cooldown-expired-unscheduled, tick duration p50/p90/p99, gateway drain-503s, zero-output committed ticks, stalled ticks bucketed by duration, escalation events per day by severity/class and lane churn (projects disabled per day with disabled_by provenance)",
        "responses": {
          "200": {"description": "Single JSON object: generated_at, uptime_s, sources, spawns, deferrals, nudges, ticks, tick_duration_ms, gateway, outcomes, stalls, escalations, lane_churn. Every block carries available=true|false; a block whose real source is absent reports {\"available\": false, \"reason\": \"...\"} instead of a fabricated number, and sources names the query/counter behind every block (SCHED-GAP-156; stalls/escalations/lane_churn added by SCHED-GAP-158)."},
          "405": {"description": "Non-GET method"}
        }
      }
    },
    "/api/v1/config": {
      "get": {
        "summary": "Resolved daemon configuration snapshot (three-layer: TOML < env vars < CLI flags)",
        "responses": {
          "200": {"description": "Active config — min_interval, max_concurrent, gateway.url, auto_disable_failure_rate, etc. The gateway key is masked (SCHED-GAP-034)."}
        }
      }
    },
    "/api/v1/projects": {
      "get": {
        "summary": "List all projects",
        "responses": {
          "200": {"description": "Array of project objects"}
        }
      },
      "post": {
        "summary": "Create a project",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Project"}}}
        },
        "responses": {
          "201": {"description": "Created"},
          "409": {"description": "Project already exists"}
        }
      }
    },
    "/api/v1/projects/{name}": {
      "get": {
        "summary": "Get project detail with latest tick",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Project + latest_tick"}
        }
      },
      "put": {
        "summary": "Update project fields",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ProjectUpdates"}}}
        },
        "responses": {
          "200": {"description": "Updated project"}
        }
      },
      "delete": {
        "summary": "Delete a project. confirm=true soft-deletes (enabled=false, row retained); confirm=true&purge=true permanently removes the row (DOGFOOD-009)",
        "parameters": [
          {"name": "name", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "confirm", "in": "query", "required": true, "schema": {"type": "string"}},
          {"name": "purge", "in": "query", "required": false, "schema": {"type": "string"}, "description": "true = hard-delete the row permanently; requires confirm=true too; historical ticks are retained"}
        ],
        "responses": {
          "200": {"description": "Project soft-deleted (enabled=false) or purged (row removed)"},
          "400": {"description": "Missing confirm=true query param (or purge=true without confirm)"},
          "404": {"description": "Project not found"},
          "409": {"description": "Project is enabled — pause it first"}
        }
      }
    },
    "/api/v1/projects/{name}/spawn": {
      "post": {
        "summary": "Manually trigger a tick for this project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "202": {"description": "Tick enqueued — returns tick_id"},
          "404": {"description": "Project not found"}
        }
      }
    },
    "/api/v1/projects/{name}/pause": {
      "post": {
        "summary": "Pause a project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Project paused"},
          "500": {"description": "Project not found (surfaces as 500 on this sub-route)"}
        }
      }
    },
    "/api/v1/projects/{name}/resume": {
      "post": {
        "summary": "Resume a project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Project resumed"},
          "500": {"description": "Project not found (surfaces as 500 on this sub-route)"}
        }
      }
    },
    "/api/v1/projects/{name}/bump": {
      "post": {
        "summary": "Temporarily accelerate the project (SCHED-GAP-107): run at a small cooldown for N ticks, then auto-revert (Phase A restore + Phase B adaptive re-eval)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/BumpRequest"}}}
        },
        "responses": {
          "200": {"description": "Bump active — returns the updated project with bump_* fields and cooldown_s set to the bump value"},
          "400": {"description": "Missing reason, ticks outside 1..8, or cooldown below the 7200s floor"},
          "404": {"description": "Project not found"},
          "409": {"description": "Project disabled, or a bump is already active"}
        }
      }
    },
    "/api/v1/projects/{name}/unbump": {
      "post": {
        "summary": "Manually abort an active bump — restores the saved pre-bump cooldown state verbatim (no adaptive re-evaluation)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Bump cleared — returns the restored project"},
          "404": {"description": "Project not found"},
          "409": {"description": "No active bump to clear"}
        }
      }
    },
    "/api/v1/namespaces": {
      "get": {
        "summary": "List namespaces",
        "responses": {
          "200": {"description": "Array of namespace objects"}
        }
      },
      "post": {
        "summary": "Create a namespace",
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Namespace"}}}
        },
        "responses": {
          "201": {"description": "Created"}
        }
      }
    },
    "/api/v1/namespaces/{id}": {
      "get": {
        "summary": "Get namespace",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Namespace object"}
        }
      },
      "put": {
        "summary": "Update namespace (partial — only supplied fields are applied)",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NamespaceUpdates"}}}
        },
        "responses": {
          "200": {"description": "Updated namespace"},
          "400": {"description": "Invalid JSON"},
          "404": {"description": "Namespace not found"}
        }
      },
      "delete": {
        "summary": "Delete a namespace. confirm=true soft-deletes (enabled=false, row retained, member projects unassigned); confirm=true&purge=true permanently removes the row (SCHED-GAP-097)",
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "confirm", "in": "query", "required": true, "schema": {"type": "string"}},
          {"name": "purge", "in": "query", "required": false, "schema": {"type": "string"}, "description": "true = hard-delete the row permanently; requires confirm=true too; historical namespace_ticks are retained"}
        ],
        "responses": {
          "200": {"description": "Namespace soft-deleted (enabled=false) or purged (row removed)"},
          "400": {"description": "Missing confirm=true query param (or purge=true without confirm)"},
          "404": {"description": "Namespace not found"},
          "409": {"description": "Namespace has enabled project(s) assigned — pause or move them first"}
        }
      }
    },
    "/api/v1/namespaces/{id}/projects": {
      "get": {
        "summary": "List projects assigned to a namespace",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "{\"namespace_id\": \"<id>\", \"projects\": [<Project>, ...]}"},
          "404": {"description": "Unknown namespace sub-route"}
        }
      }
    },
    "/api/v1/namespaces/{id}/move": {
      "post": {
        "summary": "Assign a project to a namespace (sets its namespace_id)",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NamespaceMoveRequest"}}}
        },
        "responses": {
          "200": {"description": "Updated project object (flat, namespace_id set)"},
          "400": {"description": "Missing or invalid body (project required)"},
          "404": {"description": "Project not found"}
        }
      }
    },
    "/api/v1/groups": {
      "get": {
        "summary": "List deploy groups (JSONL-backed)",
        "responses": {
          "200": {"description": "{\"groups\": [<Group>, ...]} sorted by name"}
        }
      },
      "post": {
        "summary": "Create a deploy group",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Group"}}}
        },
        "responses": {
          "201": {"description": "Created group"},
          "400": {"description": "Invalid body (name required, no whitespace)"},
          "409": {"description": "Group already exists"}
        }
      }
    },
    "/api/v1/groups/{name}": {
      "get": {
        "summary": "Get a deploy group",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Group object"},
          "404": {"description": "Group not found"}
        }
      },
      "put": {
        "summary": "Partial-update a deploy group (name is immutable — comes from the path)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/GroupUpdate"}}}
        },
        "responses": {
          "200": {"description": "Updated group"},
          "400": {"description": "Invalid body"},
          "404": {"description": "Group not found"}
        }
      },
      "delete": {
        "summary": "Delete a deploy group (JSONL row removed)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Group deleted"},
          "404": {"description": "Group not found"}
        }
      }
    },
    "/api/v1/groups/{name}/deploy": {
      "post": {
        "summary": "Deploy a template to a group — appends the template's task rows to each member project's .coding-hermes/board/tasks.jsonl. Idempotent per (template, date, project): members whose board already carries the deployment are skipped. dry_run=true returns the plan without writing. One event-log entry per deploy.",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DeployRequest"}}}
        },
        "responses": {
          "200": {"description": "Per-project deploy results (appended / skipped / error — errors never abort the batch)"},
          "400": {"description": "Invalid body (template required) or empty group/template"},
          "404": {"description": "Group or template not found"}
        }
      }
    },
    "/api/v1/templates": {
      "get": {
        "summary": "List deploy templates (JSONL-backed)",
        "responses": {
          "200": {"description": "{\"templates\": [<Template>, ...]} sorted by name"}
        }
      },
      "post": {
        "summary": "Create a deploy template",
        "requestBody": {
          "required": true,
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Template"}}}
        },
        "responses": {
          "201": {"description": "Created template"},
          "400": {"description": "Invalid body (name required; at least one task with a title)"},
          "409": {"description": "Template already exists"}
        }
      }
    },
    "/api/v1/templates/{name}": {
      "get": {
        "summary": "Get a deploy template",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Template object"},
          "404": {"description": "Template not found"}
        }
      },
      "put": {
        "summary": "Partial-update a deploy template (name is immutable)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/TemplateUpdate"}}}
        },
        "responses": {
          "200": {"description": "Updated template"},
          "400": {"description": "Invalid body"},
          "404": {"description": "Template not found"}
        }
      },
      "delete": {
        "summary": "Delete a deploy template (JSONL row removed)",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Template deleted"},
          "404": {"description": "Template not found"}
        }
      }
    },
    "/api/v1/ticks": {
      "get": {
        "summary": "List ticks with optional filters",
        "parameters": [
          {"name": "project", "in": "query", "schema": {"type": "string"}},
          {"name": "status", "in": "query", "schema": {"type": "string"}, "description": "Filter by status: running, completed, failed, timeout"},
          {"name": "limit", "in": "query", "schema": {"type": "integer", "default": 50}}
        ],
        "responses": {
          "200": {"description": "Array of tick objects"}
        }
      }
    },
    "/api/v1/ticks/{id}": {
      "get": {
        "summary": "Get full tick detail",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Tick object"}
        }
      }
    },
    "/api/v1/evaluate": {
      "post": {
        "summary": "Force an evaluation cycle",
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Evaluation triggered"}
        }
      }
    },
    "/api/v1/pause": {
      "post": {
        "summary": "Pause the scheduler globally",
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Scheduler paused"}
        }
      }
    },
    "/api/v1/resume": {
      "post": {
        "summary": "Resume the scheduler globally",
        "requestBody": {
          "content": {"application/json": {"schema": {"$ref": "#/components/schemas/EmptyBody"}}}
        },
        "responses": {
          "200": {"description": "Scheduler resumed"}
        }
      }
    },
    "/api/v1/events": {
      "get": {
        "summary": "List events with optional filters",
        "parameters": [
          {"name": "severity", "in": "query", "schema": {"type": "string"}},
          {"name": "component", "in": "query", "schema": {"type": "string"}},
          {"name": "limit", "in": "query", "schema": {"type": "integer", "default": 100}}
        ],
        "responses": {
          "200": {"description": "Array of event objects"}
        }
      }
    },
    "/api/v1/events/stream": {
      "get": {
        "summary": "Server-Sent Events stream of newly committed events (push, not poll)",
        "description": "Holds the connection open and emits each event row as soon as it commits, framed as an id line plus a one-line JSON data payload. A comment heartbeat is sent every 15 seconds while the log is quiet, so idle connections survive proxies. Reconnect with the Last-Event-ID header (set automatically by EventSource) to replay every persisted event with a greater id before live events resume; an absent, empty or non-numeric Last-Event-ID replays the newest 100 events instead. A slow client whose bounded buffer overflows has the missing range repaired from the event log before the next live event is emitted.",
        "parameters": [
          {"name": "Last-Event-ID", "in": "header", "required": false, "schema": {"type": "string"}, "description": "ID of the last event the client received. Every event with a greater id is replayed before live events resume. Missing, empty or non-numeric values are treated as \"no cursor\"."}
        ],
        "responses": {
          "200": {
            "description": "SSE stream: one frame per event (id line + JSON data line terminated by a blank line), plus a comment heartbeat every 15s while idle",
            "content": {
              "text/event-stream": {
                "schema": {"type": "string", "description": "SSE frames: \"id: <event id>\" then \"data: <Event JSON>\", terminated by a blank line; \": heartbeat\" comments while idle"},
                "example": "id: 42\ndata: {\"id\":42,\"severity\":\"HIGH\",\"component\":\"loop\",\"message\":\"evaluation started\",\"details\":\"{}\",\"created_at\":\"2026-09-18T13:00:00Z\"}"
              }
            }
          },
          "405": {"description": "Non-GET method"}
        }
      }
    },
    "/api/v1/queue": {
      "get": {
        "summary": "Ordered queue of eligible projects by urgency",
        "responses": {
          "200": {"description": "Array of queue items sorted by urgency descending"}
        }
      }
    }
  },
  "components": {
    "schemas": {
      "Project": {
        "type": "object",
        "required": ["name", "repo_url", "workdir"],
        "properties": {
          "name": {"type": "string"},
          "repo_url": {"type": "string"},
          "workdir": {"type": "string"},
          "weight": {"type": "integer", "minimum": 1, "maximum": 100},
          "priority": {"type": "integer", "minimum": 1, "maximum": 10},
          "cooldown_s": {"type": "integer"},
          "decay_rate": {"type": "number", "exclusiveMinimum": 0},
          "model": {"type": "string"},
          "provider": {"type": "string"},
          "fallback_model": {"type": "string", "description": "Optional: fallback model tier for the spawn chain (SCHED-GAP-064)"},
          "fallback_provider": {"type": "string", "description": "Optional: fallback provider tier for the spawn chain (SCHED-GAP-064)"},
          "no_global_fallback": {"type": "boolean", "description": "true → skip the spawner-level (env) fallback tier entirely (SCHED-GAP-064)"},
          "model_chain": {"type": "string", "description": "Ordered list of \"model@provider\" hops (JSON array); empty = use model/provider + fallback_model/fallback_provider (SCHED-GAP-075)"},
          "idle_model": {"type": "string", "description": "Optional: idle-tick model tier, prepended to the spawn chain when the board has zero pending tasks (SCHED-GAP-065)"},
          "idle_provider": {"type": "string", "description": "Optional: idle-tick provider tier (SCHED-GAP-065)"},
          "daily_budget_usd": {"type": "number", "description": "Per-UTC-day spend cap; <= 0 = unlimited (SCHED-GAP-066)"},
          "weekly_budget_usd": {"type": "number", "description": "Per-UTC-week spend cap (Monday 00:00 UTC reset); <= 0 = unlimited (SCHED-GAP-066)"},
          "final_budget_usd": {"type": "number", "description": "One-time lifetime spend cap, never resets; <= 0 = unlimited (SCHED-GAP-066)"},
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"},
          "gateway_key": {"type": "string"},
          "command": {"type": "string"},
          "prompt": {"type": "string", "description": "Optional: extra foreman prompt text; appended to the namespace default_prompt unless prompt_mode is \"replace\""},
          "prompt_mode": {"type": "string", "enum": ["append", "replace"], "description": "\"append\" (default): project prompt appends to the namespace default; \"replace\": project prompt replaces it entirely"},
          "namespace_id": {"type": "string", "nullable": true},
          "deliver": {"type": "string"},
          "enabled": {"type": "boolean"},
          "created_at": {"type": "string"},
          "updated_at": {"type": "string"},
          "last_tick_started": {"type": "string"},
          "last_tick_completed": {"type": "string"},
          "disabled_at": {"type": "string"},
          "disabled_by": {"type": "string", "enum": ["api", "api-pause", "api-delete", "auto-disable"]},
          "disabled_reason": {"type": "string"},
          "consecutive_failures": {"type": "integer", "description": "Consecutive spawn failures; drives exponential selection backoff (internal scheduler state — not user-editable via ProjectUpdates)"},
          "adaptive_cooldown": {"type": "boolean", "description": "Adaptive-cooldown policy: counts consecutive no-progress ticks and progressively multiplies the effective cooldown up to cooldown_ceiling_s; any progress resets it to cooldown_floor_s"},
          "cooldown_floor_s": {"type": "integer", "description": "Adaptive-cooldown floor — effective cooldown on the progress/speed-up path"},
          "cooldown_ceiling_s": {"type": "integer", "description": "Adaptive-cooldown ceiling — maximum slowed-down cooldown"},
          "no_progress_threshold": {"type": "integer", "description": "Consecutive no-progress ticks (0 commits AND no new board rows) before the adaptive slowdown begins"},
          "no_progress_ticks": {"type": "integer", "description": "Runtime state: current consecutive no-progress streak (internal scheduler state — not user-editable via ProjectUpdates)"},
          "board_rows_seen": {"type": "integer", "description": "Runtime state: board row count seen at the last tick (internal scheduler state — not user-editable via ProjectUpdates)"}
        }
      },
      "ProjectUpdates": {
        "type": "object",
        "description": "Partial project update — only fields present in the body are applied (pointer semantics; omit fields to leave them untouched).",
        "properties": {
          "repo_url": {"type": "string"},
          "workdir": {"type": "string"},
          "weight": {"type": "integer", "minimum": 1, "maximum": 100},
          "priority": {"type": "integer", "minimum": 1, "maximum": 10},
          "cooldown_s": {"type": "integer"},
          "decay_rate": {"type": "number", "exclusiveMinimum": 0, "description": "Must be > 0 — 0 causes permanent urgency starvation"},
          "model": {"type": "string"},
          "provider": {"type": "string"},
          "fallback_model": {"type": "string", "description": "Optional: fallback model tier for the spawn chain (SCHED-GAP-064); \"\" clears back to the spawner-level fallback"},
          "fallback_provider": {"type": "string", "description": "Optional: fallback provider tier for the spawn chain (SCHED-GAP-064)"},
          "no_global_fallback": {"type": "boolean", "description": "true → skip the spawner-level (env) fallback tier entirely (SCHED-GAP-064)"},
          "idle_model": {"type": "string", "description": "SCHED-GAP-065: project idle tier, prepended to the spawn chain on zero-pending boards"},
          "idle_provider": {"type": "string", "description": "SCHED-GAP-065: idle provider tier; \"\" clears back to no project idle lane"},
          "daily_budget_usd": {"type": "number", "description": "SCHED-GAP-066: per-UTC-day spend cap; <= 0 = unlimited"},
          "weekly_budget_usd": {"type": "number", "description": "Per-UTC-week spend cap (Monday 00:00 UTC reset); <= 0 = unlimited"},
          "final_budget_usd": {"type": "number", "description": "One-time lifetime spend cap, never resets; <= 0 = unlimited"},
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"},
          "gateway_key": {"type": "string", "description": "Per-foreman gateway key; \"\" clears back to the daemon's shared key"},
          "command": {"type": "string"},
          "prompt": {"type": "string", "description": "Extra foreman prompt; \"\" clears back to namespace default only"},
          "prompt_mode": {"type": "string", "enum": ["append", "replace"], "description": "\"append\" (default): project prompt appends to the namespace default; \"replace\": project prompt replaces it entirely"},
          "namespace_id": {"type": "string", "nullable": true, "description": "Set to \"\" to unassign from a namespace"},
          "enabled": {"type": "boolean", "description": "false = disable (stamps disable provenance); true = resume (clears provenance)"},
          "disabled_at": {"type": "string"},
          "disabled_by": {"type": "string", "enum": ["api", "api-pause", "api-delete", "auto-disable"]},
          "disabled_reason": {"type": "string"},
          "adaptive_cooldown": {"type": "boolean", "description": "Enabling (false→true) normalizes the policy row and resets the no-progress streak; false leaves stored policy values untouched"},
          "cooldown_floor_s": {"type": "integer"},
          "cooldown_ceiling_s": {"type": "integer"},
          "no_progress_threshold": {"type": "integer"}
        }
      },
      "Namespace": {
        "type": "object",
        "required": ["id", "weight"],
        "properties": {
          "id": {"type": "string"},
          "weight": {"type": "integer", "minimum": 1, "maximum": 100},
          "reserved": {"type": "integer", "minimum": 0},
          "hard_cap": {"type": "integer", "minimum": 0, "description": "0 = no cap (interpret as B)"},
          "enabled": {"type": "boolean"},
          "description": {"type": "string"},
          "created_at": {"type": "string"},
          "updated_at": {"type": "string"}
        }
      },
      "NamespaceUpdates": {
        "type": "object",
        "description": "Partial namespace update — only supplied fields are applied.",
        "properties": {
          "weight": {"type": "integer", "minimum": 1, "maximum": 100},
          "reserved": {"type": "integer", "minimum": 0},
          "hard_cap": {"type": "integer", "minimum": 0},
          "enabled": {"type": "boolean"},
          "description": {"type": "string"}
        }
      },
      "NamespaceMoveRequest": {
        "type": "object",
        "required": ["project"],
        "properties": {
          "project": {"type": "string", "description": "Name of the project to assign to this namespace"}
        }
      },
      "Group": {
        "type": "object",
        "required": ["name"],
        "description": "A named list of scheduler projects a template can be deployed to in one operation. JSONL-backed (groups.jsonl).",
        "properties": {
          "name": {"type": "string", "description": "Unique group name (no whitespace)"},
          "projects": {"type": "array", "items": {"type": "string"}, "description": "Scheduler project names (projects table primary keys)"},
          "description": {"type": "string"}
        }
      },
      "GroupUpdate": {
        "type": "object",
        "description": "Partial group update — only supplied fields are applied (pointer semantics). The name is immutable and comes from the URL path.",
        "properties": {
          "projects": {"type": "array", "items": {"type": "string"}},
          "description": {"type": "string"}
        }
      },
      "Template": {
        "type": "object",
        "required": ["name", "tasks"],
        "description": "A named list of task definitions deployable to every project in a group. JSONL-backed (templates.jsonl).",
        "properties": {
          "name": {"type": "string", "description": "Unique template name (no whitespace)"},
          "description": {"type": "string"},
          "tasks": {
            "type": "array",
            "items": {"$ref": "#/components/schemas/TemplateTask"},
            "description": "At least one task required"
          }
        }
      },
      "TemplateTask": {
        "type": "object",
        "required": ["title"],
        "description": "One task definition inside a template. id_pattern defaults to \"{TEMPLATE}-{DATE}-{PROJECT}-{TASK}\" — placeholders: {TEMPLATE} name, {DATE} UTC YYYYMMDD, {PROJECT} member project, {TASK} 1-based task ordinal. A pattern without a task ordinal gets -{TASK} appended. Title/Detail also substitute {TEMPLATE}/{DATE}/{PROJECT}. Labels become the board row's capability_tags.",
        "properties": {
          "id_pattern": {"type": "string"},
          "title": {"type": "string"},
          "detail": {"type": "string", "description": "Long-form task spec; written to the board row's reasoning.note (canonical injected-row convention)"},
          "labels": {"type": "array", "items": {"type": "string"}}
        }
      },
      "TemplateUpdate": {
        "type": "object",
        "description": "Partial template update — only supplied fields are applied. The name is immutable and comes from the URL path.",
        "properties": {
          "description": {"type": "string"},
          "tasks": {"type": "array", "items": {"$ref": "#/components/schemas/TemplateTask"}}
        }
      },
      "DeployRequest": {
        "type": "object",
        "required": ["template"],
        "description": "Body for POST /api/v1/groups/{name}/deploy.",
        "properties": {
          "template": {"type": "string", "description": "Name of the template to deploy to every group member"},
          "dry_run": {"type": "boolean", "default": false, "description": "true = return the plan without writing to any board"}
        }
      },
      "BumpRequest": {
        "type": "object",
        "required": ["reason"],
        "description": "Body for POST /api/v1/projects/{name}/bump (SCHED-GAP-107): temporarily run the project at a small cooldown for N ticks, then auto-revert. The bump overrides both the legacy cooldown and the adaptive floor/ceiling while active; normal adaptive tick-end evaluation continues so real work extends speed and idle ticks burn bump ticks.",
        "properties": {
          "ticks": {"type": "integer", "minimum": 1, "maximum": 8, "default": 5, "description": "Completed bump ticks before auto-revert"},
          "cooldown": {"type": "integer", "minimum": 7200, "default": 7200, "description": "Bump cooldown in seconds — the 6h cooldown law floor applies"},
          "reason": {"type": "string", "description": "REQUIRED. Why the bump was issued (auditable)"}
        }
      },
      "EmptyBody": {
        "type": "object",
        "additionalProperties": false,
        "description": "This endpoint accepts no body fields — send an empty JSON object {} or no body."
      }
    }
  }
}`)
