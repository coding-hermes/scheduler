package api_test

import (
	"testing"
)

// SCHED-GAP-1587 — GET /api/v1/lanes/tree: hierarchical lane topology.
//
// The tree is resolved from projects.parent (SCHED-GAP-1586, migration v42)
// through database.BuildLaneTree — the ONE shared resolver — and serves the
// whole forest in one response: roots with nested children (lane name ASC at
// every level), lane/root counts and max depth. The flat /api/v1/projects
// list stays for compatibility but is deprecated as a topology source.
//
// Contract covered here:
//   - nesting: parent-set lanes hang under their primary, arbitrary depth;
//   - non-null children on every node (leaves carry "children": []);
//   - counts agree: lane_count over roots' subtrees == lanes created;
//   - disabled lanes keep their real position in the tree;
//   - a dangling parent (purged lane name) surfaces as a root-level orphan
//     with parent_known=false — the broken reference stays visible;
//   - the flat list remains reachable (compat), unchanged in shape.

// laneTreeCount sums a node and its descendants.
func laneTreeCount(n map[string]interface{}) int {
	total := 1
	if kids, ok := n["children"].([]interface{}); ok {
		for _, k := range kids {
			total += laneTreeCount(k.(map[string]interface{}))
		}
	}
	return total
}

// TestSCHEDGAP1587_LanesTree covers the nesting, counts, non-null children
// and the compat guarantee of the flat list.
func TestSCHEDGAP1587_LanesTree(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "1587-primary")
	mustCreateAPITestProject(t, a.db, "1587-sat")
	mustCreateAPITestProject(t, a.db, "1587-sub")
	mustCreateAPITestProject(t, a.db, "1587-lonely")

	// Chain: primary ← sat ← sub (two levels of nesting); lonely stays a root.
	for _, set := range []struct{ child, parent string }{
		{"1587-sat", "1587-primary"},
		{"1587-sub", "1587-sat"},
	} {
		code, body := a.do(t, "PUT", "/api/v1/projects/"+set.child, map[string]interface{}{"parent": set.parent})
		if code != 200 {
			t.Fatalf("PUT parent %s→%s: status %d body %v", set.child, set.parent, code, body)
		}
	}

	// Disable one lane: it must STAY in the tree at its position.
	code, body := a.do(t, "PUT", "/api/v1/projects/1587-sub", map[string]interface{}{"enabled": false})
	if code != 200 {
		t.Fatalf("disable 1587-sub: status %d body %v", code, body)
	}

	code, body = a.do(t, "GET", "/api/v1/lanes/tree", nil)
	if code != 200 {
		t.Fatalf("GET /api/v1/lanes/tree: status %d body %v", code, body)
	}
	roots, ok := body["roots"].([]interface{})
	if !ok {
		t.Fatalf("no roots array in body: %v", body)
	}
	if len(roots) != 2 {
		t.Fatalf("roots = %d, want 2 (primary + lonely)", len(roots))
	}

	byName := map[string]map[string]interface{}{}
	for _, r := range roots {
		node := r.(map[string]interface{})
		byName[node["name"].(string)] = node
	}
	primary, ok := byName["1587-primary"]
	if !ok {
		t.Fatalf("1587-primary not among roots: %v", body)
	}
	if primary["parent_known"] != true {
		t.Errorf("primary parent_known = %v, want true", primary["parent_known"])
	}
	// Nesting: primary → sat → sub.
	kids, _ := primary["children"].([]interface{})
	if len(kids) != 1 {
		t.Fatalf("primary children = %d, want 1 (sat)", len(kids))
	}
	sat := kids[0].(map[string]interface{})
	if sat["name"] != "1587-sat" {
		t.Fatalf("primary child = %v, want 1587-sat", sat["name"])
	}
	satKids, _ := sat["children"].([]interface{})
	if len(satKids) != 1 || satKids[0].(map[string]interface{})["name"] != "1587-sub" {
		t.Fatalf("sat children = %v, want [1587-sub]", satKids)
	}
	sub := satKids[0].(map[string]interface{})
	// Non-null children on a leaf + disabled lane keeps its position.
	if kids2, ok := sub["children"].([]interface{}); !ok || kids2 == nil {
		t.Errorf("leaf children = %v, want non-null []", sub["children"])
	}
	if sub["enabled"] != false {
		t.Errorf("disabled lane enabled = %v, want false", sub["enabled"])
	}
	if sub["parent"] != "1587-sat" {
		t.Errorf("sub parent = %v, want 1587-sat", sub["parent"])
	}

	// Counts: lane_count over the whole forest equals the 4 created lanes.
	total := 0
	for _, r := range roots {
		total += laneTreeCount(r.(map[string]interface{}))
	}
	if body["lane_count"].(float64) != 4 || total != 4 {
		t.Errorf("lane_count %v / tree count %d, want 4/4", body["lane_count"], total)
	}
	if body["max_depth"].(float64) != 3 {
		t.Errorf("max_depth %v, want 3", body["max_depth"])
	}

	// Compat: the flat list still answers with the pagination envelope.
	code, body = a.do(t, "GET", "/api/v1/projects", nil)
	if code != 200 {
		t.Fatalf("flat list: status %d", code)
	}
	if _, ok := body["projects"].([]interface{}); !ok {
		t.Errorf("flat list projects = %v, want array (compat)", body["projects"])
	}
}

// TestSCHEDGAP1587_LanesTreeDanglingParent: a lane whose parent names a
// purged lane surfaces as a root-level orphan with parent_known=false.
func TestSCHEDGAP1587_LanesTreeDanglingParent(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "1587-orphan")
	code, body := a.do(t, "PUT", "/api/v1/projects/1587-orphan", map[string]interface{}{"parent": "1587-purged-lane"})
	if code != 200 {
		t.Fatalf("dangling PUT: status %d body %v", code, body)
	}
	code, body = a.do(t, "GET", "/api/v1/lanes/tree", nil)
	if code != 200 {
		t.Fatalf("GET tree: status %d", code)
	}
	roots := body["roots"].([]interface{})
	if len(roots) != 1 {
		t.Fatalf("roots = %d, want 1 (dangling orphan surfaces at root)", len(roots))
	}
	node := roots[0].(map[string]interface{})
	if node["parent_known"] != false {
		t.Errorf("orphan parent_known = %v, want false", node["parent_known"])
	}
	if node["parent"] != "1587-purged-lane" {
		t.Errorf("orphan parent = %v, want 1587-purged-lane (kept visible)", node["parent"])
	}
}

// TestSCHEDGAP1587_LanesTreeEmpty: an empty fleet answers 200 with an empty
// roots array (non-null), zero counts.
func TestSCHEDGAP1587_LanesTreeEmpty(t *testing.T) {
	a := newAPITestServer(t)
	code, body := a.do(t, "GET", "/api/v1/lanes/tree", nil)
	if code != 200 {
		t.Fatalf("GET tree: status %d", code)
	}
	roots, ok := body["roots"].([]interface{})
	if !ok || roots == nil {
		t.Fatalf("roots = %v, want non-null []", body["roots"])
	}
	if len(roots) != 0 || body["lane_count"].(float64) != 0 {
		t.Errorf("empty fleet: roots %d lane_count %v, want 0/0", len(roots), body["lane_count"])
	}
}
