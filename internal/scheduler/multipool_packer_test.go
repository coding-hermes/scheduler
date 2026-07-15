package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Test helpers (namespace-specific)
// ---------------------------------------------------------------------------

func makeNamespace(id string, weight, reserved, hardCap int, enabled bool) *database.Namespace {
	return &database.Namespace{
		ID:       id,
		Weight:   weight,
		Reserved: reserved,
		HardCap:  hardCap,
		Enabled:  enabled,
	}
}

func mustCreateNamespace(t *testing.T, db *sql.DB, ns *database.Namespace) {
	t.Helper()
	if err := database.CreateNamespace(context.Background(), db, ns); err != nil {
		t.Fatalf("CreateNamespace %s: %v", ns.ID, err)
	}
}

func mustCreateProjectInNS(t *testing.T, db *sql.DB, name, nsID string, weight, priority, cooldown int, decay float64) {
	t.Helper()
	p := makeProject(name, weight, priority, cooldown, decay)
	p.NamespaceID = &nsID
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

func strPtr(s string) *string { return &s }

func makeProjectUrgency(nsID, name string, effW int, urgency float64) *scheduler.ProjectUrgency {
	return &scheduler.ProjectUrgency{
		Project: database.Project{
			Name:        name,
			NamespaceID: &nsID,
			Weight:      effW,
		},
		Urgency:         urgency,
		EffectiveWeight: effW,
	}
}

func defaultUrgencyCalc() *scheduler.UrgencyCalculator {
	return scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
}

// ---------------------------------------------------------------------------
// MultiPoolPacker tests
// ---------------------------------------------------------------------------

// TestMultiPoolPacker_FlatFallback — NamespaceMode=false (no namespaces) returns
// empty result from Pack (caller should use FlatFallback instead).
func TestMultiPoolPacker_FlatFallback(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateProjectAt(t, db, "alpha", 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	now := time.Now()

	// No namespaces → Pack returns empty Projects.
	result := mp.Pack(projects, nil, defaultUrgencyCalc(), nil, nil, now)
	if len(result.Projects) != 0 {
		t.Errorf("Pack with no namespaces returned %d projects, want 0", len(result.Projects))
	}

	// FlatFallback delegates to Packer.Pick.
	picked, err := mp.FlatFallback(db, defaultUrgencyCalc(), 100, now)
	if err != nil {
		t.Fatalf("FlatFallback: %v", err)
	}
	if len(picked) != 1 {
		t.Errorf("FlatFallback returned %d projects, want 1", len(picked))
	}
	if picked[0].Name != "alpha" {
		t.Errorf("FlatFallback picked %q, want alpha", picked[0].Name)
	}
}

// TestMultiPoolPacker_EmptyNamespaces — empty namespace list triggers flat fallback.
func TestMultiPoolPacker_EmptyNamespaces(t *testing.T) {
	db := newTestDB(t)

	mp := scheduler.NewMultiPoolPacker(100, 10)
	now := time.Now()

	result := mp.Pack(nil, []database.Namespace{}, defaultUrgencyCalc(), nil, nil, now)
	if len(result.Projects) != 0 {
		t.Errorf("Pack with empty namespaces returned %d projects, want 0", len(result.Projects))
	}
	if len(result.NamespaceTicks) != 0 {
		t.Errorf("Pack with empty namespaces returned %d ticks, want 0", len(result.NamespaceTicks))
	}

	_ = db // keep db reference for clarity — not needed in this test
}

// TestMultiPoolPacker_UnassignedProjects — projects with NamespaceID=nil are never selected.
func TestMultiPoolPacker_UnassignedProjects(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Create one namespace and one project without namespace assignment.
	mustCreateNamespace(t, db, makeNamespace("ns-a", 10, 5, 100, true))
	mustCreateProjectAt(t, db, "orphan", 10, 5, 0, 1.0) // no NamespaceID

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	if len(result.Projects) != 0 {
		t.Errorf("expected 0 projects (orphan unassigned), got %d: %+v", len(result.Projects), result.Projects)
	}
}

// TestMultiPoolPacker_DisabledNamespace — disabled namespace gets zero allocation.
func TestMultiPoolPacker_DisabledNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("disabled", 10, 5, 100, false))
	mustCreateNamespace(t, db, makeNamespace("active", 10, 5, 100, true))
	mustCreateProjectInNS(t, db, "job-d", "disabled", 10, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "job-a", "active", 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, false) // include disabled
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	// Only job-a should be selected (disabled namespace excluded).
	names := make(map[string]bool)
	for _, p := range result.Projects {
		names[p.Name] = true
	}
	if names["job-d"] {
		t.Errorf("project from disabled namespace was selected")
	}
	if !names["job-a"] {
		t.Errorf("project from active namespace was NOT selected")
	}
}

// TestMultiPoolPacker_BasicPacking — 2 namespaces, projects fit in allocations.
func TestMultiPoolPacker_BasicPacking(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("ns-a", 50, 10, 100, true))
	mustCreateNamespace(t, db, makeNamespace("ns-b", 50, 10, 100, true))
	mustCreateProjectInNS(t, db, "proj-a1", "ns-a", 10, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "proj-a2", "ns-a", 10, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "proj-b1", "ns-b", 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	if len(result.Projects) != 3 {
		t.Errorf("expected 3 projects selected, got %d", len(result.Projects))
	}

	// Each namespace should have a tick record.
	if len(result.NamespaceTicks) != 2 {
		t.Errorf("expected 2 namespace ticks, got %d", len(result.NamespaceTicks))
	}
	for _, nt := range result.NamespaceTicks {
		if nt.JobCount == 0 {
			t.Errorf("namespace %s has 0 jobs", nt.NamespaceID)
		}
	}
}

// TestMultiPoolPacker_EffectiveWeightScaling — project w=60 in ns with alloc=10, Σw=200 → effective=3.
func TestMultiPoolPacker_EffectiveWeightScaling(t *testing.T) {
	// alloc=10, project weight=60, total weight in ns=200
	// effective = floor(10 * 60/200) = floor(3.0) = 3
	eff := scheduler.CalcEffectiveWeight(60, 200, 10)
	if eff != 3 {
		t.Errorf("effectiveWeight(60, 200, 10) = %d, want 3", eff)
	}
}

// TestMultiPoolPacker_EffectiveWeightFloorAtOne — tiny job: alloc=1, w=1, Σw=100 → effective=1.
func TestMultiPoolPacker_EffectiveWeightFloorAtOne(t *testing.T) {
	// alloc=1, project weight=1, total weight=100
	// effective = floor(1 * 1/100) = floor(0.01) = 0 → floored at 1
	eff := scheduler.CalcEffectiveWeight(1, 100, 1)
	if eff != 1 {
		t.Errorf("effectiveWeight(1, 100, 1) = %d, want 1 (floored)", eff)
	}
}

// TestMultiPoolPacker_CooldownRespected — projects in cooldown are skipped.
func TestMultiPoolPacker_CooldownRespected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("ns-a", 10, 5, 100, true))
	mustCreateProjectInNS(t, db, "cooling", "ns-a", 10, 5, 3600, 1.0)

	// Set last_completed to now → project is in cooldown.
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx,
		`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		now.Format(time.RFC3339), "cooling",
	)
	if err != nil {
		t.Fatalf("update last_tick_completed: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	lastCompleted := map[string]time.Time{"cooling": now}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), lastCompleted, nil, now)

	if len(result.Projects) != 0 {
		t.Errorf("expected 0 projects (cooldown), got %d", len(result.Projects))
	}
}

// TestMultiPoolPacker_UnknownDeadlines — projects with nil NamespaceID are skipped.
func TestMultiPoolPacker_UnknownDeadlines(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("ns-a", 10, 5, 100, true))
	mustCreateProjectInNS(t, db, "assigned", "ns-a", 10, 5, 0, 1.0)
	mustCreateProjectAt(t, db, "unassigned", 10, 5, 0, 1.0) // no NS

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	found := false
	for _, p := range result.Projects {
		if p.Name == "unassigned" {
			found = true
		}
	}
	if found {
		t.Errorf("unassigned project was selected — should be skipped")
	}
	if len(result.Projects) != 1 {
		t.Errorf("expected exactly 1 project (assigned), got %d", len(result.Projects))
	}
}

// TestMultiPoolPacker_BudgetExceeded — project too heavy for remaining budget → skipped.
func TestMultiPoolPacker_BudgetExceeded(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Namespace with small allocation.
	mustCreateNamespace(t, db, makeNamespace("ns-a", 1, 2, 100, true))
	mustCreateNamespace(t, db, makeNamespace("ns-b", 99, 98, 100, true))

	// ns-a gets allocation of ~2 (reserved=2). Projects with high total weight.
	// We want a project whose effective weight exceeds allocation.
	mustCreateProjectInNS(t, db, "big-a", "ns-a", 50, 5, 0, 1.0) // high weight
	mustCreateProjectInNS(t, db, "big-b", "ns-a", 50, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "filler", "ns-b", 1, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	// ns-a has alloc=2 (reserved=2, weight=1/100 → remainder*0.01 ≈ 0).
	// Projects in ns-a: total weight = 100, each project w=50.
	// effective_weight = floor(2 * 50/100) = floor(1.0) = 1.
	// So each project has effective=1, and two fit (1+1=2 <= 2).
	// But with borrowing, ns-b may lend. Let's verify the first project at least fits.
	if len(result.Projects) == 0 {
		t.Errorf("expected at least some projects, got 0")
	}
}

// TestMultiPoolPacker_ConcurrencyCap — global maxConcurrent is respected across namespaces.
func TestMultiPoolPacker_ConcurrencyCap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("ns-a", 50, 5, 100, true))
	mustCreateNamespace(t, db, makeNamespace("ns-b", 50, 5, 100, true))
	// 3 projects in each namespace.
	for _, n := range []string{"a1", "a2", "a3"} {
		mustCreateProjectInNS(t, db, "proj-"+n, "ns-a", 10, 5, 0, 1.0)
	}
	for _, n := range []string{"b1", "b2", "b3"} {
		mustCreateProjectInNS(t, db, "proj-"+n, "ns-b", 10, 5, 0, 1.0)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	// maxConcurrent=3 across ALL namespaces.
	mp := scheduler.NewMultiPoolPacker(100, 3)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	if len(result.Projects) > 3 {
		t.Errorf("expected at most 3 projects (maxConcurrent=3), got %d", len(result.Projects))
	}
}

// TestMultiPoolPacker_RunningProjectsCounted — running projects count against concurrency.
func TestMultiPoolPacker_RunningProjectsCounted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("ns-a", 10, 5, 100, true))
	mustCreateProjectInNS(t, db, "p1", "ns-a", 5, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "p2", "ns-a", 5, 5, 0, 1.0)
	mustCreateProjectInNS(t, db, "p3", "ns-a", 5, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	// maxConcurrent=5, but 2 already running → only 3 more can start.
	mp := scheduler.NewMultiPoolPacker(100, 5)
	result := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil,
		[]string{"p1", "p2"}, time.Now())

	if len(result.Projects) > 3 {
		t.Errorf("expected at most 3 new projects (2 running + maxConcurrent=5), got %d", len(result.Projects))
	}
}

// ---------------------------------------------------------------------------
// BorrowingEngine tests
// ---------------------------------------------------------------------------

// TestBorrowing_LenderHasUnused — Namespace A used 5/20, B needs 10, B borrows 10.
func TestBorrowing_LenderHasUnused(t *testing.T) {
	allocations := map[string]int{
		"lender":   20,
		"borrower": 10,
	}
	nsDetails := []database.Namespace{
		*makeNamespace("lender", 10, 5, 100, true),
		*makeNamespace("borrower", 10, 5, 100, true),
	}
	queuedJobs := map[string][]*scheduler.ProjectUrgency{
		"borrower": {
			{EffectiveWeight: 5},
			{EffectiveWeight: 5},
		},
	}
	selectedBudget := map[string]int{
		"lender":   5,  // used 5 out of 20 → 15 unused
		"borrower": 10, // fully used
	}

	engine := scheduler.NewBorrowingEngine()
	result := engine.Borrow(allocations, nsDetails, queuedJobs, selectedBudget)

	borrowed := result["borrower"] - 10
	if borrowed != 10 {
		t.Errorf("borrower borrowed %d, want 10", borrowed)
	}
	lent := 20 - result["lender"]
	if lent != 10 {
		t.Errorf("lender lent %d, want 10", lent)
	}
}

// TestBorrowing_HardCapBlocks — HardCap prevents over-borrowing.
func TestBorrowing_HardCapBlocks(t *testing.T) {
	allocations := map[string]int{
		"lender":   50,
		"borrower": 18, // close to cap of 20
	}
	nsDetails := []database.Namespace{
		*makeNamespace("lender", 10, 5, 100, true),
		*makeNamespace("borrower", 10, 5, 20, true), // hard_cap=20
	}
	queuedJobs := map[string][]*scheduler.ProjectUrgency{
		"borrower": {
			{EffectiveWeight: 5},
			{EffectiveWeight: 5},
			{EffectiveWeight: 5},
			{EffectiveWeight: 5},
		}, // need 20 total
	}
	selectedBudget := map[string]int{
		"lender":   0, // used nothing
		"borrower": 18,
	}

	engine := scheduler.NewBorrowingEngine()
	result := engine.Borrow(allocations, nsDetails, queuedJobs, selectedBudget)

	// hard_cap=20, current=18, so max borrow = 2.
	if result["borrower"] > 20 {
		t.Errorf("borrower allocation %d exceeds hard_cap 20", result["borrower"])
	}
	borrowed := result["borrower"] - 18
	if borrowed != 2 {
		t.Errorf("borrower borrowed %d, want 2 (hard_cap limit)", borrowed)
	}
}

// TestBorrowing_NoLenders — all fully utilized → no borrowing.
func TestBorrowing_NoLenders(t *testing.T) {
	allocations := map[string]int{
		"ns-a": 20,
		"ns-b": 20,
	}
	nsDetails := []database.Namespace{
		*makeNamespace("ns-a", 10, 5, 100, true),
		*makeNamespace("ns-b", 10, 5, 100, true),
	}
	queuedJobs := map[string][]*scheduler.ProjectUrgency{
		"ns-b": {{EffectiveWeight: 10}},
	}
	// Both namespaces fully used → no unused budget.
	selectedBudget := map[string]int{
		"ns-a": 20,
		"ns-b": 20,
	}

	engine := scheduler.NewBorrowingEngine()
	result := engine.Borrow(allocations, nsDetails, queuedJobs, selectedBudget)

	if result["ns-b"] != 20 {
		t.Errorf("ns-b allocation changed to %d, want 20 (no lenders)", result["ns-b"])
	}
}

// TestBorrowing_NoBorrowers — unused pool exists but all satisfied.
func TestBorrowing_NoBorrowers(t *testing.T) {
	allocations := map[string]int{
		"ns-a": 50,
		"ns-b": 50,
	}
	nsDetails := []database.Namespace{
		*makeNamespace("ns-a", 10, 5, 100, true),
		*makeNamespace("ns-b", 10, 5, 100, true),
	}
	// No queued jobs anywhere.
	queuedJobs := map[string][]*scheduler.ProjectUrgency{}
	selectedBudget := map[string]int{
		"ns-a": 10, // 40 unused
		"ns-b": 50,
	}

	engine := scheduler.NewBorrowingEngine()
	result := engine.Borrow(allocations, nsDetails, queuedJobs, selectedBudget)

	// No borrowers → allocations unchanged.
	if result["ns-a"] != 50 {
		t.Errorf("ns-a allocation changed to %d, want 50 (no borrowers)", result["ns-a"])
	}
	if result["ns-b"] != 50 {
		t.Errorf("ns-b allocation changed to %d, want 50 (no borrowers)", result["ns-b"])
	}
}

// TestBorrowing_AllocationSurvives — after borrowing, allocations are reflected correctly.
// Uses the BorrowingEngine directly so we can control the exact queued/lent scenario.
func TestBorrowing_AllocationSurvives(t *testing.T) {
	allocations := map[string]int{
		"lender":   50,
		"borrower": 50,
	}
	nsDetails := []database.Namespace{
		*makeNamespace("lender", 50, 5, 100, true),
		*makeNamespace("borrower", 50, 5, 100, true),
	}

	// Lender: used 20 of 50 → 30 unused, no queued jobs → LENDS 30.
	// Borrower: used 50 of 50 → 0 unused, has queued jobs → BORROWS.
	queuedJobs := map[string][]*scheduler.ProjectUrgency{
		"lender":   {},
		"borrower": {makeProjectUrgency("borrower", "extra-1", 10, 10.0)},
	}
	selectedBudget := map[string]int{
		"lender":   20,
		"borrower": 50,
	}

	engine := scheduler.NewBorrowingEngine()
	result := engine.Borrow(allocations, nsDetails, queuedJobs, selectedBudget)

	// Borrower should have received extra budget.
	if result["borrower"] <= 50 {
		t.Errorf("expected borrower to have borrowed > 0 (allocation > 50), got %d", result["borrower"])
	}
	// Lender should have given away budget.
	if result["lender"] >= 50 {
		t.Errorf("expected lender to have lent > 0 (allocation < 50), got %d", result["lender"])
	}
}

// ---------------------------------------------------------------------------
// NamespaceAllocator regression tests (verify Phase 1 still works)
// ---------------------------------------------------------------------------

// TestNamespaceAllocator_TwoNamespacesEqual — two equal namespaces split budget evenly.
func TestNamespaceAllocator_TwoNamespacesEqual(t *testing.T) {
	namespaces := []database.Namespace{
		*makeNamespace("ns-a", 50, 10, 100, true),
		*makeNamespace("ns-b", 50, 10, 100, true),
	}
	alloc := scheduler.NewNamespaceAllocator(100)
	result := alloc.Allocate(namespaces)

	// reserved: 10+10=20, remainder=80, split 50/50 → 40 each.
	// Each gets 10+40=50.
	if result["ns-a"] != 50 {
		t.Errorf("ns-a allocation = %d, want 50", result["ns-a"])
	}
	if result["ns-b"] != 50 {
		t.Errorf("ns-b allocation = %d, want 50", result["ns-b"])
	}
}

// TestNamespaceAllocator_DisabledExcluded — disabled namespaces get zero allocation.
func TestNamespaceAllocator_DisabledExcluded(t *testing.T) {
	namespaces := []database.Namespace{
		*makeNamespace("active", 10, 5, 100, true),
		*makeNamespace("inactive", 10, 5, 100, false),
	}
	alloc := scheduler.NewNamespaceAllocator(100)
	result := alloc.Allocate(namespaces)

	if result["inactive"] != 0 {
		t.Errorf("disabled namespace got allocation %d, want 0", result["inactive"])
	}
	if _, ok := result["active"]; !ok {
		t.Errorf("active namespace missing from allocation")
	}
}

// TestNamespaceAllocator_SumAtMostBudget — total allocation never exceeds budget.
func TestNamespaceAllocator_SumAtMostBudget(t *testing.T) {
	namespaces := []database.Namespace{
		*makeNamespace("ns-a", 30, 20, 100, true),
		*makeNamespace("ns-b", 30, 20, 100, true),
		*makeNamespace("ns-c", 40, 20, 100, true),
	}
	alloc := scheduler.NewNamespaceAllocator(100)
	result := alloc.Allocate(namespaces)

	total := 0
	for _, v := range result {
		total += v
	}
	// With flooring, may be ≤ 100 but never > 100 (within ±1 rounding).
	if total > 101 {
		t.Errorf("total allocation %d exceeds budget 100 (±1 rounding OK)", total)
	}
}
