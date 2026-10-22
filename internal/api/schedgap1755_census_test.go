package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1755 AC 2: /api/v1/health must expose the per-family stall census
// with the deadline value USED for each family. Two lanes are seeded with
// real tick rows — one satellite (sync) whose deadline is exempt and one
// primary (foreman) whose deadline is armed — and the response must report
// the counts AND the policy that produced them.
func TestSchedGap1755_HealthStallCensus(t *testing.T) {
	a := newAPITestServer(t)
	// The daemon-wide knob; the census must resolve each family against it.
	a.loop.SetGatewayResponseTimeout(30 * time.Minute)

	const syncLane = "duckbrain-sync"
	const primaryLane = "duckbrain-foreman"
	mustCreateAPITestProject(t, a.db, syncLane)
	mustCreateAPITestProject(t, a.db, primaryLane)

	now := time.Now()
	seed := func(id, project, status, errText string, age time.Duration) {
		t.Helper()
		if _, err := a.db.ExecContext(context.Background(), `
INSERT INTO ticks (id, project_name, status, spawned_at, error, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
			id, project, status, now.Add(-age).Format(time.RFC3339), errText,
			now.Add(-age).Format(time.RFC3339)); err != nil {
			t.Fatalf("seed tick %s: %v", id, err)
		}
	}
	// 4 sync ticks, 1 of them the stall class under test.
	seed("sgap1755-sync-1", syncLane, "failed", "stalled: no progress for 30m0s (gateway /v1/responses per-turn deadline; family \"sync\"; heartbeat resets 0; tick timeout 3h0m0s)", time.Hour)
	seed("sgap1755-sync-2", syncLane, "completed", "", 2*time.Hour)
	seed("sgap1755-sync-3", syncLane, "completed", "", 3*time.Hour)
	seed("sgap1755-sync-4", syncLane, "failed", "gateway unreachable and exec fallback disabled", 4*time.Hour)
	// 2 primary ticks, no stalls.
	seed("sgap1755-primary-1", primaryLane, "completed", "", time.Hour)
	seed("sgap1755-primary-2", primaryLane, "completed", "", 90*time.Minute)
	// A tick OUTSIDE the 24h window must not be counted.
	seed("sgap1755-old-1", syncLane, "failed", "stalled: no progress for 30m0s", 30*time.Hour)

	status, body := a.do(t, "GET", "/api/v1/health", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	rawCensus, ok := body["stalls_24h"].(map[string]interface{})
	if !ok {
		t.Fatalf("stalls_24h missing from /api/v1/health: %v", body)
	}
	if got := rawCensus["window_hours"].(float64); got != 24 {
		t.Errorf("window_hours = %v, want 24", got)
	}
	if got := rawCensus["total"].(float64); got != 1 {
		t.Errorf("total stalls_24h = %v, want 1 (only the in-window sync stall)", got)
	}
	families, ok := rawCensus["by_family"].([]interface{})
	if !ok || len(families) == 0 {
		t.Fatalf("by_family missing or empty: %v", rawCensus["by_family"])
	}
	rows := map[string]map[string]interface{}{}
	for _, f := range families {
		row, ok := f.(map[string]interface{})
		if !ok {
			t.Fatalf("by_family row is not an object: %v", f)
		}
		rows[row["family"].(string)] = row
	}
	// Every policy family must be present (the deadline column is the
	// load-bearing half — a family with no ticks still reports its deadline).
	for _, p := range scheduler.StallFamilyPolicies(30 * time.Minute) {
		if _, ok := rows[p.Family]; !ok {
			t.Errorf("by_family is missing family %q (policy row %+v)", p.Family, p)
		}
	}

	syncRow, ok := rows["sync"]
	if !ok {
		t.Fatalf("no sync row in census: %v", rows)
	}
	if got := syncRow["ticks"].(float64); got != 4 {
		t.Errorf("sync ticks = %v, want 4", got)
	}
	if got := syncRow["stalled_24h"].(float64); got != 1 {
		t.Errorf("sync stalled_24h = %v, want 1", got)
	}
	if got := syncRow["stall_rate"].(float64); got != 0.25 {
		t.Errorf("sync stall_rate = %v, want 0.25", got)
	}
	if got := syncRow["deadline"].(string); got != "exempt" {
		t.Errorf("sync deadline = %q, want \"exempt\" (SCHED-GAP-1755)", got)
	}
	if got := syncRow["deadline_ms"].(float64); got != 0 {
		t.Errorf("sync deadline_ms = %v, want 0", got)
	}
	if exempt, _ := syncRow["exempt"].(bool); !exempt {
		t.Errorf("sync exempt = %v, want true", syncRow["exempt"])
	}
	if hb, _ := syncRow["heartbeat"].(bool); !hb {
		t.Errorf("sync heartbeat = %v, want true", syncRow["heartbeat"])
	}

	primaryRow, ok := rows["primary"]
	if !ok {
		t.Fatalf("no primary row in census: %v", rows)
	}
	if got := primaryRow["ticks"].(float64); got != 2 {
		t.Errorf("primary ticks = %v, want 2", got)
	}
	if got := primaryRow["stalled_24h"].(float64); got != 0 {
		t.Errorf("primary stalled_24h = %v, want 0", got)
	}
	if got := primaryRow["deadline"].(string); got != "30m0s" {
		t.Errorf("primary deadline = %q, want \"30m0s\" (the daemon knob)", got)
	}
	if exempt, _ := primaryRow["exempt"].(bool); exempt {
		t.Errorf("primary exempt = true, want false (primary lanes keep the deadline)")
	}
}
