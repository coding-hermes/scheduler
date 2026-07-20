package dashboard_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coding-herms/scheduler/internal/dashboard"
	"github.com/coding-herms/scheduler/internal/database"
)

// TestGenerateFleetTable_RendersTBody verifies that the htmx partial produces
// only the project table body (no <html>, <head>, or page chrome).
func TestGenerateFleetTable_RendersTBody(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := gen.GenerateFleetTable(&buf); err != nil {
		t.Fatalf("GenerateFleetTable: %v", err)
	}
	out := buf.String()

	// Must contain the tbody wrapper that the dashboard page expects.
	if !strings.Contains(out, `<tbody id="fleet-overview">`) {
		t.Errorf("expected <tbody id=\"fleet-overview\"> wrapper, got: %q", snippet(out, "fleet"))
	}
	// Must NOT contain full-page chrome (this is a partial, not a page).
	if strings.Contains(out, "<!DOCTYPE html>") {
		t.Errorf("partial should not contain DOCTYPE; it's not a full page")
	}
	if strings.Contains(out, "<title>") {
		t.Errorf("partial should not contain <title>; it's not a full page")
	}
}

// TestGenerateFleetTable_WithProjects verifies project rows render with
// anchor links to /projects/{name} so users can drill down.
func TestGenerateFleetTable_WithProjects(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 30, 5)
	mustCreateProject(t, db, "beta", 20, 3)

	gen := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := gen.GenerateFleetTable(&buf); err != nil {
		t.Fatalf("GenerateFleetTable: %v", err)
	}
	out := buf.String()

	// Each project name should appear as a link to its detail page.
	if !strings.Contains(out, `href="/projects/alpha"`) {
		t.Errorf("expected link to /projects/alpha, got: %s", snippet(out, "alpha"))
	}
	if !strings.Contains(out, `href="/projects/beta"`) {
		t.Errorf("expected link to /projects/beta, got: %s", snippet(out, "beta"))
	}
	// Closing tbody must appear (the partial must be a complete fragment).
	if !strings.Contains(out, "</tbody>") {
		t.Errorf("expected closing </tbody> in partial")
	}
}

// TestGenerateProjectDetail_ValidName renders a detail page for an existing
// project and verifies all required fields are present.
func TestGenerateProjectDetail_ValidName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{
		Name:      "alpha",
		RepoURL:   "https://example.com/alpha",
		Workdir:   "/tmp/alpha",
		Weight:    30,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test-model",
		Provider:  "test-provider",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	gen := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := gen.GenerateProjectDetail(&buf, "alpha"); err != nil {
		t.Fatalf("GenerateProjectDetail: %v", err)
	}
	out := buf.String()

	// Required structural elements.
	for _, want := range []string{
		"<!DOCTYPE html>",
		"Project:",
		"alpha",
		"back to fleet",
		"Weight",
		"Priority",
		"Cooldown",
		"Enabled",
		"Configuration",
		"Recent Ticks",
		"https://example.com/alpha",
		"test-model",
		"test-provider",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("project detail page missing %q", want)
		}
	}
}

// TestGenerateProjectDetail_WithTicks renders the detail page when the project
// has recorded ticks — verifies the tick table renders with row data.
func TestGenerateProjectDetail_WithTicks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{
		Name:      "alpha",
		RepoURL:   "https://example.com/alpha",
		Workdir:   "/tmp/alpha",
		Weight:    30,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "m",
		Provider:  "p",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Create three ticks with distinct IDs.
	for i, id := range []string{"tick-aaa", "tick-bbb", "tick-ccc"} {
		err := database.CreateTick(ctx, db, &database.Tick{
			ID:          id,
			ProjectName: "alpha",
			Status:      database.StatusCompleted,
			Outcome:     database.OutcomeCommitted,
			SpawnedAt:   "2026-07-19T10:00:00Z",
			CompletedAt: "2026-07-19T10:05:00Z",
			Commits:     i + 1,
		})
		if err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}

	gen := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := gen.GenerateProjectDetail(&buf, "alpha"); err != nil {
		t.Fatalf("GenerateProjectDetail: %v", err)
	}
	out := buf.String()

	// Latest Tick section should show one of the tick IDs.
	for _, id := range []string{"tick-aaa", "tick-bbb", "tick-ccc"} {
		if !strings.Contains(out, id) {
			t.Errorf("expected tick id %q in detail page", id)
		}
	}
}

// TestGenerateProjectDetail_NotFound verifies that querying a project that
// doesn't exist returns an error wrapping ErrProjectNotFound.
func TestGenerateProjectDetail_NotFound(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db)

	var buf strings.Builder
	err := gen.GenerateProjectDetail(&buf, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent project, got nil")
	}
	if !strings.Contains(err.Error(), "project not found") &&
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("expected ErrProjectNotFound in chain, got: %v", err)
	}
}

// TestGenerateProjectDetail_EmptyName returns an error without touching the DB.
func TestGenerateProjectDetail_EmptyName(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := gen.GenerateProjectDetail(&buf, ""); err == nil {
		t.Error("expected error for empty project name, got nil")
	}
}

// TestHTMXJS_Embedded verifies the htmx library is bundled and serves as
// non-empty JS content. Used by the /static/htmx.min.js route.
func TestHTMXJS_Embedded(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db)

	js := gen.HTMXJS()
	if len(js) == 0 {
		t.Fatal("HTMXJS returned empty bytes; embed likely failed")
	}
	// htmx.min.js v1.x starts with the UMD wrapper "(function(e,t)".
	if !strings.HasPrefix(string(js[:min(50, len(js))]), "(function") {
		t.Errorf("htmx bytes don't look like UMD wrapper, got prefix: %q", string(js[:min(50, len(js))]))
	}
}

func TestGenerateTickHistory_PaginatesGlobalTicks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustCreateProject(t, db, "alpha", 10, 5)
	mustCreateProject(t, db, "beta", 10, 5)

	for i := 0; i < 51; i++ {
		project := "alpha"
		if i%2 == 1 {
			project = "beta"
		}
		tick := &database.Tick{
			ID:          project + "-history-" + string(rune('A'+i)),
			ProjectName: project,
			Status:      database.StatusCompleted,
			Outcome:     database.OutcomeCommitted,
			CreatedAt:   "2026-07-20T10:" + string(rune('A'+i)) + ":00Z",
		}
		if err := database.CreateTick(ctx, db, tick); err != nil {
			t.Fatalf("CreateTick %d: %v", i, err)
		}
	}

	gen := dashboard.NewGenerator(db)
	var page1, page2 strings.Builder
	if err := gen.GenerateTickHistory(&page1, 1); err != nil {
		t.Fatalf("GenerateTickHistory page 1: %v", err)
	}
	if err := gen.GenerateTickHistory(&page2, 2); err != nil {
		t.Fatalf("GenerateTickHistory page 2: %v", err)
	}

	for _, want := range []string{"Tick History", "Page 1", `hx-get="/ticks?page=2"`, `hx-target="#tick-history"`, "/projects/alpha", "/projects/beta"} {
		if !strings.Contains(page1.String(), want) {
			t.Errorf("page 1 missing %q", want)
		}
	}
	if !strings.Contains(page2.String(), "Page 2") || !strings.Contains(page2.String(), `hx-get="/ticks?page=1"`) {
		t.Errorf("page 2 missing previous-page controls: %s", snippet(page2.String(), "pagination"))
	}
}

func TestGenerateNamespaceView_ShowsProjectsAndUtilization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "platform", Weight: 40, Reserved: 10, HardCap: 60, Enabled: true, Description: "Platform services",
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	namespaceID := "platform"
	for _, name := range []string{"alpha", "beta"} {
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
			Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1, Model: "test", Provider: "test",
			NamespaceID: &namespaceID, Enabled: true,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup: "group-1", NamespaceID: namespaceID, Allocated: 40, Used: 20, Borrowed: 2, Lent: 1, JobCount: 2,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}

	gen := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := gen.GenerateNamespaceView(&buf, namespaceID); err != nil {
		t.Fatalf("GenerateNamespaceView: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Namespace: platform", "Platform services", "alpha", "beta", "50%", "+2", "-1", "Utilization History"} {
		if !strings.Contains(out, want) {
			t.Errorf("namespace view missing %q", want)
		}
	}
}

func TestGenerateNamespaceView_NotFound(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db)

	var buf strings.Builder
	err := gen.GenerateNamespaceView(&buf, "missing")
	if !errors.Is(err, database.ErrNamespaceNotFound) {
		t.Fatalf("GenerateNamespaceView error = %v, want ErrNamespaceNotFound", err)
	}
}

func TestGenerateHealth_ProbesGatewayAndAutoRefreshes(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, gateway.URL)
	var buf strings.Builder
	if err := gen.GenerateHealth(&buf); err != nil {
		t.Fatalf("GenerateHealth: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"System Health", "Daemon", "Gateway", "connected", "Uptime", "Goroutines", "Memory", `hx-get="/health"`, `hx-trigger="every 10s"`} {
		if !strings.Contains(out, want) {
			t.Errorf("health page missing %q", want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
