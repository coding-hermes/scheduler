package api_test

import (
	"testing"
	"time"
)

// TestSCHEDGAP176_TicksPayloadOmitsUrgencyAndWeightUsed proves the REST
// ticks payload no longer emits the documented-but-inert urgency /
// weight_used fields. The fields still exist in the DB schema and the
// database.Tick struct, but they are marked `json:"-"` so a JSON
// consumer cannot be misled by always-zero numbers.
//
// Measured live 2026-09-19: 72,393 ticks rows, `urgency > 0` = 0,
// `weight_used > 0` = 0 — RecordTickMetrics (the only writer) is dead
// code, so both fields are structurally always zero.
func TestSCHEDGAP176_TicksPayloadOmitsUrgencyAndWeightUsed(t *testing.T) {
	a := newAPITestServer(t)
	// ticks.project_name REFERENCES projects(name) — the parent row must
	// exist or the seed insert is rejected by the foreign key.
	mustCreateAPITestProject(t, a.db, "fakeheading")
	if _, err := a.db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, cost_usd, created_at)
		 VALUES ('t-schedgap176', 'fakeheading', 'completed', ?, ?, 0.42, ?)`,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	// /api/v1/ticks list
	code, body := a.do(t, "GET", "/api/v1/ticks?project=fakeheading&limit=5", nil)
	if code != 200 {
		t.Fatalf("list status: %d body=%v", code, body)
	}
	ticks, _ := body["ticks"].([]any)
	if len(ticks) != 1 {
		t.Fatalf("expected 1 tick, got %d", len(ticks))
	}
	for _, raw := range ticks {
		m, _ := raw.(map[string]any)
		for _, forbidden := range []string{"urgency", "weight_used"} {
			if _, ok := m[forbidden]; ok {
				t.Errorf("list payload still emits %q: %v", forbidden, m)
			}
		}
	}

	// /api/v1/ticks/<id>
	code2, one := a.do(t, "GET", "/api/v1/ticks/t-schedgap176", nil)
	if code2 != 200 {
		t.Fatalf("byid status: %d body=%v", code2, one)
	}
	for _, forbidden := range []string{"urgency", "weight_used"} {
		if _, ok := one[forbidden]; ok {
			t.Errorf("single-tick payload still emits %q: %v", forbidden, one)
		}
	}
}
