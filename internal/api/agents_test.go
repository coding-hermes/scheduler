package api_test

// SCHED-GAP-1665 — the agent registry surface (SCHED-GAP-1665 ACCEPTANCE 1–4):
// list, register, unregister, persistence, validation, and the operator gate.

import (
	"net/http"
	"testing"
)

// TestAgentsRegisterListUnregister drives the documented lifecycle over the
// wire: register (201) → list (documented shape) → capability filter →
// re-register (200, updated) → unregister (200) → list empty → unregister
// again (404). It also asserts the row is really in the DB, not just echoed
// back by the handler.
func TestAgentsRegisterListUnregister(t *testing.T) {
	a := newAPITestServer(t)

	status, resp := a.do(t, "POST", "/api/v1/agents", map[string]interface{}{
		"name":         "helix-agent",
		"capabilities": "control, dispatch",
		"endpoint":     "http://10.0.0.7:8767",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /api/v1/agents status = %d, want 201: %v", status, resp)
	}
	if resp["status"] != "registered" {
		t.Errorf("status = %v, want registered", resp["status"])
	}
	agent, ok := resp["agent"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing agent object: %v", resp)
	}
	for _, key := range []string{"name", "capabilities", "endpoint", "registered_at", "updated_at"} {
		if _, ok := agent[key]; !ok {
			t.Errorf("agent object missing documented key %q: %v", key, agent)
		}
	}
	if agent["name"] != "helix-agent" || agent["endpoint"] != "http://10.0.0.7:8767" {
		t.Errorf("agent fields wrong: %v", agent)
	}

	// The row is durable: a direct DB read (not the handler's echo) must see it.
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE name = 'helix-agent'`).Scan(&count); err != nil {
		t.Fatalf("read agents table: %v", err)
	}
	if count != 1 {
		t.Fatalf("agents rows = %d, want 1 (the registration must be persisted, not echoed)", count)
	}

	// List.
	status, resp = a.do(t, "GET", "/api/v1/agents", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/agents status = %d, want 200", status)
	}
	agents, ok := resp["agents"].([]interface{})
	if !ok {
		t.Fatalf("list body missing agents array: %v", resp)
	}
	if len(agents) != 1 || resp["count"] != float64(1) {
		t.Fatalf("list = %v (count %v), want exactly one agent", agents, resp["count"])
	}

	// Capability filter — token-wise, case-insensitive.
	status, resp = a.do(t, "GET", "/api/v1/agents?capability=DISPATCH", nil)
	if status != http.StatusOK {
		t.Fatalf("filtered list status = %d, want 200", status)
	}
	if got := len(resp["agents"].([]interface{})); got != 1 {
		t.Errorf("capability=dispatch returned %d agents, want 1", got)
	}
	status, resp = a.do(t, "GET", "/api/v1/agents?capability=query", nil)
	if status != http.StatusOK {
		t.Fatalf("filtered list status = %d, want 200", status)
	}
	if got := len(resp["agents"].([]interface{})); got != 0 {
		t.Errorf("capability=query returned %d agents, want 0", got)
	}

	// Re-register: 200 (update), never 201, and registered_at is preserved.
	status, resp = a.do(t, "POST", "/api/v1/agents", map[string]interface{}{
		"name":         "helix-agent",
		"capabilities": "dispatch",
	})
	if status != http.StatusOK {
		t.Fatalf("re-register status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "updated" {
		t.Errorf("re-register status field = %v, want updated", resp["status"])
	}
	updated := resp["agent"].(map[string]interface{})
	if updated["registered_at"] != agent["registered_at"] {
		t.Errorf("registered_at moved on refresh: %v -> %v", agent["registered_at"], updated["registered_at"])
	}
	if updated["endpoint"] != "" {
		t.Errorf("endpoint = %v, want cleared (the refresh omitted it)", updated["endpoint"])
	}

	// Unregister.
	status, resp = a.do(t, "DELETE", "/api/v1/agents/helix-agent", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "unregistered" || resp["name"] != "helix-agent" {
		t.Errorf("delete body = %v, want {status:unregistered, name:helix-agent}", resp)
	}
	status, resp = a.do(t, "GET", "/api/v1/agents", nil)
	if status != http.StatusOK {
		t.Fatalf("list after delete status = %d", status)
	}
	if got := len(resp["agents"].([]interface{})); got != 0 {
		t.Errorf("agents after unregister = %d, want 0", got)
	}

	// A delete that removes nothing is a named 404, never a silent success.
	status, resp = a.do(t, "DELETE", "/api/v1/agents/helix-agent", nil)
	if status != http.StatusNotFound {
		t.Fatalf("second DELETE status = %d, want 404: %v", status, resp)
	}
}

// TestAgentsValidation — the write path refuses what cannot be a bus identity,
// what cannot be addressed, and malformed JSON. Every case is a 400 and NOTHING
// is written.
func TestAgentsValidation(t *testing.T) {
	a := newAPITestServer(t)
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing name", map[string]interface{}{"capabilities": "dispatch"}},
		{"blank name", map[string]interface{}{"name": "   "}},
		{"path in name", map[string]interface{}{"name": "box/agent"}},
		{"space in name", map[string]interface{}{"name": "agent one"}},
		{"bare host:port endpoint", map[string]interface{}{"name": "a1", "endpoint": "10.0.0.7:8767"}},
		{"relative endpoint", map[string]interface{}{"name": "a1", "endpoint": "/relay"}},
		{"non-http endpoint", map[string]interface{}{"name": "a1", "endpoint": "ftp://relay"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := a.do(t, "POST", "/api/v1/agents", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %v", status, resp)
			}
			if msg, _ := resp["error"].(string); msg == "" {
				t.Errorf("400 body carries no error message: %v", resp)
			}
		})
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&count); err != nil {
		t.Fatalf("read agents: %v", err)
	}
	if count != 0 {
		t.Errorf("agents rows = %d after refused registrations, want 0", count)
	}
}

// TestAgentsAuthGate — the registry is fleet topology, so every route —
// including the read — is operator-gated (fail-closed 401 without a credential).
func TestAgentsAuthGate(t *testing.T) {
	a := newAPITestServer(t)
	if status, resp := a.do(t, "POST", "/api/v1/agents", map[string]interface{}{"name": "helix-agent"}); status != http.StatusCreated {
		t.Fatalf("seeding registration status = %d, want 201: %v", status, resp)
	}
	for _, call := range []struct{ method, path string }{
		{"GET", "/api/v1/agents"},
		{"POST", "/api/v1/agents"},
		{"DELETE", "/api/v1/agents/helix-agent"},
	} {
		status, resp := a.doAnon(t, call.method, call.path, map[string]interface{}{"name": "intruder"})
		if status != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s status = %d, want 401: %v", call.method, call.path, status, resp)
		}
	}
	// The refused calls must not have changed the registry.
	status, resp := a.do(t, "GET", "/api/v1/agents", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	agents := resp["agents"].([]interface{})
	if len(agents) != 1 || agents[0].(map[string]interface{})["name"] != "helix-agent" {
		t.Errorf("registry changed by refused calls: %v", agents)
	}
}

// TestAgentsRouteAndMethodErrors pins the surface's edge semantics: a wrong
// method on a real path is 405 (never 404), an unknown sub-path is 404, and an
// unknown agent is 404.
func TestAgentsRouteAndMethodErrors(t *testing.T) {
	a := newAPITestServer(t)
	cases := []struct {
		method, path string
		want         int
	}{
		{"PUT", "/api/v1/agents", http.StatusMethodNotAllowed},
		{"DELETE", "/api/v1/agents", http.StatusMethodNotAllowed},
		{"POST", "/api/v1/agents/helix-agent", http.StatusMethodNotAllowed},
		{"GET", "/api/v1/agents/helix-agent", http.StatusMethodNotAllowed},
		{"DELETE", "/api/v1/agents/helix-agent/extra", http.StatusNotFound},
		{"DELETE", "/api/v1/agents/", http.StatusNotFound},
		{"DELETE", "/api/v1/agents/never-registered", http.StatusNotFound},
	}
	for _, tc := range cases {
		status, resp := a.do(t, tc.method, tc.path, map[string]interface{}{})
		if status != tc.want {
			t.Errorf("%s %s status = %d, want %d: %v", tc.method, tc.path, status, tc.want, resp)
		}
	}
}
