package api_test

import (
	"net/http"
	"testing"
)

// TestAPI_CooldownResidue pins the SCHED-GAP-1670 residue endpoint's wire
// shape: {"residue": [{name, cooldown_s, pin_s, pin_by}], "count": N} with
// an empty residue serializing as [] (never null), and GET-only semantics.
// The endpoint is read-only by contract — this test also asserts the
// residue row is UNCHANGED after a query (no hidden reconcile-on-read).
func TestAPI_CooldownResidue(t *testing.T) {
	a := newAPITestServer(t)

	// The residue row: live 900 below pin 86400 (the measured 2026-09-28
	// python-audit shape), provenance fleet-toml-import.
	if _, err := a.db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 cooldown_pin_s, cooldown_pin_by, cooldown_pin_at)
		VALUES ('hot-lane', 'https://example.com/hot', '/tmp/hot', 10, 5, 900, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'),
		 86400, 'fleet-toml-import', '2026-09-28T12:00:00Z')`); err != nil {
		t.Fatalf("insert hot-lane: %v", err)
	}
	// The clean row: live at its pin.
	if _, err := a.db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 cooldown_pin_s, cooldown_pin_by, cooldown_pin_at)
		VALUES ('clean-lane', 'https://example.com/clean', '/tmp/clean', 10, 5, 21600, 1.0,
		 'm', 'p', 1, datetime('now'), datetime('now'),
		 21600, 'api', '2026-09-28T12:00:00Z')`); err != nil {
		t.Fatalf("insert clean-lane: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/cooldown/residue", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	residue, ok := body["residue"].([]interface{})
	if !ok {
		t.Fatalf("residue field not an array: %T", body["residue"])
	}
	count, ok := body["count"].(float64)
	if !ok {
		t.Fatalf("count field not a number: %T", body["count"])
	}
	if int(count) != 1 {
		t.Fatalf("count = %d, want 1 (only hot-lane is live below its pin)", int(count))
	}
	if len(residue) != 1 {
		t.Fatalf("residue lanes = %d (%v), want exactly [hot-lane]", len(residue), residue)
	}
	lane, ok := residue[0].(map[string]interface{})
	if !ok {
		t.Fatalf("residue element not an object: %T", residue[0])
	}
	if lane["name"] != "hot-lane" {
		t.Errorf("residue name = %v, want hot-lane", lane["name"])
	}
	if lane["cooldown_s"] != float64(900) {
		t.Errorf("residue cooldown_s = %v, want 900", lane["cooldown_s"])
	}
	if lane["pin_s"] != float64(86400) {
		t.Errorf("residue pin_s = %v, want 86400", lane["pin_s"])
	}
	if lane["pin_by"] != "fleet-toml-import" {
		t.Errorf("residue pin_by = %v, want fleet-toml-import", lane["pin_by"])
	}

	// Read-only contract: the query must not reconcile the row.
	var live, pin int
	if err := a.db.QueryRow(`SELECT cooldown_s, cooldown_pin_s FROM projects WHERE name = 'hot-lane'`).Scan(&live, &pin); err != nil {
		t.Fatalf("read back hot-lane: %v", err)
	}
	if live != 900 || pin != 86400 {
		t.Errorf("endpoint mutated the row: live = %d, pin = %d — GET /api/v1/cooldown/residue is read-only by contract", live, pin)
	}

	// GET-only semantics.
	if status, _ := a.do(t, "POST", "/api/v1/cooldown/residue", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", status)
	}

	// Empty residue serializes as [] (never null): drop the pin and the
	// endpoint must return an empty array.
	if _, err := a.db.Exec(`UPDATE projects SET cooldown_pin_s = NULL, cooldown_pin_by = '' WHERE name = 'hot-lane'`); err != nil {
		t.Fatalf("clear pin: %v", err)
	}
	status, body = a.do(t, "GET", "/api/v1/cooldown/residue", nil)
	if status != http.StatusOK {
		t.Fatalf("status (empty) = %d, want 200", status)
	}
	residue, ok = body["residue"].([]interface{})
	if !ok {
		t.Fatalf("empty residue not an array (got %T — a null would break iterating clients)", body["residue"])
	}
	if len(residue) != 0 {
		t.Errorf("residue = %v, want empty", residue)
	}
	if body["count"] != float64(0) {
		t.Errorf("count = %v, want 0", body["count"])
	}
}
