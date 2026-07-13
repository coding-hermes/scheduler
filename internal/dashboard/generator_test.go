package dashboard_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/dashboard"
	"github.com/coding-herms/scheduler/internal/database"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustCreateProject(t *testing.T, db *sql.DB, name string, weight, priority int) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    weight,
		Priority:  priority,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestGenerate_EmptyDatabase renders the dashboard with no projects.
func TestGenerate_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// Must contain core HTML scaffolding.
	for _, want := range []string{"<!DOCTYPE html>", "<title>Coding Hermes Fleet</title>", "Generated ", "Auto-refresh 60s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// With no projects, the counts should be 0/0 and budget 0/100.
	if !strings.Contains(out, "<div class=\"value\">0/0</div>") {
		t.Errorf("expected 0/0 in Enabled Projects card, got: %s", snippet(out, "Enabled Projects"))
	}
}

// TestGenerate_WithProjects renders the dashboard with seeded data.
func TestGenerate_WithProjects(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 20, 5)
	mustCreateProject(t, db, "beta", 30, 7)

	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"alpha", "beta", "Coding Hermes Fleet"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}

	// Budget used = 20+30 = 50, total = 100 → percent(50, 100) = 50 → "width:50%".
	if !strings.Contains(out, `width:50%`) {
		t.Errorf("expected budget bar to be width:50%%, got: %s", snippet(out, "budget-fill"))
	}
}

// TestGenerate_DisabledProject verifies disabled projects get the .disabled class.
func TestGenerate_DisabledProject(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	// Disable it.
	enabled := false
	if err := database.UpdateProject(context.Background(), db, "alpha", database.ProjectUpdates{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `class="disabled"`) {
		t.Errorf("expected disabled class on disabled project row, got: %s", snippet(out, "<tbody>"))
	}
}

// TestGenerate_WithTick verifies recent ticks are rendered with shortTime.
func TestGenerate_WithTick(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)

	// Insert a tick with a known spawned_at.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"alpha-1", "alpha", "completed", now, now); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// The recent ticks table should contain "alpha-1".
	if !strings.Contains(out, "alpha-1") {
		t.Errorf("expected tick ID 'alpha-1' in output")
	}
	// shortTime extracts HH:MM from RFC3339 — at least 5 chars of HH:MM.
	// We don't know the exact time, but the format should match HH:MM.
	if !strings.Contains(out, `class="meta">`) {
		t.Errorf("expected meta class for spawned-at rendering")
	}
}

// TestGenerate_BudgetZero verifies percent(0, total) returns 0 and doesn't divide by zero.
func TestGenerate_BudgetZero(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// No projects → BudgetUsed=0. percent(0, 100) = 0. Should not panic on /0.
	if !strings.Contains(out, `width:0%`) {
		t.Errorf("expected width:0%% for empty budget, got: %s", snippet(out, "budget-fill"))
	}
}

// TestGenerate_ContainsSummaryCards checks the 3 cards are present.
func TestGenerate_ContainsSummaryCards(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	for _, label := range []string{"Enabled Projects", "Active Ticks", "Budget"} {
		if !strings.Contains(out, label) {
			t.Errorf("missing summary card label %q", label)
		}
	}
}

// TestGenerate_GeneratedAtIsRFC3339 verifies the timestamp format.
func TestGenerate_GeneratedAtIsRFC3339(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// Extract "Generated <timestamp>" line.
	idx := strings.Index(out, "Generated ")
	if idx < 0 {
		t.Fatal("missing 'Generated ' marker")
	}
	rest := out[idx+len("Generated "):]
	end := strings.Index(rest, " ")
	if end < 0 {
		t.Fatal("malformed 'Generated' line")
	}
	ts := rest[:end]
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", ts, err)
	}
}

// snippet returns a small window of text centered on needle for diagnostic output.
func snippet(haystack, needle string) string {
	idx := strings.Index(haystack, needle)
	if idx < 0 {
		return "(needle not found)"
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := idx + 200
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}