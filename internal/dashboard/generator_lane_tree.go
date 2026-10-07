package dashboard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane tree page (SCHED-GAP-1587): /lanes/tree renders the fleet's lane
// hierarchy — primaries at level 0, satellites nested under their parents —
// with the DEPTH visible per row. The whole point of SCHED-GAP-1586's parent
// reference is seeing the stacked nesting at a glance; the flat list views
// (SCHED-GAP-1590) annotate satellites in place, this page draws the tree.
//
// Parenthood comes from projects.parent resolved through
// database.BuildLaneTree — the ONE shared resolver (same source as the API's
// /api/v1/lanes/tree and the list-view nesting), never a per-surface fork.
// Disabled lanes keep their position; a dangling parent (the named lane does
// not resolve — purged or renamed primary) renders at root level with the
// broken name kept visible, never dropped.

// laneTreeRow is one row of the /lanes/tree page: a lane plus its position in
// the resolved tree, pre-flattened depth-first so the template stays a plain
// table (legible as text, sortable by nothing — the order IS the tree).
type laneTreeRow struct {
	Name    string
	Parent  string
	Depth   int
	Enabled bool
	// Dangling marks a parent that does not resolve in the current snapshot —
	// the row renders the broken reference visibly (⚠ + "(dangling)") at root
	// level instead of hiding it.
	Dangling bool
	// Indent is the depth rail (one └ marker per level, "" at depth 0) —
	// text, not colour: nesting must survive without CSS.
	Indent string
}

// laneTreePageData is the /lanes/tree template's data.
type laneTreePageData struct {
	Title            string
	GeneratedAt      string
	Total            int
	RootCount        int
	MaxDepth         int
	Rows             []laneTreeRow
	FleetPaused      bool
	FleetPausedKnown bool
}

// GenerateLaneTree renders the nested lane-tree page (SCHED-GAP-1587).
func (g *Generator) GenerateLaneTree(w io.Writer) error {
	ctx := context.Background()
	lanes, err := database.ListProjects(ctx, g.db, false)
	if err != nil {
		return fmt.Errorf("load lanes: %w", err)
	}

	data := laneTreePageData{
		Title: "Lane Tree",
		Total: len(lanes),
	}
	data.GeneratedAt = g.clock().Now().UTC().Format("2006-01-02 15:04:05 MST")
	data.FleetPaused, data.FleetPausedKnown = g.globalPaused()

	known := make(map[string]bool, len(lanes))
	for _, p := range lanes {
		known[p.Name] = true
	}

	tree := database.BuildLaneTree(lanes)
	var walk func(node *database.LaneNode, depth int)
	walk = func(node *database.LaneNode, depth int) {
		p := node.Project
		dangling := p.Parent != "" && !known[p.Parent]
		if depth > data.MaxDepth {
			data.MaxDepth = depth
		}
		data.Rows = append(data.Rows, laneTreeRow{
			Name:     p.Name,
			Parent:   p.Parent,
			Depth:    depth,
			Enabled:  p.Enabled,
			Dangling: dangling,
			// One └ marker per level, trailing space trimmed at depth 1
			// ("└") — same vocabulary as laneNesting.Rail (SCHED-GAP-1590).
			Indent: strings.TrimSuffix(strings.Repeat("└ ", depth), " "),
		})
		for _, kid := range node.Children {
			walk(kid, depth+1)
		}
	}
	for _, root := range tree.Roots {
		data.RootCount++
		walk(root, 0)
	}

	return g.laneTreeTmpl.Execute(w, data)
}
