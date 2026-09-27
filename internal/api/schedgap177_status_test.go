package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSCHEDGAP177_StatusLaneOutputFamilies pins the /api/v1/status
// lane_output_families block (SCHED-GAP-177): per family — lanes (enabled
// only), summed lifetime output ticks, worst zero-output streak, lanes at
// the alert threshold — plus duckbrain-sync resolving as the sync family
// and non-family namespaces excluded.
func TestSCHEDGAP177_StatusLaneOutputFamilies(t *testing.T) {
	a := newAPITestServer(t)

	ctx := context.Background()
	seedNS := func(id string) {
		t.Helper()
		if _, err := a.db.ExecContext(ctx,
			`INSERT INTO namespaces (id) VALUES (?)`, id); err != nil {
			t.Fatalf("insert namespace %s: %v", id, err)
		}
	}
	for _, ns := range []string{"qa", "pm", "duckbrain-sync", "coding-hermes"} {
		seedNS(ns)
	}
	seedProj := func(name, ns string, enabled int) {
		t.Helper()
		var nsArg any
		if ns != "" {
			nsArg = ns
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO projects
			(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
			 model, provider, enabled, created_at, updated_at, namespace_id)
			VALUES (?, 'https://github.com/example/x', '/tmp/x', 10, 5, 900, 1.0,
			 'm', 'p', ?, datetime('now'), datetime('now'), ?)`,
			name, enabled, nsArg); err != nil {
			t.Fatalf("insert project %s: %v", name, err)
		}
	}
	// qa family: 2 enabled lanes (4 and 9 output ticks), worst streak 9
	// (one lane at/past the alert threshold of 8), 1 disabled lane that
	// must not count.
	seedProj("qa-a", "qa", 1)
	seedProj("qa-b", "qa", 1)
	seedProj("qa-disabled", "qa", 0)
	// sync family via the duckbrain-sync slug: 1 enabled lane.
	seedProj("sync-a", "duckbrain-sync", 1)
	// A coding lane: excluded from every family.
	seedProj("code-a", "coding-hermes", 1)

	setCounters := func(name string, qOut, qSt, sOut, sSt int) {
		t.Helper()
		if _, err := a.db.ExecContext(ctx, `UPDATE projects
			SET qa_output_count = ?, qa_zero_output_streak = ?,
			    sync_output_count = ?, sync_zero_output_streak = ?
			WHERE name = ?`, qOut, qSt, sOut, sSt, name); err != nil {
			t.Fatalf("update %s counters: %v", name, err)
		}
	}
	setCounters("qa-a", 4, 2, 0, 0)
	setCounters("qa-b", 9, 9, 0, 0)
	setCounters("qa-disabled", 100, 100, 0, 0)
	setCounters("sync-a", 0, 0, 5, 1)
	setCounters("code-a", 7, 7, 0, 0)

	status, resp := a.do(t, http.MethodGet, "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/status = %d", status)
	}
	raw, ok := resp["lane_output_families"]
	if !ok {
		t.Fatalf("status body has no lane_output_families block (SCHED-GAP-177); keys: %v", keysOf(resp))
	}
	var fams map[string]struct {
		Lanes       int `json:"lanes"`
		OutputTicks int `json:"output_ticks"`
		MaxStreak   int `json:"max_zero_output_streak"`
		AtThreshold int `json:"lanes_at_alert_threshold"`
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal block: %v", err)
	}
	if err := json.Unmarshal(b, &fams); err != nil {
		t.Fatalf("decode lane_output_families: %v (%s)", err, b)
	}
	qa := fams["qa"]
	if qa.Lanes != 2 || qa.OutputTicks != 13 || qa.MaxStreak != 9 || qa.AtThreshold != 1 {
		t.Errorf("qa family block = %+v, want lanes=2 output=13 maxStreak=9 atThreshold=1 (disabled lane excluded)", qa)
	}
	sy := fams["sync"]
	if sy.Lanes != 1 || sy.OutputTicks != 5 {
		t.Errorf("sync family block = %+v, want lanes=1 output=5 (duckbrain-sync slug resolves to sync)", sy)
	}
	if _, ok := fams["coding-hermes"]; ok {
		t.Error("coding-hermes appears as a family; non-family namespaces must be absent")
	}
	for _, fam := range []string{"qa", "pm", "sync", "dogfood"} {
		if _, ok := fams[fam]; !ok {
			t.Errorf("family %s missing from lane_output_families (all four must always be present)", fam)
		}
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
