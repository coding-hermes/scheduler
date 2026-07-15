package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// Server is the MCP-over-HTTP server for Hermes integration.
type Server struct {
	db   *sql.DB
	loop *scheduler.Loop
}

// NewServer creates an MCP server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	return &Server{db: db, loop: loop}
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
		Description: "Add a new project to the fleet.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":    map[string]interface{}{"type": "string", "description": "Project name"},
				"repo":    map[string]interface{}{"type": "string", "description": "Git repo URL"},
				"workdir": map[string]interface{}{"type": "string", "description": "Local working directory"},
				"weight":  map[string]interface{}{"type": "integer", "description": "Initial weight (default 10)"},
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
			"version": "1.0.0",
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
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) toolFleetStatus(ctx context.Context) (string, error) {
	projects, _ := database.ListProjects(ctx, s.db, true)
	activeTicks := 0
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&activeTicks)
	return jsonString(map[string]interface{}{
		"total_projects": len(projects),
		"active_ticks":   activeTicks,
		"budget":         100,
	}), nil
}

func (s *Server) toolFleetProjects(ctx context.Context) (string, error) {
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		return "", err
	}
	return jsonString(map[string]interface{}{"projects": projects}), nil
}

func (s *Server) toolFleetProjectDetail(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		return "", err
	}
	// Get last 5 ticks.
	rows, _ := s.db.QueryContext(ctx, `SELECT id, status, outcome, spawned_at, completed_at, commits, files_changed 
		FROM ticks WHERE project_name=? ORDER BY spawned_at DESC LIMIT 5`, name)
	type tickSummary struct {
		ID, Status, Outcome, SpawnedAt, CompletedAt string
		Commits, FilesChanged                       int
	}
	var ticks []tickSummary
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ts tickSummary
			rows.Scan(&ts.ID, &ts.Status, &ts.Outcome, &ts.SpawnedAt, &ts.CompletedAt, &ts.Commits, &ts.FilesChanged)
			ticks = append(ticks, ts)
		}
	}
	return jsonString(map[string]interface{}{"project": p, "recent_ticks": ticks}), nil
}

func (s *Server) toolFleetSetWeight(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	w := getIntArg(args, "weight")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if w < 1 || w > 100 {
		return "", fmt.Errorf("weight must be 1-100, got %d", w)
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Weight: &w}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "weight": strconv.Itoa(w)}), nil
}

func (s *Server) toolFleetSetPriority(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	p := getIntArg(args, "priority")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if p < 1 || p > 10 {
		return "", fmt.Errorf("priority must be 1-10, got %d", p)
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Priority: &p}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "priority": strconv.Itoa(p)}), nil
}

func (s *Server) toolFleetSetCooldown(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	c := getIntArg(args, "cooldown")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{CooldownS: &c}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "cooldown_s": strconv.Itoa(c)}), nil
}

func (s *Server) toolFleetSetDecay(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	d := getFloatArg(args, "decay")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{DecayRate: &d}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "decay": fmt.Sprintf("%.2f", d)}), nil
}

func (s *Server) toolFleetPause(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(false)}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "paused", "project": name}), nil
}

func (s *Server) toolFleetResume(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "resumed", "project": name}), nil
}

func (s *Server) toolFleetAdd(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	repo := getStringArg(args, "repo")
	workdir := getStringArg(args, "workdir")
	weight := getIntArg(args, "weight")
	if name == "" || repo == "" || workdir == "" {
		return "", fmt.Errorf("name, repo, and workdir are required")
	}
	if weight == 0 {
		weight = 10
	}
	p := &database.Project{
		Name:    name,
		RepoURL: repo,
		Workdir: workdir,
		Weight:  weight,
	}
	if err := database.CreateProject(ctx, s.db, p); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "added", "project": name}), nil
}

func (s *Server) toolFleetTicks(ctx context.Context, args map[string]interface{}) (string, error) {
	project := getStringArg(args, "project")
	limit := getIntArg(args, "limit")
	if limit == 0 {
		limit = 20
	}
	q := "SELECT id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed FROM ticks"
	var queryArgs []interface{}
	if project != "" {
		q += " WHERE project_name = ?"
		queryArgs = append(queryArgs, project)
	}
	q += " ORDER BY spawned_at DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)

	rows, err := s.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type tickRow struct {
		ID, ProjectName, Status, Outcome, SpawnedAt, CompletedAt string
		ExitCode, Commits, FilesChanged                          int
	}
	var ticks []tickRow
	for rows.Next() {
		var t tickRow
		rows.Scan(&t.ID, &t.ProjectName, &t.Status, &t.Outcome, &t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged)
		ticks = append(ticks, t)
	}
	return jsonString(map[string]interface{}{"ticks": ticks, "count": len(ticks)}), nil
}

func (s *Server) toolFleetEvaluate() (string, error) {
	s.loop.ForceEvaluate()
	return jsonString(map[string]string{"status": "evaluation triggered"}), nil
}

func (s *Server) toolFleetPauseScheduler() (string, error) {
	s.loop.Pause()
	return jsonString(map[string]string{"status": "scheduler paused"}), nil
}

func (s *Server) toolFleetResumeScheduler() (string, error) {
	s.loop.Resume()
	return jsonString(map[string]string{"status": "scheduler resumed"}), nil
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
