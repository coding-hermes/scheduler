package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/version"
)

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

// ProjectFailureRate is the per-project failure-rate breakdown for a single
// project over a window of recent ticks. It appears in /api/v1/status under
// the "projects_failure_rates" key (SCHED-GAP-018).
type ProjectFailureRate struct {
	Failed      int     `json:"failed"`
	Total       int     `json:"total"`
	FailureRate float64 `json:"failure_rate"`

	// AutoDisableArmed reports whether this project currently meets the
	// auto-disable condition (GAP-047): the feature is enabled
	// (threshold > 0), the sample size reaches minTicks, and the failure
	// rate is at or above the threshold. It mirrors the exact condition in
	// internal/scheduler/alert_escalation.go CheckFailureRateAutoDisable.
	AutoDisableArmed bool `json:"auto_disable_armed"`
}

// computeProjectFailureRates returns a per-project failure-rate breakdown
// computed over the last `window` completed ticks per project. Only projects
// with at least one tick in the window are included. "failed" counts both
// 'failed' and 'timeout' statuses (both are waste — non-completed outcomes).
// "total" is the number of ticks in the window with a non-null completed_at
// (running/queued ticks are excluded). failure_rate = failed/total, rounded
// to 4 decimal places.
//
// `threshold` and `minTicks` drive the AutoDisableArmed flag (GAP-047) and
// mirror the auto-disable policy in alert_escalation.go: armed when
// threshold > 0 && total >= minTicks && rate >= threshold, where rate is the
// unrounded failed/total ratio (matching CheckFailureRateAutoDisable exactly).
func computeProjectFailureRates(ctx context.Context, db *sql.DB, window int, threshold float64, minTicks int) map[string]ProjectFailureRate {
	if window <= 0 {
		window = 100
	}
	// Mirror CheckFailureRateAutoDisable's effective sample-size default.
	if minTicks <= 0 {
		minTicks = 50
	}
	out := map[string]ProjectFailureRate{}

	// Per-project indexed loop (PERF-001). The windowed-CTE alternative was
	// measured through the modernc.org/sqlite driver against the production
	// DB (57k ticks, 254k events) and is a REGRESSION: ROW_NUMBER() OVER
	// PARTITION BY forces a temp b-tree over ALL ~57k completed rows and
	// takes 128-212ms, while this loop's per-project ORDER BY spawned_at DESC
	// LIMIT is served by the idx_ticks_project_spawned(project_name,
	// spawned_at) index — ~0.1ms per project, ~5ms total for 44 projects.
	// The N+1 was never the bottleneck; window functions are what's slow in
	// the pure-Go driver.
	//
	// DOGFOOD-009: only EXISTING projects are considered. A hard-deleted
	// project (row purged from the projects table, e.g. eduos-e2e) leaves
	// historical ticks behind; without the join those ticks resurfaced as
	// ghost failure-rate entries (failure_rate=1.0, auto_disable_armed=true)
	// that could never be cleared. The DISTINCT list is built through the
	// projects JOIN so ghost ticks never enter the loop.
	projects, err := db.QueryContext(ctx,
		`SELECT DISTINCT t.project_name FROM ticks t
		 JOIN projects p ON p.name = t.project_name
		 WHERE t.completed_at IS NOT NULL`)
	if err != nil {
		return out
	}
	defer projects.Close()

	var names []string
	for projects.Next() {
		var name string
		if err := projects.Scan(&name); err == nil {
			names = append(names, name)
		}
	}

	for _, name := range names {
		rows, err := db.QueryContext(ctx,
			`SELECT status FROM ticks
			 WHERE project_name = ? AND completed_at IS NOT NULL
			 ORDER BY spawned_at DESC LIMIT ?`,
			name, window)
		if err != nil {
			continue
		}
		var failed, total int
		for rows.Next() {
			var status string
			if err := rows.Scan(&status); err != nil {
				continue
			}
			total++
			if status == "failed" || status == "timeout" {
				failed++
			}
		}
		rows.Close()
		if total == 0 {
			continue
		}
		rate := float64(failed) / float64(total)
		// GAP-047: armed uses the unrounded rate, exactly like
		// CheckFailureRateAutoDisable (which compares the raw ratio against
		// the threshold before any display rounding).
		armed := threshold > 0 && total >= minTicks && rate >= threshold
		// Round to 4 decimal places for clean JSON output.
		rate = float64(int(rate*10000)) / 10000
		out[name] = ProjectFailureRate{
			Failed:           failed,
			Total:            total,
			FailureRate:      rate,
			AutoDisableArmed: armed,
		}
	}
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
		       COALESCE(urgency,0.0) as urgency,
		       COALESCE(weight_used,0) as weight_used,
		       COALESCE(error,'') as error,
		       created_at
		FROM ticks WHERE project_name = ? ORDER BY spawned_at DESC LIMIT 1
	`, project)
	var t database.Tick
	err := row.Scan(&t.ID, &t.ProjectName, &t.SessionID, &t.Status, &t.Outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed, &t.Error, &t.CreatedAt)
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
		       COALESCE(urgency,0.0) as urgency,
		       COALESCE(weight_used,0) as weight_used,
		       COALESCE(error,'') as error,
		       created_at
		FROM ticks WHERE id = ?
	`, id)
	var t database.Tick
	err := row.Scan(&t.ID, &t.ProjectName, &t.SessionID, &t.Status, &t.Outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed, &t.Error, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func listTicks(ctx context.Context, db *sql.DB, project, status string, limit int) ([]database.Tick, error) {
	q := "SELECT id, project_name, COALESCE(session_id,'') as session_id, status, COALESCE(outcome,'') as outcome, COALESCE(spawned_at,'') as spawned_at, COALESCE(completed_at,'') as completed_at, COALESCE(exit_code,0) as exit_code, COALESCE(commits,0) as commits, COALESCE(files_changed,0) as files_changed, COALESCE(tokens_in,0) as tokens_in, COALESCE(tokens_out,0) as tokens_out, COALESCE(cost_usd,0.0) as cost_usd, COALESCE(urgency,0.0) as urgency, COALESCE(weight_used,0) as weight_used, COALESCE(error,'') as error, created_at FROM ticks WHERE 1=1"
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
			&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed, &t.Error, &t.CreatedAt); err != nil {
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
	now := time.Now()
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
          "200": {"description": "Returns budget, active projects, tick counts, recent outcomes, gateway_errors (transient gateway spawn failures since restart)"}
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
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"},
          "gateway_key": {"type": "string"},
          "command": {"type": "string"},
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
          "consecutive_failures": {"type": "integer"}
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
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"},
          "gateway_key": {"type": "string", "description": "Per-foreman gateway key; \"\" clears back to the daemon's shared key"},
          "command": {"type": "string"},
          "namespace_id": {"type": "string", "nullable": true, "description": "Set to \"\" to unassign from a namespace"},
          "enabled": {"type": "boolean", "description": "false = disable (stamps disable provenance); true = resume (clears provenance)"},
          "disabled_at": {"type": "string"},
          "disabled_by": {"type": "string", "enum": ["api", "api-pause", "api-delete", "auto-disable"]},
          "disabled_reason": {"type": "string"}
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
      "EmptyBody": {
        "type": "object",
        "additionalProperties": false,
        "description": "This endpoint accepts no body fields — send an empty JSON object {} or no body."
      }
    }
  }
}`)
