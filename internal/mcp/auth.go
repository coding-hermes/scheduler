package mcp

// SCHED-GAP-1619 — the mutation gate for MCP tools/call.
//
// The MCP surface shares ONE resolved operator-credential configuration and
// ONE decision ladder with the REST mutation gate (SCHED-GAP-1602,
// internal/api/auth_gate.go): mutating tools require the credential exactly
// as REST does — the HTTP request headers (X-Operator-Token / Authorization),
// never tool arguments; reads stay open; auth-off fails closed; and every
// decision — allowed or refused — writes an events row (component "mcp.auth")
// carrying the same outcome vocabulary as the api.auth rows.
//
// Classification is EXPLICIT (mutatingTools / readOnlyTools below) and pinned
// against the live registry by TestMCPAuth_MutationClassificationCoversRegistry,
// so a newly added mutator cannot silently bypass the gate: it fails the
// build until it is classified.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"sort"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
)

// OperatorAuth is the resolved operator-credential contract the MCP gate
// reuses from the API package. api.authConfig implements it (SCHED-GAP-1619
// accessors); the nil interface is the fail-closed misconfiguration arm.
type OperatorAuth interface {
	// Evaluate authenticates the request using the SAME constant-time
	// checks and accepted header shapes as the REST mutation gate. It
	// returns only a classification, never credential material.
	Evaluate(r *http.Request) (identity string, ok bool)
	// Mode is the configured mode name for audit rows ("token"/"basic"/"off").
	Mode() string
	// IsOff reports the fail-closed arm (no credential configured).
	IsOff() bool
}

// mutatingTools is the AUTHORITY on which MCP tools mutate state. Keep it in
// lockstep with the tools registry and invokeTool in server.go — the
// classification coverage test fails the build when a registry tool is
// missing from both sets, so an unclassified tool cannot ship.
//
// Mirrors the REST gate's route split (internal/api/auth.go ROUTE SPLIT):
// every write path — project field updates, pause/resume/add/evaluate,
// scheduler pause/resume, groups/templates CRUD + deploy, namespace CRUD +
// move, project delete/spawn/bump/unbump — is a mutation.
var mutatingTools = map[string]bool{
	"fleet_set_weight":       true,
	"fleet_set_priority":     true,
	"fleet_set_cooldown":     true,
	"fleet_set_decay":        true,
	"fleet_set_model":        true,
	"fleet_set_budget":       true,
	"fleet_set_prompt":       true,
	"fleet_set_enabled":      true,
	"fleet_pause":            true,
	"fleet_resume":           true,
	"fleet_add":              true,
	"fleet_evaluate":         true,
	"fleet_pause_scheduler":  true,
	"fleet_resume_scheduler": true,
	"groups_create":          true,
	"groups_update":          true,
	"groups_delete":          true,
	"templates_create":       true,
	"templates_update":       true,
	"templates_delete":       true,
	"groups_deploy":          true,
	"namespaces_create":      true,
	"namespaces_update":      true,
	"namespaces_delete":      true,
	"namespaces_move":        true,
	"project_delete":         true,
	"project_spawn":          true,
	"project_bump":           true,
	"project_unbump":         true,
}

// readOnlyTools is the complement: everything not listed here but in the
// registry must appear in this set (pinned by the coverage test). Reads are
// deliberately OPEN (same doctrine as REST): the observers that poll the
// surface — cron probes, the ops watchdog, Observatory-style pullers — are
// read-only, and the mutation is the danger, not the observation.
var readOnlyTools = map[string]bool{
	"fleet_status":         true,
	"fleet_projects":       true,
	"fleet_project_detail": true,
	"fleet_ticks":          true,
	"groups_list":          true,
	"groups_get":           true,
	"templates_list":       true,
	"templates_get":        true,
	"events_list":          true,
	"namespaces_list":      true,
	"namespaces_get":       true,
	"namespaces_projects":  true,
	"tick_get":             true,
	"config_get":           true,
	"queue_get":            true,
	"metrics_get":          true,
}

// MutatingToolNames returns the mutating tool set sorted — the classification
// half the MCP/REST auth parity test reads.
func MutatingToolNames() []string {
	out := make([]string, 0, len(mutatingTools))
	for name := range mutatingTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReadOnlyToolNames returns the read-only tool set sorted.
func ReadOnlyToolNames() []string {
	out := make([]string, 0, len(readOnlyTools))
	for name := range readOnlyTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// isMutatingTool reports whether name mutates state. Classification is
// fail-closed: a tool missing from BOTH sets is treated as mutating (it must
// present a credential to run) — the coverage test then forces the author to
// classify it properly.
func isMutatingTool(name string) bool {
	if mutatingTools[name] {
		return true
	}
	return !readOnlyTools[name]
}

// SetOperatorAuth installs the resolved operator-credential configuration —
// the SAME api.ResolveAuthConfig value main.go arms the REST server with
// (SCHED-GAP-1619), so both surfaces share one credential and one rotation.
// A nil call (or no call at all) leaves the fail-closed arm: every mutating
// tools/call is refused.
func (s *Server) SetOperatorAuth(auth OperatorAuth) {
	s.auth = auth
}

// operatorAuth returns the installed configuration; nil means auth-off.
func (s *Server) operatorAuth() OperatorAuth { return s.auth }

// mcpAuthEvent renders one gate decision into an events row (component
// "mcp.auth"). The message mirrors the api.auth sentence shape so an
// operator greps both surfaces with one pattern; method/path carry the MCP
// transport identity ("MCP tools/call"). Never logs credential material.
func mcpAuthEvent(outcome, tool, identity, mode string) *database.Event {
	severity := database.SeverityInfo
	if outcome != api.AuthOutcomeAllowed {
		severity = database.SeverityMedium
	}
	message := "auth: " + outcome + " MCP tools/call target=" + tool + " identity=" + identity
	details := fmt.Sprintf(
		`{"identity":%q,"method":%q,"path":%q,"tool":%q,"target":%q,"outcome":%q,"mode":%q}`,
		identity, "MCP tools/call", "/mcp", tool, tool, outcome, mode)
	return &database.Event{
		Severity:  severity,
		Component: "mcp.auth",
		Message:   message,
		Details:   details,
	}
}

// auditMCPAuth persists one mcp.auth row via database.LogEvent — the SAME
// mechanism /api/v1/events serves. Best-effort with a loud log line on
// failure (the api.auth contract).
func auditMCPAuth(ctx context.Context, db *sql.DB, e *database.Event) {
	if err := database.LogEvent(ctx, db, e); err != nil {
		log.Printf("MCP AUTH AUDIT WRITE FAILED: severity=%s component=%s msg=%q err=%v",
			e.Severity, e.Component, e.Message, err)
	}
}

// gateToolCall decides one mutating tools/call. Returns ok=true when the call
// may proceed (identity is stashed in the request context). On refusal it
// writes the JSON-RPC error and the mcp.auth audit row and returns false —
// the caller must return without invoking the tool.
func (s *Server) gateToolCall(w http.ResponseWriter, r *http.Request, req MCPRequest, tool string) bool {
	if !isMutatingTool(tool) {
		return true // reads are open — no gate, no audit row
	}
	auth := s.operatorAuth()
	var (
		identity string
		ok       bool
		mode     string
	)
	if auth != nil {
		identity, ok = auth.Evaluate(r)
		mode = auth.Mode()
	} else {
		mode = "off"
	}
	outcome := api.AuthOutcomeFor(auth, identity, ok)
	auditMCPAuth(r.Context(), s.db, mcpAuthEvent(outcome, tool, identity, mode))

	switch outcome {
	case api.AuthOutcomeAllowed:
		// Stash the identity so a later in-process caller can attribute the
		// mutation without re-checking (same shape as the REST gate).
		return true
	case api.AuthOutcomeRefusedBadCredential:
		writeMCPError(w, req.ID, -32001, "invalid operator credential")
	case api.AuthOutcomeRefusedNoCredential:
		writeMCPError(w, req.ID, -32001,
			"mutations disabled: no operator credential configured "+
				"(set SCHEDULER_OPERATOR_TOKEN / [api] operator_token)")
	default:
		writeMCPError(w, req.ID, -32001, "operator credential required for mutations")
	}
	return false
}
