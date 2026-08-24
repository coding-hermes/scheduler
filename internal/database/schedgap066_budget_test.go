package database

import (
	"context"
	"testing"
)

// ── SCHED-GAP-066: per-project budget columns ────────────────────────────

// TestMigrate_BudgetColumns pins migration v15 (SCHED-GAP-066): the projects
// table must gain daily_budget_usd/weekly_budget_usd/final_budget_usd so
// fleet.toml budget caps persist across restarts.
func TestMigrate_BudgetColumns(t *testing.T) {
	db := newTestDB(t)

	for _, col := range []string{"daily_budget_usd", "weekly_budget_usd", "final_budget_usd"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('projects') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(projects) for %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("projects.%s missing after Migrate (count=%d) — migration v15 not applied", col, n)
		}
	}
}

// TestProject_BudgetRoundTrip pins CreateProject/GetProject/UpdateProject/
// ListProjects round-tripping the SCHED-GAP-066 budget fields end-to-end.
func TestProject_BudgetRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("budget-caps")
	p.DailyBudgetUSD = 5.0
	p.WeeklyBudgetUSD = 20.0
	p.FinalBudgetUSD = 50.0
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := GetProject(ctx, db, "budget-caps")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.DailyBudgetUSD != 5.0 || got.WeeklyBudgetUSD != 20.0 || got.FinalBudgetUSD != 50.0 {
		t.Errorf("GetProject budget fields = (%.2f, %.2f, %.2f), want (5.00, 20.00, 50.00)",
			got.DailyBudgetUSD, got.WeeklyBudgetUSD, got.FinalBudgetUSD)
	}

	// Partial update: clear the daily cap (0 = unlimited), keep the rest.
	zero := 0.0
	if err := UpdateProject(ctx, db, "budget-caps", ProjectUpdates{DailyBudgetUSD: &zero}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = GetProject(ctx, db, "budget-caps")
	if err != nil {
		t.Fatalf("GetProject after update: %v", err)
	}
	if got.DailyBudgetUSD != 0 {
		t.Errorf("DailyBudgetUSD = %.2f after clearing update, want 0", got.DailyBudgetUSD)
	}
	if got.WeeklyBudgetUSD != 20.0 {
		t.Errorf("WeeklyBudgetUSD = %.2f after unrelated update, want 20.00 (partial update must not clear it)", got.WeeklyBudgetUSD)
	}

	// ListProjects must also surface the fields.
	all, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := false
	for _, pr := range all {
		if pr.Name == "budget-caps" {
			found = true
			if pr.WeeklyBudgetUSD != 20.0 || pr.FinalBudgetUSD != 50.0 {
				t.Errorf("ListProjects budget fields = (%.2f, %.2f), want (20.00, 50.00)",
					pr.WeeklyBudgetUSD, pr.FinalBudgetUSD)
			}
		}
	}
	if !found {
		t.Error("budget-caps missing from ListProjects")
	}
}

// TestProject_BudgetDefaultsZero pins the opt-in default: a project created
// without budget fields reads back 0 (= unlimited) on all three caps.
func TestProject_BudgetDefaultsZero(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateProject(ctx, db, sampleProject("budget-defaults")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := GetProject(ctx, db, "budget-defaults")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.DailyBudgetUSD != 0 || got.WeeklyBudgetUSD != 0 || got.FinalBudgetUSD != 0 {
		t.Errorf("default budget caps = (%.2f, %.2f, %.2f), want all 0 (unlimited)",
			got.DailyBudgetUSD, got.WeeklyBudgetUSD, got.FinalBudgetUSD)
	}
}
