package database

// SCHED-GAP-1700 — the lane relationship map at the STORAGE layer.
//
// What these tests pin, in acceptance order:
//
//  1. MANY-TO-MANY: a lane carries several parents AND several children, and
//     either endpoint may sit in a different namespace; every edge round-trips
//     through the storage API with both namespaces reported.
//  2. The three lookup questions are answerable from this layer in one call
//     each — parents, children, and "is Y an ancestor of X" — with no
//     name-suffix inference anywhere.
//  3. A link write leaves every scheduling field of every lane byte-
//     identical (admission_mode, cooldown_s, cooldown pins, weight, priority,
//     enabled …): the map is reference data, not scheduling state.
//  4. Cycles and unknown lanes are refused with a NAMED error, and a refused
//     write leaves no row behind.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

// lane1700 creates a project row, optionally bound to a namespace.
func lane1700(t *testing.T, ctx context.Context, db *sql.DB, name, ns string) {
	t.Helper()
	p := &Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0,
		Model: "test", Provider: "test", Enabled: true,
	}
	if ns != "" {
		nsCopy := ns
		p.NamespaceID = &nsCopy
	}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// ns1700 creates the namespace rows the lanes bind to.
func ns1700(t *testing.T, ctx context.Context, db *sql.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := CreateNamespace(ctx, db, &Namespace{
			ID: id, Weight: 10, Reserved: 0, HardCap: 0, MaxConcurrent: 0,
			Enabled: true, Description: "SCHED-GAP-1700 fixture",
		}); err != nil {
			t.Fatalf("CreateNamespace %s: %v", id, err)
		}
	}
}

// linkNames flattens a link slice to its child (or parent) names, so the
// assertions read as the lookup question the operator asked.
func linkChildNames(links []LaneLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.ChildLane)
	}
	return out
}

func linkParentNames(links []LaneLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.ParentLane)
	}
	return out
}

func nodeNames(nodes []LaneLinkNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Lane)
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestSCHEDGAP1700_ManyToManyAcrossNamespaces is acceptance 1 + 2 at the
// storage layer: a lane with TWO parents and TWO children, across
// namespaces, answered by parents/children/ancestor lookups.
func TestSCHEDGAP1700_ManyToManyAcrossNamespaces(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns1700(t, ctx, db, "ns-alpha", "ns-beta")

	// a, b live in ns-alpha; c in ns-beta; d in ns-alpha (so both a
	// same-namespace and a cross-namespace edge exist).
	lane1700(t, ctx, db, "1700-a", "ns-alpha")
	lane1700(t, ctx, db, "1700-b", "ns-alpha")
	lane1700(t, ctx, db, "1700-c", "ns-beta")
	lane1700(t, ctx, db, "1700-d", "ns-alpha")

	edges := [][2]string{
		{"1700-a", "1700-c"}, // cross-namespace
		{"1700-b", "1700-c"}, // c gets a SECOND parent
		{"1700-c", "1700-d"}, // cross-namespace
		{"1700-a", "1700-d"}, // a gets a SECOND child, d a SECOND parent
	}
	for _, e := range edges {
		if _, err := CreateLaneLink(ctx, db, e[0], e[1], "fixture", "test"); err != nil {
			t.Fatalf("CreateLaneLink %s→%s: %v", e[0], e[1], err)
		}
	}

	// ── many-to-many, both directions ───────────────────────────────────
	parentsOfC, err := LaneParents(ctx, db, "1700-c")
	if err != nil {
		t.Fatalf("LaneParents(c): %v", err)
	}
	if got := linkParentNames(parentsOfC); len(got) != 2 || !containsName(got, "1700-a") || !containsName(got, "1700-b") {
		t.Errorf("c's parents = %v, want [1700-a 1700-b] (more than one parent)", got)
	}
	childrenOfA, err := LaneChildren(ctx, db, "1700-a")
	if err != nil {
		t.Fatalf("LaneChildren(a): %v", err)
	}
	if got := linkChildNames(childrenOfA); len(got) != 2 || !containsName(got, "1700-c") || !containsName(got, "1700-d") {
		t.Errorf("a's children = %v, want [1700-c 1700-d] (more than one child)", got)
	}

	// ── endpoint namespaces are live and the cross flag is derived ──────
	for _, l := range parentsOfC {
		if l.ParentNamespace != "ns-alpha" || l.ChildNamespace != "ns-beta" {
			t.Errorf("edge %s→%s namespaces = (%q,%q), want (ns-alpha,ns-beta)",
				l.ParentLane, l.ChildLane, l.ParentNamespace, l.ChildNamespace)
		}
		if !l.CrossNamespace {
			t.Errorf("edge %s→%s cross_namespace = false, want true", l.ParentLane, l.ChildLane)
		}
	}
	sameNS, err := LaneChildren(ctx, db, "1700-a")
	if err != nil {
		t.Fatalf("LaneChildren(a): %v", err)
	}
	for _, l := range sameNS {
		if l.ChildLane == "1700-d" && l.CrossNamespace {
			t.Errorf("same-namespace edge a→d reported cross_namespace=true")
		}
	}

	// A live namespace move must be reported, never the recorded value.
	if err := UpdateProject(ctx, db, "1700-c", ProjectUpdates{NamespaceID: strPtr1700("ns-alpha")}); err != nil {
		t.Fatalf("move c to ns-alpha: %v", err)
	}
	moved, err := LaneParents(ctx, db, "1700-c")
	if err != nil {
		t.Fatalf("LaneParents(c) after move: %v", err)
	}
	for _, l := range moved {
		if l.ChildNamespace != "ns-alpha" || l.CrossNamespace {
			t.Errorf("after moving c to ns-alpha: child_namespace=%q cross=%v, want (ns-alpha,false)",
				l.ChildNamespace, l.CrossNamespace)
		}
	}

	// ── transitive lookups, one call each ───────────────────────────────
	anc, err := LaneAncestorNodes(ctx, db, "1700-d")
	if err != nil {
		t.Fatalf("LaneAncestorNodes(d): %v", err)
	}
	if got := nodeNames(anc); len(got) != 3 {
		t.Errorf("d's ancestors = %v, want 3 lanes (a, b, c)", got)
	} else {
		// d has TWO direct parents (a and c → depth 1, name-ASC within the
		// depth) and one grandparent (b, reachable through c → depth 2).
		byLane := map[string]int{}
		for _, n := range anc {
			byLane[n.Lane] = n.Depth
		}
		if byLane["1700-a"] != 1 || byLane["1700-c"] != 1 {
			t.Errorf("direct ancestors of d = %v (depths %v), want a and c at depth 1", nodeNames(anc), byLane)
		}
		if byLane["1700-b"] != 2 {
			t.Errorf("b depth = %d, want 2 (b→c→d)", byLane["1700-b"])
		}
		if anc[0].Lane != "1700-a" {
			t.Errorf("ancestor[0] = %q, want name-ASC ordering within a depth (1700-a)", anc[0].Lane)
		}
	}
	desc, err := LaneDescendantNodes(ctx, db, "1700-a")
	if err != nil {
		t.Fatalf("LaneDescendantNodes(a): %v", err)
	}
	if got := nodeNames(desc); len(got) != 2 || !containsName(got, "1700-c") || !containsName(got, "1700-d") {
		t.Errorf("a's descendants = %v, want c and d", got)
	}

	// (2) "is Y an ancestor of X" in ONE call, with a witness path.
	ok, path, err := IsLaneAncestor(ctx, db, "1700-b", "1700-d")
	if err != nil {
		t.Fatalf("IsLaneAncestor(b,d): %v", err)
	}
	if !ok {
		t.Fatalf("IsLaneAncestor(b,d) = false, want true (b→c→d)")
	}
	if len(path) != 3 || path[0] != "1700-b" || path[len(path)-1] != "1700-d" {
		t.Errorf("witness path = %v, want b→c→d", path)
	}
	ok, _, err = IsLaneAncestor(ctx, db, "1700-d", "1700-b")
	if err != nil {
		t.Fatalf("IsLaneAncestor(d,b): %v", err)
	}
	if ok {
		t.Errorf("IsLaneAncestor(d,b) = true — the reverse direction must not hold")
	}
	// A lane is never its own ancestor.
	if ok, _, err := IsLaneAncestor(ctx, db, "1700-a", "1700-a"); err != nil || ok {
		t.Errorf("IsLaneAncestor(a,a) = (%v,%v), want (false,nil)", ok, err)
	}

	// ── round-trip through the listing surface ──────────────────────────
	all, err := ListLaneLinks(ctx, db)
	if err != nil {
		t.Fatalf("ListLaneLinks: %v", err)
	}
	if len(all) != len(edges) {
		t.Errorf("ListLaneLinks = %d edges, want %d", len(all), len(edges))
	}
	touching, err := ListLaneLinksTouching(ctx, db, "1700-c")
	if err != nil {
		t.Fatalf("ListLaneLinksTouching(c): %v", err)
	}
	if len(touching) != 3 { // 2 as child + 1 as parent
		t.Errorf("edges touching c = %d, want 3", len(touching))
	}
	if all[0].CreatedBy != "test" || all[0].CreatedAt == "" {
		t.Errorf("edge audit fields = (%q,%q), want created_by=test and a timestamp", all[0].CreatedBy, all[0].CreatedAt)
	}
}

// strPtr1700 is the *string helper ProjectUpdates needs for a namespace move.
func strPtr1700(s string) *string { return &s }

// TestSCHEDGAP1700_CycleAndSelfRefused is acceptance 4: a cycle is refused
// with the NAMED error, a self link is refused, and NEITHER leaves a row.
func TestSCHEDGAP1700_CycleAndSelfRefused(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, n := range []string{"1700-cyc-a", "1700-cyc-b", "1700-cyc-c"} {
		lane1700(t, ctx, db, n, "")
	}
	if _, err := CreateLaneLink(ctx, db, "1700-cyc-a", "1700-cyc-b", "", ""); err != nil {
		t.Fatalf("a→b: %v", err)
	}
	if _, err := CreateLaneLink(ctx, db, "1700-cyc-b", "1700-cyc-c", "", ""); err != nil {
		t.Fatalf("b→c: %v", err)
	}

	// The direct 3-cycle: c→a would make a its own ancestor.
	if _, err := CreateLaneLink(ctx, db, "1700-cyc-c", "1700-cyc-a", "", ""); !errors.Is(err, ErrLaneLinkCycle) {
		t.Errorf("c→a err = %v, want ErrLaneLinkCycle", err)
	}
	// A self link is its own named class.
	if _, err := CreateLaneLink(ctx, db, "1700-cyc-a", "1700-cyc-a", "", ""); !errors.Is(err, ErrLaneLinkSelf) {
		t.Errorf("a→a err = %v, want ErrLaneLinkSelf", err)
	}
	// Refusals must leave the graph untouched.
	rows, err := ListLaneLinks(ctx, db)
	if err != nil {
		t.Fatalf("ListLaneLinks: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("edge count after two refused writes = %d, want 2 (no row written)", len(rows))
	}
}

// TestSCHEDGAP1700_UnknownLaneRefused is acceptance 4 for the other named
// error: both endpoints must name a real lane, and a refusal writes nothing.
func TestSCHEDGAP1700_UnknownLaneRefused(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lane1700(t, ctx, db, "1700-known", "")

	if _, err := CreateLaneLink(ctx, db, "1700-ghost", "1700-known", "", ""); !errors.Is(err, ErrUnknownLane) {
		t.Errorf("unknown parent err = %v, want ErrUnknownLane", err)
	}
	if _, err := CreateLaneLink(ctx, db, "1700-known", "1700-ghost", "", ""); !errors.Is(err, ErrUnknownLane) {
		t.Errorf("unknown child err = %v, want ErrUnknownLane", err)
	}
	rows, err := ListLaneLinks(ctx, db)
	if err != nil {
		t.Fatalf("ListLaneLinks: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("edge count = %d after refused writes, want 0", len(rows))
	}
}

// TestSCHEDGAP1700_DuplicateAndDelete pins the delete contract: a duplicate
// edge is link_exists, a delete that changes nothing is link_not_found, and a
// real delete removes exactly one edge.
func TestSCHEDGAP1700_DuplicateAndDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lane1700(t, ctx, db, "1700-del-a", "")
	lane1700(t, ctx, db, "1700-del-b", "")

	if _, err := CreateLaneLink(ctx, db, "1700-del-a", "1700-del-b", "note", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := CreateLaneLink(ctx, db, "1700-del-a", "1700-del-b", "again", ""); !errors.Is(err, ErrLaneLinkExists) {
		t.Errorf("duplicate err = %v, want ErrLaneLinkExists", err)
	}
	if err := DeleteLaneLink(ctx, db, "1700-del-b", "1700-del-a"); !errors.Is(err, ErrLaneLinkNotFound) {
		t.Errorf("delete of absent edge err = %v, want ErrLaneLinkNotFound", err)
	}
	if err := DeleteLaneLink(ctx, db, "1700-del-a", "1700-del-b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := DeleteLaneLink(ctx, db, "1700-del-a", "1700-del-b"); !errors.Is(err, ErrLaneLinkNotFound) {
		t.Errorf("second delete err = %v, want ErrLaneLinkNotFound", err)
	}
}

// TestSCHEDGAP1700_SchedulingFieldsUntouched is acceptance 3 MEASURED: the
// full project row (every column GetProject reads, including cooldown pins
// and admission mode) is identical before and after a link write, and no tick
// row is created.
func TestSCHEDGAP1700_SchedulingFieldsUntouched(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lane1700(t, ctx, db, "1700-inv-a", "")
	lane1700(t, ctx, db, "1700-inv-b", "")

	// Give lane a non-default scheduling shape so the snapshot is not all
	// zero values that could hide a stray write.
	if _, err := db.ExecContext(ctx, `UPDATE projects SET admission_mode='tasks', cooldown_s=42,
		cooldown_floor_s=30, cooldown_ceiling_s=240, weight=7, priority=9, enabled=0,
		cooldown_pin_s=77, noop_allowed=1 WHERE name='1700-inv-a'`); err != nil {
		t.Fatalf("seed scheduling shape: %v", err)
	}

	snapshot := func(name string) string {
		t.Helper()
		p, err := GetProject(ctx, db, name)
		if err != nil {
			t.Fatalf("GetProject %s: %v", name, err)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal project %s: %v", name, err)
		}
		return string(raw)
	}
	before := snapshot("1700-inv-a")
	beforeB := snapshot("1700-inv-b")

	var ticksBefore int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ticks`).Scan(&ticksBefore); err != nil {
		t.Fatalf("count ticks: %v", err)
	}

	if _, err := CreateLaneLink(ctx, db, "1700-inv-b", "1700-inv-a", "reference only", "test"); err != nil {
		t.Fatalf("CreateLaneLink: %v", err)
	}
	if after := snapshot("1700-inv-a"); after != before {
		t.Errorf("lane a's row changed across a link write:\nbefore: %s\nafter:  %s", before, after)
	}
	if after := snapshot("1700-inv-b"); after != beforeB {
		t.Errorf("lane b's row changed across a link write:\nbefore: %s\nafter:  %s", beforeB, after)
	}
	if err := DeleteLaneLink(ctx, db, "1700-inv-b", "1700-inv-a"); err != nil {
		t.Fatalf("DeleteLaneLink: %v", err)
	}
	if after := snapshot("1700-inv-a"); after != before {
		t.Errorf("lane a's row changed across a link delete:\nbefore: %s\nafter:  %s", before, after)
	}
	var ticksAfter int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ticks`).Scan(&ticksAfter); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticksAfter != ticksBefore {
		t.Errorf("tick count = %d after link writes, want %d (a link must never spawn work)", ticksAfter, ticksBefore)
	}
}

// TestSCHEDGAP1700_MigrationShape pins migration v70: the table exists with
// the documented columns, self-links are refused by the CHECK at the SQL
// layer too (a bypassing writer cannot corrupt the map), and the primary key
// dedupes an edge.
func TestSCHEDGAP1700_MigrationShape(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('lane_links')
		WHERE name IN ('parent_lane','child_lane','namespace_parent','namespace_child','note','created_at','created_by')`).Scan(&cols); err != nil {
		t.Fatalf("pragma_table_info(lane_links): %v", err)
	}
	if cols != 7 {
		t.Fatalf("lane_links columns = %d, want 7 — migration v70 not applied", cols)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO lane_links (parent_lane, child_lane, created_at) VALUES ('x','x','2026-10-17T00:00:00Z')`); err == nil {
		t.Errorf("self-link INSERT succeeded — the CHECK(parent_lane <> child_lane) constraint is missing")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO lane_links (parent_lane, child_lane, created_at) VALUES ('x','y','2026-10-17T00:00:00Z')`); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO lane_links (parent_lane, child_lane, created_at) VALUES ('x','y','2026-10-17T00:00:00Z')`); err == nil {
		t.Errorf("duplicate edge INSERT succeeded — the (parent_lane, child_lane) primary key is missing")
	}
}

// TestSCHEDGAP1700_TraversalSurvivesOutOfBandCycle proves the traversal is
// loop-safe on data written around the API (a direct SQL cycle). The walk must
// terminate and report each lane once — never hang, never recurse forever.
func TestSCHEDGAP1700_TraversalSurvivesOutOfBandCycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lane1700(t, ctx, db, "1700-loop-a", "")
	lane1700(t, ctx, db, "1700-loop-b", "")
	// Bypass the write API: the CHECK only forbids self-links, so a 2-cycle
	// is representable at the SQL layer.
	for _, pair := range [][2]string{{"1700-loop-a", "1700-loop-b"}, {"1700-loop-b", "1700-loop-a"}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO lane_links (parent_lane, child_lane, created_at) VALUES (?,?,'2026-10-17T00:00:00Z')`,
			pair[0], pair[1]); err != nil {
			t.Fatalf("seed out-of-band edge %v: %v", pair, err)
		}
	}
	anc, err := LaneAncestorNodes(ctx, db, "1700-loop-a")
	if err != nil {
		t.Fatalf("LaneAncestorNodes on a cyclic graph: %v", err)
	}
	if got := nodeNames(anc); len(got) != 1 || got[0] != "1700-loop-b" {
		t.Errorf("ancestors of a in a 2-cycle = %v, want exactly [1700-loop-b] (each lane once)", got)
	}
	desc, err := LaneDescendantNodes(ctx, db, "1700-loop-a")
	if err != nil {
		t.Fatalf("LaneDescendantNodes on a cyclic graph: %v", err)
	}
	if got := nodeNames(desc); len(got) != 1 || got[0] != "1700-loop-b" {
		t.Errorf("descendants of a in a 2-cycle = %v, want exactly [1700-loop-b]", got)
	}
}
