package mcp_test

// SCHED-GAP-1665 — the agent registry over MCP (CTL-003 parity with
// /api/v1/agents). The same store functions back both surfaces; these tests pin
// the TOOL behaviour: register → list (with capability filter) → unregister,
// plus the not-found contract and the mutation gate.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCP_AgentsLifecycle(t *testing.T) {
	m := newMCPTestServer(t)

	// Register (mutation — the harness sends the operator credential).
	text, mcpErr := callTool(t, m, 1, "agents_register", map[string]interface{}{
		"name":         "helix-agent",
		"capabilities": " control , dispatch ,Control ",
		"endpoint":     "https://relay.example.net:8767",
	})
	if mcpErr != nil {
		t.Fatalf("agents_register: %v", mcpErr)
	}
	var reg struct {
		Status string `json:"status"`
		Agent  struct {
			Name         string `json:"name"`
			Capabilities string `json:"capabilities"`
			Endpoint     string `json:"endpoint"`
			RegisteredAt string `json:"registered_at"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(text), &reg); err != nil {
		t.Fatalf("agents_register payload is not JSON: %v (%s)", err, text)
	}
	if reg.Status != "registered" {
		t.Errorf("status = %q, want registered", reg.Status)
	}
	if reg.Agent.Capabilities != "control, dispatch" {
		t.Errorf("capabilities = %q, want the normalized form", reg.Agent.Capabilities)
	}
	if reg.Agent.Endpoint != "https://relay.example.net:8767" || reg.Agent.RegisteredAt == "" {
		t.Errorf("agent fields wrong: %+v", reg.Agent)
	}

	// List — every row, then filtered.
	text, mcpErr = callTool(t, m, 2, "agents_list", map[string]interface{}{})
	if mcpErr != nil {
		t.Fatalf("agents_list: %v", mcpErr)
	}
	var list struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("agents_list payload is not JSON: %v (%s)", err, text)
	}
	if list.Count != 1 || len(list.Agents) != 1 || list.Agents[0].Name != "helix-agent" {
		t.Fatalf("agents_list = %+v, want exactly helix-agent", list)
	}
	text, mcpErr = callTool(t, m, 3, "agents_list", map[string]interface{}{"capability": "DISPATCH"})
	if mcpErr != nil {
		t.Fatalf("agents_list(capability): %v", mcpErr)
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("filtered agents_list payload is not JSON: %v (%s)", err, text)
	}
	if list.Count != 1 {
		t.Errorf("capability=dispatch count = %d, want 1", list.Count)
	}
	text, mcpErr = callTool(t, m, 4, "agents_list", map[string]interface{}{"capability": "query"})
	if mcpErr != nil {
		t.Fatalf("agents_list(capability=query): %v", mcpErr)
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("agents_list payload is not JSON: %v (%s)", err, text)
	}
	if list.Count != 0 {
		t.Errorf("capability=query count = %d, want 0", list.Count)
	}

	// Unregister, then a second unregister is a not-found error (never a
	// silent success).
	if _, mcpErr = callTool(t, m, 5, "agents_unregister", map[string]interface{}{"name": "helix-agent"}); mcpErr != nil {
		t.Fatalf("agents_unregister: %v", mcpErr)
	}
	_, mcpErr = callTool(t, m, 6, "agents_unregister", map[string]interface{}{"name": "helix-agent"})
	if mcpErr == nil {
		t.Fatal("second agents_unregister returned OK, want a not-found error")
	}
	if !strings.Contains(mcpErr.Message, "not found") {
		t.Errorf("second unregister error = %q, want a not-found message", mcpErr.Message)
	}
}

// TestMCP_AgentsValidation — the shared write gate rejects what cannot be a bus
// identity or a reachable endpoint, over MCP as over REST.
func TestMCP_AgentsValidation(t *testing.T) {
	m := newMCPTestServer(t)
	for _, args := range []map[string]interface{}{
		{"capabilities": "dispatch"},
		{"name": "   "},
		{"name": "box/agent"},
		{"name": "a1", "endpoint": "bare-host:8767"},
		{"name": "a1", "endpoint": "ftp://relay"},
	} {
		if _, mcpErr := callTool(t, m, 1, "agents_register", args); mcpErr == nil {
			t.Errorf("agents_register(%v) returned OK, want a refusal", args)
		}
	}
	// Nothing was written.
	text, mcpErr := callTool(t, m, 2, "agents_list", map[string]interface{}{})
	if mcpErr != nil {
		t.Fatalf("agents_list: %v", mcpErr)
	}
	if !strings.Contains(text, `"count":0`) {
		t.Errorf("registry after refused registrations = %s, want count 0", text)
	}
}

// TestMCP_AgentsMutationGate — the two writes are classified mutating, so an
// unauthenticated caller is refused and nothing changes.
func TestMCP_AgentsMutationGate(t *testing.T) {
	bare := newMCPBareServer(t)
	if _, mcpErr := callTool(t, bare, 1, "agents_register", map[string]interface{}{"name": "intruder"}); mcpErr == nil {
		t.Fatal("unauthenticated agents_register returned OK, want a refusal")
	}
	// The read stays open on the MCP surface (same doctrine as every other
	// introspection tool); the REST read is the gated one.
	if _, mcpErr := callTool(t, bare, 2, "agents_list", map[string]interface{}{}); mcpErr != nil {
		t.Errorf("agents_list on an unauthenticated stack: %v, want the open-read behaviour", mcpErr)
	}
}
