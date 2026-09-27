package api_test

import (
	"net/http"
	"testing"
)

// TestAPI_ListProjects_LaneOutputFields pins the SCHED-GAP-177 wire surface:
// GET /api/v1/projects returns the four per-family output counts on every
// project element (0 for lanes with no recorded output — the honest
// default), and GET /api/v1/projects/{name} carries the same fields.
func TestAPI_ListProjects_LaneOutputFields(t *testing.T) {
	a := newAPITestServer(t)

	if _, err := a.db.Exec(`INSERT OR IGNORE INTO namespaces (id) VALUES ('qa')`); err != nil {
		t.Fatalf("seed qa namespace: %v", err)
	}
	if _, err := a.db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at, namespace_id,
		 qa_output_count, qa_zero_output_streak)
		VALUES ('lane-qa-probe', 'https://example.com/x', '/tmp/x', 10, 5, 900, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'), 'qa', 7, 0)`); err != nil {
		t.Fatalf("insert lane project: %v", err)
	}
	mustCreateAPITestProject(t, a.db, "plain-lane")

	status, body := a.do(t, "GET", "/api/v1/projects?limit=500", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	projs, ok := body["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", body["projects"])
	}

	seen := map[string]map[string]interface{}{}
	for _, raw := range projs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("project element not an object: %T", raw)
		}
		if name, _ := m["name"].(string); name == "lane-qa-probe" || name == "plain-lane" {
			seen[name] = m
		}
	}
	if len(seen) != 2 {
		t.Fatalf("probe projects missing from list response: %v", seen)
	}

	qa := seen["lane-qa-probe"]
	if qa["qa_output_count"] != float64(7) {
		t.Errorf("lane-qa-probe qa_output_count = %v, want 7", qa["qa_output_count"])
	}
	for _, f := range []string{"pm_output_count", "sync_output_count", "dogfood_output_count"} {
		if qa[f] != float64(0) {
			t.Errorf("lane-qa-probe %s = %v, want 0 (family untouched)", f, qa[f])
		}
	}
	// The streaks are json:"-" — internal state, never on the wire.
	for _, f := range []string{"qa_zero_output_streak", "pm_zero_output_streak", "sync_zero_output_streak", "dogfood_zero_output_streak"} {
		if _, present := qa[f]; present {
			t.Errorf("lane-qa-probe %s present on the wire, want absent (json:\"-\")", f)
		}
	}

	plain := seen["plain-lane"]
	for _, f := range []string{"qa_output_count", "pm_output_count", "sync_output_count", "dogfood_output_count"} {
		if plain[f] != float64(0) {
			t.Errorf("plain-lane %s = %v, want 0 (no recorded output)", f, plain[f])
		}
	}

	// Single-project GET carries the same fields (nested under "project").
	status, one := a.do(t, "GET", "/api/v1/projects/lane-qa-probe", nil)
	if status != http.StatusOK {
		t.Fatalf("single GET status = %d, want 200", status)
	}
	inner, ok := one["project"].(map[string]interface{})
	if !ok {
		t.Fatalf("single GET project field not an object: %T", one["project"])
	}
	if inner["qa_output_count"] != float64(7) {
		t.Errorf("single GET qa_output_count = %v, want 7", inner["qa_output_count"])
	}
}
