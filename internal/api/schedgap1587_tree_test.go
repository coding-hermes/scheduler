package api_test

// SCHED-GAP-1587 — lane tree views: GET /api/v1/lanes/tree returns the fleet's
// lane hierarchy (parent → children) resolved through the ONE shared resolver,
// database.BuildLaneTree (SCHED-GAP-1586) — never a per-surface fork. The flat
// GET /api/v1/lanes listing is kept for backward compatibility but is
// deprecated in favour of the tree (docs/api.md §5).

import (
	"context"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

type laneSpec struct{ name, parent string }

// node1587 walks a decoded tree node map.
type node1587 struct {
	name     string
	parent   string
	depth    int
	isRoot   bool
	enabled  bool
	children []node1587
}

func decode1587Node(t *testing.T, raw interface{}) node1587 {
	t.Helper()
	m, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("tree node is not an object: %T (%v)", raw, raw)
	}
	n := node1587{}
	if v, ok := m["name"].(string); ok {
		n.name = v
	}
	if v, ok := m["parent"].(string); ok {
		n.parent = v
	}
	if v, ok := m["depth"].(float64); ok {
		n.depth = int(v)
	}
	if v, ok := m["is_root"].(bool); ok {
		n.isRoot = v
	}
	if v, ok := m["enabled"].(bool); ok {
		n.enabled = v
	}
	if kids, ok := m["children"].([]interface{}); ok {
		for _, k := range kids {
			n.children = append(n.children, decode1587Node(t, k))
		}
	}
	return n
}

func find1587(nodes []node1587, name string) *node1587 {
	for i := range nodes {
		if nodes[i].name == name {
			return &nodes[i]
		}
	}
	return nil
}

// TestSCHEDGAP1587_TreeEndpoint_Hierarchy seeds a 3-level fleet shape and
// asserts the endpoint returns roots and nested children resolved through
// BuildLaneTree semantics: name-ASC ordering at every level, explicit depths,
// disabled lanes included, dangling parents surfaced as roots.
func TestSCHEDGAP1587_TreeEndpoint_Hierarchy(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()

	lanes := []laneSpec{
		{"trio", ""},
		{"trio-qa", "trio"},
		{"trio-qa-sync", "trio-qa"},
		{"trio-sync", "trio"},
		{"fan", ""},
		{"fan-zeta", "fan"},
		{"fan-mid", "fan"},
		{"fan-alpha", "fan"},
		{"lone-disabled", ""},
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
		if err := database.CreateProject(ctx, a.db, p); err != nil {
			t.Fatalf("create %s: %v", l.name, err)
		}
	}
	if err := database.UpdateProject(ctx, a.db, "lone-disabled", database.ProjectUpdates{Enabled: database.BoolPtr(false)}); err != nil {
		t.Fatalf("disable lone-disabled: %v", err)
	}
	// Dangling: parent names a lane that never resolves in this snapshot.
	if err := database.CreateProject(ctx, a.db, &database.Project{
		Name: "ghost-child", RepoURL: "https://example.com/ghost", Workdir: "/tmp/ghost",
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
		Model: "test", Provider: "test", Enabled: true, Parent: "vanished-primary",
	}); err != nil {
		t.Fatalf("create ghost-child: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/lanes/tree", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}

	rawRoots, ok := body["roots"].([]interface{})
	if !ok {
		t.Fatalf("roots missing or not an array: %v", body)
	}
	total, ok := body["total"].(float64)
	if !ok {
		t.Fatalf("total missing or not a number: %v", body)
	}
	if int(total) != 10 {
		t.Errorf("total = %v, want 10 (every lane appears exactly once)", total)
	}

	roots := make([]node1587, 0, len(rawRoots))
	for _, r := range rawRoots {
		n := decode1587Node(t, r)
		if !n.isRoot {
			t.Errorf("root %q carries is_root=false", n.name)
		}
		if n.depth != 0 {
			t.Errorf("root %q carries depth=%d, want 0", n.name, n.depth)
		}
		roots = append(roots, n)
	}

	// Roots are name-ASC: fan, ghost-child, lone-disabled, trio.
	if len(roots) != 4 {
		t.Fatalf("got %d roots (%v), want 4", len(roots), rootNames1587(roots))
	}
	wantRootOrder := []string{"fan", "ghost-child", "lone-disabled", "trio"}
	for i, w := range wantRootOrder {
		if roots[i].name != w {
			t.Errorf("root[%d] = %q, want %q (name-ASC)", i, roots[i].name, w)
		}
	}

	// trio: two children, name-ASC, depths 1.
	trio := find1587(roots, "trio")
	if trio == nil {
		t.Fatal("trio root missing")
	}
	if len(trio.children) != 2 || trio.children[0].name != "trio-qa" || trio.children[1].name != "trio-sync" {
		t.Fatalf("trio children = %v, want [trio-qa trio-sync] name-ASC", childNames1587(trio.children))
	}
	for _, c := range trio.children {
		if c.depth != 1 {
			t.Errorf("child %q depth = %d, want 1", c.name, c.depth)
		}
		if c.parent != "trio" {
			t.Errorf("child %q parent = %q, want trio", c.name, c.parent)
		}
		if c.isRoot {
			t.Errorf("child %q carries is_root=true", c.name)
		}
	}

	// Depth-2 chain survives nesting: trio-qa → trio-qa-sync.
	qa := find1587(trio.children, "trio-qa")
	if qa == nil {
		t.Fatal("trio-qa missing under trio")
	}
	if len(qa.children) != 1 || qa.children[0].name != "trio-qa-sync" {
		t.Fatalf("trio-qa children = %v, want [trio-qa-sync]", childNames1587(qa.children))
	}
	if qa.children[0].depth != 2 {
		t.Errorf("trio-qa-sync depth = %d, want 2 (satellite of a satellite)", qa.children[0].depth)
	}

	// fan's children were created out of alpha order — the resolver re-sorts.
	fan := find1587(roots, "fan")
	if fan == nil {
		t.Fatal("fan root missing")
	}
	gotFan := childNames1587(fan.children)
	if len(gotFan) != 3 || gotFan[0] != "fan-alpha" || gotFan[1] != "fan-mid" || gotFan[2] != "fan-zeta" {
		t.Errorf("fan children = %v, want name-ASC [fan-alpha fan-mid fan-zeta]", gotFan)
	}

	// Dangling parent: ghost-child surfaces as a ROOT (never dropped), the
	// unresolvable parent name stays visible on the node.
	ghost := find1587(roots, "ghost-child")
	if ghost == nil {
		t.Fatal("ghost-child must surface as a root when its parent dangles")
	}
	if ghost.parent != "vanished-primary" {
		t.Errorf("ghost-child parent = %q, want vanished-primary (dangling name kept visible)", ghost.parent)
	}
	if len(ghost.children) != 0 {
		t.Errorf("ghost-child children = %v, want none", childNames1587(ghost.children))
	}

	// Disabled lanes keep their position in the tree (enabledOnly=false read).
	lone := find1587(roots, "lone-disabled")
	if lone == nil {
		t.Fatal("lone-disabled must stay in the tree")
	}
	if lone.enabled {
		t.Error("lone-disabled carries enabled=true, want false")
	}
}

// TestSCHEDGAP1587_TreeEndpoint_EmptyFleet answers an honest empty tree —
// never null, never an error.
func TestSCHEDGAP1587_TreeEndpoint_EmptyFleet(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/lanes/tree", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	roots, ok := body["roots"].([]interface{})
	if !ok {
		t.Fatalf("roots missing or not an array: %v", body)
	}
	if len(roots) != 0 {
		t.Errorf("empty fleet: roots = %v, want []", roots)
	}
	if total, _ := body["total"].(float64); int(total) != 0 {
		t.Errorf("empty fleet: total = %v, want 0", body["total"])
	}
}

// TestSCHEDGAP1587_TreeEndpoint_405 pins the method gate: the endpoint is
// read-only, wrong methods answer 405 like every other read surface.
func TestSCHEDGAP1587_TreeEndpoint_405(t *testing.T) {
	a := newAPITestServer(t)
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		status, _ := a.do(t, method, "/api/v1/lanes/tree", nil)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/lanes/tree status = %d, want 405", method, status)
		}
	}
}

// TestSCHEDGAP1587_FlatLanes_BackwardCompat keeps the flat listing alive
// (acceptance 3): same lane set as the tree's total, full project rows,
// still a plain unpaginated envelope. It is deprecated in the docs in favour
// of /api/v1/lanes/tree.
func TestSCHEDGAP1587_FlatLanes_BackwardCompat(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()
	for _, s := range []laneSpec{{"flat-a", ""}, {"flat-b", "flat-a"}} {
		p := &database.Project{
			Name: s.name, RepoURL: "https://example.com/" + s.name, Workdir: "/tmp/" + s.name,
			Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
			Model: "test", Provider: "test", Enabled: true,
		}
		if s.parent != "" {
			p.Parent = s.parent
		}
		if err := database.CreateProject(ctx, a.db, p); err != nil {
			t.Fatalf("create %s: %v", s.name, err)
		}
	}

	status, body := a.do(t, "GET", "/api/v1/lanes", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	lanes, ok := body["lanes"].([]interface{})
	if !ok {
		t.Fatalf("lanes missing or not an array: %v", body)
	}
	if len(lanes) != 2 {
		t.Fatalf("lanes = %d rows, want 2", len(lanes))
	}
	first, ok := lanes[0].(map[string]interface{})
	if !ok {
		t.Fatalf("lane row is not an object: %T", lanes[0])
	}
	// Full project rows: the flat listing stays a drop-in for consumers that
	// read the row shape — parent included (SCHED-GAP-1586 column).
	if first["name"] != "flat-a" {
		t.Errorf("lanes[0].name = %v, want flat-a (name-ASC)", first["name"])
	}
	if first["parent"] != "" {
		t.Errorf("lanes[0].parent = %v, want '' (primary)", first["parent"])
	}
	if total, _ := body["total"].(float64); int(total) != 2 {
		t.Errorf("total = %v, want 2", body["total"])
	}

	// The flat surface is GET-only like its siblings.
	for _, method := range []string{"POST", "DELETE"} {
		if status, _ := a.do(t, method, "/api/v1/lanes", nil); status != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/lanes status = %d, want 405", method, status)
		}
	}
}

func rootNames1587(nodes []node1587) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.name)
	}
	return out
}

func childNames1587(nodes []node1587) []string {
	return rootNames1587(nodes)
}
