package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// Server is the HTTP API server for the fleet scheduler.
type Server struct {
	db      *sql.DB
	loop    *scheduler.Loop
	started time.Time

	// failureWindow is the number of recent ticks (per project) over which
	// the /api/v1/status per-project failure-rate breakdown is computed.
	// Zero or negative = default of 100.
	failureWindow int

	// duckbrainHealth, when set, is called to include DuckBrain sync health
	// in /api/v1/status. Kept as a func so the API package doesn't import
	// the sync package (no dependency cycle; nil = feature off).
	duckbrainHealth func() map[string]interface{}

	// resolvedConfig is the startup-time snapshot of the active
	// three-layer config served by GET /api/v1/config (SCHED-GAP-034).
	// Populated by main.go via SetResolvedConfig after TOML/env resolution.
	resolvedConfig ResolvedConfig

	// urgencyCalc computes engine-formula urgency scores for
	// GET /api/v1/queue (GAP-054). Built from the resolved interval range
	// (MinInterval/MaxInterval/NumLevels) by SetResolvedConfig; nil when
	// unconfigured or the range is unparseable — listQueue falls back to
	// priority-only scores in that case (tests construct the Server without
	// SetResolvedConfig).
	urgencyCalc *scheduler.UrgencyCalculator
}

// NewServer creates an API server.
func NewServer(db *sql.DB, loop *scheduler.Loop) *Server {
	return &Server{
		db:            db,
		loop:          loop,
		started:       time.Now(),
		failureWindow: 100, // default; override via SetFailureWindow
	}
}

// SetFailureWindow sets the number of recent ticks per project used for the
// /api/v1/status per-project failure-rate breakdown (SCHED-GAP-018).
func (s *Server) SetFailureWindow(n int) {
	if n > 0 {
		s.failureWindow = n
	}
}

// SetDuckBrainHealth registers a provider for DuckBrain sync health so the
// status endpoint can surface fallback state (reachable, spool depth, etc).
func (s *Server) SetDuckBrainHealth(fn func() map[string]interface{}) {
	s.duckbrainHealth = fn
}

// Handler returns an http.Handler for all API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.health)
	mux.HandleFunc("/api/v1/status", s.status)
	mux.HandleFunc("/api/v1/config", s.config)
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
		// SCHED-GAP-080: transient gateway spawn failures since restart
		// (auth rejections never counted), alongside the spawn counters.
		"gateway_errors": s.loop.GatewayErrorCount(),
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
	// GAP-047: auto-disable policy comes from the startup resolved-config
	// snapshot. A Server built without SetResolvedConfig (tests) carries the
	// zero value → threshold == 0 → feature off, no panic.
	adThreshold := s.resolvedConfig.AutoDisableFailureRate
	adMinTicks := s.resolvedConfig.AutoDisableMinTicks
	activeTicks := countActiveTicks(ctx, s.db)
	recentOutcomes := countRecentOutcomes(ctx, s.db)
	failureRates := computeProjectFailureRates(ctx, s.db, s.failureWindow, adThreshold, adMinTicks)
	// PERF-001: serve last_evaluation from the loop's in-memory state when a
	// loop is attached. evaluate() sets lastEval immediately BEFORE emitting
	// the 'evaluation started' event (internal/scheduler/tick_process.go), so
	// LastEvalTime() is the exact same timestamp the events-table query would
	// return — zero cost instead of a full scan of the events table (no index
	// on message; measured ~43ms on 254k rows). Serialization matches the
	// health handler (RFC3339 UTC, zero time → empty string). getLastEvalTime
	// remains the no-loop fallback (used by tests).
	var lastEval string
	if s.loop != nil {
		if t := s.loop.LastEvalTime(); !t.IsZero() {
			lastEval = t.UTC().Format(time.RFC3339)
		}
	} else {
		lastEval = getLastEvalTime(ctx, s.db)
	}
	status := map[string]interface{}{
		"budget_total":           100,
		"active_projects":        len(projects),
		"active_ticks":           activeTicks,
		"recent_outcomes":        recentOutcomes,
		"projects_failure_rates": failureRates,
		"failure_window":         s.failureWindow,
		"last_evaluation":        lastEval,
		// GAP-047: auto-disable configuration driving the per-project
		// auto_disable_armed flags in projects_failure_rates.
		"auto_disable": map[string]interface{}{
			"enabled":   adThreshold > 0,
			"threshold": adThreshold,
			"window":    s.failureWindow,
			"min_ticks": adMinTicks,
		},
	}
	// GAP-043: zero-select diagnostics — consecutive zero-select evals with
	// eligible projects present, and the eligible count at the last one.
	if s.loop != nil {
		zsCount, zsEligible, zsLast := s.loop.ZeroSelectStats()
		status["zero_select_consecutive"] = zsCount
		status["zero_select_eligible"] = zsEligible
		status["zero_select_last_at"] = zsLast
		// SCHED-GAP-080: transient gateway spawn failures since restart
		// (auth rejections never counted), alongside the health endpoint's
		// spawns_http/spawns_exec counters.
		status["gateway_errors"] = s.loop.GatewayErrorCount()
	}
	if s.duckbrainHealth != nil {
		status["duckbrain"] = s.duckbrainHealth()
	}
	writeJSON(w, 200, status)
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
