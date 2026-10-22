package api_test

import (
	"net/http"
	"testing"
)

// SCHED-GAP-1755 AC 2: /api/v1/health carries a per-family stall census —
// stalls_24h, the family's terminal-tick denominator, the resulting stall
// rate, and the per-turn deadline VALUE armed for the family. The row's
// post-deploy verification ("duckbrain-sync stall rate <10%, qa <20%, quoted
// from ticks.error counts") is read straight off this block, so the test
// seeds the exact shape the live window produces: stalled rows in two
// families with different armed deadlines, plus non-stalled terminal ticks
// that must count toward the denominator without inflating the numerator.
func TestSCHEDGAP1755_HealthExposesStallCensus(t *testing.T) {
	a := newAPITestServer(t)

	// Two families with the two knob states the mechanism defines:
	//   duckbrain-sync → '0s' (per-turn EXEMPT, migration v73)
	//   qa             → ''   (inherits the global 30m knob)
	for _, ns := range []struct{ id, deadline string }{
		{"duckbrain-sync", "0s"},
		{"qa", ""},
	} {
		if _, err := a.db.Exec(`INSERT INTO namespaces (id) VALUES (?)`, ns.id); err != nil {
			t.Fatalf("insert namespace %s: %v", ns.id, err)
		}
		if _, err := a.db.Exec(`UPDATE namespaces SET turn_deadline = ? WHERE id = ?`, ns.deadline, ns.id); err != nil {
			t.Fatalf("set turn_deadline %s: %v", ns.id, err)
		}
	}

	seed := func(project, ns string) {
		mustCreateAPITestProject(t, a.db, project)
		if _, err := a.db.Exec(`UPDATE projects SET namespace_id = ? WHERE name = ?`, ns, project); err != nil {
			t.Fatalf("assign %s to %s: %v", project, ns, err)
		}
	}
	seed("sgap1755-sync-1", "duckbrain-sync")
	seed("sgap1755-qa-1", "qa")

	// Terminal ticks inside the 24h window: 1 stalled + 3 completed on the
	// exempt family (25% in the seed — the fix drives it to 0); 3 stalled +
	// 1 completed on qa.
	stalledErr := "stalled: no progress for 30m0s (gateway /v1/responses per-turn deadline; tick timeout 3h0m0s)"
	// outcomeFor maps the tick status onto the row CHECK's closed vocabulary
	// ('committed','dry_run','failed','timeout','deferred','aborted:no_artifact',
	// 'no_work'): a completed tick carries 'committed', a failed one
	// 'failed', a timeout one 'timeout'.
	outcomeFor := func(status string) string {
		switch status {
		case "completed":
			return "committed"
		case "timeout":
			return "timeout"
		default:
			return "failed"
		}
	}
	insertTick := func(id, project, status, errText string) {
		var errVal any
		if errText != "" {
			errVal = errText
		}
		if _, err := a.db.Exec(`INSERT INTO ticks
			(id, project_name, status, outcome, spawned_at, completed_at, error, created_at)
			VALUES (?, ?, ?, ?, datetime('now','-5 hours'), datetime('now','-4 hours'), ?, datetime('now','-5 hours'))`,
			id, project, status, outcomeFor(status), errVal); err != nil {
			t.Fatalf("insert tick %s: %v", id, err)
		}
	}
	insertTick("sgap1755-sync-t1", "sgap1755-sync-1", "failed", stalledErr)
	insertTick("sgap1755-sync-t2", "sgap1755-sync-1", "completed", "")
	insertTick("sgap1755-sync-t3", "sgap1755-sync-1", "completed", "")
	insertTick("sgap1755-sync-t4", "sgap1755-sync-1", "completed", "")
	insertTick("sgap1755-qa-t1", "sgap1755-qa-1", "failed", stalledErr)
	insertTick("sgap1755-qa-t2", "sgap1755-qa-1", "failed", stalledErr)
	insertTick("sgap1755-qa-t3", "sgap1755-qa-1", "timeout", stalledErr)
	insertTick("sgap1755-qa-t4", "sgap1755-qa-1", "completed", "")

	// A stale OUT-OF-WINDOW stalled tick must not be counted (the census is
	// a 24h window, not lifetime history).
	if _, err := a.db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at, error, created_at)
		VALUES ('sgap1755-qa-old', 'sgap1755-qa-1', 'failed', 'failed',
		        datetime('now','-3 days'), datetime('now','-3 days'), ?, datetime('now','-3 days'))`,
		stalledErr); err != nil {
		t.Fatalf("insert out-of-window tick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/health", nil)
	if status != http.StatusOK {
		t.Fatalf("health status = %d, want 200", status)
	}
	raw, ok := body["stall_census"]
	if !ok {
		t.Fatalf("stall_census missing from /api/v1/health: %v", body)
	}
	census, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("stall_census is %T, want an object", raw)
	}

	check := func(family string, wantStalls, wantTerminal float64, wantDeadline string, wantExempt bool) {
		t.Helper()
		ent, ok := census[family].(map[string]any)
		if !ok {
			t.Fatalf("stall_census[%q] missing or wrong type: %v", family, census)
		}
		if got := ent["stalls_24h"]; got != wantStalls {
			t.Errorf("stall_census[%q].stalls_24h = %v, want %v", family, got, wantStalls)
		}
		if got := ent["terminal_ticks_24h"]; got != wantTerminal {
			t.Errorf("stall_census[%q].terminal_ticks_24h = %v, want %v", family, got, wantTerminal)
		}
		if got := ent["turn_deadline"]; got != wantDeadline {
			t.Errorf("stall_census[%q].turn_deadline = %v, want %q (the deadline VALUE used)", family, got, wantDeadline)
		}
		if got := ent["exempt_from_turn_deadline"]; got != wantExempt {
			t.Errorf("stall_census[%q].exempt_from_turn_deadline = %v, want %v", family, got, wantExempt)
		}
	}
	check("duckbrain-sync", 1, 4, "0s", true)
	check("qa", 3, 4, "", false)

	// The rate is the AC's own arithmetic: stalls_24h / terminal_ticks_24h.
	if ent, ok := census["duckbrain-sync"].(map[string]any); ok {
		if got, want := ent["stall_rate_24h"], 0.25; got != want {
			t.Errorf("stall_census[duckbrain-sync].stall_rate_24h = %v, want %v", got, want)
		}
	}
}
