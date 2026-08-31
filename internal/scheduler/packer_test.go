package scheduler_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func makeProject(name string, weight, priority, cooldown int, decay float64) *database.Project {
	return &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    weight,
		Priority:  priority,
		CooldownS: cooldown,
		DecayRate: decay,
		Model:     "test-model",
		Provider:  "test-provider",
		Enabled:   true,
	}
}

func mustCreateProjectAt(t *testing.T, db *sql.DB, name string, weight, priority, cooldown int, decay float64) {
	t.Helper()
	p := makeProject(name, weight, priority, cooldown, decay)
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestNewPacker_StoresBudget verifies the constructor captures budget + maxConcurrent.
func TestNewPacker_StoresBudget(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 50, 5, nil)

	if p.Budget() != 50 {
		t.Errorf("Budget() = %d, want 50", p.Budget())
	}
}

// TestPick_EmptyDatabase returns nil/empty when no enabled projects exist.
func TestPick_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 5, nil)

	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pick with empty DB returned %d projects, want 0", len(got))
	}
}

// TestPick_RespectsBudget verifies that the total weight of picked projects stays within budget.
func TestPick_RespectsBudget(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Create 5 projects, each with weight=30. Budget=100 → should fit at most 3.
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if err := database.CreateProject(ctx, db, makeProject(n, 30, 5, 0, 1.0)); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("Pick returned %d projects, want 3 (budget=100, weight=30 each)", len(got))
	}
	total := 0
	for _, proj := range got {
		total += proj.Weight
	}
	if total > 100 {
		t.Errorf("total weight %d exceeds budget 100", total)
	}
}

// TestPick_SkipsDisabled verifies that disabled projects are excluded.
func TestPick_SkipsDisabled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	disabled := makeProject("off", 10, 5, 0, 1.0)
	disabled.Enabled = false
	if err := database.CreateProject(ctx, db, disabled); err != nil {
		t.Fatalf("CreateProject off: %v", err)
	}
	if err := database.CreateProject(ctx, db, makeProject("on", 10, 5, 0, 1.0)); err != nil {
		t.Fatalf("CreateProject on: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Pick returned %d projects, want 1 (disabled excluded)", len(got))
	}
	if got[0].Name != "on" {
		t.Errorf("Picked %q, want on", got[0].Name)
	}
}

// TestPick_SortedByUrgency verifies higher urgency is preferred.
func TestPick_SortedByUrgency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Low priority, recent last_tick.
	if err := database.CreateProject(ctx, db, makeProject("low", 10, 1, 0, 1.0)); err != nil {
		t.Fatalf("CreateProject low: %v", err)
	}
	// High priority, just created — should have higher urgency.
	if err := database.CreateProject(ctx, db, makeProject("high", 10, 10, 0, 1.0)); err != nil {
		t.Fatalf("CreateProject high: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("Pick returned %d projects, want >= 2", len(got))
	}
	if got[0].Name != "high" {
		t.Errorf("first picked = %q, want high (higher urgency)", got[0].Name)
	}
}

// TestPick_RespectsCooldown verifies that projects inside their cooldown window are skipped.
func TestPick_RespectsCooldown(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Project with cooldown=3600s (1h).
	if err := database.CreateProject(ctx, db, makeProject("cool", 10, 5, 3600, 1.0)); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Simulate that this project just completed by setting last_tick_completed = now.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `UPDATE projects SET last_tick_completed = ? WHERE name = ?`, now, "cool"); err != nil {
		t.Fatalf("update last_tick_completed: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Pick returned %d, want 0 (project within cooldown)", len(got))
	}
}

// TestPick_RespectsMaxConcurrent verifies the packer stops when concurrency cap is reached.
func TestPick_RespectsMaxConcurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for _, n := range []string{"a", "b", "c"} {
		if err := database.CreateProject(ctx, db, makeProject(n, 5, 5, 0, 1.0)); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	// maxConcurrent=2, budget=100 → packer should pick at most 2.
	p := scheduler.NewPacker(db, calc, 100, 2, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) > 2 {
		t.Errorf("Pick returned %d, want <= 2 (maxConcurrent=2)", len(got))
	}
}

// benchPackerDB creates an in-memory DB with n projects, all enabled, with
// varying priority and weight so the urgency-sort and budget-fit paths both
// run during the benchmark.
func benchPackerDB(b *testing.B, n int) (*sql.DB, *scheduler.Packer, time.Time) {
	b.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		b.Fatalf("InitDB(:memory:): %v", err)
	}
	b.Cleanup(func() { db.Close() })

	ctx := context.Background()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("proj-%04d", i)
		// Cycle priority 1..10 and weight 1..20 so each project differs.
		prio := (i % 10) + 1
		wt := (i % 20) + 1
		if err := database.CreateProject(ctx, db, makeProject(name, wt, prio, 0, 1.0)); err != nil {
			b.Fatalf("CreateProject %s: %v", name, err)
		}
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	// Budget sized so roughly half of n projects fit; budget > total so all fit
	// is also realistic — use a generous budget that exercises the sort + scan
	// paths but still hits the early-break when maxConcurrent is low.
	p := scheduler.NewPacker(db, calc, n*5, n, nil)
	return db, p, time.Now()
}

// BenchmarkPick measures Packer.Pick() across project counts. The hot path is
// Query → Scan → urgency compute → sort → greedy fit. We don't reuse the
// DB across iterations because Pick is read-only and the budget keeps growing.
func BenchmarkPick(b *testing.B) {
	for _, n := range []int{5, 50, 200} {
		b.Run(fmt.Sprintf("Projects=%d", n), func(b *testing.B) {
			_, packer, now := benchPackerDB(b, n)
			running := make(map[string]bool)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := packer.Pick(now, running)
				if err != nil {
					b.Fatalf("Pick: %v", err)
				}
				// Use the result so the compiler can't elide the call.
				if len(got) == 0 && n > 0 {
					b.Fatalf("Pick returned 0 projects for n=%d", n)
				}
			}
		})
	}
}

// BenchmarkPick_WithRunning measures the packer when some projects are
// already in the running set — exercises the spawnerRunning skip path
// that hot loops check on every iteration.
func BenchmarkPick_WithRunning(b *testing.B) {
	for _, n := range []int{5, 50, 200} {
		b.Run(fmt.Sprintf("Projects=%d", n), func(b *testing.B) {
			_, packer, now := benchPackerDB(b, n)
			// Mark every other project as already running.
			running := make(map[string]bool, n/2)
			for i := 0; i < n; i += 2 {
				running[fmt.Sprintf("proj-%04d", i)] = true
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := packer.Pick(now, running)
				if err != nil {
					b.Fatalf("Pick: %v", err)
				}
				_ = got
			}
		})
	}
}

// TestPick_PopulatesFields verifies the returned PackedProject carries through DB fields.
func TestPick_PopulatesFields(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := makeProject("alpha", 42, 7, 0, 1.5)
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	packer := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := packer.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Pick returned %d projects, want 1", len(got))
	}
	proj := got[0]
	if proj.Name != "alpha" {
		t.Errorf("Name = %q, want alpha", proj.Name)
	}
	if proj.Weight != 42 {
		t.Errorf("Weight = %d, want 42", proj.Weight)
	}
	if proj.Priority != 7 {
		t.Errorf("Priority = %f, want 7", proj.Priority)
	}
	if proj.Workdir != "/tmp/alpha" {
		t.Errorf("Workdir = %q, want /tmp/alpha", proj.Workdir)
	}
	if proj.RepoURL != "https://example.com/alpha" {
		t.Errorf("RepoURL = %q, want https://example.com/alpha", proj.RepoURL)
	}
	if proj.Urgency <= 0 {
		t.Errorf("Urgency = %f, want > 0", proj.Urgency)
	}
}

// ── ListEnabled tests ──

func TestListEnabled_Empty(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 5, nil)

	got, err := p.ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListEnabled with empty DB returned %d projects, want 0", len(got))
	}
}

func TestListEnabled_WithProjects(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectAt(t, db, "alpha", 10, 5, 0, 1.0)
	mustCreateProjectAt(t, db, "beta", 20, 7, 0, 1.5)

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 5, nil)

	got, err := p.ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListEnabled returned %d projects, want 2", len(got))
	}

	// Verify fields are populated.
	found := map[string]scheduler.PackedProject{}
	for _, proj := range got {
		found[proj.Name] = proj
	}

	alpha, ok := found["alpha"]
	if !ok {
		t.Fatal("alpha not found in ListEnabled result")
	}
	if alpha.Weight != 10 {
		t.Errorf("alpha.Weight = %d, want 10", alpha.Weight)
	}
	if alpha.Priority != 5 {
		t.Errorf("alpha.Priority = %f, want 5", alpha.Priority)
	}
	if alpha.Workdir != "/tmp/alpha" {
		t.Errorf("alpha.Workdir = %q, want /tmp/alpha", alpha.Workdir)
	}
	if alpha.RepoURL != "https://example.com/alpha" {
		t.Errorf("alpha.RepoURL = %q", alpha.RepoURL)
	}

	beta, ok := found["beta"]
	if !ok {
		t.Fatal("beta not found in ListEnabled result")
	}
	if beta.Weight != 20 {
		t.Errorf("beta.Weight = %d, want 20", beta.Weight)
	}
}

func TestListEnabled_SkipsDisabled(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Create a disabled project using raw SQL (mustCreateProjectAt always enables).
	if err := database.CreateProject(ctx, db, &database.Project{
		Name:     "disabled",
		RepoURL:  "https://example.com/disabled",
		Workdir:  "/tmp/disabled",
		Weight:   10,
		Priority: 5,
		Model:    "test",
		Provider: "test",
		Enabled:  false,
	}); err != nil {
		t.Fatalf("CreateProject disabled: %v", err)
	}
	mustCreateProjectAt(t, db, "enabled", 5, 3, 0, 1.0)

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 5, nil)

	got, err := p.ListEnabled(context.Background())
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListEnabled returned %d projects, want 1 (disabled excluded)", len(got))
	}
	if got[0].Name != "enabled" {
		t.Errorf("ListEnabled picked %q, want enabled", got[0].Name)
	}
}
func TestPacker_BlackoutSlowdown_DoublesCooldown(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)

	lt := now.Add(-700 * time.Second)
	if _, err := db.Exec("INSERT INTO projects (name, weight, priority, enabled, cooldown_s, decay_rate, repo_url, workdir, created_at, updated_at, last_tick_completed) VALUES (?, ?, ?, 1, ?, ?, 'https://example.com/test', '/tmp/test', ?, ?, ?)",
		"peak-test", 10, 5, 600, 1.0,
		now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339),
		lt.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)

	p1 := scheduler.NewPacker(db, calc, 100, 5, nil)
	packed, _ := p1.Pick(now, nil)
	if len(packed) == 0 {
		t.Fatal("no blackout: expected project to be picked (700s > 600s)")
	}

	windows := []config.BlackoutWindow{{Start: "06:00", End: "10:00", Multiplier: 2.0}}
	p2 := scheduler.NewPacker(db, calc, 100, 5, windows)
	packed, _ = p2.Pick(now, nil)
	if len(packed) > 0 {
		t.Fatalf("with 2x blackout: expected 0 projects, got %d", len(packed))
	}
}

func TestPacker_BlackoutSlowdown_OutsideWindow_NoEffect(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 30, 5, 0, 0, 0, time.UTC)

	lt := now.Add(-700 * time.Second)
	db.Exec("INSERT INTO projects (name, weight, priority, enabled, cooldown_s, decay_rate, repo_url, workdir, created_at, updated_at, last_tick_completed) VALUES (?, ?, ?, 1, ?, ?, 'https://example.com/test', '/tmp/test', ?, ?, ?)",
		"outside-test", 10, 5, 600, 1.0,
		now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339),
		lt.Format(time.RFC3339))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	windows := []config.BlackoutWindow{
		{Start: "01:00", End: "04:00", Multiplier: 2.0},
		{Start: "06:00", End: "10:00", Multiplier: 2.0},
	}
	p := scheduler.NewPacker(db, calc, 100, 5, windows)
	packed, _ := p.Pick(now, nil)
	if len(packed) == 0 {
		t.Fatal("outside peak: expected project to be picked")
	}
}

func TestPacker_BlackoutSlowdown_SkipMode(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)

	lt := now.Add(-2000 * time.Second)
	db.Exec("INSERT INTO projects (name, weight, priority, enabled, cooldown_s, decay_rate, repo_url, workdir, created_at, updated_at, last_tick_completed) VALUES (?, ?, ?, 1, ?, ?, 'https://example.com/test', '/tmp/test', ?, ?, ?)",
		"skip-test", 10, 5, 600, 1.0,
		now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339),
		lt.Format(time.RFC3339))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)

	p1 := scheduler.NewPacker(db, calc, 100, 5, nil)
	packed, _ := p1.Pick(now, nil)
	if len(packed) == 0 {
		t.Fatal("no blackout: expected project to be picked")
	}

	windows := []config.BlackoutWindow{{Start: "01:00", End: "04:00", Multiplier: 0}}
	p2 := scheduler.NewPacker(db, calc, 100, 5, windows)
	packed, _ = p2.Pick(now, nil)
	if len(packed) > 0 {
		t.Fatalf("skip mode: expected 0 projects, got %d", len(packed))
	}
}

// ── GAP-011: due-aware force selection ────────────────────────────────────
//
// A project whose time since last completed tick is STRICTLY greater than 2x
// its effective cooldown MUST be selected regardless of weight, urgency, or
// ordering — force-selected before greedy budget packing so neither the
// weight budget nor the SCHED-GAP-066 spend gate can starve it.

// setLastTick stamps last_tick_completed for a project (the packer's
// cooldown/due clock). The timestamp is second-aligned so RFC3339
// round-trips through the DB are exact, making the exactly-2x boundary
// deterministic.
func setLastTick(t *testing.T, db *sql.DB, name string, ts time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		ts.UTC().Format(time.RFC3339), name); err != nil {
		t.Fatalf("set last_tick_completed for %s: %v", name, err)
	}
}

// packedNames returns the names in a flat Pick result.
func packedNames(ps []scheduler.PackedProject) []string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	return names
}

// mustCreateWave creates two high-weight, high-priority projects that were
// created after `now` — never completed, not overdue — so the greedy pack
// alone fills a 100-weight budget exactly (2 × 50).
func mustCreateWave(t *testing.T, db *sql.DB) {
	t.Helper()
	mustCreateProjectAt(t, db, "wave-a", 50, 10, 600, 1.0)
	mustCreateProjectAt(t, db, "wave-b", 50, 10, 600, 1.0)
}

// TestPick_Gap011_OverdueSelectedDuringBudgetWave is THE acceptance test for
// GAP-011 criterion (a): an overdue low-weight project is force-selected
// while a high-weight wave fills the weight budget — the greedy pack alone
// would have excluded it (100 + 20 > 100).
func TestPick_Gap011_OverdueSelectedDuringBudgetWave(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateWave(t, db)
	// Overdue low-weight project: last completed tick 3h ago (> 2x 900s).
	mustCreateProjectAt(t, db, "starved-low", 20, 1, 900, 1.0)
	setLastTick(t, db, "starved-low", now.Add(-3*time.Hour))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if !containsName(names, "starved-low") {
		t.Errorf("overdue starved-low not selected: %v (force-select must bypass the weight budget)", names)
	}
	if !containsName(names, "wave-a") || !containsName(names, "wave-b") {
		t.Errorf("wave projects should still be selected: %v", names)
	}
}

// TestPick_Gap011_Exactly2xNotForced pins the strictly-greater boundary: a
// project exactly 2x cooldown past its last tick is NOT force-selected and,
// with the budget already consumed by the wave, is not packed at all.
func TestPick_Gap011_Exactly2xNotForced(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateWave(t, db)
	// Last completed tick EXACTLY 2x cooldown (1800s) ago.
	mustCreateProjectAt(t, db, "exact-2x", 20, 1, 900, 1.0)
	setLastTick(t, db, "exact-2x", now.Add(-1800*time.Second))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if containsName(names, "exact-2x") {
		t.Errorf("exact-2x must NOT be forced (only strictly greater than 2x): %v", names)
	}
	if len(names) != 2 {
		t.Errorf("Pick returned %v, want exactly the 2 wave projects", names)
	}
}

// TestPick_Gap011_WithinCooldownStaysIneligible pins that a project inside
// its cooldown window is neither force-selected nor greedily packed.
func TestPick_Gap011_WithinCooldownStaysIneligible(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateWave(t, db)
	// Last completed tick 10 min ago, cooldown 3600s — inside cooldown and
	// well inside the 2x due threshold.
	mustCreateProjectAt(t, db, "in-cooldown", 20, 1, 3600, 1.0)
	setLastTick(t, db, "in-cooldown", now.Add(-10*time.Minute))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if containsName(names, "in-cooldown") {
		t.Errorf("in-cooldown project must stay ineligible: %v", names)
	}
}

// TestPick_Gap011_NeverCompletedPast2x pins that a never-completed project
// counts as never cooldown-satisfied: once its created_at age passes 2x
// cooldown it is force-selected, while a fresh never-completed project is
// not (it loses to the budget-filled wave in the greedy pack).
func TestPick_Gap011_NeverCompletedPast2x(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateWave(t, db)
	mustCreateProjectAt(t, db, "old-never", 20, 1, 900, 1.0)
	mustCreateProjectAt(t, db, "fresh-never", 20, 1, 900, 1.0)
	// Backdate old-never's created_at to 3h before now (never completed).
	if _, err := db.Exec(`UPDATE projects SET created_at = ? WHERE name = ?`,
		now.Add(-3*time.Hour).UTC().Format(time.RFC3339), "old-never"); err != nil {
		t.Fatalf("backdate created_at for old-never: %v", err)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if !containsName(names, "old-never") {
		t.Errorf("never-completed project past 2x cooldown must be force-selected: %v", names)
	}
	if containsName(names, "fresh-never") {
		t.Errorf("fresh never-completed project must not be forced: %v", names)
	}
}

// TestPick_Gap011_DisabledAndRunningNeverForced pins that disabled projects
// (excluded by the enabled=1 query) and currently-running projects are never
// force-selected, even when overdue.
func TestPick_Gap011_DisabledAndRunningNeverForced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	mustCreateWave(t, db)
	// Overdue but currently running — must stay out.
	mustCreateProjectAt(t, db, "running-overdue", 20, 1, 900, 1.0)
	setLastTick(t, db, "running-overdue", now.Add(-3*time.Hour))
	// Overdue but disabled — the enabled=1 query excludes it outright.
	disabled := makeProject("disabled-overdue", 20, 1, 900, 1.0)
	disabled.Enabled = false
	if err := database.CreateProject(ctx, db, disabled); err != nil {
		t.Fatalf("CreateProject disabled-overdue: %v", err)
	}
	setLastTick(t, db, "disabled-overdue", now.Add(-3*time.Hour))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	got, err := p.Pick(now, map[string]bool{"running-overdue": true})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if containsName(names, "running-overdue") {
		t.Errorf("running overdue project must never be force-selected: %v", names)
	}
	if containsName(names, "disabled-overdue") {
		t.Errorf("disabled overdue project must never be force-selected: %v", names)
	}
	if !containsName(names, "wave-a") || !containsName(names, "wave-b") {
		t.Errorf("wave projects should still be selected: %v", names)
	}
}

// TestPick_Gap011_OverdueBypassesSpendGate pins that the SCHED-GAP-066 USD
// spend gate — which excludes a budget-exhausted project from the greedy
// pack — cannot exclude an overdue project: the force-select still picks it.
func TestPick_Gap011_OverdueBypassesSpendGate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Fixed midday UTC: the completed cost tick at -3h falls inside the
	// daily window, so the spend gate reliably blocks the project.
	now := time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC)

	mustCreateWave(t, db)
	mustCreateProjectAt(t, db, "capped-overdue", 20, 1, 900, 1.0)
	setBudgetCaps(t, db, "capped-overdue", 5.0, 0, 0)
	insertCostTick(t, db, "t-cap-overdue", "capped-overdue", "completed", now.Add(-3*time.Hour), 6.25)
	setLastTick(t, db, "capped-overdue", now.Add(-3*time.Hour))

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)
	p.SetBudgetGate(scheduler.NewBudgetGate(ctx, db, now))
	got, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	names := packedNames(got)
	if !containsName(names, "capped-overdue") {
		t.Errorf("overdue project must bypass the SCHED-GAP-066 spend gate: %v", names)
	}
}
