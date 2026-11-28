package api

// Approved destination-migration APPLY / ROLLBACK over HTTP (SCHED-GAP-17084
// rework).
//
// The preview (internal/api/destination_preview.go) is read-only by contract and
// stays that way. These two verbs act on an ALREADY-APPROVED group policy and
// are mounted on the same identity-gated deploy endpoint the preview already
// uses, as body keys:
//
//	POST /api/v1/groups/{name}/deploy  {"destination_apply":    {"approvals":[...]}}
//	POST /api/v1/groups/{name}/deploy  {"destination_rollback": {"plan_digest":"..."}}
//
// The writes are the existing ones, never new ones: the project delivery target
// goes through database.UpdateProject (the same writer PUT /api/v1/projects
// uses) and the migration ledger goes through blocks.Store into groups.jsonl.
// Both verbs are all-or-nothing: any refused item, any stale digest, or any
// failed lane write leaves the fleet exactly as it was.
//
// Nothing here renders a destination. The response carries the sanitized
// <sha256:8> refs of the ledger view, and the daemon log line names the group,
// the actor, the counts and the plan digest only.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

// destinationApplyRequest is the POST body of a migration apply.
type destinationApplyRequest struct {
	Approvals []struct {
		Project string `json:"project"`
		From    string `json:"from"`
		To      string `json:"to"`
	} `json:"approvals"`
}

// destinationRollbackRequest is the POST body of a migration rollback. The plan
// digest is optional; when supplied it must name the captured migration.
type destinationRollbackRequest struct {
	PlanDigest string `json:"plan_digest,omitempty"`
}

// destinationProjects is the engine's view of the fleet: every project's name,
// namespace and current delivery target, read once per request.
func (s *Server) destinationProjects(ctx context.Context) ([]blocks.DestinationProject, error) {
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		return nil, err
	}
	out := make([]blocks.DestinationProject, 0, len(projects))
	for _, project := range projects {
		namespace := ""
		if project.NamespaceID != nil {
			namespace = *project.NamespaceID
		}
		out = append(out, blocks.DestinationProject{
			Name:        project.Name,
			NamespaceID: namespace,
			Destination: project.Deliver,
		})
	}
	return out, nil
}

// applyDestinationMigration applies one approved batch of transitions.
func (s *Server) applyDestinationMigration(w http.ResponseWriter, r *http.Request, group blocks.Group, request *destinationApplyRequest) {
	projects, err := s.destinationProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load projects")
		return
	}
	approvals := make([]blocks.DestinationApproval, 0, len(request.Approvals))
	for _, approval := range request.Approvals {
		approvals = append(approvals, blocks.DestinationApproval{
			Project: approval.Project,
			From:    approval.From,
			To:      approval.To,
		})
	}
	db := s.db
	result, err := s.blocksStore.ApplyDestinationMigration(blocks.DestinationApplyInput{
		Group:     group.Name,
		Approvals: approvals,
		Projects:  projects,
		Actor:     IdentityFromContext(r.Context()),
		At:        s.clock().Now(),
		WriteTarget: func(project, destination string) error {
			target := destination
			return database.UpdateProject(r.Context(), db, project, database.ProjectUpdates{Deliver: &target})
		},
	})
	if err != nil {
		s.writeDestinationMigrationError(w, "apply", group.Name, err)
		return
	}

	log.Printf("DEST-APPLY: group=%s actor=%s applied=%d already=%d plan=%s",
		group.Name, IdentityFromContext(r.Context()), result.Applied, result.Already, result.PlanDigest)
	body := map[string]interface{}{
		"dry_run":         false,
		"applied":         result.Applied,
		"already_applied": result.Already,
		"plan_digest":     result.PlanDigest,
	}
	if result.Applied > 0 {
		change := blocks.SanitizeDestinationChange(result.Change)
		body["moves"] = change.Moves
		body["change"] = change
		s.logDestinationMigrationEvent(r.Context(), "applied", group.Name, change)
	} else {
		body["moves"] = []blocks.DestinationMoveRef{}
	}
	writeJSON(w, http.StatusOK, body)
}

// rollbackDestinationMigration reverses the most recent captured apply.
func (s *Server) rollbackDestinationMigration(w http.ResponseWriter, r *http.Request, group blocks.Group, request *destinationRollbackRequest) {
	projects, err := s.destinationProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load projects")
		return
	}
	db := s.db
	result, err := s.blocksStore.RollbackDestinationMigration(blocks.DestinationRollbackInput{
		Group:      group.Name,
		PlanDigest: request.PlanDigest,
		Projects:   projects,
		Actor:      IdentityFromContext(r.Context()),
		At:         s.clock().Now(),
		WriteTarget: func(project, destination string) error {
			target := destination
			return database.UpdateProject(r.Context(), db, project, database.ProjectUpdates{Deliver: &target})
		},
	})
	if err != nil {
		s.writeDestinationMigrationError(w, "rollback", group.Name, err)
		return
	}

	change := blocks.SanitizeDestinationChange(result.Change)
	log.Printf("DEST-ROLLBACK: group=%s actor=%s restored=%d plan=%s",
		group.Name, IdentityFromContext(r.Context()), result.Restored, result.PlanDigest)
	s.logDestinationMigrationEvent(r.Context(), "rolled back", group.Name, change)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dry_run":     false,
		"rolled_back": result.Restored,
		"plan_digest": result.PlanDigest,
		"moves":       change.Moves,
		"change":      change,
	})
}

// writeDestinationMigrationError maps an engine error onto the HTTP surface.
// A refusal is a 409 client error carrying the machine-readable code and the
// refused items (again: no destination values); a store/DB fault is a 500.
func (s *Server) writeDestinationMigrationError(w http.ResponseWriter, verb, group string, err error) {
	refusal, ok := blocks.IsDestinationRefusal(err)
	if !ok {
		if errors.Is(err, blocks.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		log.Printf("DEST-%s: group=%s refused by a store/DB fault: %v", upperVerb(verb), group, err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("DEST-%s: group=%s refused (%s): %s", upperVerb(verb), group, refusal.Code, refusal.Reason)
	response := map[string]interface{}{
		"error": refusal.Reason,
		"code":  refusal.Code,
	}
	if len(refusal.Items) > 0 {
		response["refused"] = refusal.Items
	}
	if len(refusal.Lanes) > 0 {
		response["lanes"] = refusal.Lanes
	}
	writeJSON(w, http.StatusConflict, response)
}

// logDestinationMigrationEvent records the change in the scheduler event log
// (the audit surface the dashboard already reads). The details are the
// sanitized ledger view, so the event log never holds a destination either. A
// failed event write is logged and never fails the migration — the ledger in
// groups.jsonl is the durable record.
func (s *Server) logDestinationMigrationEvent(ctx context.Context, verb, group string, change blocks.DestinationChangeView) {
	details, err := json.Marshal(change)
	if err != nil {
		log.Printf("WARN: destination migration event details: %v", err)
		return
	}
	if err := database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message:   "destination migration " + verb + ": group " + group + " (" + change.Actor + ", " + change.PlanDigest + ")",
		Details:   string(details),
	}); err != nil {
		log.Printf("WARN: destination migration event-log entry failed: %v", err)
	}
}

func upperVerb(verb string) string {
	if verb == "apply" {
		return "APPLY"
	}
	return "ROLLBACK"
}
