package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// handleNamespaces handles GET (list) and POST (create) on /namespaces.
func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listNamespaces(w, r)
	case http.MethodPost:
		s.createNamespace(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	namespaces, err := database.ListNamespaces(ctx, s.db, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if namespaces == nil {
		namespaces = []database.Namespace{}
	}
	writeJSON(w, 200, map[string]interface{}{"namespaces": namespaces})
}

func (s *Server) createNamespace(w http.ResponseWriter, r *http.Request) {
	var ns database.Namespace
	if err := json.NewDecoder(r.Body).Decode(&ns); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if ns.ID == "" {
		writeError(w, 400, "id is required")
		return
	}
	if ns.Weight <= 0 {
		writeError(w, 400, "weight must be greater than 0")
		return
	}
	if err := database.CreateNamespace(context.Background(), s.db, &ns); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, 409, "namespace already exists")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, ns)
}

// handleNamespaceByID handles GET, PUT, DELETE on /namespaces/:id and sub-routes.
func (s *Server) handleNamespaceByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/")
	parts := splitPath(path)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, 400, "namespace id required")
		return
	}
	id := parts[0]

	// Sub-routes: /namespaces/{id}/projects and /namespaces/{id}/move
	if len(parts) == 2 {
		switch parts[1] {
		case "projects":
			if r.Method != http.MethodGet {
				writeError(w, 405, "GET only")
				return
			}
			s.listNamespaceProjects(w, r, id)
			return
		case "move":
			if r.Method != http.MethodPost {
				writeError(w, 405, "POST only")
				return
			}
			s.moveProjectToNamespace(w, r, id)
			return
		}
		writeError(w, 404, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getNamespace(w, r, id)
	case http.MethodPut:
		s.updateNamespace(w, r, id)
	case http.MethodDelete:
		s.deleteNamespace(w, r, id)
	default:
		writeError(w, 405, "GET, PUT, or DELETE only")
	}
}

func (s *Server) getNamespace(w http.ResponseWriter, r *http.Request, id string) {
	ctx := context.Background()
	ns, err := database.GetNamespace(ctx, s.db, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "namespace not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ns)
}

func (s *Server) updateNamespace(w http.ResponseWriter, r *http.Request, id string) {
	var patch database.NamespacePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := database.UpdateNamespace(context.Background(), s.db, id, patch); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "namespace not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	ns, _ := database.GetNamespace(context.Background(), s.db, id)
	writeJSON(w, 200, ns)
}

// deleteNamespace removes a namespace. With only confirm=true it soft-deletes
// (sets enabled=false; the row is retained so historical namespace_ticks stay
// referentially valid) and unassigns member projects (namespace_id → NULL —
// the retained row never fires the FK ON DELETE SET NULL). With
// confirm=true&purge=true it hard-deletes the row permanently
// (SCHED-GAP-097): member projects are unassigned and historical
// namespace_ticks keep their namespace_id. It requires an explicit
// confirm=true query param (purge has its own confirm requirement —
// ?purge=true alone is refused just like a bare DELETE) and refuses
// namespaces that still have ENABLED member projects, so a stray DELETE can
// never silently unassign live fleet members.
func (s *Server) deleteNamespace(w http.ResponseWriter, r *http.Request, id string) {
	ctx := context.Background()
	q := r.URL.Query()
	purge := q.Get("purge") == "true"

	// Existence checked before the confirm gate: an unknown namespace id
	// 404s on any DELETE variant (bare, confirm, or purge).
	if _, err := database.GetNamespace(ctx, s.db, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "namespace not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	// Confirm flag checked before the enabled-member guard: even a
	// namespace with enabled members must not be touched without explicit
	// confirmation.
	if q.Get("confirm") != "true" {
		writeError(w, 400, "confirm=true query param required — this soft-deletes the namespace (enabled=false); add purge=true to permanently remove the row")
		return
	}
	// Enabled-member guard: deleting a namespace that live fleet members
	// still belong to would silently unassign them — require the operator
	// to pause or move them first. Disabled members are fine to unassign.
	members, err := database.ListProjectsByNamespace(ctx, s.db, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var enabledNames []string
	for _, m := range members {
		if m.Enabled {
			enabledNames = append(enabledNames, m.Name)
		}
	}
	if len(enabledNames) > 0 {
		writeError(w, 409, "namespace has enabled project(s) assigned — pause or move them first: "+strings.Join(enabledNames, ", "))
		return
	}
	if purge {
		if err := database.PurgeNamespace(ctx, s.db, id); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// The row is gone, so a soft-delete entry makes no sense; log a
		// purge audit event instead.
		_ = database.LogEvent(ctx, s.db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "api",
			Message:   fmt.Sprintf("namespace purged (hard delete): %s", id),
			Details:   `{"namespace":"` + id + `","action":"purge","via":"DELETE ?confirm=true&purge=true"}`,
		})
		writeJSON(w, 200, map[string]string{"status": "purged", "namespace": id})
		return
	}
	if err := database.DeleteNamespace(ctx, s.db, id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted", "namespace": id})
}

func (s *Server) listNamespaceProjects(w http.ResponseWriter, r *http.Request, id string) {
	ctx := context.Background()
	projects, err := database.ListProjectsByNamespace(ctx, s.db, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if projects == nil {
		projects = []database.Project{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"namespace_id": id,
		"projects":     projects,
	})
}

func (s *Server) moveProjectToNamespace(w http.ResponseWriter, r *http.Request, nsID string) {
	var body struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if body.Project == "" {
		writeError(w, 400, "project is required")
		return
	}
	if err := database.UpdateProject(context.Background(), s.db, body.Project, database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	p, _ := database.GetProject(context.Background(), s.db, body.Project)
	writeJSON(w, 200, p)
}
