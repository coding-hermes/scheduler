package api_test

import (
	"testing"
)

// SCHED-GAP-1655 — deliverable 4 over the API surface: the per-lane
// outcome split (no_work = the zero-tool-call waste count) rides
// GET /api/v1/projects/{name} beside the dispatch split, following the
// same zero-filled-vocabulary contract (SCHED-GAP-1653's dispatch_split
// shape, which the 1653 API tests pin).
//
// The aggregate itself (CountProjectOutcomes: zero-filled buckets, NoWork
// count, Unset for outcome-less terminal rows) is pinned at the storage
// layer by TestSCHEDGAP1655_CountProjectOutcomes in internal/database —
// this file pins the WIRE contract: the field is present in the project
// detail payload, its no_work number matches the seeded ticks, and an
// unknown project still 404s rather than fabricating a split.

// TestSCHEDGAP1655_ProjectDetailCarriesOutcomeSplit seeds ticks across the
// outcome vocabulary and reads the detail endpoint.
func TestSCHEDGAP1655_ProjectDetailCarriesOutcomeSplit(t *testing.T) {
	a := newAPITestServer(t)

	// Seed the lane.
	if code, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name": "waste-lane", "repo_url": "https://example.com/waste", "workdir": "/tmp/waste-lane",
	}); code != 201 && code != 200 {
		t.Fatalf("create project: status %d body %v", code, body)
	}

	// Two no_work ticks + one committed tick, terminal.
	for i, outcome := range []string{"no_work", "no_work", "committed"} {
		if _, err := a.db.Exec(`INSERT INTO ticks (id, project_name, status, outcome, created_at)
			VALUES (?, 'waste-lane', 'completed', ?, datetime('now'))`,
			"1655-"+outcome+"-"+string(rune('a'+i)), outcome); err != nil {
			t.Fatalf("seed tick %s: %v", outcome, err)
		}
	}

	code, body := a.do(t, "GET", "/api/v1/projects/waste-lane", nil)
	if code != 200 {
		t.Fatalf("GET detail: status %d body %v", code, body)
	}
	split, ok := body["outcome_split"].(map[string]interface{})
	if !ok {
		t.Fatalf("outcome_split missing or not an object: %v", body)
	}
	if got := split["no_work"].(float64); got != 2 {
		t.Errorf("outcome_split.no_work = %v, want 2", got)
	}
	if got := split["total"].(float64); got != 3 {
		t.Errorf("outcome_split.total = %v, want 3", got)
	}
	buckets, ok := split["buckets"].(map[string]interface{})
	if !ok {
		t.Fatalf("outcome_split.buckets missing or not an object: %v", split)
	}
	// Zero-filled vocabulary: a consumer reads any outcome without a
	// presence check.
	for _, o := range []string{"committed", "dry_run", "failed", "timeout", "deferred", "aborted:no_artifact", "no_work"} {
		if _, ok := buckets[o]; !ok {
			t.Errorf("buckets[%q] missing — the vocabulary must be zero-filled over the wire", o)
		}
	}
}

// TestSCHEDGAP1655_ProjectDetailUnknownProject404s pins the honest-error
// arm: the split never fabricates numbers for a lane that does not exist.
func TestSCHEDGAP1655_ProjectDetailUnknownProject404s(t *testing.T) {
	a := newAPITestServer(t)
	code, _ := a.do(t, "GET", "/api/v1/projects/no-such-lane", nil)
	if code != 404 {
		t.Errorf("GET unknown project: status %d, want 404", code)
	}
}
