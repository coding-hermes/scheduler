package api

// SCHED-GAP-1665 — the AGENT REGISTRY surface.
//
//   - GET    /api/v1/agents          — list registered agents (+ ?capability=)
//   - POST   /api/v1/agents          — register/refresh an agent (upsert)
//   - DELETE /api/v1/agents/{name}   — unregister an agent
//
// WHICH QUESTION EACH SURFACE ANSWERS. The dispatch-targets file says which
// agent runs a LANE (a lane property, read on the spawn path —
// internal/scheduler/dispatch_targets.go). This registry says which agents
// EXIST, what each one can do, and where it is reachable. They are deliberately
// separate: the routing property must be per-lane and file-backed so it can
// outrank router decisions, while the inventory has to be writable at runtime
// and must survive a restart, which is a database row.
//
// THE REGISTRY IS ADVISORY FOR ROUTING. An explicit targeted dispatch
// (POST /api/v1/projects/{name}/spawn {"agent": "…"}) does NOT require the agent
// to be registered here: the relay's own 404 is the authority on whether an
// agent exists (docs/dispatch-spec.md §4), and a scheduler-side gate would
// refuse work the relay would happily accept. What registration buys is the
// declared ENDPOINT — a targeted dispatch to a registered agent with a
// non-empty endpoint hands the work to THAT relay — plus the capability
// inventory an operator or policy reads.
//
// AUTH. Every route here is operator-gated, reads included (SCHED-GAP-1602).
// The list exposes fleet topology — agent names, capabilities and relay
// endpoints, which are internal addresses — exactly the class of material the
// peer registry already gates for anti-enumeration.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// agentRegisterRequest is the POST /api/v1/agents body.
//
//	maxAgentBody bounds the request body; a registration is a handful of short
//	strings, so a multi-megabyte body is a mistake worth refusing early.
type agentRegisterRequest struct {
	Name         string `json:"name"`
	Capabilities string `json:"capabilities"`
	Endpoint     string `json:"endpoint"`
}

// maxAgentBody bounds the registration body. Capabilities are free-form, so the
// ceiling is generous while still far below anything that would fill a row.
const maxAgentBody = 64 << 10

// handleAgents routes the /api/v1/agents collection (GET list, POST register).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.registerAgent(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

// listAgents implements GET /api/v1/agents.
//
// Optional ?capability=<token> keeps only agents declaring that capability
// (token-wise, case-insensitive): the routing question "which agents can take
// this class of work" without a client-side scan.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "-") {
		return
	}
	capability := strings.TrimSpace(r.URL.Query().Get("capability"))
	agents, err := database.ListAgents(r.Context(), s.db, capability)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// agents is never nil (ListAgents returns an empty slice), so the payload
	// carries "agents": [] rather than null for an empty registry.
	writeJSON(w, 200, map[string]interface{}{
		"agents": agents,
		"count":  len(agents),
	})
}

// registerAgent implements POST /api/v1/agents — register or refresh an agent.
//
// 201 when the row is new, 200 when an existing registration was refreshed. The
// distinction is not cosmetic: a client that re-registers on every boot wants to
// know whether it just claimed the name.
func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "-") {
		return
	}
	var req agentRegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentBody)).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	agent := &database.Agent{
		Name:         strings.TrimSpace(req.Name),
		Capabilities: req.Capabilities,
		Endpoint:     req.Endpoint,
	}
	// NormalizeAgent is the shared write gate: name charset, endpoint shape and
	// capability canonicalization all live in the store so the MCP surface
	// cannot disagree with this one.
	if err := database.NormalizeAgent(agent); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	created, err := database.UpsertAgent(r.Context(), s.db, agent)
	if err != nil {
		if strings.Contains(err.Error(), "agent name") || strings.Contains(err.Error(), "agent endpoint") {
			writeError(w, 400, err.Error())
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	status := "updated"
	code := http.StatusOK
	if created {
		status = "registered"
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]interface{}{
		"status": status,
		"agent":  agent,
	})
}

// handleAgentByName routes the /api/v1/agents/{name} subtree. Today the only
// operation is unregister (DELETE); a deeper path is a 404 so a mistyped route
// surfaces instead of silently matching.
func (s *Server) handleAgentByName(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
	if rest == "" {
		writeError(w, 404, "agent name required")
		return
	}
	if strings.Contains(rest, "/") {
		writeError(w, 404, "unknown agents sub-route")
		return
	}
	name, err := url.PathUnescape(rest)
	if err != nil || strings.TrimSpace(name) == "" {
		writeError(w, 400, "invalid agent name")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, 405, "DELETE only")
		return
	}
	s.unregisterAgent(w, r, name)
}

// unregisterAgent implements DELETE /api/v1/agents/{name}. A name that is not
// registered is a 404 — an unregister that removed nothing must not read as a
// success.
func (s *Server) unregisterAgent(w http.ResponseWriter, r *http.Request, name string) {
	if !s.requireOperator(w, r, "agent "+name) {
		return
	}
	if err := database.DeleteAgent(r.Context(), s.db, name); err != nil {
		if errors.Is(err, database.ErrAgentNotFound) {
			writeError(w, 404, "unknown agent: "+name)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"status": "unregistered", "name": name})
}

// isRegisteredAgent reports whether name is in the registry. It is the spawn
// path's ADVISORY lookup: the boolean rides the spawn response so an operator
// can see that a targeted dispatch named an agent the scheduler has no record
// of, WITHOUT the scheduler refusing work the relay would accept.
func (s *Server) isRegisteredAgent(r *http.Request, name string) bool {
	if s.db == nil || strings.TrimSpace(name) == "" {
		return false
	}
	if _, err := database.GetAgent(r.Context(), s.db, name); err != nil {
		return false
	}
	return true
}
