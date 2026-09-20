package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/version"
)

// Server is the MCP-over-HTTP server for Hermes integration.
type Server struct {
	// clk is the server's time seam (SCHED-GAP-169). NewServer seeds it from
	// the loop's clock so MCP-written timestamps share the scheduler's
	// timeline; the zero value reads as the wall clock.
	clk  clock.Seam
	db   *sql.DB
	loop *scheduler.Loop
	// blocksStore is the JSONL-backed deploy groups/templates store
	// (internal/blocks), shared with the API server. Installed by main.go
	// via SetBlocksStore; when nil the blocks tools answer a clear
	// configuration error instead of panicking.
	blocksStore *blocks.Store
	// started is when this server was built. It is the origin for the
	// metrics_get uptime_s field (the daemon constructs the MCP server at
	// boot, so this is daemon uptime). Seeded in NewServer from the same
	// clock the rest of the server reads time through.
	started time.Time
}

// NewServer creates an MCP server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	s := &Server{db: db, loop: loop}
	if loop != nil {
		s.SetClock(loop.Clock())
	}
	s.started = s.clock().Now()
	return s
}

// SetClock installs the clock this server reads time through (SCHED-GAP-169).
// nil keeps the current clock.
func (s *Server) SetClock(c clock.Clock) { s.clk.Set(c) }

// clock returns the server's clock, never nil.
func (s *Server) clock() clock.Clock { return s.clk.Get() }

// SetBlocksStore installs the JSONL-backed deploy groups/templates store
// behind the groups_*/templates_*/groups_deploy MCP tools. Mirrors
// api.Server.SetBlocksStore: main.go resolves the store paths once (the
// --db dir by default, --groups-file/--templates-file or [scheduler] TOML
// overrides) so MCP and the REST API read and write the SAME JSONL files.
func (s *Server) SetBlocksStore(st *blocks.Store) {
	s.blocksStore = st
}

// Handler returns HTTP handler for MCP endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	return mux
}

// MCPRequest is the JSON-RPC envelope from MCP clients.
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse is the JSON-RPC response envelope.
type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *MCPError   `json:"error,omitempty"`
}

// MCPError is a JSON-RPC error.
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ToolDefinition is an MCP tool schema.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

var tools = []ToolDefinition{
	{
		Name:        "fleet_status",
		Description: "Return fleet-wide status: total projects, active ticks, budget, running foremen",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "fleet_projects",
		Description: "List all managed projects with weight, priority, and last tick info",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "fleet_project_detail",
		Description: "Get detailed info for one project including tick history",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Project name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "fleet_set_weight",
		Description: "Set a project's weight (1-100). Higher weight = more budget consumed per tick.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":   map[string]interface{}{"type": "string", "description": "Project name"},
				"weight": map[string]interface{}{"type": "integer", "description": "New weight (1-100)"},
			},
			"required": []string{"name", "weight"},
		},
	},
	{
		Name:        "fleet_set_priority",
		Description: "Set a project's priority (1-10). Higher priority = more frequent ticks.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Project name"},
				"priority": map[string]interface{}{"type": "integer", "description": "New priority (1-10)"},
			},
			"required": []string{"name", "priority"},
		},
	},
	{
		Name:        "fleet_set_cooldown",
		Description: "Set minimum seconds between successive ticks for a project.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Project name"},
				"cooldown": map[string]interface{}{"type": "integer", "description": "Cooldown seconds"},
			},
			"required": []string{"name", "cooldown"},
		},
	},
	{
		Name:        "fleet_set_decay",
		Description: "Set a project's urgency decay rate. Higher = urgency builds faster when idle.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":  map[string]interface{}{"type": "string", "description": "Project name"},
				"decay": map[string]interface{}{"type": "number", "description": "Decay rate (default 1.0)"},
			},
			"required": []string{"name", "decay"},
		},
	},
	{
		Name:        "fleet_pause",
		Description: "Pause a project (disable scheduling).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Project name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "fleet_resume",
		Description: "Resume a paused project.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Project name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "fleet_add",
		Description: "Add a new project to the fleet. Accepts repo or repo_url (alias) for the git URL.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Project name"},
				"repo":     map[string]interface{}{"type": "string", "description": "Git repo URL"},
				"repo_url": map[string]interface{}{"type": "string", "description": "Alias for repo (REST-style name)"},
				"workdir":  map[string]interface{}{"type": "string", "description": "Local working directory"},
				"weight":   map[string]interface{}{"type": "integer", "description": "Initial weight (default 10)"},
			},
			"required": []string{"name", "repo", "workdir"},
		},
	},
	{
		Name:        "fleet_ticks",
		Description: "List recent ticks with optional project filter.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project": map[string]interface{}{"type": "string", "description": "Filter by project name"},
				"limit":   map[string]interface{}{"type": "integer", "description": "Max results (default 20)"},
			},
		},
	},
	{
		Name:        "fleet_evaluate",
		Description: "Force immediate evaluation cycle.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "fleet_pause_scheduler",
		Description: "Pause the entire scheduler loop.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "fleet_resume_scheduler",
		Description: "Resume the scheduler loop.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "groups_list",
		Description: "List all deploy groups (named project lists) from the scheduler's groups.jsonl store",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "groups_get",
		Description: "Get one deploy group by name",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Group name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "groups_create",
		Description: "Create a deploy group (a named list of scheduler projects a template can be deployed to)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":        map[string]interface{}{"type": "string", "description": "Group name (no whitespace)"},
				"description": map[string]interface{}{"type": "string", "description": "What the group is for"},
				"projects":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Member project names"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "groups_update",
		Description: "Partially update a deploy group (projects and/or description)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Group name"},
				"patch": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"projects":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
						"description": map[string]interface{}{"type": "string"},
					},
				},
			},
			"required": []string{"name", "patch"},
		},
	},
	{
		Name:        "groups_delete",
		Description: "Delete a deploy group",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Group name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "templates_list",
		Description: "List all deploy templates (named task definitions) from the scheduler's templates.jsonl store",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "templates_get",
		Description: "Get one deploy template by name",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Template name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "templates_create",
		Description: "Create a deploy template (a named list of task definitions deployable to groups)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":        map[string]interface{}{"type": "string", "description": "Template name (no whitespace)"},
				"description": map[string]interface{}{"type": "string", "description": "What the template deploys"},
				"tasks": map[string]interface{}{
					"type":        "array",
					"description": "Task definitions ({id_pattern,title,detail,labels}); title required",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id_pattern": map[string]interface{}{"type": "string", "description": "Task id pattern ({TEMPLATE},{DATE},{PROJECT},{TASK} placeholders)"},
							"title":      map[string]interface{}{"type": "string", "description": "Task title"},
							"detail":     map[string]interface{}{"type": "string", "description": "Task detail/body"},
							"labels":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Capability tags"},
						},
					},
				},
			},
			"required": []string{"name", "tasks"},
		},
	},
	{
		Name:        "templates_update",
		Description: "Partially update a deploy template (description and/or tasks)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Template name"},
				"patch": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"description": map[string]interface{}{"type": "string"},
						"tasks": map[string]interface{}{
							"type":  "array",
							"items": map[string]interface{}{"type": "object"},
						},
					},
				},
			},
			"required": []string{"name", "patch"},
		},
	},
	{
		Name:        "templates_delete",
		Description: "Delete a deploy template",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Template name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "groups_deploy",
		Description: "Deploy a template's task rows to every member project of a group (dry_run plans without writing)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"group":    map[string]interface{}{"type": "string", "description": "Group name"},
				"template": map[string]interface{}{"type": "string", "description": "Template name"},
				"dry_run":  map[string]interface{}{"type": "boolean", "description": "Plan only — write nothing (default false)"},
			},
			"required": []string{"group", "template"},
		},
	},
	{
		Name:        "events_list",
		Description: "Read the scheduler event log; since returns only events with id > since (incremental tail polling)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"since":     map[string]interface{}{"type": "integer", "description": "Return only events with id > since"},
				"limit":     map[string]interface{}{"type": "integer", "description": "Max results (default 100)"},
				"severity":  map[string]interface{}{"type": "string", "description": "Filter by severity (CRITICAL/HIGH/MEDIUM/LOW/INFO)"},
				"component": map[string]interface{}{"type": "string", "description": "Filter by component"},
			},
		},
	},
	// ── CTL-003: parity with the remaining /api/v1 control + read routes ──
	// One tool per uncovered openapi operation (namespaces CRUD/projects/move,
	// project delete/spawn/bump/unbump, tick detail, config, queue, metrics).
	// The parity guard (mcp_api_parity_test.go) fails the build if a route
	// ships without a tool here.
	{
		Name:        "namespaces_list",
		Description: "List all namespaces (enabled and disabled) — the allocation pools projects are assigned to",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "namespaces_get",
		Description: "Get one namespace by id (weight, caps, admission_mode, load_gate, wave config)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "Namespace id"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "namespaces_create",
		Description: "Create a namespace. Field names match the REST body; id and a positive weight are required.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":             map[string]interface{}{"type": "string", "description": "Namespace id (unique slug, e.g. \"coding-hermes\")"},
				"weight":         map[string]interface{}{"type": "integer", "description": "Relative weight for proportional allocation (1..100, required)"},
				"reserved":       map[string]interface{}{"type": "integer", "description": "Guaranteed floor budget units (>= 0)"},
				"hard_cap":       map[string]interface{}{"type": "integer", "description": "Maximum budget; 0 = no cap"},
				"max_concurrent": map[string]interface{}{"type": "integer", "description": "Max ticks running at once in this namespace; 0 = unlimited (global cap still applies)"},
				"enabled":        map[string]interface{}{"type": "boolean", "description": "Disabled namespaces get zero allocation"},
				"description":    map[string]interface{}{"type": "string", "description": "Human-readable label"},
				"default_prompt": map[string]interface{}{"type": "string", "description": "Foreman prompt default for this namespace's projects"},
				"model_chain":    map[string]interface{}{"type": "string", "description": "Ordered model@provider hops (JSON array string)"},
				"wave_enabled":   map[string]interface{}{"type": "boolean", "description": "Allow concurrent wave scheduling in this namespace"},
				"admission_mode": map[string]interface{}{"type": "string", "description": "\"cooldown\" (default) or \"tasks\""},
				"load_gate":      map[string]interface{}{"type": "string", "description": "\"off\" opts this namespace out of the load gate; empty = gate applies"},
			},
			"required": []string{"id", "weight"},
		},
	},
	{
		Name:        "namespaces_update",
		Description: "Partially update a namespace (only the fields you pass are written). Patch fields are top-level (same shape as the REST PUT body) or nested under \"patch\".",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":             map[string]interface{}{"type": "string", "description": "Namespace id"},
				"weight":         map[string]interface{}{"type": "integer", "description": "New weight"},
				"reserved":       map[string]interface{}{"type": "integer", "description": "New reserved floor"},
				"hard_cap":       map[string]interface{}{"type": "integer", "description": "New hard cap (0 = no cap)"},
				"max_concurrent": map[string]interface{}{"type": "integer", "description": "New max concurrent ticks (0 = unlimited)"},
				"enabled":        map[string]interface{}{"type": "boolean", "description": "Enable/disable the namespace"},
				"description":    map[string]interface{}{"type": "string", "description": "New description"},
				"default_prompt": map[string]interface{}{"type": "string", "description": "New namespace foreman prompt default"},
				"model_chain":    map[string]interface{}{"type": "string", "description": "New model chain (JSON array string)"},
				"wave_enabled":   map[string]interface{}{"type": "boolean", "description": "Wave switch"},
				"admission_mode": map[string]interface{}{"type": "string", "description": "\"cooldown\" or \"tasks\""},
				"load_gate":      map[string]interface{}{"type": "string", "description": "\"off\" or \"\""},
				"patch":          map[string]interface{}{"type": "object", "description": "Optional nested patch object carrying the same fields"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "namespaces_delete",
		Description: "Delete a namespace. confirm=true soft-deletes it (enabled=false, members unassigned, row retained); confirm=true&purge=true hard-deletes the row. Refused while the namespace still has ENABLED member projects.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":      map[string]interface{}{"type": "string", "description": "Namespace id"},
				"confirm": map[string]interface{}{"type": "boolean", "description": "Required: acknowledges the soft delete"},
				"purge":   map[string]interface{}{"type": "boolean", "description": "Also hard-delete the row permanently (requires confirm=true)"},
			},
			"required": []string{"id", "confirm"},
		},
	},
	{
		Name:        "namespaces_projects",
		Description: "List the projects assigned to a namespace (enabled and disabled alike)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "Namespace id"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "namespaces_move",
		Description: "Assign a project to a namespace (sets the project's namespace_id). Returns the updated project.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":      map[string]interface{}{"type": "string", "description": "Target namespace id"},
				"project": map[string]interface{}{"type": "string", "description": "Project name to move"},
			},
			"required": []string{"id", "project"},
		},
	},
	{
		Name:        "project_delete",
		Description: "Delete a project. confirm=true soft-deletes it (enabled=false, row retained, disable provenance + event logged); confirm=true&purge=true hard-deletes the row. Refused while the project is ENABLED — pause it first.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":    map[string]interface{}{"type": "string", "description": "Project name"},
				"confirm": map[string]interface{}{"type": "boolean", "description": "Required: acknowledges the soft delete"},
				"purge":   map[string]interface{}{"type": "boolean", "description": "Also hard-delete the row permanently (requires confirm=true)"},
			},
			"required": []string{"name", "confirm"},
		},
	},
	{
		Name:        "project_spawn",
		Description: "Spawn a tick for a project immediately (bypasses cooldown). Returns the real stored tick_id, resolvable via tick_get. Refused with the scheduler's error when the project already has a tick in flight.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Project name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "project_bump",
		Description: "Temporarily accelerate a project (bump): run at a small cooldown for up to 8 ticks, then auto-revert. reason is REQUIRED; ticks default 5 (1..8); cooldown defaults 7200s and must be >= 7200s. Refused while the project is disabled or already bumped.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":     map[string]interface{}{"type": "string", "description": "Project name"},
				"ticks":    map[string]interface{}{"type": "integer", "description": "Ticks to run at the bump cooldown, 1..8 (default 5)"},
				"cooldown": map[string]interface{}{"type": "integer", "description": "Bump cooldown seconds, >= 7200 (default 7200)"},
				"reason":   map[string]interface{}{"type": "string", "description": "Why the speed-up is needed (required — bumps are auditable)"},
			},
			"required": []string{"name", "reason"},
		},
	},
	{
		Name:        "project_unbump",
		Description: "Manually abort an active bump, restoring the exact pre-bump cooldown state (Phase A only). Errors when the project does not exist or has no active bump.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Project name"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "tick_get",
		Description: "Get one tick by id, including tick_workers when the tick dispatched a worker wave (serial ticks return the tick alone)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{"type": "string", "description": "Tick id (as returned by fleet_ticks or project_spawn)"},
			},
			"required": []string{"id"},
		},
	},
	{
		Name:        "config_get",
		Description: "Resolved runtime config, HONEST SUBSET of GET /api/v1/config: db_path, weight_budget, paused, gateway_response_timeout, version. The main.go-resolved snapshot fields (listen, min_interval, max_interval, num_levels, max_concurrent, tick_timeout, auto_disable_*, gateway.*, duckbrain) are NOT reachable from MCP and are listed in \"omitted\" — read them from the REST endpoint.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "queue_get",
		Description: "The scheduling queue: enabled projects ordered by urgency, descending (same query as GET /api/v1/queue). urgency is the api handler's priority-only fallback — the MCP server holds no resolved-config interval range, so the payload states that in urgency_source.",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
	{
		Name:        "metrics_get",
		Description: "Fleet metrics in one read-only call (mirror of GET /api/v1/metrics): spawns by namespace/outcome, deferrals by reason, orphan nudges by path, active/queued ticks by namespace, cooldown-expired-unscheduled, tick duration p50/p90/p99, gateway drain-503s, zero-output committed ticks. Every block carries available=true|false — an absent source reports available=false with a reason instead of a zero. ticks.global_cap is not derivable over MCP and is listed in \"unavailable\".",
		InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	},
}

// handleMCP routes MCP protocol requests.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMCPError(w, nil, -32600, "Method Not Allowed")
		return
	}

	var req MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMCPError(w, nil, -32700, "Parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	default:
		writeMCPError(w, req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req MCPRequest) {
	writeMCPResult(w, req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]bool{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "coding-hermes-scheduler",
			"version": version.Current(),
		},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, req MCPRequest) {
	writeMCPResult(w, req.ID, map[string]interface{}{
		"tools": tools,
	})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req MCPRequest) {
	type callParams struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	var params callParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeMCPError(w, req.ID, -32602, "Invalid params: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := s.invokeTool(ctx, params.Name, params.Arguments)
	if err != nil {
		writeMCPError(w, req.ID, -32000, err.Error())
		return
	}

	writeMCPResult(w, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": result},
		},
	})
}

func (s *Server) invokeTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	switch name {
	case "fleet_status":
		return s.toolFleetStatus(ctx)
	case "fleet_projects":
		return s.toolFleetProjects(ctx)
	case "fleet_project_detail":
		return s.toolFleetProjectDetail(ctx, args)
	case "fleet_set_weight":
		return s.toolFleetSetWeight(ctx, args)
	case "fleet_set_priority":
		return s.toolFleetSetPriority(ctx, args)
	case "fleet_set_cooldown":
		return s.toolFleetSetCooldown(ctx, args)
	case "fleet_set_decay":
		return s.toolFleetSetDecay(ctx, args)
	case "fleet_pause":
		return s.toolFleetPause(ctx, args)
	case "fleet_resume":
		return s.toolFleetResume(ctx, args)
	case "fleet_add":
		return s.toolFleetAdd(ctx, args)
	case "fleet_ticks":
		return s.toolFleetTicks(ctx, args)
	case "fleet_evaluate":
		return s.toolFleetEvaluate()
	case "fleet_pause_scheduler":
		return s.toolFleetPauseScheduler()
	case "fleet_resume_scheduler":
		return s.toolFleetResumeScheduler()
	case "groups_list":
		return s.toolGroupsList(ctx)
	case "groups_get":
		return s.toolGroupsGet(ctx, args)
	case "groups_create":
		return s.toolGroupsCreate(ctx, args)
	case "groups_update":
		return s.toolGroupsUpdate(ctx, args)
	case "groups_delete":
		return s.toolGroupsDelete(ctx, args)
	case "templates_list":
		return s.toolTemplatesList(ctx)
	case "templates_get":
		return s.toolTemplatesGet(ctx, args)
	case "templates_create":
		return s.toolTemplatesCreate(ctx, args)
	case "templates_update":
		return s.toolTemplatesUpdate(ctx, args)
	case "templates_delete":
		return s.toolTemplatesDelete(ctx, args)
	case "groups_deploy":
		return s.toolGroupsDeploy(ctx, args)
	case "events_list":
		return s.toolEventsList(ctx, args)
	case "namespaces_list":
		return s.toolNamespacesList(ctx)
	case "namespaces_get":
		return s.toolNamespacesGet(ctx, args)
	case "namespaces_create":
		return s.toolNamespacesCreate(ctx, args)
	case "namespaces_update":
		return s.toolNamespacesUpdate(ctx, args)
	case "namespaces_delete":
		return s.toolNamespacesDelete(ctx, args)
	case "namespaces_projects":
		return s.toolNamespacesProjects(ctx, args)
	case "namespaces_move":
		return s.toolNamespacesMove(ctx, args)
	case "project_delete":
		return s.toolProjectDelete(ctx, args)
	case "project_spawn":
		return s.toolProjectSpawn(ctx, args)
	case "project_bump":
		return s.toolProjectBump(ctx, args)
	case "project_unbump":
		return s.toolProjectUnbump(ctx, args)
	case "tick_get":
		return s.toolTickGet(ctx, args)
	case "config_get":
		return s.toolConfigGet(ctx)
	case "queue_get":
		return s.toolQueueGet(ctx)
	case "metrics_get":
		return s.toolMetricsGet(ctx)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// -- helpers --

func writeMCPResult(w http.ResponseWriter, id *int, result interface{}) {
	resp := MCPResponse{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func writeMCPError(w http.ResponseWriter, id *int, code int, msg string) {
	resp := MCPResponse{JSONRPC: "2.0", ID: id, Error: &MCPError{Code: code, Message: msg}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func jsonString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func getStringArg(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func getIntArg(args map[string]interface{}, key string) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}

func getFloatArg(args map[string]interface{}, key string) float64 {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 1.0
}
