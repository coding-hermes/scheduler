package mcp

// REMOTE-010 (docs/federation-query-spec.md §3, the "MCP" row): one tool
// per read-catalogue op plus the generic escape hatch — how an MCP agent
// ASKS this scheduler a question as first-class tools.
//
// THE ONE CONTRACT (spec §2/§3): every fed_* tool routes to the SAME
// internal query entry point the HTTP surface runs —
// api.Server.FederationQueryHandler (REMOTE-008's federationOps + the
// shared replay window, §2.5). The MCP layer is a thin adapter: it parses
// its tool arguments, builds the §2.1 Query envelope, calls the shared
// entry point, and returns the §2.2 envelope VERBATIM (status,
// as_of/age_ms, data, gaps, error, contract). No second staleness rule, no
// second error shape — an adapter that computes an answer itself is a bug
// (spec §3). The entry point is installed by main.go via
// SetFederationQueryHandler (the daemon's ONE wired api.Server, so MCP
// shares the HTTP surface's replay window, freshness clock and audit row);
// an unwired server fails closed with a configuration error, like the
// blocks-store nil arm.
//
// READ CLASSIFICATION (auth.go): every fed_* tool is in readOnlyTools —
// the transport READS the peer's answer; nothing here mutates scheduler
// state (spec §7: queries are read-only by construction).
//
// Audit (spec §4: "Every answered query writes one local audit row … a
// query the peer REFUSED is audited too"): the shared entry point writes
// the api.federation row for every answered/refused query; the caller
// identity ("mcp:auth-off" / "mcp:auth-<mode>") names the MCP transport in
// that row and keys the shared replay window, so a retried tool call
// returns the FIRST answer byte-identically (§2.5).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/coding-hermes/scheduler/internal/bus"
)

// mcpFedDefaultBudgetMS is the MCP transport's caller-side default budget
// (ms) — the same number the bus client waits by default. When the tool
// call omits budget_ms the adapter arms it into the envelope so the read
// is bounded by the contract's own deadline mechanism instead of racing
// the JSON-RPC handler's fixed 10s context (spec §2.1 budget_ms).
var mcpFedDefaultBudgetMS = 10000

// federationQueryEntry is the shared internal query entry point signature:
// the §2.1 envelope + the caller identity in, the §2.2 envelope out
// (api.Server.FederationQueryHandler). Held as a func (not an api.Server
// pointer) so the mcp package never imports api's server constructor graph
// and the daemon wires ONE api.Server instance — one replay window, one
// audit path, one freshness clock shared by every transport.
type federationQueryEntry func(q bus.QueryEnvelope, caller string) bus.ResponseEnvelope

// federationOpToolIDs is the §2.3 read catalogue, one MCP tool per op. The
// description text states the op id and the catalogue's data description —
// the tool surface IS the catalogue for MCP agents (keep in lockstep with
// api's federationCatalogue; the per-op tools are generated from THIS map,
// and the shared entry point's unknown_op refusal lists the authoritative
// catalogue if the two ever drift).
var federationOpToolIDs = map[string]string{
	"peer.status":   "identity, version, clock, load, uptime of the answering scheduler",
	"fleet.status":  "peers, projects, active ticks, budget of the answering scheduler",
	"projects.list": "project rows (name, enabled, weight, priority, cooldown)",
	"queue.get":     "the answering scheduler's ordered scheduling queue",
	"ticks.list":    "tick rows, newest first",
	"events.list":   "event rows, newest first",
}

// federationOpToolName maps a catalogue op id to its MCP tool name (the
// spec §3 MCP row's naming convention: fed_<op with . → _>, e.g.
// fed_queue_get). fed_query itself is the generic escape hatch.
func federationOpToolName(op string) string {
	return "fed_" + strings.ReplaceAll(op, ".", "_")
}

// federationOpFromToolName is the inverse mapping — a tool name outside
// the catalogue never invents an op (the entry point's unknown_op refusal
// is the named answer instead).
func federationOpFromToolName(name string) (string, bool) {
	if !strings.HasPrefix(name, "fed_") || name == "fed_query" {
		return "", false
	}
	op := strings.Replace(strings.TrimPrefix(name, "fed_"), "_", ".", 1)
	if _, ok := federationOpToolIDs[op]; ok {
		return op, true
	}
	return "", false
}

// federationTools is the REMOTE-010 tool block: `fed_query` (the generic
// escape hatch, spec §3 MCP row) plus one per-catalogue-op tool so an
// agent sees every read op as a first-class tool. All reads.
func federationTools() []ToolDefinition {
	toolDefs := make([]ToolDefinition, 0, 1+len(federationOpToolIDs))
	toolDefs = append(toolDefs, ToolDefinition{
		Name:        "fed_query",
		Description: "Federation query (REMOTE-010): ask THIS scheduler a read-catalogue question with the full §2.1 envelope. op is one of: peer.status, fleet.status, projects.list, queue.get, ticks.list, events.list. corr_id is REQUIRED — caller-assigned, unique per (caller, minute), the idempotency key (a retried query returns the first answer). Returns the §2.2 response envelope verbatim: status (ok/partial/error/stale), peer, as_of/age_ms (the data's freshness), data, gaps, error, contract.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"op":        map[string]interface{}{"type": "string", "description": "Catalogue op id (e.g. queue.get, projects.list)"},
				"args":      map[string]interface{}{"type": "object", "description": "Op-specific arguments, all optional (e.g. {\"filter\": \"name\"} for projects.list; {\"since\": RFC3339, \"limit\": int} for ticks.list/events.list)"},
				"corr_id":   map[string]interface{}{"type": "string", "description": "REQUIRED correlation/idempotency id, echoed in the reply"},
				"budget_ms": map[string]interface{}{"type": "integer", "description": "Read budget in ms (default 10000); the peer clamps to its own cap"},
				"want":      map[string]interface{}{"type": "string", "description": "\"answer\" (default, all-or-error) or \"partial\" (best-effort with named gaps)"},
			},
			"required": []string{"op", "corr_id"},
		},
	})
	// One tool per catalogue op, deterministically ordered: the op is the
	// tool's identity, args carry the op's catalogue shape verbatim, and
	// federation_corr_id is the required idempotency key (a distinct name
	// keeps the per-op args surface purely op-specific).
	ops := make([]string, 0, len(federationOpToolIDs))
	for op := range federationOpToolIDs {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		toolDefs = append(toolDefs, ToolDefinition{
			Name:        federationOpToolName(op),
			Description: "Federation read op " + op + " — " + federationOpToolIDs[op] + ". Same one contract as fed_query: returns the §2.2 response envelope verbatim (status ok/partial/error/stale, peer, as_of/age_ms, data, gaps, contract). federation_corr_id is required.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"args":               map[string]interface{}{"type": "object", "description": "Op-specific arguments, all optional"},
					"federation_corr_id": map[string]interface{}{"type": "string", "description": "REQUIRED correlation/idempotency id, echoed in the reply"},
					"budget_ms":          map[string]interface{}{"type": "integer", "description": "Read budget in ms (default 10000); the peer clamps to its own cap"},
				},
				"required": []string{"federation_corr_id"},
			},
		})
	}
	return toolDefs
}

// federationCallerID is the spec §4 caller identity MCP queries are
// audited/replay-keyed under: the transport + the resolved auth mode
// (reads are open in every mode; the mode is disclosure, not a gate).
// Mirrors api.FederationBusCaller's transport-naming discipline.
func (s *Server) federationCallerID() string {
	auth := s.operatorAuth()
	if auth == nil {
		return "mcp:auth-off"
	}
	return "mcp:auth-" + auth.Mode()
}

// toolFedQuery is the fed_query adapter: validate the two REQUIRED
// envelope members (spec §2.1), build the envelope, call the ONE shared
// entry point, return the §2.2 envelope verbatim. A missing op/corr_id is
// a JSON-RPC-level argument error (the §2.2 error envelope echoes corr_id,
// which does not exist to echo here — same named-refusal text the HTTP
// surface's validation writes).
func (s *Server) toolFedQuery(_ context.Context, args map[string]interface{}) (string, error) {
	op := getStringArg(args, "op")
	if op == "" {
		return "", fmt.Errorf("op is required (catalogue: peer.status, fleet.status, projects.list, queue.get, ticks.list, events.list)")
	}
	corrID := getStringArg(args, "corr_id")
	if corrID == "" {
		return "", fmt.Errorf("corr_id is required (the idempotency key, spec §2.5)")
	}
	q, err := federationEnvelopeFromArgs(op, corrID, args)
	if err != nil {
		return "", err
	}
	return s.federationExecForMCP(q)
}

// toolFedOpQuery is the per-op adapter (fed_peer_status, fed_fleet_status,
// fed_projects_list, fed_queue_get, fed_ticks_list, fed_events_list): same
// entry point, the op pinned by the tool name.
func (s *Server) toolFedOpQuery(_ context.Context, tool string, args map[string]interface{}) (string, error) {
	op, ok := federationOpFromToolName(tool)
	if !ok {
		return "", fmt.Errorf("unknown federation tool: %s", tool)
	}
	corrID := getStringArg(args, "federation_corr_id")
	if corrID == "" {
		return "", fmt.Errorf("federation_corr_id is required (the idempotency key, spec §2.5)")
	}
	q, err := federationEnvelopeFromArgs(op, corrID, args)
	if err != nil {
		return "", err
	}
	return s.federationExecForMCP(q)
}

// federationEnvelopeFromArgs builds the §2.1 Query envelope from the tool
// arguments: args verbatim, budget_ms armed to the transport default when
// absent, want when the tool carried one (only fed_query exposes it; the
// per-op reads' catalogue answers are complete by construction). This is
// the whole "builds the envelope" step of spec §3's adapter law — the
// contract member set, nothing else.
func federationEnvelopeFromArgs(op, corrID string, args map[string]interface{}) (*bus.QueryEnvelope, error) {
	q := &bus.QueryEnvelope{Op: op, CorrID: corrID, BudgetMS: &mcpFedDefaultBudgetMS}
	if raw, ok := args["args"]; ok && raw != nil {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("args must be an object")
		}
		q.Args = m
	}
	if ms := getIntArg(args, "budget_ms"); ms > 0 {
		b := ms
		q.BudgetMS = &b
	}
	if w := getStringArg(args, "want"); w != "" {
		q.Want = w
	}
	return q, nil
}

// federationExecForMCP runs the ONE shared internal query entry point and
// serializes the §2.2 envelope verbatim — no re-render, no second
// staleness rule, no second error shape. The entry point arms the
// budget_ms deadline and writes the spec §4 audit row (caller = the MCP
// transport identity). The tool context is intentionally unused: the
// contract's own budget mechanism governs the read (the bus adapter runs
// the same way), so a replayed answer and a fresh one share one deadline
// story.
func (s *Server) federationExecForMCP(q *bus.QueryEnvelope) (string, error) {
	entry := s.federationQuery
	if entry == nil {
		// Fail closed like the blocks-store nil arm: an unwired entry
		// point is a configuration error, never a locally-computed
		// answer (spec §3: an adapter that computes an answer itself
		// is a bug).
		return "", fmt.Errorf("federation query surface is not wired (SetFederationQueryHandler was not called by the daemon)")
	}
	resp := entry(*q, s.federationCallerID())
	b, err := json.Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("federation envelope encode: %w", err)
	}
	return string(b), nil
}
