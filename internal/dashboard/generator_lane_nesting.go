package dashboard

import (
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane nesting in the LIST views (SCHED-GAP-1590).
//
// SORT-VS-NESTING DECISION — for /queue we chose GLOBAL URGENCY ORDER WITH
// ANNOTATION, not grouping-under-primaries. Reason: the SCHED-GAP-174 parity
// contract (TestQueueSurface_HTMLUrgencyEqualsAPIUrgency) pins the dashboard
// queue's row order to /api/v1/queue's array — one formula, one ordering, so
// an operator can never read two different "who runs next" answers. Grouping
// families while keeping urgency inside each family would fork the HTML
// ordering from the API's for every fleet that has satellites, with no
// matching change on the API surface (out of scope for this row). The cost is
// that a family can interleave with other lanes; the ↳ annotation naming the
// primary keeps each satellite's membership legible anyway. The fleet
// overview table (no parity constraint) additionally carries the same
// annotation, so the family reading is available there too.
//
// PARENTHOOD SOURCE: projects.parent (SCHED-GAP-1586, migration v42) is
// authoritative and depth is resolved through database.BuildLaneTree — the
// ONE shared resolver, not a fork. Name-suffix inference (-qa/-pm/-sync/
// -dogfood, the same vocabulary as the SCHED-GAP-180 cascade) is a FALLBACK
// applied only when a lane's parent column is empty. Every annotated row
// states which source produced it (data-parent-source="explicit" vs
// "inferred"). Dangling parents (soft-deleted/purged lanes) never panic:
// BuildLaneTree surfaces them as roots, and the row keeps the parent NAME as
// text so the broken link stays visible instead of silently disappearing.

// inferredParentSuffixes are the satellite suffixes the legacy heuristics
// recognized (same vocabulary as satelliteLaneSuffixes in
// internal/api/server_projects.go). Used ONLY when a lane's parent column is
// empty.
var inferredParentSuffixes = []string{"-qa", "-pm", "-sync", "-dogfood"}

// laneNesting is everything a list-view template needs to render one lane's
// position in the fleet hierarchy.
type laneNesting struct {
	// Depth is 0 for a primary/root lane, 1 for a direct satellite, 2 for a
	// satellite's satellite, etc. Position in the resolved tree, not a
	// property of the lane row itself.
	Depth int
	// Parent is the name of the lane this lane is attached to, when known.
	// Empty for roots. For a DANGLING parent (the named lane does not resolve
	// in the snapshot) the name is kept anyway — the annotation renders the
	// broken link as text rather than hiding it.
	Parent string
	// ParentKnown reports whether Parent resolved to a lane in the same
	// snapshot. Only a known parent grants depth > 0 and the (L<depth>)
	// suffix; a dangling parent renders as the top of its own fragment.
	ParentKnown bool
	// ParentSource states where the relation came from: "explicit" (the
	// projects.parent column) or "inferred" (name-suffix fallback). Empty for
	// roots.
	ParentSource string
}

// Rail returns the depth-indent rail for the Project cell — one └ marker per
// nesting level ("└ └" at depth 2). Text, not colour: the nesting must be
// legible without CSS.
func (n laneNesting) Rail() string {
	if n.Depth <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("└ ", n.Depth), " ")
}

// laneNestingIndex is the precomputed nesting view over one lane snapshot.
type laneNestingIndex struct {
	// parentNames maps every lane in the snapshot to its explicit parent
	// ('' = none). Doubles as the "does this lane exist" set.
	parentNames map[string]string
	// depths maps lane name → depth in the tree database.BuildLaneTree
	// resolves over the SAME snapshot (explicit edges only).
	depths map[string]int
}

// buildLaneNestingIndex resolves the nesting index for a lane snapshot.
// Only the Name and Parent fields of each lane are read.
func buildLaneNestingIndex(lanes []database.Project) *laneNestingIndex {
	parentNames := make(map[string]string, len(lanes))
	for _, p := range lanes {
		parentNames[p.Name] = p.Parent
	}
	return &laneNestingIndex{parentNames: parentNames, depths: laneDepthByName(lanes)}
}

// laneDepthByName walks the shared resolver's tree once and records the depth
// of every lane that hangs from a root. Lanes excluded by BuildLaneTree
// (data-level cycles) are simply absent from the map.
func laneDepthByName(lanes []database.Project) map[string]int {
	tree := database.BuildLaneTree(lanes)
	depths := make(map[string]int, len(lanes))
	var walk func(node *database.LaneNode, depth int)
	walk = func(node *database.LaneNode, depth int) {
		depths[node.Project.Name] = depth
		for _, child := range node.Children {
			walk(child, depth+1)
		}
	}
	for _, root := range tree.Roots {
		walk(root, 0)
	}
	return depths
}

// forLane computes one lane's nesting view. Order of precedence:
//
//  1. explicit parent set → authoritative; depth from the shared tree; a
//     parent that does not resolve in the snapshot dangles (named, depth 0).
//  2. explicit parent empty + recognized name suffix whose base lane exists
//     in the snapshot → legacy inference fallback (source "inferred").
//  3. otherwise → root.
func (idx *laneNestingIndex) forLane(name string) laneNesting {
	if parent, ok := idx.parentNames[name]; ok && parent != "" {
		if d, onTree := idx.depths[name]; onTree && d > 0 {
			return laneNesting{Depth: d, Parent: parent, ParentKnown: true, ParentSource: "explicit"}
		}
		// Dangling (or cycle-excluded) parent: keep the name visible, render
		// at root depth, without an (L…) depth claim.
		return laneNesting{Depth: 0, Parent: parent, ParentSource: "explicit"}
	}
	if base, _ := matchSatelliteSuffix(name); base != "" {
		if _, exists := idx.parentNames[base]; exists {
			// Inferred edges are deliberately NOT fed into BuildLaneTree
			// (the shared tree resolves explicit edges only), so the depth
			// comes from the suffix chain itself.
			return laneNesting{Depth: suffixChainDepth(name), Parent: base, ParentKnown: true, ParentSource: "inferred"}
		}
	}
	return laneNesting{Depth: 0}
}

// matchSatelliteSuffix returns the inferred base name when the lane name ends
// with a recognized satellite suffix. base == "" when nothing matches.
func matchSatelliteSuffix(name string) (base string, suffixLen int) {
	for _, suffix := range inferredParentSuffixes {
		if n := len(suffix); len(name) > n && strings.HasSuffix(name, suffix) {
			return name[:len(name)-n], n
		}
	}
	return "", 0
}

// suffixChainDepth counts the satellite-suffix hops in a name:
// "proj-qa-sync" → base "proj-qa" (hop 1) → base "proj" (hop 2) → depth 2.
// A name without a satellite suffix is depth 0.
func suffixChainDepth(name string) int {
	depth := 0
	cur := name
	for {
		base, _ := matchSatelliteSuffix(cur)
		if base == "" {
			return depth
		}
		depth++
		cur = base
	}
}

// annotateQueueNesting attaches each entry's laneNesting in place. lanes must
// be the same snapshot the entries were built from (name+parent suffice).
func annotateQueueNesting(entries []QueueEntry, lanes []database.Project) {
	idx := buildLaneNestingIndex(lanes)
	for i := range entries {
		entries[i].Nesting = idx.forLane(entries[i].Name)
	}
}

// annotateFleetNesting is annotateQueueNesting for the fleet overview rows.
func annotateFleetNesting(rows []FleetRow, lanes []database.Project) {
	idx := buildLaneNestingIndex(lanes)
	for i := range rows {
		rows[i].Nesting = idx.forLane(rows[i].Name)
	}
}
