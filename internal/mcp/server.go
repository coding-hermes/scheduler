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
}

// NewServer creates an MCP server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	s := &Server{db: db, loop: loop}
	if loop != nil {
		s.SetClock(loop.Clock())
	}
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
