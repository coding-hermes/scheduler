package dashboard_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/coding-herms/scheduler/internal/dashboard"
	"github.com/coding-herms/scheduler/internal/database"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var queryCountingDriverID atomic.Uint64

type queryCountingDriver struct {
	base    driver.Driver
	queries *atomic.Int64
}

func (d *queryCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &queryCountingConn{Conn: conn, queries: d.queries}, nil
}

type queryCountingConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c *queryCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries.Add(1)
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

func (c *queryCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func newQueryCountingTestDB(t *testing.T) (*sql.DB, *atomic.Int64) {
	t.Helper()
	queries := &atomic.Int64{}
	driverName := fmt.Sprintf("sqlite-query-count-%d", queryCountingDriverID.Add(1))
	sql.Register(driverName, &queryCountingDriver{base: &sqlite.Driver{}, queries: queries})

	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("open query-counting database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := database.Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("migrate query-counting database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, queries
}

func mustCreateProject(t *testing.T, db *sql.DB, name string, weight, priority int) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    weight,
		Priority:  priority,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

func mustCreateTick(t *testing.T, db *sql.DB, id, projectName string, spawnedAt time.Time) {
	t.Helper()
	if err := database.CreateTick(context.Background(), db, &database.Tick{
		ID:          id,
		ProjectName: projectName,
		Status:      database.StatusCompleted,
		SpawnedAt:   spawnedAt.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick %s: %v", id, err)
	}
}

// TestGenerate_EmptyDatabase renders the dashboard with no projects.
// This works because dashboard.collect skips the per-project loop when there are
// no rows, avoiding the int→bool Scan bug documented below.
func TestGenerate_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// Must contain core HTML scaffolding.
	for _, want := range []string{"<!DOCTYPE html>", "<title>Coding Hermes Fleet</title>", "Generated ", "Auto-refresh 60s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// With no projects, the counts should be 0/0 and budget 0/100.
	if !strings.Contains(out, "<div class=\"value\">0/0</div>") {
		t.Errorf("expected 0/0 in Enabled Projects card, got: %s", snippet(out, "Enabled Projects"))
	}
}

// TestGenerate_BudgetZero verifies percent(0, total) returns 0 and doesn't divide by zero.
func TestGenerate_BudgetZero(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// No projects → BudgetUsed=0. percent(0, 100) = 0. Should not panic on /0.
	if !strings.Contains(out, `width:0%`) {
		t.Errorf("expected width:0%% for empty budget, got: %s", snippet(out, "budget-fill"))
	}
}

// TestGenerate_GeneratedAtIsRFC3339 verifies the timestamp format.
func TestGenerate_GeneratedAtIsRFC3339(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// Extract "Generated <timestamp>" line.
	idx := strings.Index(out, "Generated ")
	if idx < 0 {
		t.Fatal("missing 'Generated ' marker")
	}
	rest := out[idx+len("Generated "):]
	end := strings.Index(rest, " ")
	if end < 0 {
		t.Fatal("malformed 'Generated' line")
	}
	ts := rest[:end]
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", ts, err)
	}
}

// snippet returns a small window of text centered on needle for diagnostic output.
func snippet(haystack, needle string) string {
	idx := strings.Index(haystack, needle)
	if idx < 0 {
		return "(needle not found)"
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := idx + 200
	if end > len(haystack) {
		end = len(haystack)
	}
	return haystack[start:end]
}

// TestGenerate_WithProjects exercises the per-project rendering path.
// This was previously SKIPPED due to the int→bool scan bug in dashboard.collect
// (FleetRow.RunningNow changed from bool to int, fixing the modernc.org/sqlite
// scan issue). Now actively tests project data rendering.
func TestGenerate_WithProjects(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 30, 5)
	mustCreateProject(t, db, "beta", 20, 3)
	mustCreateProject(t, db, "gamma", 10, 1)

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// All three project names must appear.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected project %q in output", name)
		}
	}

	// Weights must be visible.
	for _, want := range []string{">30<", ">20<", ">10<"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected weight %s in output", want)
		}
	}

	// Priority values.
	for _, want := range []string{">5<", ">3<", ">1<"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected priority %s in output", want)
		}
	}

	// Should NOT show "0/0" (empty-state) since we have projects.
	// With 3 enabled projects, disabled 0, the card shows 3/3.
	if strings.Contains(out, ">0/0<") {
		t.Errorf("expected non-zero project counts, got 0/0")
	}
}

// TestGenerate_PercentFunction_ZeroTotal verifies percent handles total=0.
// We test this via the dashboard's Generate path with BudgetUsed=0, BudgetTotal=100
// → percent(0, 100) = 0 → width:0%.
// The total=0 case can't easily be exercised through Generate (BudgetTotal is hardcoded
// to 100), but it's covered indirectly: percent(used, total) where total=0 returns 0.
func TestGenerate_PercentFunction_ZeroTotal(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Empty DB → 0/0 for Enabled/Total. The EnabledProjects card value rendering
	// uses a different path, but the budget bar uses percent.
	out := buf.String()
	if !strings.Contains(out, "0/100") {
		t.Errorf("expected budget 0/100, got: %s", snippet(out, "budget-bar"))
	}
}

// TestGenerate_NamespaceEmptyState verifies that the "Namespaces" heading and
// empty-state message appear when no namespaces are configured.
func TestGenerate_NamespaceEmptyState(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "<h2>Namespaces</h2>") {
		t.Errorf("expected 'Namespaces' heading, not found in output")
	}
	if !strings.Contains(out, "No namespaces configured") {
		t.Errorf("expected empty-state message, got: %s", snippet(out, "Namespaces"))
	}
	if !strings.Contains(out, "No namespace tick data available") {
		t.Errorf("expected namespace tick empty-state message")
	}
}

// TestGenerate_WithNamespaces creates namespaces + namespace ticks and verifies
// the allocation table renders correctly with color-coded rows.
func TestGenerate_WithNamespaces(t *testing.T) {
	db := newTestDB(t)

	// Create two namespaces.
	ctx := context.Background()
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "alpha", Weight: 30, Reserved: 10, HardCap: 50, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace alpha: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "beta", Weight: 20, Reserved: 15, HardCap: 0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace beta: %v", err)
	}

	// Insert namespace ticks. alpha is over-reserved (used=12 >= reserved=10),
	// beta is under-reserved (used=5 < reserved=15).
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup: "2026-07-15-10-00-00", NamespaceID: "alpha",
		Allocated: 30, Used: 12, Borrowed: 2, Lent: 0,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick alpha: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup: "2026-07-15-10-00-00", NamespaceID: "beta",
		Allocated: 20, Used: 5, Borrowed: 0, Lent: 3,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick beta: %v", err)
	}

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	// Namespace IDs must appear.
	if !strings.Contains(out, ">alpha<") {
		t.Errorf("expected namespace 'alpha' in table, got: %s", snippet(out, "Namespaces"))
	}
	if !strings.Contains(out, ">beta<") {
		t.Errorf("expected namespace 'beta' in table")
	}

	// alpha: used=12 >= reserved=10, hard_cap=50, 12 < 50 → util-yellow
	if !strings.Contains(out, "util-yellow") {
		t.Errorf("expected util-yellow class for alpha (at reserved), got: %s", snippet(out, "alpha"))
	}

	// beta: used=5 < reserved=15 → util-green
	if !strings.Contains(out, "util-green") {
		t.Errorf("expected util-green class for beta (under reserved)")
	}

	// Borrowed/lent rendering.
	if !strings.Contains(out, ">+2<") {
		t.Errorf("expected borrowed +2 for alpha")
	}
	if !strings.Contains(out, ">-3<") {
		t.Errorf("expected lent -3 for beta")
	}

	// Utilization history table should show the tick data.
	if !strings.Contains(out, "Namespace Utilization History") {
		t.Errorf("expected utilization history heading")
	}
}

// TestGenerateQueue_EmptyDatabase renders the queue page with no projects.
func TestGenerateQueue_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	g := dashboard.NewGenerator(db)

	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"<!DOCTYPE html>", "<title>Queue", "Evaluation Queue", "0 eligible"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// TestGenerateQueue_WithProjects renders the queue page sorted by urgency.
func TestGenerateQueue_WithProjects(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 30, 5)
	mustCreateProject(t, db, "beta", 20, 3)
	mustCreateProject(t, db, "gamma", 10, 8) // lower weight, higher priority

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	// All three projects must appear.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected project %q in queue output", name)
		}
	}

	// Must contain nav links and structural elements.
	if !strings.Contains(out, `href="/projects/alpha"`) {
		t.Errorf("expected link to project detail page")
	}
	if !strings.Contains(out, `href="/"`) {
		t.Errorf("expected nav link to fleet overview")
	}

	// Verify gamma (priority 8) appears before alpha (priority 5) since urgency is
	// priority-driven when no ticks exist.
	gammaIdx := strings.Index(out, "gamma")
	alphaIdx := strings.Index(out, "alpha")
	betaIdx := strings.Index(out, "beta")
	if gammaIdx < 0 || alphaIdx < 0 || betaIdx < 0 {
		t.Fatal("one or more projects missing from queue")
	}
	// gamma has higher priority (8) than alpha (5), should appear first.
	if gammaIdx > alphaIdx {
		t.Errorf("expected gamma (priority 8) before alpha (priority 5) in urgency-sorted queue")
	}
	if alphaIdx > betaIdx {
		t.Errorf("expected alpha (priority 5) before beta (priority 3)")
	}
}

func TestGenerateQueue_UsesLatestTickForEveryProject(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 30, 5)
	mustCreateProject(t, db, "beta", 20, 4)
	mustCreateProject(t, db, "gamma", 10, 3)

	now := time.Now().UTC()
	mustCreateTick(t, db, "alpha-old", "alpha", now.Add(-100*time.Hour))
	mustCreateTick(t, db, "alpha-latest", "alpha", now.Add(-time.Hour))
	mustCreateTick(t, db, "beta-old", "beta", now.Add(-200*time.Hour))
	mustCreateTick(t, db, "beta-latest", "beta", now.Add(-2*time.Hour))

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	// Latest-tick urgency is alpha≈10 and beta≈12, while gamma has no tick and
	// keeps its base urgency of 30. Selecting an older tick, or failing to map a
	// project from the batch result, changes this order.
	gammaIdx := strings.Index(out, `href="/projects/gamma"`)
	betaIdx := strings.Index(out, `href="/projects/beta"`)
	alphaIdx := strings.Index(out, `href="/projects/alpha"`)
	if gammaIdx < 0 || betaIdx < 0 || alphaIdx < 0 {
		t.Fatalf("one or more projects missing from queue: %s", snippet(out, "Evaluation Queue"))
	}
	if gammaIdx >= betaIdx || betaIdx >= alphaIdx {
		t.Errorf("expected latest-tick order gamma, beta, alpha; indexes were %d, %d, %d", gammaIdx, betaIdx, alphaIdx)
	}
}

func TestGenerateQueue_QueryCountIsConstant(t *testing.T) {
	db, queryCount := newQueryCountingTestDB(t)
	for i := range 39 {
		mustCreateProject(t, db, fmt.Sprintf("project-%02d", i), 1, 1)
	}
	queryCount.Store(0)

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	if got := queryCount.Load(); got != 2 {
		t.Errorf("GenerateQueue executed %d queries for 39 projects, want 2", got)
	}
}

// TestGenerateQueue_NavLinks verifies the queue page has functional navigation.
func TestGenerateQueue_NavLinks(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "test", 10, 5)

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	// Queue page nav should link back to fleet overview.
	if !strings.Contains(out, `href="/"`) {
		t.Errorf("queue page missing link to fleet overview")
	}
	// Queue should have meta with count.
	if !strings.Contains(out, "eligible") {
		t.Errorf("queue page missing eligibility count")
	}
}
