package api

// HTTP handlers for the JSONL-backed deploy blocks — GROUPS (named project
// lists) and TEMPLATES (named task definitions) — plus the deploy action
// that appends a template's task rows to every group member's foreman board.
//
// Route map (mirrors the internal/blocks store 1:1):
//
//	GET    /api/v1/groups              list groups
//	POST   /api/v1/groups              create a group
//	GET    /api/v1/groups/{name}       get a group
//	PUT    /api/v1/groups/{name}       partial-update a group
//	DELETE /api/v1/groups/{name}       delete a group
//	POST   /api/v1/groups/{name}/deploy deploy a template to the group
//	GET    /api/v1/templates           list templates
//	POST   /api/v1/templates           create a template
//	GET    /api/v1/templates/{name}    get a template
//	PUT    /api/v1/templates/{name}    partial-update a template
//	DELETE /api/v1/templates/{name}    delete a template
//
// Storage is JSONL (groups.jsonl / templates.jsonl next to the scheduler
// DB) — config-like data, no speed requirements, easy to back up and to
// point other teams' tooling at (Bane 2026-09 storage mandate). Deploy is
// the fleet operation: instead of hand-adding task rows across namespaces
// one at a time, define the template once and deploy it to a group in a
// single API call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

// blocksStoreRequired writes the 503 answer used by every blocks route when
// the daemon was started without a configured store (NewServer without
// SetBlocksStore). Returns false once the response has been written.
func (s *Server) blocksStoreRequired(w http.ResponseWriter) bool {
	if s.blocksStore == nil {
		writeError(w, 503, "groups/templates store not configured — daemon must be started with the default <db dir> JSONL paths or --groups-file/--templates-file")
		return false
	}
	return true
}

// blocksStoreError maps store-op failures to HTTP statuses: ErrExists → 409,
// ErrNotFound → 404, everything else (JSONL IO) → 500. Validation errors are
// handled up front by the handlers (400), never here.
func blocksStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, blocks.ErrNotFound):
		writeError(w, 404, err.Error())
	case errors.Is(err, blocks.ErrExists):
		writeError(w, 409, err.Error())
	default:
		writeError(w, 500, err.Error())
	}
}

// ── Groups ────────────────────────────────────────────────────────────────

// handleGroups handles GET (list) and POST (create) on /api/v1/groups.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGroups(w, r)
	case http.MethodPost:
		s.createGroup(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	groups, err := s.blocksStore.ListGroups()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if groups == nil {
		groups = []blocks.Group{}
	}
	writeJSON(w, 200, map[string]interface{}{"groups": groups})
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	var g blocks.Group
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if g.Projects == nil {
		g.Projects = []string{}
	}
	if err := blocks.ValidateGroup(g); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.blocksStore.CreateGroup(g); err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 201, g)
}

// handleGroupByID handles GET/PUT/DELETE on /api/v1/groups/{name} and the
// POST sub-route /api/v1/groups/{name}/deploy.
func (s *Server) handleGroupByID(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/groups/")
	parts := splitPath(path)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, 400, "group name required")
		return
	}
	name := parts[0]
	if len(parts) == 2 {
		if parts[1] == "deploy" {
			if r.Method != http.MethodPost {
				writeError(w, 405, "POST only")
				return
			}
			s.deployGroup(w, r, name)
			return
		}
		writeError(w, 404, "not found")
		return
	}
	if len(parts) > 2 {
		writeError(w, 404, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getGroup(w, r, name)
	case http.MethodPut:
		s.updateGroup(w, r, name)
	case http.MethodDelete:
		s.deleteGroup(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, or DELETE only")
	}
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request, name string) {
	g, err := s.blocksStore.GetGroup(name)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, name string) {
	var patch blocks.GroupUpdate
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	// Pre-validate the patched object so validation failures are 400s, not
	// ambiguous store errors (UpdateGroup re-validates internally).
	current, err := s.blocksStore.GetGroup(name)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	if patch.Projects != nil {
		current.Projects = *patch.Projects
	}
	if current.Projects == nil {
		current.Projects = []string{}
	}
	if patch.Description != nil {
		current.Description = *patch.Description
	}
	if err := blocks.ValidateGroup(current); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	updated, err := s.blocksStore.UpdateGroup(name, patch)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, updated)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.blocksStore.DeleteGroup(name); err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted", "group": name})
}

// ── Templates ─────────────────────────────────────────────────────────────

// handleTemplates handles GET (list) and POST (create) on /api/v1/templates.
func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listTemplates(w, r)
	case http.MethodPost:
		s.createTemplate(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	templates, err := s.blocksStore.ListTemplates()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if templates == nil {
		templates = []blocks.Template{}
	}
	writeJSON(w, 200, map[string]interface{}{"templates": templates})
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	var t blocks.Template
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := blocks.ValidateTemplate(t); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.blocksStore.CreateTemplate(t); err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 201, t)
}

// handleTemplateByID handles GET/PUT/DELETE on /api/v1/templates/{name}.
func (s *Server) handleTemplateByID(w http.ResponseWriter, r *http.Request) {
	if !s.blocksStoreRequired(w) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/templates/")
	parts := splitPath(path)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, 400, "template name required")
		return
	}
	name := parts[0]
	if len(parts) > 1 {
		writeError(w, 404, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getTemplate(w, r, name)
	case http.MethodPut:
		s.updateTemplate(w, r, name)
	case http.MethodDelete:
		s.deleteTemplate(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, or DELETE only")
	}
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request, name string) {
	t, err := s.blocksStore.GetTemplate(name)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request, name string) {
	var patch blocks.TemplateUpdate
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	current, err := s.blocksStore.GetTemplate(name)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	if patch.Description != nil {
		current.Description = *patch.Description
	}
	if patch.Tasks != nil {
		current.Tasks = *patch.Tasks
	}
	if err := blocks.ValidateTemplate(current); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	updated, err := s.blocksStore.UpdateTemplate(name, patch)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, updated)
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.blocksStore.DeleteTemplate(name); err != nil {
		blocksStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted", "template": name})
}

// ── Deploy ────────────────────────────────────────────────────────────────

// deployRequest is the POST /api/v1/groups/{name}/deploy body.
type deployRequest struct {
	Template string `json:"template"`
	DryRun   bool   `json:"dry_run"`
}

// deployGroup resolves a group to its member projects (workdirs from the
// projects table), then appends the named template's task rows to each
// member's foreman board (.coding-hermes/board/tasks.jsonl). Per-project
// failures (unknown project, missing workdir, no JSONL board) are reported
// in the response and never abort the batch. dry_run=true returns the plan
// without writing. Exactly one event-log entry is recorded per deploy.
func (s *Server) deployGroup(w http.ResponseWriter, r *http.Request, groupName string) {
	var body deployRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Template) == "" {
		writeError(w, 400, "template is required")
		return
	}

	group, err := s.blocksStore.GetGroup(groupName)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	template, err := s.blocksStore.GetTemplate(body.Template)
	if err != nil {
		blocksStoreError(w, err)
		return
	}
	if len(group.Projects) == 0 {
		writeError(w, 400, "group "+group.Name+" has no projects — add members before deploying")
		return
	}

	// Resolve every group member's workdir from the projects table in one
	// query; members missing from the DB become per-project errors.
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	workdirByProject := make(map[string]string, len(projects))
	for _, p := range projects {
		workdirByProject[p.Name] = p.Workdir
	}
	targets := make([]blocks.ProjectTarget, 0, len(group.Projects))
	seen := map[string]bool{}
	for _, member := range group.Projects {
		if seen[member] {
			continue
		}
		seen[member] = true
		targets = append(targets, blocks.ProjectTarget{Name: member, Workdir: workdirByProject[member]})
	}

	note := fmt.Sprintf("deployed via scheduler template %s to group %s", template.Name, group.Name)
	res := blocks.Deploy(blocks.DeployRequest{
		Group:       group,
		Template:    template,
		Projects:    targets,
		DryRun:      body.DryRun,
		ForemanNote: note,
	})

	// One event-log entry per deploy (INFO, component "api"). A failure to
	// record it is logged but never fails the deploy — the boards already
	// hold the authoritative result.
	mode := ""
	if body.DryRun {
		mode = ", dry run"
	}
	ev := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message: fmt.Sprintf("template deploy: %s → group %s (%d projects, %d task rows%s)",
			template.Name, group.Name, res.Summary.Projects, res.Summary.TaskRows, mode),
		Details: blocks.DeployErrorDetail(res),
	}
	if err := database.LogEvent(ctx, s.db, ev); err != nil {
		log.Printf("WARN: deploy event-log entry failed: %v", err)
	}

	writeJSON(w, 200, res)
}
