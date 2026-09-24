package dashboard_test

// SCHED-GAP-1582 — dashboard budget-panel truthfulness tests.
//
// The operator's complaint: the panel compared a fleet-wide weight SUM
// (BudgetUsed) against a PER-TICK packing budget (BudgetTotal) — numbers of
// different kinds, so the "bar" overflowed by construction and told them
// nothing. SCHED-GAP-1583 already replaced the fill with labelled facts;
// this row adds the honest OVERSUBSCRIPTION state: enabled namespaces'
// measured demand vs the per-tick budget, stated as a labelled note that
// names both sides verbatim — never as a percentage, never as a fraction
// of the fleet weight sum.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// assertNoOverflowingWidth is the SCHED-GAP-1583 property, stated locally:
// whatever the data, the page emits no fill wider than 100%.
func assertNoOverflowingWidth(t *testing.T, page string) {
	t.Helper()
	re := regexp.MustCompile(`width:(\d+)%`)
	matches := re.FindAllStringSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("no rendered widths found — the assertion would be vacuous")
	}
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > 100 {
			t.Fatalf("rendered %s exceeds its track (must be <= 100%%)", m[0])
		}
	}
}

// TestDashboard_OversubscriptionNoteNotRendered — the under-budget default:
// no namespaces (or namespaces whose demand fits the per-tick budget) means
// no OVERSUBSCRIBED note; the two labelled facts stay exactly as pinned by
// the SCHED-GAP-1583 tests.
func TestDashboard_OversubscriptionNoteNotRendered(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	page := buf.String()

	if strings.Contains(page, "OVERSUBSCRIBED") {
		t.Errorf("oversubscription note rendered with no namespace demand at all: %s", snippet(page, "budget-bar"))
	}
	for _, want := range []string{
		"Fleet weight — sum of enabled lanes",
		"Per-tick weight budget",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("labelled panel lost %q", want)
		}
	}
}

// TestDashboard_OversubscriptionNoteRenders — with namespaces whose latest
// measured demand exceeds the per-tick budget, the note states the state
// explicitly and names BOTH sides (demand total, per-tick budget) — and
// still renders no fill wider than 100% (the SCHED-GAP-1583 property).
func TestDashboard_OversubscriptionNoteRenders(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Two namespaces: ns-fat (weight 20) and ns-fit (weight 80). Their
	// latest namespace_ticks demand 60 + 60 = 120 against the default
	// per-tick budget of 100 → oversubscribed.
	for _, ns := range []struct {
		id     string
		weight int
	}{
		{"ns-fat", 20},
		{"ns-fit", 80},
	} {
		n := &database.Namespace{ID: ns.id, Weight: ns.weight, Reserved: 0, HardCap: 100, Enabled: true}
		if err := database.CreateNamespace(ctx, db, n); err != nil {
			t.Fatalf("CreateNamespace %s: %v", ns.id, err)
		}
	}
	for _, nt := range []struct {
		ns     string
		demand int
		alloc  int
		over   int
	}{
		{"ns-fat", 60, 40, 20}, // over: 60 > 40 → "over by 20"
		{"ns-fit", 60, 60, 0},  // within budget: 60 <= 60
	} {
		if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
			TickGroup:     "2031-05-04-03-02-01",
			NamespaceID:   nt.ns,
			Allocated:     nt.alloc,
			Used:          nt.alloc,
			JobCount:      1,
			Demand:        nt.demand,
			Overcommitted: nt.over,
		}); err != nil {
			t.Fatalf("InsertNamespaceTick %s: %v", nt.ns, err)
		}
	}

	g := dashboard.NewGenerator(db, nil)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	page := buf.String()

	for _, want := range []string{
		"OVERSUBSCRIBED",
		"Namespace demand vs per-tick budget",
		"demand 120 against a per-tick budget of 100",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("oversubscription note missing %q", want)
		}
	}

	// The namespace allocation table carries the explicit per-namespace
	// state (Demand column + over-by pill), not an implied overflow.
	for _, want := range []string{"<th>Demand</th>", "<th>Status</th>", "over by 20", "within budget"} {
		if !strings.Contains(page, want) {
			t.Errorf("namespace table missing %q", want)
		}
	}

	// The SCHED-GAP-1583 property holds: no rendered width exceeds 100%.
	assertNoOverflowingWidth(t, page)
}

// TestDashboard_NamespaceViewBudgetState — /namespaces/{id} renders the
// demand card, the explicit budget-state pill, and per-cycle Over By values
// from the persisted columns.
func TestDashboard_NamespaceViewBudgetState(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	n := &database.Namespace{ID: "ns-fat", Weight: 20, Reserved: 0, HardCap: 100, Enabled: true}
	if err := database.CreateNamespace(ctx, db, n); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup:     "2031-05-04-03-02-01",
		NamespaceID:   "ns-fat",
		Allocated:     20,
		Used:          20,
		JobCount:      6,
		Demand:        36,
		Overcommitted: 16,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup:     "2031-05-04-02-02-01",
		NamespaceID:   "ns-fat",
		Allocated:     20,
		Used:          20,
		JobCount:      6,
		Demand:        30,
		Overcommitted: 10,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}

	// A history row for a within-budget cycle renders the dash, not a pill.
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup:   "2031-05-04-01-02-01",
		NamespaceID: "ns-fat",
		Allocated:   40,
		Used:        30,
		JobCount:    5,
		Demand:      30,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}

	var out strings.Builder
	if err := dashboard.NewGenerator(db, nil).GenerateNamespaceView(&out, "ns-fat"); err != nil {
		t.Fatalf("GenerateNamespaceView: %v", err)
	}
	page := out.String()

	for _, want := range []string{
		"Demand (enabled weight)",
		"<span class=\"pill fail\">over by 16</span>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("namespace view missing %q", want)
		}
	}
	// The older over-budget cycle shows its own Over By pill (just the
	// number); the within-budget cycle shows the dash.
	if !strings.Contains(page, `<span class="pill fail">10</span>`) {
		t.Errorf("history row missing the over-by pill for the 10-unit overcommit")
	}
	if !strings.Contains(page, `<span class="meta">—</span>`) {
		t.Errorf("within-budget history row missing the dash placeholder")
	}
}

// TestDashboard_NamespaceViewWithoutTicks — the budget-state cards are
// gated on tick data existing; a namespace with no history renders without
// them (and without panicking).
func TestDashboard_NamespaceViewWithoutTicks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	n := &database.Namespace{ID: "ns-empty", Weight: 10, Reserved: 0, HardCap: 100, Enabled: true}
	if err := database.CreateNamespace(ctx, db, n); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	var out strings.Builder
	if err := dashboard.NewGenerator(db, nil).GenerateNamespaceView(&out, "ns-empty"); err != nil {
		t.Fatalf("GenerateNamespaceView: %v", err)
	}
	if strings.Contains(out.String(), "Budget State") {
		t.Errorf("budget-state card rendered without any tick data")
	}
}
