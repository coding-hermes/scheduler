package api_test

// SCHED-GAP-1665 — targeted tick dispatch from the operator surface.
//
// POST /api/v1/projects/{name}/spawn gains an OPTIONAL body {"agent": "…"}:
// when present, THAT tick is routed to that named agent; when absent the
// historical project-based routing applies unchanged (the backward-compatible
// half of the acceptance criteria).
//
// These tests pin the HTTP contract. The ROUTING itself — the hand-out reaching
// the named agent with no request touching the shared gateway — is proven
// hermetically in internal/scheduler/dispatch_agent_override_test.go (a relay
// double records the hand-out), because that is where the dispatch leg lives.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// seedSpawnableLane inserts one enabled lane and points the dispatch-target
// resolution at an EMPTY temp file, so a spawn in a test can never consult the
// host's real ~/.hermes/coding-hermes/dispatch-targets.jsonl.
func seedSpawnableLane(t *testing.T, a *apiTestServer, name string) {
	t.Helper()
	targets := filepath.Join(t.TempDir(), "dispatch-targets.jsonl")
	if err := os.WriteFile(targets, nil, 0o644); err != nil {
		t.Fatalf("write empty dispatch targets: %v", err)
	}
	t.Setenv("SCHEDULER_DISPATCH_TARGETS", targets)

	p := &database.Project{
		Name:      name,
		Workdir:   filepath.Join(t.TempDir(), name),
		Weight:    10,
		Priority:  5,
		CooldownS: 3600,
		DecayRate: 0.1,
		Enabled:   true,
	}
	if err := database.CreateProject(context.Background(), a.db, p); err != nil {
		t.Fatalf("create lane %q: %v", name, err)
	}
}

// TestSpawnWithoutAgentKeepsProjectRouting — the backward-compatible arm: no
// body at all still spawns, and the response carries no agent fields.
func TestSpawnWithoutAgentKeepsProjectRouting(t *testing.T) {
	a := newAPITestServer(t)
	seedSpawnableLane(t, a, "lane-plain")

	status, resp := a.do(t, "POST", "/api/v1/projects/lane-plain/spawn", nil)
	if status != http.StatusAccepted {
		t.Fatalf("spawn status = %d, want 202: %v", status, resp)
	}
	if resp["tick_id"] == nil || resp["tick_id"] == "" {
		t.Errorf("spawn response missing tick_id: %v", resp)
	}
	if _, present := resp["agent"]; present {
		t.Errorf("response carries an agent field for a non-targeted spawn: %v", resp)
	}
	if _, present := resp["agent_registered"]; present {
		t.Errorf("response carries agent_registered for a non-targeted spawn: %v", resp)
	}
}

// TestSpawnWithRegisteredAgent — the targeted arm: the agent rides the request
// body, the response reports the target and that the registry knows it, and the
// spawned tick row exists (the HTTP contract of "this tick went to agent X").
func TestSpawnWithRegisteredAgent(t *testing.T) {
	a := newAPITestServer(t)
	seedSpawnableLane(t, a, "lane-targeted")

	if status, resp := a.do(t, "POST", "/api/v1/agents", map[string]interface{}{
		"name":         "helix-agent",
		"capabilities": "dispatch",
	}); status != http.StatusCreated {
		t.Fatalf("register agent status = %d, want 201: %v", status, resp)
	}

	status, resp := a.do(t, "POST", "/api/v1/projects/lane-targeted/spawn", map[string]interface{}{
		"agent": "helix-agent",
	})
	if status != http.StatusAccepted {
		t.Fatalf("targeted spawn status = %d, want 202: %v", status, resp)
	}
	if resp["agent"] != "helix-agent" {
		t.Errorf("agent = %v, want helix-agent", resp["agent"])
	}
	if resp["agent_registered"] != true {
		t.Errorf("agent_registered = %v, want true (the agent is in the registry)", resp["agent_registered"])
	}
	tickID, _ := resp["tick_id"].(string)
	if tickID == "" {
		t.Fatalf("targeted spawn returned no tick_id: %v", resp)
	}
	// The returned id must resolve — the same contract the plain spawn keeps.
	status, tickResp := a.do(t, "GET", "/api/v1/ticks/"+tickID, nil)
	if status != http.StatusOK {
		t.Errorf("GET /api/v1/ticks/%s status = %d, want 200 (the returned id must resolve)", tickID, status)
	}
	if tickResp["project_name"] != "lane-targeted" {
		t.Errorf("tick project_name = %v, want lane-targeted", tickResp["project_name"])
	}
}

// TestSpawnWithUnregisteredAgent — the registry is ADVISORY: an agent the
// scheduler has no row for is still dispatched (the relay's 404 is the
// authority), and the response says plainly that it is unregistered.
func TestSpawnWithUnregisteredAgent(t *testing.T) {
	a := newAPITestServer(t)
	seedSpawnableLane(t, a, "lane-advisory")

	status, resp := a.do(t, "POST", "/api/v1/projects/lane-advisory/spawn", map[string]interface{}{
		"agent": "stranger-agent",
	})
	if status != http.StatusAccepted {
		t.Fatalf("spawn status = %d, want 202 (registration is NOT a gate): %v", status, resp)
	}
	if resp["agent_registered"] != false {
		t.Errorf("agent_registered = %v, want false for an unregistered target", resp["agent_registered"])
	}
}

// TestSpawnRejectsInvalidAgentAndBody — a name that cannot be a bus identity, a
// disabled lane, and malformed JSON are refused before any tick exists.
func TestSpawnRejectsInvalidAgentAndBody(t *testing.T) {
	a := newAPITestServer(t)
	seedSpawnableLane(t, a, "lane-invalid")

	status, resp := a.do(t, "POST", "/api/v1/projects/lane-invalid/spawn", map[string]interface{}{
		"agent": "bad name/with/separators",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid agent status = %d, want 400: %v", status, resp)
	}
	var ticks int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'lane-invalid'`).Scan(&ticks); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticks != 0 {
		t.Errorf("ticks = %d after a refused spawn, want 0 (nothing may be enqueued)", ticks)
	}
}
