package dashboard_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1587 — /lanes/tree dashboard page: the lane hierarchy rendered
// nested from projects.parent through the shared BuildLaneTree resolver.

// TestLaneTreePageRendersNesting seeds primary ← sat ← sub and asserts the
// rendered page nests them (rail markers on satellites, all lanes present,
// dangling parents marked missing, disabled lanes still listed).
func TestLaneTreePageRendersNesting(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db, nil)

	seedLane := func(t *testing.T, name, parent string, enabled bool) {
		t.Helper()
		if err := database.CreateProject(t.Context(), db, &database.Project{
			Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
			Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
			Model: "test", Provider: "test", Enabled: enabled, Parent: parent,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	seedLane(t, "1587-primary", "", true)
	seedLane(t, "1587-sat", "1587-primary", true)
	seedLane(t, "1587-sub", "1587-sat", false) // disabled but positioned
	seedLane(t, "1587-orphan", "1587-purged", true)

	var out bytes.Buffer
	if err := g.GenerateLaneTree(&out); err != nil {
		t.Fatalf("GenerateLaneTree: %v", err)
	}
	page := out.String()
	for _, want := range []string{"Lane Tree", "1587-primary", "1587-sat", "1587-sub", "1587-orphan"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Satellites carry the └ rail.
	if !strings.Contains(page, "└") {
		t.Error("no nesting rail rendered")
	}
	// Dangling parent named as missing, not silently dropped.
	if !strings.Contains(page, "missing") || !strings.Contains(page, "1587-purged") {
		t.Error("dangling parent not rendered as missing")
	}
	// Disabled lane still listed (position kept).
	if !strings.Contains(page, ">no<") {
		t.Error("disabled lane state not rendered")
	}
}
