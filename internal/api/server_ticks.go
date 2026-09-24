package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// handleTicks handles GET /ticks with optional query params.
//
// SCHED-GAP-1575-B: the limit is caller-supplied (unbounded before this row),
// so the read runs under the request-scoped deadline — a scan that exceeds it
// answers 504 naming listTicks instead of hanging the handler.
func (s *Server) handleTicks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx, obs := s.newRequestDeadline(r.Context(), "ticks", s.readTimeout())
	defer obs.finish()
	project := r.URL.Query().Get("project")
	status := r.URL.Query().Get("status")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	obs.enter("listTicks")
	ticks, err := listTicks(ctx, s.db, project, status, limit)
	if !obs.check(w, ctx) {
		return
	}
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
	// SCHED-GAP-112 / S12 §9.4: include the worker-attribution rows when
	// present. Embedding the Tick (rather than re-listing its fields)
	// guarantees the wave envelope carries every Tick field, present and
	// future; tick_workers rides alongside. When no rows exist the handler
	// writes the Tick directly, so serial-tick responses stay
	// byte-identical to the pre-v27 shape.
	workers, err := database.ListTickWorkersByTick(ctx, s.db, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if len(workers) == 0 {
		writeJSON(w, 200, tick)
		return
	}
	writeJSON(w, 200, struct {
		*database.Tick
		TickWorkers []database.TickWorker `json:"tick_workers"`
	}{tick, workers})
}

// events returns the event log with optional filters.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	severity := r.URL.Query().Get("severity")
	component := r.URL.Query().Get("component")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := listEvents(ctx, s.db, severity, component, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if events == nil {
		events = []database.Event{}
	}
	writeJSON(w, 200, map[string]interface{}{"events": events, "count": len(events)})
}

// queue returns the ordered queue of eligible projects with urgency scores.
func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := context.Background()
	items, err := s.listQueue(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []queueItem{}
	}
	writeJSON(w, 200, map[string]interface{}{"queue": items, "count": len(items)})
}

// openapi returns the OpenAPI 3.0 specification for the scheduler API.
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(openapiSpec)
}
