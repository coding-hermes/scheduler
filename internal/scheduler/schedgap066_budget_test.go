package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── SCHED-GAP-066: per-project budget enforcement ────────────────────────
//
// These pin the PASS criteria at the selection level:
//   - a project whose spend reaches a configured cap is EXCLUDED from
//     selection (zero new spawns) while any running tick row is untouched
//     (budgets gate spawns only — never kill a running tick);
//   - daily/weekly windows reset on UTC boundaries (Monday 00:00 UTC for the
//     weekly window); final_budget_usd never resets;
//   - caps are opt-in: 0/unset = unlimited.

// insertCostTick inserts a tick row with an explicit spawned_at and cost.
func insertCostTick(t *testing.T, db *sql.DB, tickID, project, status string, spawnedAt time.Time, cost float64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, cost_usd, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tickID, project, status, spawnedAt.UTC().Format(time.RFC3339), cost, spawnedAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert cost tick %s: %v", tickID, err)
	}
}

// setBudgetCaps writes the three budget columns directly (simulating a
// fleet.toml pin or API PUT).
func setBudgetCaps(t *testing.T, db *sql.DB, name string, daily, weekly, final float64) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE projects SET daily_budget_usd = ?, weekly_budget_usd = ?, final_budget_usd = ? WHERE name = ?`,
		daily, weekly, final, name); err != nil {
		t.Fatalf("set budget caps for %s: %v", name, err)
	}
}

// TestSchedGap066_DailyCapBlocksSelection is THE acceptance test for
// criterion (1)+(2)+(6): a project over its daily_budget_usd is excluded from
// namespace-pack selection while a running tick row for it stays 'running'
// (never killed mid-run), and an uncapped sibling is still selected.
func TestSchedGap066_DailyCapBlocksSelection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "budget-capped", "coding-hermes", 10, 5, 900, 1.0)
	mustCreateProjectInNS(t, db, "budget-free", "coding-hermes", 10, 5, 900, 1.0)
	setBudgetCaps(t, db, "budget-capped", 5.0, 0, 0)

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) // Wednesday
	// $6.25 already spent TODAY (inside the UTC day window) → over the $5 cap.
	insertCostTick(t, db, "t-cap-1", "budget-capped", "completed", now.Add(-3*time.Hour), 6.25)
	// A tick currently in flight — enforcement must NOT touch it.
	insertCostTick(t, db, "t-cap-running", "budget-capped", "running", now.Add(-30*time.Minute), 0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	mp.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, now))
	// running=nil deliberately: any exclusion of budget-capped is attributable
	// to the budget gate alone, not the already-running filter.
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), nil, nil, now)

	got := packNames(result)
	if containsName(got, "budget-capped") {
		t.Errorf("Pack selected budget-capped despite exhausted daily budget ($6.25/$5.00) — zero new spawns required")
	}
	if !containsName(got, "budget-free") {
		t.Errorf("Pack did not select uncapped sibling budget-free: %v", got)
	}

	// The running tick row must be untouched — budgets gate spawns only.
	var status string
	if err := db.QueryRow(`SELECT status FROM ticks WHERE id = 't-cap-running'`).Scan(&status); err != nil {
		t.Fatalf("query running tick: %v", err)
	}
	if status != "running" {
		t.Errorf("running tick status = %q after Pack, want \"running\" (never kill a running tick)", status)
	}
}

// TestSchedGap066_FlatPathsBlockSelection covers the other two selection
// paths: the no-namespaces Pack fallback (packFlat) and the DB-backed flat
// Packer.Pick must both exclude a budget-exhausted project.
func TestSchedGap066_FlatPathsBlockSelection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateProjectAt(t, db, "flat-capped", 10, 5, 900, 1.0) // nil namespace → unassigned/flat
	setBudgetCaps(t, db, "flat-capped", 0, 10.0, 0)

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	insertCostTick(t, db, "t-flat-1", "flat-capped", "completed", now.Add(-26*time.Hour), 12.0) // this week → over $10

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	// (a) MultiPoolPacker with NO namespaces → packFlat path.
	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	mp.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, now))
	res := mp.Pack(projects, nil, prodUrgencyCalc(), nil, nil, now)
	if containsName(packNames(res), "flat-capped") {
		t.Errorf("packFlat selected flat-capped despite exhausted weekly budget ($12.00/$10.00)")
	}

	// (b) Flat single-pool Packer.Pick.
	fp := scheduler.NewPacker(db, prodUrgencyCalc(), 100, 4, nil)
	fp.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, now))
	picked, err := fp.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	for _, p := range picked {
		if p.Name == "flat-capped" {
			t.Errorf("Packer.Pick selected flat-capped despite exhausted weekly budget ($12.00/$10.00)")
		}
	}
}

// TestSchedGap066_WeeklyResetBoundary pins criterion (4): spend before the
// UTC week boundary (Monday 00:00 UTC) does NOT count against the current
// week; spend inside it does.
func TestSchedGap066_WeeklyResetBoundary(t *testing.T) {
	// Window helpers: Sunday belongs to the week that started the PREVIOUS
	// Monday; Wednesday's day starts at its own 00:00 UTC.
	sunday := time.Date(2026, 8, 23, 23, 0, 0, 0, time.UTC)
	if got, want := scheduler.UTCWeekStart(sunday), time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("UTCWeekStart(Sun 2026-08-23 23:00) = %v, want Mon 2026-08-17 00:00", got)
	}
	wed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if got, want := scheduler.UTCWeekStart(wed), time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("UTCWeekStart(Wed 2026-08-26 12:00) = %v, want Mon 2026-08-24 00:00", got)
	}
	if got, want := scheduler.UTCDayStart(wed), time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("UTCDayStart(Wed 2026-08-26 12:00) = %v, want 2026-08-26 00:00", got)
	}

	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProjectAt(t, db, "weekly-capped", 10, 5, 900, 1.0)
	setBudgetCaps(t, db, "weekly-capped", 0, 10.0, 0)

	// $20 spent LAST week (Sunday 23:00 UTC) — must not count against the
	// current week.
	insertCostTick(t, db, "t-w-last", "weekly-capped", "completed", sunday, 20.0)

	spends, err := scheduler.LoadBudgetSpends(ctx, db, wed)
	if err != nil {
		t.Fatalf("LoadBudgetSpends: %v", err)
	}
	s := spends["weekly-capped"]
	if s.Weekly != 0 {
		t.Errorf("weekly spend = %.2f, want 0 — last-week spend leaked across the Monday 00:00 UTC boundary", s.Weekly)
	}
	if s.Total != 20.0 {
		t.Errorf("total spend = %.2f, want 20.00", s.Total)
	}
	if _, blocked := scheduler.NewBudgetGate(ctx, db, wed)("weekly-capped", 0, 10.0, 0); blocked {
		t.Error("blocked after last-week spend only — weekly window must reset at Monday 00:00 UTC")
	}

	// $10.50 spent THIS week → over the $10 cap → blocked.
	insertCostTick(t, db, "t-w-this", "weekly-capped", "completed", time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC), 10.50)
	spends, err = scheduler.LoadBudgetSpends(ctx, db, wed)
	if err != nil {
		t.Fatalf("LoadBudgetSpends: %v", err)
	}
	if s := spends["weekly-capped"]; s.Weekly != 10.50 {
		t.Errorf("weekly spend = %.2f, want 10.50", s.Weekly)
	}
	detail, blocked := scheduler.NewBudgetGate(ctx, db, wed)("weekly-capped", 0, 10.0, 0)
	if !blocked {
		t.Error("not blocked after $10.50/$10.00 this-week spend — weekly cap must bite")
	} else if detail == "" {
		t.Error("blocked with empty detail — gate must say which window exhausted")
	}
}

// TestSchedGap066_FinalBudgetPermanent pins criterion (3): final_budget_usd
// is a one-time lifetime cap — once exhausted the project stays blocked with
// no reset window, however far in the future the evaluation happens.
func TestSchedGap066_FinalBudgetPermanent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProjectAt(t, db, "one-shot", 10, 5, 900, 1.0)
	setBudgetCaps(t, db, "one-shot", 0, 0, 25.0)

	// $30 lifetime spend, all of it WEEKS old (outside any daily/weekly
	// window) — only the final cap can catch it.
	insertCostTick(t, db, "t-f-1", "one-shot", "completed", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 15.0)
	insertCostTick(t, db, "t-f-2", "one-shot", "completed", time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), 15.0)

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	spends, err := scheduler.LoadBudgetSpends(ctx, db, now)
	if err != nil {
		t.Fatalf("LoadBudgetSpends: %v", err)
	}
	if s := spends["one-shot"]; s.Daily != 0 || s.Weekly != 0 || s.Total != 30.0 {
		t.Errorf("spend windows = (daily %.2f, weekly %.2f, total %.2f), want (0, 0, 30.00)",
			s.Daily, s.Weekly, s.Total)
	}
	if detail, blocked := scheduler.NewBudgetGate(ctx, db, now)("one-shot", 0, 0, 25.0); !blocked {
		t.Error("not blocked at $30.00/$25.00 lifetime — final budget must bite")
	} else if detail == "" {
		t.Error("blocked with empty detail")
	}

	// Months later: still blocked. No window resets the lifetime total.
	later := now.AddDate(0, 3, 0)
	if _, blocked := scheduler.NewBudgetGate(ctx, db, later)("one-shot", 0, 0, 25.0); !blocked {
		t.Error("final-budget block lifted 3 months later — lifetime caps must be permanent")
	}

	// And it never reaches selection again (flat path, arbitrary future now).
	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	mp.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, later))
	res := mp.Pack(projects, nil, prodUrgencyCalc(), nil, nil, later)
	if containsName(packNames(res), "one-shot") {
		t.Error("Pack selected one-shot after final budget exhaustion — project must stop scheduling for good")
	}
}

// TestSchedGap066_OptInDefaultUnlimited pins criterion (4, opt-in half):
// budget 0 (the schema default) means unlimited — a project with zero-value
// caps is never blocked, regardless of spend.
func TestSchedGap066_OptInDefaultUnlimited(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProjectAt(t, db, "uncapped", 10, 5, 900, 1.0)
	// No setBudgetCaps call — columns stay at the 0.0 schema default.

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	insertCostTick(t, db, "t-u-1", "uncapped", "completed", now.Add(-time.Hour), 1000.0)

	if _, blocked := scheduler.NewBudgetGate(ctx, db, now)("uncapped", 0, 0, 0); blocked {
		t.Error("zero-capped project blocked on $1000 spend — 0 must mean unlimited")
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	mp.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, now))
	res := mp.Pack(projects, nil, prodUrgencyCalc(), nil, nil, now)
	if !containsName(packNames(res), "uncapped") {
		t.Errorf("Pack did not select uncapped project with $1000 spend: %v", packNames(res))
	}
}
