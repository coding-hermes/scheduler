package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-17002 — the wake-table API.
//
// A wake is a persisted promise: run this lane at that time (or when an
// external event says so). The endpoints are deliberately small — create,
// list, get, cancel, fire — because the AUDIT lives in the table itself:
// rows are never deleted, and fired/skipped/deferred are states with
// timestamps and the tick id they produced.
//
// Admission semantics (docs/wake-tables.md): a fired wake spawns through the
// operator path — the manual bypass (e) — with its own nudge stamp
// (wake_table) so provenance survives; a blackout window defers the wake
// (it stays pending and retries); a busy lane is recorded as skipped, not
// failed.

// SetWakeFirer wires the wake-table runner's external-fire entry:
//   - (wake, true, nil)   — claimed and fired
//   - (nil, false, nil)   — not pending (409)
//   - (nil, false, err)   — err is a semantic refusal (400) or a failure (500)
//
// Nil (never wired) = the fire endpoint answers 503 rather than pretending.
func (s *Server) SetWakeFirer(fn func(ctx context.Context, id string) (any, bool, error)) {
	s.wakeFirerFn = fn
}

// createWake handles POST /api/v1/wakes.
//
// Body: {"project": "...", "at": "2026-11-08T02:00:00-05:00"} for a one-shot,
// {"project": "...", "in_minutes": 90}, {"project": "...", "daily": "02:00"}
// for a recurring local-time wake, or {"project": "...", "external": true}
// for an API-fired wake. Optional: payload (opaque, carried onto the event
// and the tick), note, created_by (defaults to "api").
//
// Exactly ONE scheduling clause per wake — a body that says both "at" and
// "daily" is a 400, not a guess.
func (s *Server) createWake(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, "read body: "+err.Error())
		return
	}
	var req struct {
		Project   string `json:"project"`
		At        string `json:"at"`
		InMin     *int   `json:"in_minutes"`
		Daily     string `json:"daily"`
		External  bool   `json:"external"`
		Payload   string `json:"payload"`
		Note      string `json:"note"`
		CreatedBy string `json:"created_by"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	if req.Project == "" {
		writeError(w, 400, "project is required")
		return
	}
	p, err := database.GetProject(r.Context(), s.db, req.Project)
	if err != nil {
		writeError(w, 404, "project not found: "+req.Project)
		return
	}
	if !p.Enabled {
		writeError(w, 409, "project is disabled — a wake for a disabled lane would be a promise the runner has to skip")
		return
	}

	wake := database.Wake{
		Project:   req.Project,
		Payload:   req.Payload,
		Note:      req.Note,
		CreatedBy: strings.TrimSpace(req.CreatedBy),
	}
	if wake.CreatedBy == "" {
		wake.CreatedBy = "api"
	}

	clauses := 0
	if req.At != "" {
		clauses++
	}
	if req.InMin != nil {
		clauses++
	}
	if req.Daily != "" {
		clauses++
	}
	if req.External {
		clauses++
	}
	if clauses != 1 {
		writeError(w, 400, "exactly one of at | in_minutes | daily | external is required")
		return
	}

	now := s.clock().Now()
	switch {
	case req.At != "":
		t, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			writeError(w, 400, "at must be RFC3339 (e.g. 2026-11-08T02:00:00-05:00): "+err.Error())
			return
		}
		if !t.After(now) {
			writeError(w, 400, "at is in the past — a wake that fires immediately is a spawn request, use /spawn")
			return
		}
		wake.Kind = database.WakeKindAt
		wake.AtTime = &t
	case req.InMin != nil:
		if *req.InMin <= 0 {
			writeError(w, 400, "in_minutes must be positive")
			return
		}
		t := now.Add(time.Duration(*req.InMin) * time.Minute)
		wake.Kind = database.WakeKindAt
		wake.AtTime = &t
	case req.Daily != "":
		wake.Kind = database.WakeKindDaily
		wake.DailyHHMM = req.Daily
		// First occurrence: today at HH:MM local, or tomorrow if passed.
		first, err := firstDailyOccurrence(req.Daily, now)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		wake.AtTime = &first
	default:
		wake.Kind = database.WakeKindExternal
	}

	id, err := database.CreateWake(r.Context(), s.db, &wake, now)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	stored, err := database.GetWake(r.Context(), s.db, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"status": "scheduled", "id": id, "wake": stored})
}

// firstDailyOccurrence validates a local "HH:MM" and returns its first
// occurrence strictly after now (today, or tomorrow once passed).
func firstDailyOccurrence(hhmm string, now time.Time) (time.Time, error) {
	var h, m int
	if n, err := fmt.Sscanf(hhmm, "%02d:%02d", &h, &m); err != nil || n != 2 || h > 23 || m > 59 || strings.Count(hhmm, ":") != 1 || len(hhmm) != 5 {
		return time.Time{}, fmt.Errorf(`daily must be local "HH:MM", got %q`, hhmm)
	}
	first := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !first.After(now) {
		first = first.AddDate(0, 0, 1)
	}
	return first, nil
}

// listWakes handles GET /api/v1/wakes?state=pending.
func (s *Server) listWakes(w http.ResponseWriter, r *http.Request) {
	wakes, err := database.ListWakes(r.Context(), s.db, r.URL.Query().Get("state"))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"wakes": wakes, "count": len(wakes)})
}

// getWake handles GET /api/v1/wakes/{id}.
func (s *Server) getWake(w http.ResponseWriter, r *http.Request, id string) {
	wake, err := database.GetWake(r.Context(), s.db, id)
	if err != nil {
		writeError(w, 404, "wake not found: "+id)
		return
	}
	writeJSON(w, 200, wake)
}

// cancelWake handles DELETE /api/v1/wakes/{id} — a pending wake is marked
// cancelled (never deleted; the record is the audit). Cancelling a fired or
// already-cancelled wake is a 409.
func (s *Server) cancelWake(w http.ResponseWriter, r *http.Request, id string) {
	ok, err := database.CancelWake(r.Context(), s.db, id, "api")
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !ok {
		writeError(w, 409, "wake is not pending — only a pending wake can be cancelled")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "cancelled", "id": id})
}

// fireWake handles POST /api/v1/wakes/{id}/fire — the external-event entry.
// Fires a pending EXTERNAL wake now; anything else is a 400/409, never a
// silent fake fire. Without a wired runner: 503.
func (s *Server) fireWake(w http.ResponseWriter, r *http.Request, id string) {
	if s.wakeFirerFn == nil {
		writeError(w, 503, "wake-table runner not wired — cannot fire")
		return
	}
	wake, ok, err := s.wakeFirerFn(r.Context(), id)
	if err != nil {
		var kindErr *scheduler.WakeKindError
		if errors.As(err, &kindErr) {
			writeError(w, 400, err.Error())
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	if !ok {
		writeError(w, 409, "wake is not pending (already fired, cancelled, or unknown)")
		return
	}
	writeJSON(w, 202, map[string]any{"status": "fired", "wake": wake})
}

// wakeRoute dispatches /api/v1/wakes and its subpaths.
func (s *Server) wakeRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/wakes")
	path = strings.Trim(path, "/")
	switch {
	case path == "":
		switch r.Method {
		case http.MethodPost:
			s.createWake(w, r)
		case http.MethodGet:
			s.listWakes(w, r)
		default:
			writeError(w, 405, "method "+r.Method+" not allowed")
		}
	case strings.HasPrefix(path, "fire/"): // legacy spelling, kept lenient
		if r.Method != http.MethodPost {
			writeError(w, 405, "method "+r.Method+" not allowed")
			return
		}
		s.fireWake(w, r, strings.TrimPrefix(path, "fire/"))
	default:
		id := path
		if strings.HasSuffix(id, "/fire") {
			if r.Method != http.MethodPost {
				writeError(w, 405, "method "+r.Method+" not allowed")
				return
			}
			s.fireWake(w, r, strings.TrimSuffix(id, "/fire"))
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.getWake(w, r, id)
		case http.MethodDelete:
			s.cancelWake(w, r, id)
		default:
			writeError(w, 405, "method "+r.Method+" not allowed")
		}
	}
}
