package dashboard_test

// SCHED-GAP-1700 acceptance 5: a lane's parent/child references from the lane
// link map are VISIBLE on its dashboard page, each endpoint linkable.
//
// The page is the operator's first stop when asking "what is this lane
// related to", so the section must render on a real lane without any extra
// plumbing: parents and children from lane_links, cross-namespace edges
// marked, and each endpoint a link to its own lane page. An unlinked lane
// renders the honest "none recorded" line rather than an empty table.

import (
	"context"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

func TestSCHEDGAP1700_ProjectDetailRendersLaneLinks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateProject(t, db, "rel-primary", 10, 5)
	mustCreateProject(t, db, "rel-satellite", 10, 5)
	mustCreateProject(t, db, "rel-other", 10, 5)

	// Namespaces so one edge is genuinely cross-namespace.
	for _, id := range []string{"rel-ns-a", "rel-ns-b"} {
		if err := database.CreateNamespace(ctx, db, &database.Namespace{
			ID: id, Weight: 10, Enabled: true, Description: "SCHED-GAP-1700 fixture",
		}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", id, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE projects SET namespace_id='rel-ns-a' WHERE name='rel-primary'`); err != nil {
		t.Fatalf("bind primary: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE projects SET namespace_id='rel-ns-b' WHERE name='rel-satellite'`); err != nil {
		t.Fatalf("bind satellite: %v", err)
	}

	// primary is a CHILD of other and a PARENT of satellite — the section must
	// show one row in each direction for the same lane (many-to-many shape).
	if _, err := database.CreateLaneLink(ctx, db, "rel-other", "rel-primary", "upstream reference", "test"); err != nil {
		t.Fatalf("create other→primary: %v", err)
	}
	if _, err := database.CreateLaneLink(ctx, db, "rel-primary", "rel-satellite", "downstream reference", "test"); err != nil {
		t.Fatalf("create primary→satellite: %v", err)
	}

	gen := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := gen.GenerateProjectDetail(&buf, "rel-primary"); err != nil {
		t.Fatalf("GenerateProjectDetail: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Lane Relationships") {
		t.Fatalf("lane page has no Lane Relationships section")
	}
	// Parent row: the parent lane rendered as a link to its own page.
	if !strings.Contains(out, `href="/projects/rel-other"`) {
		t.Errorf("parent lane rel-other is not linked on the page")
	}
	// Child row.
	if !strings.Contains(out, `href="/projects/rel-satellite"`) {
		t.Errorf("child lane rel-satellite is not linked on the page")
	}
	// The directional badges distinguish the two rows.
	if !strings.Contains(out, `>parent<`) || !strings.Contains(out, `>child<`) {
		t.Errorf("relationship direction badges missing (want both parent and child rows)")
	}
	// The cross-namespace edge is marked, and the same-namespace note text is
	// present verbatim.
	if !strings.Contains(out, "cross") {
		t.Errorf("cross-namespace marker missing from the relationships table")
	}
	if !strings.Contains(out, "upstream reference") || !strings.Contains(out, "downstream reference") {
		t.Errorf("link notes are not rendered")
	}
	// The page must state that these links never affect scheduling (the
	// operator's most important reading of this section).
	if !strings.Contains(out, "never alter this lane's scheduling") {
		t.Errorf("the reference-only caveat is not rendered")
	}
}

func TestSCHEDGAP1700_ProjectDetailNoLinksIsHonest(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "rel-lonely", 10, 5)

	gen := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := gen.GenerateProjectDetail(&buf, "rel-lonely"); err != nil {
		t.Fatalf("GenerateProjectDetail: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No parent or child references recorded for this lane.") {
		t.Errorf("an unlinked lane must render the honest empty line, got: %s", out)
	}
	if strings.Contains(out, "Lane Relationships</h2>\n<div class=\"table-wrap\">") {
		t.Errorf("an unlinked lane must not render an empty relationships table")
	}
}
