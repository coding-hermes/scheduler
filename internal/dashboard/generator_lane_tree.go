package dashboard

import (
	"context"
	"io"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane tree page (SCHED-GAP-1587): /lanes/tree — the fleet's lane hierarchy
// rendered as a nested view from projects.parent (SCHED-GAP-1586, migration
// v42) through database.BuildLaneTree — the ONE shared resolver. This page
// shows the forest SHAPE (primaries with their satellites nested under
// them, arbitrary depth); the /queue and overview tables keep their own
// orderings and the ↳ in-place annotation (SCHED-GAP-1590) — a flat
// annotation and a real nested view serve different readings and neither
// replaces the other.
//
// Dangling parents (soft-deleted/purged lanes) surface at root level marked
// with their unresolvable parent name — the broken reference stays visible.

// laneTreePageData is the /lanes/tree template payload.
type laneTreePageData struct {
	Title    string
	Roots    []*laneTreeEntry
	LaneN    int
	RootN    int
	MaxDepth int
}

// laneTreeEntry is one rendered row of the nested view: a lane plus its
// recursively rendered children.
type laneTreeEntry struct {
	Name          string
	Parent        string
	ParentKnown   bool
	Enabled       bool
	AdmissionMode string
	Priority      int
	CooldownS     int
	Depth         int
	Children      []*laneTreeEntry
}

// Rail is the depth-indent rail for the Lane cell — one └ marker per level
// above the root ("" at depth 1). Text, not colour: legible without CSS,
// same convention as the queue's Rail (generator_lane_nesting.go).
func (e *laneTreeEntry) Rail() string {
	if e.Depth <= 1 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("└ ", e.Depth-1), " ")
}

// GenerateLaneTree renders the /lanes/tree page (SCHED-GAP-1587).
func (g *Generator) GenerateLaneTree(w io.Writer) error {
	data, err := g.laneTreeData(context.Background())
	if err != nil {
		return err
	}
	return g.laneTreeTmpl.Execute(w, data)
}

// laneTreeData reads every lane and resolves the forest. Disabled lanes
// stay in the tree at their real position (a satellite does not vanish
// because its lane is paused); they render with the disabled state.
func (g *Generator) laneTreeData(ctx context.Context) (laneTreePageData, error) {
	data := laneTreePageData{Title: "Lane Tree"}
	rows, err := g.db.QueryContext(ctx,
		`SELECT name, COALESCE(parent,''), COALESCE(enabled,1), COALESCE(admission_mode,''), COALESCE(priority,0), COALESCE(cooldown_s,0) FROM projects ORDER BY name ASC`)
	if err != nil {
		return data, err
	}
	defer func() { _ = rows.Close() }()

	var projects []database.Project
	for rows.Next() {
		var name, parent, admission string
		var enabled bool
		var priority, cooldown int
		if err := rows.Scan(&name, &parent, &enabled, &admission, &priority, &cooldown); err != nil {
			return data, err
		}
		projects = append(projects, database.Project{
			Name:          name,
			Parent:        parent,
			Enabled:       enabled,
			AdmissionMode: admission,
			Priority:      priority,
			CooldownS:     cooldown,
		})
	}
	if err := rows.Err(); err != nil {
		return data, err
	}
	tree := database.BuildLaneTree(projects)

	known := make(map[string]bool, len(projects))
	for _, p := range projects {
		known[p.Name] = true
	}
	var walk func(n *database.LaneNode, depth int) *laneTreeEntry
	walk = func(n *database.LaneNode, depth int) *laneTreeEntry {
		p := n.Project
		e := &laneTreeEntry{
			Name:          p.Name,
			Parent:        p.Parent,
			ParentKnown:   p.Parent == "" || known[p.Parent],
			Enabled:       p.Enabled,
			AdmissionMode: p.AdmissionMode,
			Priority:      p.Priority,
			CooldownS:     p.CooldownS,
			Depth:         depth,
		}
		for _, kid := range n.Children {
			e.Children = append(e.Children, walk(kid, depth+1))
		}
		return e
	}
	maxDepth := 0
	var maxWalk func(e *laneTreeEntry)
	maxWalk = func(e *laneTreeEntry) {
		if e.Depth > maxDepth {
			maxDepth = e.Depth
		}
		for _, kid := range e.Children {
			maxWalk(kid)
		}
	}
	for _, r := range tree.Roots {
		e := walk(r, 1)
		data.Roots = append(data.Roots, e)
		maxWalk(e)
	}
	data.LaneN = len(projects)
	data.RootN = len(tree.Roots)
	data.MaxDepth = maxDepth
	return data, nil
}
