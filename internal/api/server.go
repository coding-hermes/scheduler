package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// Server is the HTTP API server for the fleet scheduler.
type Server struct {
	db      *sql.DB
	loop    *scheduler.Loop
	started time.Time
}

// NewServer creates an API server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	return &Server{
		db:      db,
		loop:    loop,
		started: time.Now(),
	}
}

// Handler returns an http.Handler for all API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.health)
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/projects", s.handleProjects)
	mux.HandleFunc("/api/v1/projects/", s.handleProjectByID)
	mux.HandleFunc("/api/v1/ticks", s.handleTicks)
	mux.HandleFunc("/api/v1/ticks/", s.handleTickByID)
	mux.HandleFunc("/api/v1/evaluate", s.evaluate)
	mux.HandleFunc("/api/v1/pause", s.pause)
	mux.HandleFunc("/api/v1/resume", s.resume)
	mux.HandleFunc("/api/v1/events", s.events)
	return mux
}

// health returns server health status.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	activeTicks := countActiveTicks(ctx, s.db)
	dbOK := "connected"
	if err := s.db.PingContext(ctx); err != nil {
		dbOK = "error: " + err.Error()
	}
	writeJSON(w, 200, map[string]interface{}{
		"status":       "ok",
		"uptime":       time.Since(s.started).String(),
		"db":           dbOK,
		"active_ticks": activeTicks,
	})
}

// status returns fleet overview.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, s.db, true)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	activeTicks := countActiveTicks(ctx, s.db)
	recentOutcomes := countRecentOutcomes(ctx, s.db)
	writeJSON(w, 200, map[string]interface{}{
		"budget_total":    100,
		"active_projects": len(projects),
		"active_ticks":    activeTicks,
		"recent_outcomes": recentOutcomes,
	})
}

// handleProjects handles GET (list) and POST (create).
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProjects(w, r)
	case http.MethodPost:
		s.createProject(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if projects == nil {
		projects = []database.Project{}
	}
	writeJSON(w, 200, map[string]interface{}{"projects": projects})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var p database.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if p.Name == "" || p.RepoURL == "" || p.Workdir == "" {
		writeError(w, 400, "name, repo_url, workdir are required")
		return
	}
	if err := database.CreateProject(context.Background(), s.db, &p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, 409, "project already exists")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, p)
}

// handleProjectByID handles GET, PUT, POST on /projects/:name and sub-routes.
func (s *Server) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	// Strip the /api/v1/projects/ prefix to get the resource path.
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	parts := splitPath(path)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, 400, "project name required")
		return
	}
	name := parts[0]

	// POST /projects/:name/pause or /resume
	if len(parts) == 2 && r.Method == http.MethodPost {
		switch parts[1] {
		case "pause":
			s.pauseProject(w, r, name)
			return
		case "resume":
			s.resumeProject(w, r, name)
			return
		}
		writeError(w, 404, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getProject(w, r, name)
	case http.MethodPut:
		s.updateProject(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, or POST only")
	}
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	tick, _ := getLatestTick(ctx, s.db, name)
	writeJSON(w, 200, map[string]interface{}{
		"project":    p,
		"latest_tick": tick,
	})
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, name string) {
	var updates database.ProjectUpdates
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := database.UpdateProject(context.Background(), s.db, name, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	p, _ := database.GetProject(context.Background(), s.db, name)
	writeJSON(w, 200, p)
}

func (s *Server) pauseProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := database.UpdateProject(context.Background(), s.db, name, database.ProjectUpdates{Enabled: boolPtr(false)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "paused", "project": name})
}

func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := database.UpdateProject(context.Background(), s.db, name, database.ProjectUpdates{Enabled: boolPtr(true)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "resumed", "project": name})
}

// handleTicks handles GET /ticks with optional query params.
func (s *Server) handleTicks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	project := r.URL.Query().Get("project")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	ticks, err := listTicks(ctx, s.db, project, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if ticks == nil {
		ticks = []database.Tick{}
	}
	writeJSON(w, 200, map[string]interface{}{"ticks": ticks, "count": len(ticks)})
}

// handleTickByID handles GET /ticks/:id.
func (s *Server) handleTickByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/ticks/")
	if id == "" {
		writeError(w, 400, "tick id required")
		return
	}
	ctx := context.Background()
	tick, err := getTick(ctx, s.db, id)
	if err != nil {
		writeError(w, 404, "tick not found")
		return
	}
	writeJSON(w, 200, tick)
}

// evaluate triggers a forced evaluation cycle.
func (s *Server) evaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.ForceEvaluate()
	writeJSON(w, 200, map[string]string{"status": "evaluation triggered"})
}

// pause suspends the scheduler loop.
func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.Pause()
	writeJSON(w, 200, map[string]string{"status": "paused"})
}

// resume unpauses the scheduler loop.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	s.loop.Resume()
	writeJSON(w, 200, map[string]string{"status": "resumed"})
}

// events returns the event log with optional filters.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	level := r.URL.Query().Get("level")
	project := r.URL.Query().Get("project")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := listEvents(ctx, s.db, level, project, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if events == nil {
		events = []database.Event{}
	}
	writeJSON(w, 200, map[string]interface{}{"events": events, "count": len(events)})
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

func boolPtr(b bool) *bool { return &b }

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

func listTicks(ctx context.Context, db *sql.DB, project string, limit int) ([]database.Tick, error) {
	q := "SELECT id, project_name, COALESCE(session_id,'') as session_id, status, COALESCE(outcome,'') as outcome, COALESCE(spawned_at,'') as spawned_at, COALESCE(completed_at,'') as completed_at, COALESCE(exit_code,0) as exit_code, COALESCE(commits,0) as commits, COALESCE(files_changed,0) as files_changed, COALESCE(tokens_in,0) as tokens_in, COALESCE(tokens_out,0) as tokens_out, COALESCE(cost_usd,0.0) as cost_usd, COALESCE(urgency,0.0) as urgency, COALESCE(weight_used,0) as weight_used, COALESCE(error,'') as error, created_at FROM ticks"
	var args []interface{}
	if project != "" {
		q += " WHERE project_name = ?"
		args = append(args, project)
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

func listEvents(ctx context.Context, db *sql.DB, level, project string, limit int) ([]database.Event, error) {
	q := "SELECT id, timestamp, level, project_name, message, detail, created_at FROM events WHERE 1=1"
	var args []interface{}
	if level != "" {
		q += " AND level = ?"
		args = append(args, level)
	}
	if project != "" {
		q += " AND project_name = ?"
		args = append(args, project)
	}
	q += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []database.Event
	for rows.Next() {
		var e database.Event
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Level, &e.ProjectName, &e.Message, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
