package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

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
	// Fill S06 defaults for zero-valued fields so a minimal {name, repo_url,
	// workdir} body satisfies the CHECK constraints. Enabled intentionally
	// stays false — creating a project must not auto-enable it.
	if p.Weight == 0 {
		p.Weight = 10
	}
	if p.Priority == 0 {
		p.Priority = 5
	}
	if p.CooldownS == 0 {
		p.CooldownS = 900
	}
	if p.DecayRate == 0 {
		p.DecayRate = 1.0
	}
	if err := database.CreateProject(context.Background(), s.db, &p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, 409, "project already exists")
			return
		}
		if strings.Contains(err.Error(), "already registered by enabled project") {
			writeError(w, 409, err.Error())
			return
		}
		if isCheckConstraint(err) {
			writeError(w, 400, projectConstraintMessage)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, p)
}

// isCheckConstraint reports whether err is a SQLite CHECK-constraint
// violation — i.e. a client-correctable value-range problem, not a server
// fault. Handlers map it to 400 with an actionable message.
func isCheckConstraint(err error) bool {
	return strings.Contains(err.Error(), "CHECK constraint failed")
}

// projectConstraintMessage is the actionable 400 body for projects-table
// CHECK violations (weight/priority/decay_rate ranges).
const projectConstraintMessage = "invalid project fields: weight must be 1..100; priority 1..10; decay_rate > 0"

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

	// Sub-routes on /projects/:name.
	if len(parts) == 2 {
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		switch parts[1] {
		case "pause":
			s.pauseProject(w, r, name)
			return
		case "resume":
			s.resumeProject(w, r, name)
			return
		case "spawn":
			s.spawnProject(w, r, name)
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
	case http.MethodDelete:
		s.deleteProject(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, POST, or DELETE only")
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
		"project":     p,
		"latest_tick": tick,
	})
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, name string) {
	var updates database.ProjectUpdates
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	ctx := context.Background()
	// GAP-044: an enabled→disabled transition through PUT is a disable
	// path — the DB layer stamps provenance; the events table gets a
	// matching entry here.
	cur, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	wasEnabled := cur.Enabled
	// DecayRate guard: 0 makes urgency flat (priority × 1^0) so the project is
	// never picked by the packer — a silent permanent starvation. Foremen must
	// not be able to do this to themselves. Proven: dexdat-memory starved 87h
	// with a valid namespace + 900s cooldown because decay_rate was 0.
	if updates.DecayRate != nil && *updates.DecayRate <= 0 {
		writeError(w, 400, "decay_rate must be > 0 (0 causes permanent starvation — urgency never grows)")
		return
	}
	if err := database.UpdateProject(ctx, s.db, name, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		if isCheckConstraint(err) {
			writeError(w, 400, projectConstraintMessage)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// GAP-044: log a matching events-table entry when this PUT disabled
	// a previously-enabled project.
	if wasEnabled && updates.Enabled != nil && !*updates.Enabled {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	writeJSON(w, 200, p)
}

func (s *Server) pauseProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	// GAP-044: pause is a disable path — stamp explicit provenance so the
	// row and the events table both carry who/when/why.
	by := "api-pause"
	reason := "paused via POST /projects/{name}/pause"
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{
		Enabled:        database.BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if p, err := database.GetProject(ctx, s.db, name); err == nil {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	writeJSON(w, 200, map[string]string{"status": "paused", "project": name})
}

func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := database.UpdateProject(context.Background(), s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "resumed", "project": name})
}

// deleteProject removes a project. With only confirm=true it soft-deletes
// (sets enabled=false; the row is retained so historical ticks stay
// referentially valid). With confirm=true&purge=true it hard-deletes the row
// permanently (DOGFOOD-009) — historical ticks keep their project_name and
// /api/v1/status failure rates exclude projects whose row no longer exists.
// It requires an explicit confirm=true query param and refuses enabled
// projects so a live fleet project can never be silently disabled or
// removed by a stray DELETE.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	purge := q.Get("purge") == "true"
	// Confirm flag checked first: even a valid, enabled project must not be
	// touched without explicit confirmation. Purge has its OWN confirm
	// requirement — ?purge=true alone is refused just like a bare DELETE.
	if q.Get("confirm") != "true" {
		writeError(w, 400, "confirm=true query param required — this soft-deletes the project (enabled=false); add purge=true to permanently remove the row")
		return
	}
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
	// Enabled-project guard: deleting a live project would silently starve
	// it of ticks — require an explicit pause first.
	if p.Enabled {
		writeError(w, 409, "project is enabled — pause it first (PUT Enabled=false or POST /projects/{name}/pause) before deleting")
		return
	}
	if purge {
		if err := database.PurgeProject(ctx, s.db, name); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// The row is gone, so a disable-provenance event makes no sense;
		// log a purge audit entry instead.
		_ = database.LogEvent(ctx, s.db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "api",
			Message:   fmt.Sprintf("project purged (hard delete): %s", name),
			Details:   `{"project":"` + name + `","action":"purge","via":"DELETE ?confirm=true&purge=true"}`,
		})
		writeJSON(w, 200, map[string]string{"status": "purged", "project": name})
		return
	}
	if err := database.DeleteProject(ctx, s.db, name); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// GAP-044: the DELETE soft-delete stamps provenance (api-delete,
	// legacy-backfilling COALESCE) — mirror it into the events table.
	if p, err := database.GetProject(ctx, s.db, name); err == nil {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	writeJSON(w, 200, map[string]string{"status": "deleted", "project": name})
}

// logDisableEvent records a GAP-044 disable-provenance entry in the events
// table so every API disable path (pause, PUT enabled=false, DELETE
// confirm=true) has a matching audit row with who/when/why.
func logDisableEvent(ctx context.Context, db *sql.DB, name, by, reason, at string) {
	details, _ := json.Marshal(map[string]string{
		"project":         name,
		"disabled_by":     by,
		"disabled_reason": reason,
		"disabled_at":     at,
	})
	_ = database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message:   fmt.Sprintf("project disabled: %s (%s)", name, by),
		Details:   string(details),
	})
}

// spawnProject handles POST /api/v1/projects/:name/spawn.
func (s *Server) spawnProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	_ = p
	tickID := fmt.Sprintf("%s-%s", name, time.Now().UTC().Format("2006-01-02-15-04-05"))
	// Enqueue a tick for the project via the loop.
	s.loop.ForceEvaluate()
	writeJSON(w, 202, map[string]string{
		"status":  "spawned",
		"project": name,
		"tick_id": tickID,
	})
}
