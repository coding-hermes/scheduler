package database

import (
	"context"
	"errors"
	"testing"
)

// SCHED-GAP-1586 — lane parent reference system, STORAGE layer.
//
// The `projects` table carried NO hierarchy column: the satellite relation
// was INFERRED from name suffixes and the board-symlink walk. Migration v42
// adds `parent TEXT NOT NULL DEFAULT ''` — the name of the lane this lane is
// a satellite of ('' = a primary/root lane) — and BuildLaneTree is the ONE
// resolver later surfaces consume (1587 trees, 1590 nesting in lists, 1595
// parent pages).
//
// Deliberately NOT part of this row: the name-suffix heuristics and the
// fleet-topology skill stay untouched (that retirement is SCHED-GAP-1587).

// TestSCHEDGAP1586_MigrationRecorded pins the ladder bookkeeping: v42 is the
// latest migration, Migrate records it, and the column actually exists on a
// fresh DB (a missing column would break every seeded boot).
func TestSCHEDGAP1586_MigrationRecorded(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("migration version: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}
	if latestMigration != 46 {
		t.Fatalf("latestMigration = %d, want 46 (SCHED-GAP-1622 partial running-ticks index lands as v46; v45 stays the SCHED-GAP-1636 covering index)", latestMigration)
	}
	var desc string
	if err := db.QueryRowContext(ctx, `SELECT desc FROM migrations WHERE version = 42`).Scan(&desc); err != nil {
		t.Fatalf("read v42 desc: %v", err)
	}
	if !contains(desc, "SCHED-GAP-1586") {
		t.Errorf("v42 desc %q does not name SCHED-GAP-1586", desc)
	}
	// The column itself must exist with the empty-string default (every
	// pre-1586 row reads as a primary/root lane).
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE parent = ''`).Scan(&n); err != nil {
		t.Fatalf("query projects.parent: %v (migration v42 did not add the column?)", err)
	}
}

// TestSCHEDGAP1586_ParentRoundTrip covers create-and-read: a lane created
// with a parent reads it back through GetProject and ListProjects; a lane
// created without one reads ” (primary).
func TestSCHEDGAP1586_ParentRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	primary := sampleProject("1586-primary")
	if err := CreateProject(ctx, db, primary); err != nil {
		t.Fatalf("create primary: %v", err)
	}
	sat := sampleProject("1586-qa")
	sat.Parent = "1586-primary"
	if err := CreateProject(ctx, db, sat); err != nil {
		t.Fatalf("create satellite: %v", err)
	}

	got, err := GetProject(ctx, db, "1586-qa")
	if err != nil {
		t.Fatalf("get satellite: %v", err)
	}
	if got.Parent != "1586-primary" {
		t.Errorf("satellite parent = %q, want %q", got.Parent, "1586-primary")
	}
	gotPrimary, err := GetProject(ctx, db, "1586-primary")
	if err != nil {
		t.Fatalf("get primary: %v", err)
	}
	if gotPrimary.Parent != "" {
		t.Errorf("primary parent = %q, want '' (default)", gotPrimary.Parent)
	}

	all, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	parents := map[string]string{}
	for _, p := range all {
		parents[p.Name] = p.Parent
	}
	if parents["1586-qa"] != "1586-primary" || parents["1586-primary"] != "" {
		t.Errorf("ListProjects parents = %v, want satellite→primary, primary→''", parents)
	}
}

// TestSCHEDGAP1586_UpdateParent covers the write path: PUT-shaped
// ProjectUpdates sets and clears the parent exactly like deliver does.
func TestSCHEDGAP1586_UpdateParent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for _, name := range []string{"1586-a", "1586-b"} {
		if err := CreateProject(ctx, db, sampleProject(name)); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	p := "1586-a"
	if err := UpdateProject(ctx, db, "1586-b", ProjectUpdates{Parent: &p}); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	got, err := GetProject(ctx, db, "1586-b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Parent != "1586-a" {
		t.Fatalf("parent after set = %q, want %q", got.Parent, "1586-a")
	}
	clear := ""
	if err := UpdateProject(ctx, db, "1586-b", ProjectUpdates{Parent: &clear}); err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	got, err = GetProject(ctx, db, "1586-b")
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got.Parent != "" {
		t.Errorf("parent after clear = %q, want ''", got.Parent)
	}
}

// TestSCHEDGAP1586_CycleRejection proves the cycle gate: self-parenting,
// a direct 2-cycle and an indirect 3-cycle are all REJECTED with
// ErrLaneCycle and the stored parent is left untouched. Also proves a
// legitimate re-parent still lands after a refusal (clearing the blocking
// edge first).
func TestSCHEDGAP1586_CycleRejection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for _, name := range []string{"1586-r", "1586-mid", "1586-leaf"} {
		if err := CreateProject(ctx, db, sampleProject(name)); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	// Build the chain r ← mid ← leaf (leaf.parent=mid, mid.parent=r).
	mid := "1586-mid"
	if err := UpdateProject(ctx, db, "1586-leaf", ProjectUpdates{Parent: &mid}); err != nil {
		t.Fatalf("seed leaf.parent: %v", err)
	}
	r := "1586-r"
	if err := UpdateProject(ctx, db, "1586-mid", ProjectUpdates{Parent: &r}); err != nil {
		t.Fatalf("seed mid.parent: %v", err)
	}

	// 1. self-parenting.
	self := "1586-r"
	err := UpdateProject(ctx, db, "1586-r", ProjectUpdates{Parent: &self})
	if !errors.Is(err, ErrLaneCycle) {
		t.Errorf("self-parent: err = %v, want ErrLaneCycle", err)
	}

	// 2. direct 2-cycle: r's parent currently '' — point it at mid, then
	// try to point mid back at... itself through r? Direct: mid→mid is
	// self; the 2-cycle shape is r→mid plus mid→r. mid already points at
	// r, so setting r→mid is only legal if it does NOT loop — r→mid is
	// fine (r becomes child of mid, mid child of r would be a cycle only
	// if mid still pointed at r — it does, so r→mid must be REJECTED).
	if err := UpdateProject(ctx, db, "1586-r", ProjectUpdates{Parent: &mid}); !errors.Is(err, ErrLaneCycle) {
		t.Errorf("direct 2-cycle r→mid (mid→r live): err = %v, want ErrLaneCycle", err)
	}

	// 3. indirect 3-cycle: leaf→mid→r is live; setting r→leaf would close
	// r→leaf→mid→r.
	leaf := "1586-leaf"
	if err := UpdateProject(ctx, db, "1586-r", ProjectUpdates{Parent: &leaf}); !errors.Is(err, ErrLaneCycle) {
		t.Errorf("indirect 3-cycle r→leaf→mid→r: err = %v, want ErrLaneCycle", err)
	}

	// Every refusal above must have left the stored parents untouched.
	for name, want := range map[string]string{
		"1586-r":    "",
		"1586-mid":  "1586-r",
		"1586-leaf": "1586-mid",
	} {
		got, err := GetProject(ctx, db, name)
		if err != nil {
			t.Fatalf("get %s after rejections: %v", name, err)
		}
		if got.Parent != want {
			t.Errorf("%s parent after rejections = %q, want %q (a rejected write must not land)", name, got.Parent, want)
		}
	}

	// 4. A legal re-parent after clearing the blocking edge: clear
	// mid→r, then r→mid is a plain 2-lane tree.
	clear := ""
	if err := UpdateProject(ctx, db, "1586-mid", ProjectUpdates{Parent: &clear}); err != nil {
		t.Fatalf("clear mid.parent: %v", err)
	}
	if err := UpdateProject(ctx, db, "1586-r", ProjectUpdates{Parent: &mid}); err != nil {
		t.Errorf("legal re-parent r→mid after clearing the edge: %v", err)
	}
}

// TestSCHEDGAP1586_DanglingParentTolerated proves a parent that names a
// non-existent lane is STORED, not rejected (soft-deleted and purged lanes
// must never block a satellite's reparent), and BuildLaneTree surfaces the
// dangling child as a root.
func TestSCHEDGAP1586_DanglingParentTolerated(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateProject(ctx, db, sampleProject("1586-orphan")); err != nil {
		t.Fatalf("create: %v", err)
	}
	ghost := "1586-purged-parent"
	if err := UpdateProject(ctx, db, "1586-orphan", ProjectUpdates{Parent: &ghost}); err != nil {
		t.Fatalf("dangling parent must be tolerated, got: %v", err)
	}
	got, err := GetProject(ctx, db, "1586-orphan")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Parent != ghost {
		t.Fatalf("dangling parent = %q, want %q (stored verbatim)", got.Parent, ghost)
	}
	all, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	tree := BuildLaneTree(all)
	if len(tree.Roots) != 1 || tree.Roots[0].Project.Name != "1586-orphan" {
		t.Errorf("dangling-parent lane must surface as a root, got %d roots", len(tree.Roots))
	}
}

// TestSCHEDGAP1586_TreeResolver covers the resolver contract: 3-level
// nesting, a fan-out with several children, name-ASC ordering at every
// level, disabled lanes keeping their position, and determinism across
// shuffled input orders.
func TestSCHEDGAP1586_TreeResolver(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// Lane shape:
	//   trio        (root, level 0)
	//   ├── trio-qa     (level 1)
	//   │   └── trio-qa-sync (level 2 — 3-level chain)
	//   └── trio-sync   (level 1)
	//   fan         (root, level 0)
	//   ├── fan-zeta  (level 1)   ─┐ children created out of alpha order
	//   ├── fan-mid   (level 1)    │ to prove the name-ASC re-sort
	//   └── fan-alpha (level 1)   ─┘
	//   lone-disabled (root; disabled lanes stay in the tree)
	lanes := []struct{ name, parent string }{
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
		p := sampleProject(l.name)
		p.Parent = l.parent
		if err := CreateProject(ctx, db, p); err != nil {
			t.Fatalf("create %s: %v", l.name, err)
		}
	}
	if err := UpdateProject(ctx, db, "lone-disabled", ProjectUpdates{Enabled: boolPtr1586(false)}); err != nil {
		t.Fatalf("disable lone: %v", err)
	}

	all, err := ListProjects(ctx, db, false) // disabled lanes keep their position
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	build := func(list []Project) *LaneTree {
		// Resolver is pure — prove determinism by feeding two different
		// input orders (DB order is name-ASC; reverse a copy).
		rev := make([]Project, len(list))
		for i, p := range list {
			rev[len(list)-1-i] = p
		}
		t1 := BuildLaneTree(list)
		t2 := BuildLaneTree(rev)
		if flat1586(t1) != flat1586(t2) {
			t.Fatalf("resolver is order-dependent:\nASC: %s\nREV: %s", flat1586(t1), flat1586(t2))
		}
		return t1
	}

	tree := build(all)
	got := flat1586(tree)
	want := `fan
  fan-alpha
  fan-mid
  fan-zeta
lone-disabled
trio
  trio-qa
    trio-qa-sync
  trio-sync
`
	if got != want {
		t.Fatalf("tree mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}

	// Spot-check nesting arithmetic on the 3-level chain: roots at level
	// 0, each child = parent + 1 (consumers derive levels from position).
	if len(tree.Roots) != 3 {
		t.Fatalf("roots = %d, want 3", len(tree.Roots))
	}
	var trioNode *LaneNode
	for _, r := range tree.Roots {
		if r.Project.Name == "trio" {
			trioNode = r
		}
	}
	if trioNode == nil || len(trioNode.Children) != 2 {
		t.Fatalf("trio children = %v, want 2", trioNode)
	}
	var qaNode *LaneNode
	for _, c := range trioNode.Children {
		if c.Project.Name == "trio-qa" {
			qaNode = c
		}
	}
	if qaNode == nil || len(qaNode.Children) != 1 || qaNode.Children[0].Project.Name != "trio-qa-sync" {
		t.Fatalf("trio-qa subtree wrong: %+v", qaNode)
	}
}

// TestSCHEDGAP1586_CorruptedCycleDataSafe proves the resolver terminates on
// a data-level cycle (out-of-band DB edit — writes can never produce one):
// the cycle members are excluded, honest lanes still render.
func TestSCHEDGAP1586_CorruptedCycleDataSafe(t *testing.T) {
	// Two lanes pointing at each other: x.parent=y, y.parent=x.
	x := sampleProject("1586-x")
	x.Parent = "1586-y"
	y := sampleProject("1586-y")
	y.Parent = "1586-x"
	ok := sampleProject("1586-ok")

	// Resolver is pure — feed directly, no DB writes needed.
	tree := BuildLaneTree([]Project{*ok, *x, *y})
	got := flat1586(tree)
	want := "1586-ok\n"
	if got != want {
		t.Fatalf("cycle members must be excluded from the tree\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// flat1586 renders a tree as name lines, children indented two spaces per
// level — the canonical deterministic shape every ordering assertion uses.
func flat1586(t *LaneTree) string {
	var out string
	var walk func(n *LaneNode, level int)
	walk = func(n *LaneNode, level int) {
		for i := 0; i < level; i++ {
			out += "  "
		}
		out += n.Project.Name + "\n"
		for _, c := range n.Children {
			walk(c, level+1)
		}
	}
	for _, r := range t.Roots {
		walk(r, 0)
	}
	return out
}

func boolPtr1586(b bool) *bool { return &b }
