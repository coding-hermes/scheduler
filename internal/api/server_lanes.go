package api

import (
	"net/http"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane tree views (SCHED-GAP-1587).
//
// The `projects` table is a LANES list — a PROJECT is one primary foreman lane
// plus its satellites (the standing vocabulary ruling). SCHED-GAP-1586 gave a
// lane an explicit `parent` reference; this file surfaces the resolved
// hierarchy that reference implies:
//
//   - GET /api/v1/lanes/tree — the hierarchical view: roots plus nested
//     children with per-node depth, resolved through the ONE shared resolver
//     (database.BuildLaneTree — the same implementation the SCHED-GAP-1590
//     list-view nesting consumes; never a per-surface fork).
//   - GET /api/v1/lanes — the flat listing, kept for backward compatibility
//     but DEPRECATED in favour of the tree (docs/api.md §5). Consumers that
//     walk parents client-side should move to the tree.
//
// Both are read-only, unauthenticated (the read surface is deliberately open —
// see the auth note in server.go's Handler), and GET-only like their siblings.

// laneTreeResponse is the GET /api/v1/lanes/tree envelope. Roots and every
// Children slice are name-ASC ordered by the resolver, so the wire shape is
// deterministic regardless of row order.
type laneTreeResponse struct {
	Roots []*laneTreeNode `json:"roots"`
	// Total counts every lane in the snapshot (enabled AND disabled) — every
	// lane appears exactly once in the tree (roots + children), including
	// dangling-parent orphans that surface at root level.
	Total int `json:"total"`
}

// laneTreeNode is one lane in the tree response. Depth is the node's position
// in the resolved tree (root = 0, each child = parent + 1) — the nesting level
// the operator asked to SEE, not a property of the lane row itself. Parent is
// the declared parent name ("" = primary/root); a DANGLING parent (the named
// lane does not resolve) keeps its name visible on the node and the lane
// surfaces as a root, so a broken reference is legible instead of silently
// dropped.
type laneTreeNode struct {
	Name     string          `json:"name"`
	Parent   string          `json:"parent"`
	Depth    int             `json:"depth"`
	IsRoot   bool            `json:"is_root"`
	Enabled  bool            `json:"enabled"`
	Children []*laneTreeNode `json:"children"`
}

// handleLanesTree answers GET /api/v1/lanes/tree (SCHED-GAP-1587 acceptance 1):
// the fleet's lane hierarchy (parent → children) from one
// database.ListProjects(false) read — disabled lanes keep their position in
// the tree, matching the resolver's documented contract.
func (s *Server) handleLanesTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	// Read-heavy surfaces run under the per-request deadline (SCHED-GAP-1575-B
	// convention); this is a single indexed list read, far under budget.
	ctx, obs := s.newRequestDeadline(r.Context(), "lanes_tree", s.readTimeout())
	defer obs.finish()
	obs.enter("ListProjects")
	lanes, err := database.ListProjects(ctx, s.db, false)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	resp := laneTreeResponse{Roots: []*laneTreeNode{}, Total: len(lanes)}
	if len(lanes) == 0 {
		writeJSON(w, 200, resp)
		return
	}

	tree := database.BuildLaneTree(lanes)
	// Declared before assignment: a recursive closure cannot reference itself
	// inside its own := initializer (undefined: convert at compile time).
	var convert func(node *database.LaneNode, depth int) *laneTreeNode
	convert = func(node *database.LaneNode, depth int) *laneTreeNode {
		out := &laneTreeNode{
			Name:     node.Project.Name,
			Parent:   node.Project.Parent,
			Depth:    depth,
			Enabled:  node.Project.Enabled,
			Children: []*laneTreeNode{},
		}
		for _, kid := range node.Children {
			out.Children = append(out.Children, convert(kid, depth+1))
		}
		return out
	}
	for _, root := range tree.Roots {
		node := convert(root, 0)
		node.IsRoot = true
		resp.Roots = append(resp.Roots, node)
	}
	writeJSON(w, 200, resp)
}

// handleLanes answers GET /api/v1/lanes — the flat lane listing, DEPRECATED in
// favour of /api/v1/lanes/tree (SCHED-GAP-1587 acceptance 3). Kept for
// backward compatibility: the row shape is the full project row (the flat
// envelope pre-dates the tree; consumers that page parents client-side still
// work unchanged). The deprecation lives in docs/api.md §5 and the OpenAPI
// description; the endpoint itself stays plain 200 JSON.
func (s *Server) handleLanes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx, obs := s.newRequestDeadline(r.Context(), "lanes_flat", s.readTimeout())
	defer obs.finish()
	obs.enter("ListProjects")
	lanes, err := database.ListProjects(ctx, s.db, false)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Always non-nil so the JSON carries "lanes": [] rather than null.
	out := lanes
	if out == nil {
		out = []database.Project{}
	}
	writeJSON(w, 200, struct {
		Lanes []database.Project `json:"lanes"`
		Total int                `json:"total"`
	}{Lanes: out, Total: len(out)})
}
