package api

import (
	"context"
	"database/sql"
	"net/http"
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
	mux.HandleFunc("/api/v1/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/v1/namespaces/", s.handleNamespaceByID)
	mux.HandleFunc("/api/v1/ticks", s.handleTicks)
	mux.HandleFunc("/api/v1/ticks/", s.handleTickByID)
	mux.HandleFunc("/api/v1/evaluate", s.evaluate)
	mux.HandleFunc("/api/v1/pause", s.pause)
	mux.HandleFunc("/api/v1/resume", s.resume)
	mux.HandleFunc("/api/v1/events", s.events)
	mux.HandleFunc("/api/v1/queue", s.queue)
	mux.HandleFunc("/api/v1/openapi.json", s.openapi)
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
	lastEval := s.loop.LastEvalTime()
	// last_evaluation is RFC3339. Zero time serializes as "0001-01-01T00:00:00Z"
	// when the loop has never evaluated yet — callers can compare against
	// evaluation_age_seconds (which is 0 in that case) instead.
	var lastEvalStr string
	var evalAge float64
	if lastEval.IsZero() {
		lastEvalStr = ""
		evalAge = 0
	} else {
		lastEvalStr = lastEval.UTC().Format(time.RFC3339)
		evalAge = time.Since(lastEval).Seconds()
	}
	httpCount, execCount := s.loop.SpawnMethodCounts()
	writeJSON(w, 200, map[string]interface{}{
		"status":                 "ok",
		"uptime":                 time.Since(s.started).String(),
		"db":                     dbOK,
		"active_ticks":           activeTicks,
		"last_evaluation":        lastEvalStr,
		"evaluation_age_seconds": evalAge,
		"spawns_http":            httpCount,
		"spawns_exec":            execCount,
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
	lastEval := getLastEvalTime(ctx, s.db)
	writeJSON(w, 200, map[string]interface{}{
		"budget_total":    100,
		"active_projects": len(projects),
		"active_ticks":    activeTicks,
		"recent_outcomes": recentOutcomes,
		"last_evaluation": lastEval,
	})
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
