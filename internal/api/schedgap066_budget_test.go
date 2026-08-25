package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── SCHED-GAP-066: /api/v1/projects budget payload ───────────────────────

// insertAPICostTick inserts a tick row with an explicit spawned_at and cost.
func insertAPICostTick(t *testing.T, a *apiTestServer, tickID, project string, spawnedAt time.Time, cost float64) {
	t.Helper()
	_, err := a.db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, cost_usd, created_at) VALUES (?, ?, 'completed', ?, ?, ?)`,
		tickID, project, spawnedAt.UTC().Format(time.RFC3339), cost, spawnedAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert cost tick %s: %v", tickID, err)
	}
}

// findPayloadProject locates a project in the /api/v1/projects list payload.
func findPayloadProject(t *testing.T, parsed map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	projects, ok := parsed["projects"].([]interface{})
	if !ok {
		t.Fatalf("payload projects is %T, want array", parsed["projects"])
	}
	for _, raw := range projects {
		p, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("project %q missing from /api/v1/projects payload", name)
	return nil
}

// TestSchedGap066_ProjectsPayloadBudget is the criterion (5)+(6) acceptance
// test: an exhausted project carries spent/remaining plus budget_blocked=true
// and blocked_reason="budget"; an uncapped sibling reports its spend with
// null remaining and no block.
func TestSchedGap066_ProjectsPayloadBudget(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()

	mustCreateAPITestProject(t, a.db, "budget-exhausted")
	mustCreateAPITestProject(t, a.db, "budget-unlimited")

	// Cap the first project at $5/day via the API update path.
	five := 5.0
	if err := database.UpdateProject(ctx, a.db, "budget-exhausted",
		database.ProjectUpdates{DailyBudgetUSD: &five}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	// $6.25 of ticks today → over the daily cap. Anchor tick times to the
	// UTC day start, NOT time.Now(): the daily window resets at UTC
	// midnight (UTCDayStart), so now.Add(-2h) falls on the previous day
	// between 00:00-02:00 UTC and the -2h tick drops out of the window
	// (spent 2.25 instead of 6.25 — time-of-day flaky; CI runs outside
	// that window so it stayed green). dayStart+1h/+2h are always inside
	// the current UTC day at any run time.
	dayStart := scheduler.UTCDayStart(time.Now())
	insertAPICostTick(t, a, "t-api-1", "budget-exhausted", dayStart.Add(time.Hour), 4.00)
	insertAPICostTick(t, a, "t-api-2", "budget-exhausted", dayStart.Add(2*time.Hour), 2.25)
	insertAPICostTick(t, a, "t-api-3", "budget-unlimited", dayStart.Add(2*time.Hour), 9.50)

	code, parsed := a.do(t, "GET", "/api/v1/projects", nil)
	if code != 200 {
		t.Fatalf("GET /api/v1/projects = %d, want 200", code)
	}

	ex := findPayloadProject(t, parsed, "budget-exhausted")
	if got, ok := ex["spent_daily_usd"].(float64); !ok || got != 6.25 {
		t.Errorf("exhausted spent_daily_usd = %v, want 6.25", ex["spent_daily_usd"])
	}
	if got, ok := ex["spent_total_usd"].(float64); !ok || got != 6.25 {
		t.Errorf("exhausted spent_total_usd = %v, want 6.25", ex["spent_total_usd"])
	}
	if got, ok := ex["remaining_daily_usd"].(float64); !ok || got != 0 {
		t.Errorf("exhausted remaining_daily_usd = %v, want 0 (clamped)", ex["remaining_daily_usd"])
	}
	if ex["budget_blocked"] != true {
		t.Errorf("exhausted budget_blocked = %v, want true", ex["budget_blocked"])
	}
	if ex["blocked_reason"] != "budget" {
		t.Errorf("exhausted blocked_reason = %v, want \"budget\"", ex["blocked_reason"])
	}
	if ex["budget_window"] != "daily" {
		t.Errorf("exhausted budget_window = %v, want \"daily\"", ex["budget_window"])
	}
	if ex["daily_budget_usd"] != 5.0 {
		t.Errorf("exhausted daily_budget_usd cap = %v, want 5 (cap echoed from project row)", ex["daily_budget_usd"])
	}

	un := findPayloadProject(t, parsed, "budget-unlimited")
	if got, ok := un["spent_daily_usd"].(float64); !ok || got != 9.50 {
		t.Errorf("unlimited spent_daily_usd = %v, want 9.50", un["spent_daily_usd"])
	}
	if un["budget_blocked"] != false {
		t.Errorf("unlimited budget_blocked = %v, want false", un["budget_blocked"])
	}
	if un["blocked_reason"] != "" {
		t.Errorf("unlimited blocked_reason = %v, want empty", un["blocked_reason"])
	}
	// Uncapped → remaining serializes as null (unlimited sentinel).
	if rem, present := un["remaining_daily_usd"]; !present || rem != nil {
		t.Errorf("unlimited remaining_daily_usd = %v (present=%v), want explicit null", rem, present)
	}
}
