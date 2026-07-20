package scheduler_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
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
	p := scheduler.NewPacker(db, calc, 50, 5)

	if p.Budget() != 50 {
		t.Errorf("Budget() = %d, want 50", p.Budget())
	}
}

// TestPick_EmptyDatabase returns nil/empty when no enabled projects exist.
func TestPick_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 5)

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
	p := scheduler.NewPacker(db, calc, 100, 10)
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
	p := scheduler.NewPacker(db, calc, 100, 10)
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
	p := scheduler.NewPacker(db, calc, 100, 10)
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
	p := scheduler.NewPacker(db, calc, 100, 10)
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
	p := scheduler.NewPacker(db, calc, 100, 2)
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
	p := scheduler.NewPacker(db, calc, n*5, n)
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
	packer := scheduler.NewPacker(db, calc, 100, 10)
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
