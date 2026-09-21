package scheduler

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"

	_ "modernc.org/sqlite"
)

func TestSCHEDGAP127MigrationBackfillsLegacyOnce(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "sgap127-migration", RepoURL: "https://example.test/sgap127-migration",
		Workdir: t.TempDir(), Weight: 10, Priority: 5, CooldownS: 900,
		DecayRate: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, row := range []struct {
		id, source string
	}{
		{"legacy-empty", ""},
		{"already-measured", CostSourceMeasured},
	} {
		if _, err := db.ExecContext(ctx, `
INSERT INTO ticks (id, project_name, status, cost_source, created_at)
VALUES (?, 'sgap127-migration', 'completed', ?, '2026-09-16T00:00:00Z')`, row.id, row.source); err != nil {
			t.Fatalf("insert tick %s: %v", row.id, err)
		}
	}

	// newTestDB is fully migrated. Removing only v36 recreates the exact
	// pre-backfill state while retaining migration v29's cost_source column.
	if _, err := db.ExecContext(ctx, `DELETE FROM migrations WHERE version = 36`); err != nil {
		t.Fatalf("remove v36 marker: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	var legacy, measured string
	if err := db.QueryRowContext(ctx, `SELECT cost_source FROM ticks WHERE id = 'legacy-empty'`).Scan(&legacy); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT cost_source FROM ticks WHERE id = 'already-measured'`).Scan(&measured); err != nil {
		t.Fatalf("read measured row: %v", err)
	}
	if legacy != "legacy" {
		t.Errorf("empty cost_source after v36 = %q, want legacy", legacy)
	}
	if measured != CostSourceMeasured {
		t.Errorf("stamped cost_source changed to %q, want %q", measured, CostSourceMeasured)
	}

	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migrations WHERE version = 36`).Scan(&applied); err != nil {
		t.Fatalf("read v36 marker: %v", err)
	}
	if applied != 1 {
		t.Fatalf("v36 marker count = %d, want 1", applied)
	}

	// Prove the normal migration guard skips the UPDATE on a second startup:
	// a post-migration blank remains blank rather than being re-stamped.
	if _, err := db.ExecContext(ctx, `UPDATE ticks SET cost_source = '' WHERE id = 'legacy-empty'`); err != nil {
		t.Fatalf("reset post-migration row: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT cost_source FROM ticks WHERE id = 'legacy-empty'`).Scan(&legacy); err != nil {
		t.Fatalf("read row after second Migrate: %v", err)
	}
	if legacy != "" {
		t.Errorf("v36 re-ran after its marker existed: cost_source = %q, want blank", legacy)
	}
}

func TestSCHEDGAP127LaneMarginalAndCostFlow(t *testing.T) {
	if got := laneMarginalUSD("glm-5.3-flash", "zai-glm"); got != 0 {
		t.Errorf("zai-glm marginal USD = %v, want 0", got)
	}
	if got := laneMarginalUSD("deepseek-v4-flash", "deepseek-foreman"); got <= 0 {
		t.Errorf("deepseek PAYG marginal USD = %v, want > 0", got)
	}

	start := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	sub := resolveTickCost("/missing", "", "sub-project", "zai-glm", "glm-5.3-flash", routerRate{}, start, end, clock.Real())
	if sub.costUSD != 0 || sub.source != CostSourceEstimatedSubincluded {
		t.Errorf("sub-included fallback = cost %v source %q, want 0/%q", sub.costUSD, sub.source, CostSourceEstimatedSubincluded)
	}
	if sub.stickerUSD <= 0 {
		t.Errorf("sub-included listed-price sticker = %v, want > 0", sub.stickerUSD)
	}

	payg := resolveTickCost("/missing", "", "payg-project", "deepseek-foreman", "deepseek-v4-flash", routerRate{}, start, end, clock.Real())
	if payg.costUSD <= 0 || payg.source != CostSourceEstimated {
		t.Errorf("PAYG fallback = cost %v source %q, want >0/%q", payg.costUSD, payg.source, CostSourceEstimated)
	}

	home := t.TempDir()
	stateDB := filepath.Join(home, "state.db")
	createSessionUsageDB(t, stateDB, start.Add(10*time.Minute), 0.25, 0.40)
	measured := resolveTickCost(home, "", "measured-project", "zai-glm", "glm-5.3-flash", routerRate{}, start, end, clock.Real())
	if measured.source != CostSourceMeasured {
		t.Errorf("telemetry branch source = %q, want %q", measured.source, CostSourceMeasured)
	}
	// resolveRealTickCost prefers actual (0.40) over estimated (0.25).
	if measured.costUSD < 0.399 || measured.costUSD > 0.401 {
		t.Errorf("telemetry branch cost = %v, want 0.40", measured.costUSD)
	}
}

func TestSCHEDGAP127BudgetGateMeteredFlagAndZeroMarginal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"metered-project", "subincluded-project"} {
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "https://example.test/" + name, Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, cost_usd, cost_source, created_at)
VALUES ('sticker-tick', 'metered-project', 'completed', ?, ?, 9.0, 'estimated', ?),
       ('sub-tick', 'subincluded-project', 'completed', ?, ?, 0.0, 'estimated_subincluded', ?)`,
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339),
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("insert budget ticks: %v", err)
	}

	stateDB := filepath.Join(t.TempDir(), "state.db")
	// Budget mode's explicit contract is estimated + actual = 1.25 + 0.75 = 2.00.
	createSessionUsageDB(t, stateDB, now.Add(-90*time.Minute), 1.25, 0.75)
	t.Cleanup(func() { SetMeteredBudgetEnabled(false, "") })

	SetMeteredBudgetEnabled(false, stateDB)
	if detail, blocked := NewBudgetGate(ctx, db, now)("metered-project", 5, 0, 0); !blocked {
		t.Errorf("flag off did not read $9 sticker tick (detail=%q)", detail)
	}

	SetMeteredBudgetEnabled(true, stateDB)
	meteredGate := NewBudgetGate(ctx, db, now)
	if meteredGate == nil {
		t.Fatal("metered gate is nil")
	}
	if detail, blocked := meteredGate("metered-project", 5, 0, 0); blocked {
		t.Errorf("flag on used sticker instead of $2 metered value: %q", detail)
	}
	if detail, blocked := meteredGate("metered-project", 2, 0, 0); !blocked {
		t.Errorf("flag on did not enforce exact $2 metered value (detail=%q)", detail)
	}

	SetMeteredBudgetEnabled(false, stateDB)
	if detail, blocked := NewBudgetGate(ctx, db, now)("subincluded-project", 0.01, 0, 0); blocked {
		t.Errorf("zero-marginal sub-included tick falsely exhausted budget: %q", detail)
	}
}

func TestSCHEDGAP127MeteredBudgetTOMLKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedulerd.toml")
	if err := os.WriteFile(path, []byte("[scheduler]\nmetered_budget_enabled = true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadRootConfig(path)
	if err != nil {
		t.Fatalf("LoadRootConfig: %v", err)
	}
	if !cfg.Scheduler.MeteredBudgetEnabled {
		t.Error("scheduler.metered_budget_enabled did not decode true")
	}
}

func createSessionUsageDB(t *testing.T, path string, at time.Time, estimated, actual float64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session_model_usage (
		session_id TEXT, model TEXT, billing_provider TEXT DEFAULT '',
		estimated_cost_usd REAL DEFAULT 0, actual_cost_usd REAL DEFAULT 0,
		first_seen REAL, last_seen REAL,
		input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create session_model_usage: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_model_usage
		(session_id, model, estimated_cost_usd, actual_cost_usd, first_seen, last_seen, input_tokens, output_tokens)
		VALUES ('sgap127', 'model', ?, ?, ?, ?, 1000, 100)`,
		estimated, actual, at.Unix(), at.Add(time.Minute).Unix()); err != nil {
		t.Fatalf("insert session usage: %v", err)
	}
}
