package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coding-herms/scheduler/internal/database"
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

func listQueue(ctx context.Context, db *sql.DB) ([]queueItem, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, COALESCE(weight,0), COALESCE(priority,0), COALESCE(cooldown_s,0), COALESCE(enabled,1) FROM projects WHERE enabled = 1 ORDER BY priority DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []queueItem
	for rows.Next() {
		var it queueItem
		if err := rows.Scan(&it.Project, &it.Weight, &it.Priority, &it.CooldownS, &it.Enabled); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// openapiSpec is the OpenAPI 3.0 specification for the scheduler API.
var openapiSpec = []byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Coding Hermes Scheduler API",
    "version": "1.0.0",
    "description": "REST API for the Coding Hermes fleet scheduler — manage projects, namespaces, ticks, and fleet health."
  },
  "servers": [{"url": "http://127.0.0.1:9090", "description": "Local scheduler daemon"}],
  "paths": {
    "/api/v1/health": {
      "get": {
        "summary": "Daemon health check",
        "responses": {
          "200": {"description": "OK — returns uptime, DB status, active ticks, spawn counts"}
        }
      }
    },
    "/api/v1/status": {
      "get": {
        "summary": "Fleet overview",
        "responses": {
          "200": {"description": "Returns budget, active projects, tick counts, recent outcomes"}
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
      }
    },
    "/api/v1/projects/{name}/spawn": {
      "post": {
        "summary": "Manually trigger a tick for this project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "202": {"description": "Tick enqueued — returns tick_id"}
        }
      }
    },
    "/api/v1/projects/{name}/pause": {
      "post": {
        "summary": "Pause a project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Project paused"}
        }
      }
    },
    "/api/v1/projects/{name}/resume": {
      "post": {
        "summary": "Resume a project",
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Project resumed"}
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
        "summary": "Update namespace",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {
          "200": {"description": "Updated namespace"}
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
        "responses": {
          "200": {"description": "Evaluation triggered"}
        }
      }
    },
    "/api/v1/pause": {
      "post": {
        "summary": "Pause the scheduler globally",
        "responses": {
          "200": {"description": "Scheduler paused"}
        }
      }
    },
    "/api/v1/resume": {
      "post": {
        "summary": "Resume the scheduler globally",
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
        "properties": {
          "name": {"type": "string"},
          "repo_url": {"type": "string"},
          "workdir": {"type": "string"},
          "weight": {"type": "integer"},
          "priority": {"type": "integer"},
          "cooldown_s": {"type": "integer"},
          "enabled": {"type": "boolean"},
          "namespace_id": {"type": "string"},
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"}
        }
      },
      "ProjectUpdates": {
        "type": "object",
        "properties": {
          "weight": {"type": "integer"},
          "priority": {"type": "integer"},
          "cooldown_s": {"type": "integer"},
          "enabled": {"type": "boolean"},
          "worker_model": {"type": "string"},
          "worker_provider": {"type": "string"}
        }
      },
      "Namespace": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "weight": {"type": "integer"},
          "reserved": {"type": "integer"},
          "hard_cap": {"type": "integer"}
        }
      }
    }
  }
}`)
