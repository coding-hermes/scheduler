package api

import (
	"net/http"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane tree views (SCHED-GAP-1587): a hierarchical read surface over the
// lane forest resolved from projects.parent (SCHED-GAP-1586, migration v42)
// through database.BuildLaneTree — the ONE shared resolver, never a fork.
//
// TOPOLOGY MIGRATION NOTE: the flat /api/v1/projects list predates the
// explicit parent column and reports lanes without their family structure
// (the pre-SCHED-GAP-1586 surface inferred satellites from name suffixes —
// the heuristic this row retires). It is retained unchanged for
// compatibility and marked DEPRECATED for topology consumers in
// docs/reference/endpoints.md + the OpenAPI description: the tree endpoint
// is the authoritative shape for hierarchy-aware clients. The flat list
// itself is NOT removed and remains the pagination/lifecycle surface
// (POST/PUT/DELETE live only there).

// laneTreeNode is the JSON shape of one lane in the tree response. The
// project fields are a projection (name, parent, enabled, admission_mode,
// cadence, timestamps) rather than the full row: the tree is a topology
// view, and the flat list remains the enrichment surface.
type laneTreeNode struct {
	Name          string          `json:"name"`
	Parent        string          `json:"parent"`
	ParentKnown   bool            `json:"parent_known"`
	Enabled       bool            `json:"enabled"`
	AdmissionMode string          `json:"admission_mode,omitempty"`
	Priority      int             `json:"priority"`
	CooldownS     int             `json:"cooldown_s"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	Children      []*laneTreeNode `json:"children"`
}

// laneTreeResponse is the GET /api/v1/lanes/tree body: the forest roots plus
// the flat lane count so a client can verify it received every lane.
type laneTreeResponse struct {
	Roots       []*laneTreeNode `json:"roots"`
	LaneCount   int             `json:"lane_count"`
	RootCount   int             `json:"root_count"`
	MaxDepth    int             `json:"max_depth"`
	GeneratedAt string          `json:"generated_at"`
}

// newLaneTreeNode projects one database.Project onto the wire shape.
func newLaneTreeNode(p database.Project) *laneTreeNode {
	return &laneTreeNode{
		Name:          p.Name,
		Parent:        p.Parent,
		ParentKnown:   false, // set during hang, see laneTreeJSON
		Enabled:       p.Enabled,
		AdmissionMode: p.AdmissionMode,
		Priority:      p.Priority,
		CooldownS:     p.CooldownS,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		// Children is always non-nil so the JSON carries "children": []
		// rather than null for leaves (same contract as the projects list).
		Children: []*laneTreeNode{},
	}
}

// laneTreeJSON converts a resolved LaneTree into the wire shape, marking
// every attached child parent_known=true (its parent resolved in this
// snapshot). Dangling-parent orphans keep parent_known=false and render at
// the root level — the broken reference stays visible instead of hiding.
func laneTreeJSON(tree *database.LaneTree) []*laneTreeNode {
	var convert func(n *database.LaneNode, known bool) *laneTreeNode
	convert = func(n *database.LaneNode, known bool) *laneTreeNode {
		node := newLaneTreeNode(n.Project)
		node.ParentKnown = known
		for _, kid := range n.Children {
			node.Children = append(node.Children, convert(kid, true))
		}
		return node
	}
	roots := make([]*laneTreeNode, 0, len(tree.Roots))
	for _, r := range tree.Roots {
		// A root with an empty parent is a genuine primary (known=true,
		// parent ""); a root with a non-empty parent is a dangling orphan.
		roots = append(roots, convert(r, r.Project.Parent == ""))
	}
	return roots
}

// maxTreeDepth computes the depth of the forest (roots = 1), capped at 1000
// to stay safe against a corrupted resolver output (defensive only —
// BuildLaneTree excludes data-level cycles by construction).
func maxTreeDepth(roots []*laneTreeNode) int {
	const cap = 1000
	var depth func(n *laneTreeNode, d int) int
	depth = func(n *laneTreeNode, d int) int {
		if d > cap {
			return cap
		}
		max := d
		for _, kid := range n.Children {
			if v := depth(kid, d+1); v > max {
				max = v
			}
		}
		return max
	}
	max := 0
	for _, r := range roots {
		if v := depth(r, 1); v > max {
			max = v
		}
	}
	return max
}

// handleLanesTree serves GET /api/v1/lanes/tree (SCHED-GAP-1587): the whole
// lane forest in one response — hierarchical topology, not paginated. The
// read runs under the request-scoped deadline like the other heavy reads;
// a stalled DB answers 504 naming listProjects instead of hanging.
func (s *Server) handleLanesTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx, obs := s.newRequestDeadline(r.Context(), "lanes_tree", s.readTimeout())
	defer obs.finish()
	obs.enter("ListProjectsPage")
	page, err := database.ListProjectsPage(ctx, s.db, false, database.ListProjectsPageOpts(maxListProjectsLimit, 0))
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	tree := database.BuildLaneTree(page.Projects)
	roots := laneTreeJSON(tree)
	resp := laneTreeResponse{
		Roots:       roots,
		LaneCount:   len(page.Projects),
		RootCount:   len(roots),
		MaxDepth:    maxTreeDepth(roots),
		GeneratedAt: s.clock().Now().UTC().Format(time.RFC3339),
	}
	writeJSON(w, 200, resp)
}
