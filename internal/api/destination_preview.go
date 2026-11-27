package api

import (
	"net/http"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

// previewDestinations is always read-only, regardless of the deploy dry_run flag.
// All groups participate in conflict detection; only requested members appear.
func (s *Server) previewDestinations(w http.ResponseWriter, r *http.Request, group blocks.Group, request *destinationPreviewRequest) {
	groups, err := s.blocksStore.ListGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load groups")
		return
	}
	projects, err := database.ListProjects(r.Context(), s.db, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load projects")
		return
	}
	policies := make(map[string]blocks.DestinationPolicy)
	for _, g := range groups {
		if g.DestinationPolicy != nil {
			policies[g.Name] = *g.DestinationPolicy
		}
	}
	members := make(map[string]bool)
	for _, name := range group.Projects {
		members[name] = true
	}
	var targets []blocks.DestinationProject
	for _, p := range projects {
		if members[p.Name] {
			namespace := ""
			if p.NamespaceID != nil {
				namespace = *p.NamespaceID
			}
			targets = append(targets, blocks.DestinationProject{Name: p.Name, NamespaceID: namespace, Destination: p.Deliver})
		}
	}
	var approvals []blocks.DestinationApproval
	for _, a := range request.Approvals {
		approvals = append(approvals, blocks.DestinationApproval{Project: a.Project, From: a.From, To: a.To})
	}
	plan := blocks.PlanDestinationMigration(groups, policies, targets, approvals)
	writeJSON(w, http.StatusOK, map[string]interface{}{"dry_run": true, "destination_plan": plan})
}
