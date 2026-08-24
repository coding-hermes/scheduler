package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── SCHED-GAP-066: per-project budget config keys ────────────────────────

// TestLoadFleetConfig_BudgetKeys pins the fleet.toml schema: the three budget
// caps decode into ProjectDef pointers (nil when the key is absent).
func TestLoadFleetConfig_BudgetKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")

	tomlContent := `[[projects]]
name = "budgeted"
repo_url = "https://github.com/example/budgeted"
workdir = "/home/kara/budgeted"
daily_budget_usd = 5.0
weekly_budget_usd = 20.0
final_budget_usd = 50.0

[[projects]]
name = "unbudgeted"
repo_url = "https://github.com/example/unbudgeted"
workdir = "/home/kara/unbudgeted"
`
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}

	b := findProject(cfg, "budgeted")
	if b == nil {
		t.Fatal("budgeted project missing from decoded config")
	}
	if b.DailyBudgetUSD == nil || *b.DailyBudgetUSD != 5.0 {
		t.Errorf("daily_budget_usd = %v, want 5.0", b.DailyBudgetUSD)
	}
	if b.WeeklyBudgetUSD == nil || *b.WeeklyBudgetUSD != 20.0 {
		t.Errorf("weekly_budget_usd = %v, want 20.0", b.WeeklyBudgetUSD)
	}
	if b.FinalBudgetUSD == nil || *b.FinalBudgetUSD != 50.0 {
		t.Errorf("final_budget_usd = %v, want 50.0", b.FinalBudgetUSD)
	}

	u := findProject(cfg, "unbudgeted")
	if u == nil {
		t.Fatal("unbudgeted project missing from decoded config")
	}
	if u.DailyBudgetUSD != nil || u.WeeklyBudgetUSD != nil || u.FinalBudgetUSD != nil {
		t.Errorf("absent budget keys must decode as nil, got (%v, %v, %v)",
			u.DailyBudgetUSD, u.WeeklyBudgetUSD, u.FinalBudgetUSD)
	}
}

// TestApplyFleetConfig_BudgetPins (SCHED-GAP-066): fleet.toml seeds budget
// caps on NEW projects and pins them on EXISTING projects when the key is
// present — including an explicit 0, which clears an API-assigned cap back to
// unlimited. A keyless entry leaves an API-assigned budget untouched
// (GatewayKey-style conditional pin).
func TestApplyFleetConfig_BudgetPins(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	five := 5.0
	twenty := 20.0

	// New project seeded from fleet.toml.
	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "budget-seeded", RepoURL: "https://github.com/example/budget-seeded",
				Workdir:        "/home/kara/budget-seeded",
				DailyBudgetUSD: &five, WeeklyBudgetUSD: &twenty,
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (seed): %v", err)
	}
	p, err := database.GetProject(ctx, db, "budget-seeded")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.DailyBudgetUSD != 5.0 || p.WeeklyBudgetUSD != 20.0 {
		t.Errorf("seeded budgets = (%.2f, %.2f), want (5.00, 20.00)", p.DailyBudgetUSD, p.WeeklyBudgetUSD)
	}

	// Keyless re-pin must not clear an API-assigned cap.
	three := 3.0
	if err := database.UpdateProject(ctx, db, "budget-seeded", database.ProjectUpdates{FinalBudgetUSD: &three}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	cfg.Projects[0].DailyBudgetUSD = nil
	cfg.Projects[0].WeeklyBudgetUSD = nil
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (keyless re-pin): %v", err)
	}
	p, err = database.GetProject(ctx, db, "budget-seeded")
	if err != nil {
		t.Fatalf("GetProject after keyless re-pin: %v", err)
	}
	if p.DailyBudgetUSD != 5.0 || p.WeeklyBudgetUSD != 20.0 {
		t.Errorf("budgets after keyless re-pin = (%.2f, %.2f), want (5.00, 20.00) — keyless entries must not clear pins",
			p.DailyBudgetUSD, p.WeeklyBudgetUSD)
	}
	if p.FinalBudgetUSD != 3.0 {
		t.Errorf("FinalBudgetUSD after keyless re-pin = %.2f, want 3.00 (API-assigned cap must survive)", p.FinalBudgetUSD)
	}

	// An explicit 0 in fleet.toml clears the cap back to unlimited.
	zero := 0.0
	cfg.Projects[0].DailyBudgetUSD = &zero
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (explicit-zero re-pin): %v", err)
	}
	p, err = database.GetProject(ctx, db, "budget-seeded")
	if err != nil {
		t.Fatalf("GetProject after explicit-zero re-pin: %v", err)
	}
	if p.DailyBudgetUSD != 0 {
		t.Errorf("DailyBudgetUSD after explicit-0 re-pin = %.2f, want 0 (explicit zero clears the cap)", p.DailyBudgetUSD)
	}
}
