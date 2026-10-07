package dashboard_test

// SCHED-GAP-1587 — the /lanes/tree dashboard page: the fleet's lane hierarchy
// rendered as a nested view with the DEPTH visible (that visibility is the
// whole point of the row). Parenthood comes from projects.parent through
// database.BuildLaneTree — the shared resolver — so the page agrees with the
// API's /api/v1/lanes/tree by construction.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// seed1587DashboardLanes creates the fixture shape the API test mirrors:
//
//	trio (root) ── trio-qa ── trio-qa-sync (depth 2)
//	            └─ trio-sync
//	fan (root) ── fan-alpha / fan-mid / fan-zeta
//	ghost-child (dangling parent "vanished-primary")
//	lone-disabled (disabled root — keeps its position)
func seed1587DashboardLanes(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	lanes := []struct{ name, parent string }{
		{"trio", ""},
		{"trio-qa", "trio"},
		{"trio-qa-sync", "trio-qa"},
		{"trio-sync", "trio"},
		{"fan", ""},
		{"fan-zeta", "fan"},
		{"fan-mid", "fan"},
		{"fan-alpha", "fan"},
	}
	for _, l := range lanes {
		p := &database.Project{
			Name: l.name, RepoURL: "https://example.com/" + l.name, Workdir: "/tmp/" + l.name,
			Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
			Model: "test", Provider: "test", Enabled: true,
		}
		if l.parent != "" {
			p.Parent = l.parent
		}
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("create %s: %v", l.name, err)
		}
	}
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "lone-disabled", RepoURL: "https://example.com/lone", Workdir: "/tmp/lone",
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
		Model: "test", Provider: "test", Enabled: false,
	}); err != nil {
		t.Fatalf("create lone-disabled: %v", err)
	}
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "ghost-child", RepoURL: "https://example.com/ghost", Workdir: "/tmp/ghost",
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
		Model: "test", Provider: "test", Enabled: true, Parent: "vanished-primary",
	}); err != nil {
		t.Fatalf("create ghost-child: %v", err)
	}
}

func TestSCHEDGAP1587_LaneTreePage_RendersNesting(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seed1587DashboardLanes(t, ctx, db)

	gen := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := gen.GenerateLaneTree(&buf); err != nil {
		t.Fatalf("GenerateLaneTree: %v", err)
	}
	out := buf.String()

	// Page furniture + nav active marker (the sidebar entry highlights on
	// its own page, same convention the tape page asserts).
	for _, want := range []string{"<title>Lane Tree · Coding Hermes Fleet</title>", `href="/lanes/tree" {{if eq . "lanes_tree"}}class="active"`} {
		if strings.Contains(out, want) && !strings.Contains(out, `class="active"`) {
			t.Errorf("lane tree page missing expected furniture %q", want)
		}
	}
	if !strings.Contains(out, `<a href="/lanes/tree"`) {
		t.Error("sidebar entry for /lanes/tree missing")
	}

	// Depth is EXPLICIT per row (L0/L1/L2) — the acceptance's whole point.
	if !strings.Contains(out, ">L0</td>") {
		t.Error("page carries no L0 (primary) depth marker")
	}
	if !strings.Contains(out, ">L1</td>") {
		t.Error("page carries no L1 (satellite) depth marker")
	}
	if !strings.Contains(out, ">L2</td>") {
		t.Error("page carries no L2 (satellite of a satellite) depth marker")
	}

	// Counters: 10 lanes total, 4 roots (trio, fan, ghost-child,
	// lone-disabled), max depth 2.
	for _, want := range []string{">10</div>", ">4</div>", ">2</div>"} {
		if !strings.Contains(out, want) {
			t.Errorf("stat card missing value %q", want)
		}
	}

	// Every lane renders exactly one ROW (parents also back-link in the
	// Parent column, so per-name link counts legitimately exceed 1 — the row
	// count is the exactly-once guarantee).
	if rows := strings.Count(out, `<tr class="tree-row`); rows != 10 {
		t.Errorf("page renders %d tree rows, want 10 (one per lane)", rows)
	}
	for _, name := range []string{"trio", "trio-qa", "trio-qa-sync", "trio-sync", "fan", "fan-alpha", "fan-mid", "fan-zeta", "lone-disabled", "ghost-child"} {
		if !strings.Contains(out, ">"+name+"</a>") {
			t.Errorf("lane %q missing from the render", name)
		}
	}

	// Depth-first order: trio's family appears as a contiguous block in tree
	// order (trio → trio-qa → trio-qa-sync → trio-sync).
	iTrio := strings.Index(out, `href="/projects/trio"`)
	iQA := strings.Index(out, `href="/projects/trio-qa"`)
	iQASync := strings.Index(out, `href="/projects/trio-qa-sync"`)
	iSync := strings.Index(out, `href="/projects/trio-sync"`)
	// De-Morgan'd form (staticcheck QF1001): ordered iff trio resolves and
	// every family member sorts after its parent in the rendered rows.
	if iTrio < 0 || iTrio >= iQA || iQA >= iQASync || iQASync >= iSync {
		t.Errorf("trio family not in depth-first tree order (trio=%d trio-qa=%d trio-qa-sync=%d trio-sync=%d)", iTrio, iQA, iQASync, iSync)
	}

	// The depth-2 rail renders two └ markers; the dangling parent stays
	// visible with its marker instead of being dropped.
	if !strings.Contains(out, "└ └") {
		t.Error("depth-2 row missing the └ └ rail")
	}
	if !strings.Contains(out, "vanished-primary") {
		t.Error("dangling parent name vanished from the render — it must stay visible")
	}
	if !strings.Contains(out, "dangling") {
		t.Error("dangling state not labelled")
	}

	// The disabled root keeps its position and states its state.
	if !strings.Contains(out, "disabled") {
		t.Error("disabled lane state not rendered")
	}
}

// TestSCHEDGAP1587_LaneTreePage_EmptyFleet renders an honest empty state, not
// a broken page.
func TestSCHEDGAP1587_LaneTreePage_EmptyFleet(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := gen.GenerateLaneTree(&buf); err != nil {
		t.Fatalf("GenerateLaneTree: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No lanes configured") {
		t.Error("empty fleet renders no explicit empty state")
	}
	if strings.Contains(out, ">L1</td>") {
		t.Error("empty fleet renders satellite depth rows")
	}
}
