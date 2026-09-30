package mcp_test

import (
	"encoding/json"
	"testing"
)

// TestMCP_CooldownResidueTool pins the cooldown_residue tool (SCHED-GAP-1670
// MCP mirror of GET /api/v1/cooldown/residue): the tool exists, answers the
// same {"residue": [...], "count": N} shape, and lists exactly the ENABLED
// lanes whose live cooldown_s sits strictly below their own pin.
func TestMCP_CooldownResidueTool(t *testing.T) {
	m := newMCPTestServer(t)

	if _, err := m.db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 cooldown_pin_s, cooldown_pin_by, cooldown_pin_at)
		VALUES ('hot-lane', 'https://example.com/hot', '/tmp/hot', 10, 5, 900, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'),
		 86400, 'fleet-toml-import', '2026-09-28T12:00:00Z')`); err != nil {
		t.Fatalf("insert hot-lane: %v", err)
	}
	if _, err := m.db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at)
		VALUES ('clean-lane', 'https://example.com/clean', '/tmp/clean', 10, 5, 900, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("insert clean-lane: %v", err)
	}

	out, err := ctlCallTool(t, m, "cooldown_residue", nil)
	if err != nil {
		t.Fatalf("cooldown_residue: %v", err)
	}
	var payload struct {
		Residue []struct {
			Name      string `json:"name"`
			CooldownS int    `json:"cooldown_s"`
			PinS      int    `json:"pin_s"`
			PinBy     string `json:"pin_by"`
		} `json:"residue"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("tool text is not the residue payload: %v\n%s", err, out)
	}
	if payload.Count != 1 || len(payload.Residue) != 1 {
		t.Fatalf("residue = %+v (count %d), want exactly [hot-lane]", payload.Residue, payload.Count)
	}
	got := payload.Residue[0]
	if got.Name != "hot-lane" || got.CooldownS != 900 || got.PinS != 86400 || got.PinBy != "fleet-toml-import" {
		t.Errorf("residue lane = %+v, want {hot-lane 900 86400 fleet-toml-import}", got)
	}
}
