package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

// S13 §4.2: the hook surface. The FILE is the source of truth; these routes are
// a validated writer and a resolved-plan reader. A PUT rewrites hooks.toml
// atomically (backup kept) and reloads it, so the API and a hand edit converge
// on the same bytes.

// SetHooks installs the armed definition set. Nil = the routes answer 503.
func (s *Server) SetHooks(set *lanehook.Set) { s.hooks = set }

func (s *Server) handleHooks(w http.ResponseWriter, r *http.Request) {
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks not configured (no --hooks-file)")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	out := map[string]any{
		"path":  s.hooks.Path(),
		"order": string(s.hooks.Order()),
		"steps": s.hooks.Steps(),
	}
	if err := s.hooks.LastError(); err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHookPlan(w http.ResponseWriter, r *http.Request) {
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks not configured (no --hooks-file)")
		return
	}
	lane := strings.TrimSpace(r.URL.Query().Get("lane"))
	if lane == "" {
		writeError(w, http.StatusBadRequest, "lane query parameter is required")
		return
	}
	var ns, workdir, repoURL string
	err := s.db.QueryRow(
		`SELECT coalesce(namespace_id,''), coalesce(workdir,''), coalesce(repo_url,'') FROM projects WHERE name = ?`,
		lane).Scan(&ns, &workdir, &repoURL)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "no such lane: "+lane)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project lookup failed: "+err.Error())
		return
	}
	var skip []string
	if spec, ok := s.hooks.Lane(lane); ok {
		skip = spec.SkipHooks
	}
	plan, err := s.hooks.PlanFor(lane, ns, workdir, strings.TrimPrefix(repoURL, "local:"), skip)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleHookReload(w http.ResponseWriter, r *http.Request) {
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks not configured (no --hooks-file)")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireOperator(w, r, "hooks reload") {
		return
	}
	changed, err := s.hooks.ReloadIfChanged()
	if err != nil {
		writeError(w, http.StatusBadRequest, "reload rejected (previous set stays armed): "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"changed": changed,
		"steps":   len(s.hooks.Steps()),
		"path":    s.hooks.Path(),
	})
}

// handleHookItem serves PUT and DELETE /api/v1/hooks/{id}.
func (s *Server) handleHookItem(w http.ResponseWriter, r *http.Request) {
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks not configured (no --hooks-file)")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "hook id is required")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if !s.requireOperator(w, r, "hook delete "+id) {
			return
		}
		f, err := lanehook.LoadFile(s.hooks.Path())
		if err != nil {
			writeError(w, http.StatusBadRequest, "current file rejected: "+err.Error())
			return
		}
		n := f.Delete(id)
		if n == 0 {
			writeError(w, http.StatusNotFound, "no hook with id "+id)
			return
		}
		if err := lanehook.SaveFile(s.hooks.Path(), f); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, _ = s.hooks.ReloadIfChanged()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "id": id})

	case http.MethodPut:
		if !s.requireOperator(w, r, "hook upsert "+id) {
			return
		}
		var step lanehook.Step
		if err := json.NewDecoder(r.Body).Decode(&step); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		step.ID = id
		if step.Level == lanehook.LevelGlobal && step.Target == "" {
			step.Target = "*"
		}
		f, err := lanehook.LoadFile(s.hooks.Path())
		if err != nil {
			writeError(w, http.StatusBadRequest, "current file rejected: "+err.Error())
			return
		}
		prev, err := f.Upsert(step)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := lanehook.SaveFile(s.hooks.Path(), f); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := s.hooks.ReloadIfChanged(); err != nil {
			writeError(w, http.StatusInternalServerError, "saved but not reloaded: "+err.Error())
			return
		}
		resp := map[string]any{"id": id, "created": prev == nil, "steps": len(s.hooks.Steps())}
		if prev != nil {
			resp["replaced"] = prev
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		writeError(w, http.StatusMethodNotAllowed, "PUT or DELETE only")
	}
}
