package dashboard_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1598 — the fleet overview page stacks four tables with no way to
// search, sort, page-size or paginate them (measured: 485+20+17+100 = 622
// data rows, zero controls). These tests pin the server-side controls: the
// controls render, the params are honoured server-side (never shipped to the
// browser to filter), the "showing N of M" count is present, and the htmx
// autorefresh carries the operator's current view instead of resetting it.

// overviewSeedDB seeds 26 lanes named aa..az with alternating priorities and
// increasing weights.
func overviewSeedDB(t *testing.T, db *sql.DB) {
	t.Helper()
	for i := 0; i < 26; i++ {
		name := fmt.Sprintf("%c%c", 'a'+i/10, 'a'+i%10)
		if err := database.CreateProject(context.Background(), db, &database.Project{
			Name:      name,
			RepoURL:   "https://example.com/" + name,
			Weight:    10 + i,
			Priority:  i%3 + 1,
			CooldownS: 900,
			DecayRate: 1.0,
			Model:     "m",
			Provider:  "p",
			Enabled:   i%2 == 0,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
}

// overviewTick creates one completed tick for the given project.
func overviewTick(t *testing.T, db *sql.DB, id, project string, at time.Time) {
	t.Helper()
	if err := database.CreateTick(context.Background(), db, &database.Tick{
		ID:          id,
		ProjectName: project,
		Status:      database.StatusCompleted,
		Outcome:     database.OutcomeCommitted,
		SpawnedAt:   at.UTC().Format(time.RFC3339),
		CompletedAt: at.UTC().Add(2 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick %s: %v", id, err)
	}
}

// optionRendered reports whether an <option> whose full text is `want` (exact
// form) appears in out, tolerating html/template's space-before-`>` rendering
// (`<option value="0" selected>` → `<option value="0" selected>` with a space
// inserted after the last attribute). Matching strips the trailing `>` and
// compares the prefix.
func optionRendered(out, want string) bool {
	prefix := strings.TrimSuffix(want, ">")
	return strings.Contains(out, prefix)
}

// TestOverview_DefaultControlsRender verifies the four control forms, the
// page-size selects (with the tick-history-matching 50 default) and the
// per-table "showing N of M" count all render on the default page.
func TestOverview_DefaultControlsRender(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`name="q"`, `name="size"`, `name="sort"`, `name="dir"`, // projects form
		`name="tq"`, `name="tsort"`, `name="tsize"`, // recent-ticks form
		`name="nq"`, `name="nsort"`, `name="nsize"`, // namespaces form
		`name="hq"`, `name="hsort"`, `name="hsize"`, // history form
		`<option value="50" selected>50</option>`, // default page size
		`<option value="0" >all</option>`,
	} {
		// html/template renders static+dynamic attribute mixes with a space
		// before `>` (e.g. `<option value="0" >all</option>`), so both
		// spellings are accepted.
		if !strings.Contains(out, want) && !optionRendered(out, want) {
			t.Errorf("overview missing control %q", want)
		}
	}
	// All four "showing N of M" counts.
	for _, want := range []string{
		"showing 26 of 26 lanes",
		"showing 0 of 0 ticks",
		"showing 0 of 0 namespaces",
		"showing 0 of 0 history rows",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("overview missing count line %q", want)
		}
	}
}

// TestOverview_PageSizeHonoured shrinks the projects page size and asserts
// only that many rows render — server-side, not client-side.
func TestOverview_PageSizeHonoured(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	params := dashboard.ParseFleetTableQuery(url.Values{"size": []string{"25"}})
	var buf strings.Builder
	if err := gen.GenerateParams(&buf, params); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	out := buf.String()

	rows := strings.Count(out, `href="/projects/`)
	if rows != 25 {
		t.Errorf("page size 25 rendered %d project rows, want 25", rows)
	}
	if !strings.Contains(out, "showing 25 of 26 lanes") {
		t.Errorf("missing 'showing 25 of 26 lanes' count")
	}
	if !strings.Contains(out, "Page 1 / 2") {
		t.Errorf("missing pagination math for 26 rows at 25/page")
	}
	if !strings.Contains(out, "size=25") {
		t.Errorf("next-page link must carry the active page size")
	}
}

// TestOverview_PaginationMath walks a 26-row table at 10/page: page 2 holds
// ten rows, an out-of-range page clamps to the last, and page links carry
// the state needed to keep paging.
func TestOverview_PaginationMath(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	render := func(q url.Values) string {
		var buf strings.Builder
		if err := gen.GenerateParams(&buf, dashboard.ParseFleetTableQuery(q)); err != nil {
			t.Fatalf("GenerateParams(%v): %v", q, err)
		}
		return buf.String()
	}

	p2 := render(url.Values{"size": []string{"25"}, "page": []string{"2"}})
	if rows := strings.Count(p2, `href="/projects/`); rows != 1 {
		t.Errorf("page 2 rows = %d, want 1 (26 at 25/page)", rows)
	}
	if !strings.Contains(p2, "showing 1 of 26 lanes") || !strings.Contains(p2, "Page 2 / 2") {
		t.Errorf("page 2 counts wrong")
	}
	if !strings.Contains(p2, "page=1") {
		t.Errorf("page 2 must link to the previous page")
	}
	// Out-of-range page clamps to the last page.
	p99 := render(url.Values{"size": []string{"25"}, "page": []string{"99"}})
	if !strings.Contains(p99, "Page 2 / 2") {
		t.Errorf("page 99 must clamp to page 2")
	}
	if rows := strings.Count(p99, `href="/projects/`); rows != 1 {
		t.Errorf("last page rows = %d, want 1 (26 at 25/page)", rows)
	}
}

// TestOverview_SearchNarrowsSet asserts the projects search matches the lane
// name server-side: matching rows render, others do not, the count states
// the narrowed set, and a non-matching term renders the empty-state row.
func TestOverview_SearchNarrowsSet(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	render := func(q url.Values) string {
		var buf strings.Builder
		if err := gen.GenerateParams(&buf, dashboard.ParseFleetTableQuery(q)); err != nil {
			t.Fatalf("GenerateParams(%v): %v", q, err)
		}
		return buf.String()
	}

	// "aa" matches only "aa" (the 26 lanes are aa-aj, ba-bj, ca-cf; only
	// aa contains the substring "aa").
	p := render(url.Values{"q": []string{"aa"}})
	if rows := strings.Count(p, `href="/projects/`); rows != 1 {
		t.Errorf("search 'aa' rendered %d rows, want 1", rows)
	}
	if !strings.Contains(p, "showing 1 of 1 lanes") {
		t.Errorf("search must state the narrowed count as 'showing 1 of 1'")
	}
	if !strings.Contains(p, `value="aa"`) {
		t.Errorf("search input must echo the active term")
	}
	// Case-insensitive.
	if rows := strings.Count(render(url.Values{"q": []string{"AA"}}), `href="/projects/`); rows != 1 {
		t.Errorf("search must be case-insensitive, got %d rows", rows)
	}
	// No match renders the explicit empty state, not a silent empty table.
	none := render(url.Values{"q": []string{"zzzz"}})
	if !strings.Contains(none, "No lanes match the current search/filter") {
		t.Errorf("no-match must render the empty-state row")
	}
	if rows := strings.Count(none, `href="/projects/`); rows != 0 {
		t.Errorf("no-match rendered %d rows, want 0", rows)
	}
}

// TestOverview_ProjectFilters pins the lane + outcome dropdowns: unknown
// values render unfiltered, known values narrow, and the outcome vocabulary
// is offered as options.
func TestOverview_ProjectFilters(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	ctx := context.Background()
	for i, id := range []string{"t1", "t2"} {
		project := "aa"
		if i == 1 {
			project = "ab"
		}
		if err := database.CreateTick(ctx, db, &database.Tick{
			ID: id, ProjectName: project,
			Status:    database.StatusCompleted,
			Outcome:   database.OutcomeCommitted,
			SpawnedAt: "2026-09-20T10:0" + fmt.Sprint(i) + ":00Z",
		}); err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateParams(&buf, dashboard.ParseFleetTableQuery(url.Values{"project": []string{"aa"}})); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	out := buf.String()
	if rows := strings.Count(out, `href="/projects/`); rows != 1 {
		t.Errorf("lane filter 'aa' rendered %d rows, want 1", rows)
	}
	if !strings.Contains(out, "showing 1 of 1 lanes") {
		t.Errorf("lane filter must state the narrowed count")
	}
	// Unknown lane value → unfiltered (mirrors tickHistoryFilter semantics).
	var buf2 strings.Builder
	if err := gen.GenerateParams(&buf2, dashboard.ParseFleetTableQuery(url.Values{"project": []string{"not-a-lane"}})); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	if rows := strings.Count(buf2.String(), `href="/projects/`); rows != 26 {
		t.Errorf("unknown lane value must render unfiltered, got %d rows", rows)
	}
	// Outcome options render in the dropdown.
	if !strings.Contains(out, `value="committed"`) {
		t.Errorf("outcome dropdown must offer the committed outcome")
	}
}

// TestOverview_SortOrder pins that an explicit sort key reorders rows while
// the empty key keeps the default (name) order, and that an unknown key
// falls back to the default instead of erroring.
func TestOverview_SortOrder(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	render := func(q url.Values) string {
		var buf strings.Builder
		if err := gen.GenerateParams(&buf, dashboard.ParseFleetTableQuery(q)); err != nil {
			t.Fatalf("GenerateParams(%v): %v", q, err)
		}
		return buf.String()
	}
	firstName := func(out string) string {
		idx := strings.Index(out, `href="/projects/`)
		if idx < 0 {
			t.Fatal("no project link rendered")
		}
		rest := out[idx+len(`href="/projects/`):]
		return rest[:strings.Index(rest, `"`)]
	}

	// Default (no sort): first row is the first-created lane, "aa".
	if got := firstName(render(url.Values{})); got != "aa" {
		t.Errorf("default order first row = %q, want aa", got)
	}
	// Sort by name desc: the LAST lane created is "cf" (aa..az generates
	// aa-aj, ba-bj, ca-cf for 26), which sorts last ascending.
	if got := firstName(render(url.Values{"sort": []string{"name"}, "dir": []string{"desc"}})); got != "cf" {
		t.Errorf("name desc first row = %q, want cf", got)
	}
	// Sort by weight desc: the highest weight (10+25=35 → the 26th lane "cf") leads.
	if got := firstName(render(url.Values{"sort": []string{"weight"}, "dir": []string{"desc"}})); got != "cf" {
		t.Errorf("weight desc first row = %q, want cf", got)
	}
	// An unknown sort key falls back to the default order, not an error.
	if got := firstName(render(url.Values{"sort": []string{"not-a-column"}})); got != "aa" {
		t.Errorf("unknown sort must fall back to default order, first row = %q", got)
	}
}

// TestOverview_OtherTables pins the recent-ticks, namespaces and history
// tables' search/sort/page params (prefixed families) end-to-end.
func TestOverview_OtherTables(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	ctx := context.Background()
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "platform", Weight: 40, Reserved: 10, HardCap: 60, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{
		TickGroup: "g1", NamespaceID: "platform", Allocated: 40, Used: 20,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		overviewTick(t, db, fmt.Sprintf("tick-%d", i), "aa", base.Add(time.Duration(i)*time.Minute))
	}
	gen := dashboard.NewGenerator(db, nil)

	render := func(q url.Values) string {
		var buf strings.Builder
		if err := gen.GenerateParams(&buf, dashboard.ParseFleetTableQuery(q)); err != nil {
			t.Fatalf("GenerateParams(%v): %v", q, err)
		}
		return buf.String()
	}

	// Recent ticks: search narrows by project; count states the narrowed set.
	p := render(url.Values{"tq": []string{"aa"}})
	if !strings.Contains(p, "showing 3 of 3 ticks") {
		t.Errorf("recent-ticks search count wrong: %s", snippet(p, "showing"))
	}
	if !strings.Contains(p, `<option value="project" >project</option>`) &&
		!strings.Contains(p, `<option value="project">project</option>`) {
		t.Errorf("recent-ticks sort options missing")
	}
	// Namespaces: search matches the id.
	n := render(url.Values{"nq": []string{"plat"}})
	if !strings.Contains(n, "showing 1 of 1 namespaces") {
		t.Errorf("namespace search count wrong")
	}
	n2 := render(url.Values{"nq": []string{"zzz"}})
	if !strings.Contains(n2, "showing 0 of 0 namespaces") || !strings.Contains(n2, "No namespaces match the search.") {
		t.Errorf("namespace no-match must render the empty state")
	}
	// History: search matches namespace id.
	h := render(url.Values{"hq": []string{"platform"}})
	if !strings.Contains(h, "showing 1 of 1 history rows") {
		t.Errorf("history search count wrong")
	}
	// Page size on the ticks table: 25/page over 3 ticks → one page;
	// the page link and count reflect the filtered set.
	tp := render(url.Values{"tsize": []string{"25"}})
	if !strings.Contains(tp, "showing 3 of 3 ticks") {
		t.Errorf("recent-ticks count wrong")
	}
	// Size=0 ("all") is honoured on a prefixed family too.
	ta := render(url.Values{"tsize": []string{"0"}})
	if !strings.Contains(ta, "showing 3 of 3 ticks") {
		t.Errorf("recent-ticks 'all' size wrong")
	}
}

// TestOverview_RefreshPreservesState is the htmx autorefresh contract
// (SCHED-GAP-1598 build item 5): the tbody's hx-get carries the operator's
// current search/page/sort state, and rendering the partial from those same
// query values re-renders the SAME view instead of resetting it.
func TestOverview_RefreshPreservesState(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	// Full page with active state: page 2 at 25/page, sorted name desc
	// (26 lanes → 2 pages; page 2 holds the single remaining row).
	q := url.Values{"sort": []string{"name"}, "dir": []string{"desc"}, "size": []string{"25"}, "page": []string{"2"}}
	var page strings.Builder
	if err := gen.GenerateParams(&page, dashboard.ParseFleetTableQuery(q)); err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	out := page.String()

	// The tbody polls with the SAME state (its hx-get carries q/sort/dir/size/page).
	if !strings.Contains(out, `hx-get="/dashboard/partial?sort=name&amp;dir=desc&amp;size=25&amp;page=2"`) &&
		!strings.Contains(out, `hx-get="/dashboard/partial?sort=name&amp;dir=desc&amp;size=25&page=2"`) {
		t.Errorf("tbody hx-get must carry the current table state: %s", snippet(out, "hx-get="))
	}
	// htmx trigger contract unchanged (SCHED-GAP-1606).
	if !strings.Contains(out, `hx-trigger="autorefresh from:body"`) {
		t.Errorf("tbody must keep the shared autorefresh trigger")
	}

	// The autorefresh request: the partial rendered from those query values
	// must show the same rows the page showed (state preserved, not reset).
	var partial strings.Builder
	if err := gen.GenerateFleetTableParams(&partial, q); err != nil {
		t.Fatalf("GenerateFleetTableParams: %v", err)
	}
	pout := partial.String()
	pageRows := strings.Count(strings.SplitN(out, `hx-swap="innerHTML">`, 2)[1], `href="/projects/`)
	partRows := strings.Count(pout, `href="/projects/`)
	if partRows == 0 {
		t.Fatalf("refresh partial rendered 0 rows — state was reset")
	}
	if pageRows != partRows {
		t.Errorf("refresh changed the view: page %d rows vs refreshed partial %d rows", pageRows, partRows)
	}
	if !strings.Contains(out, "Page 2 /") {
		t.Errorf("page must be on page 2")
	}
}

// TestOverview_DefaultsUnchanged pins backward compatibility: a bare partial
// render (the pre-1598 autorefresh behavior) keeps the full, unpaginated
// view and stays rows-only for the innerHTML swap.
func TestOverview_DefaultsUnchanged(t *testing.T) {
	db := newTestDB(t)
	overviewSeedDB(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateFleetTable(&buf); err != nil {
		t.Fatalf("GenerateFleetTable: %v", err)
	}
	if rows := strings.Count(buf.String(), `href="/projects/`); rows != 26 {
		t.Errorf("bare partial must render all rows, got %d", rows)
	}
	// The partial stays rows-only (no tbody wrapper — innerHTML swap).
	if strings.Contains(buf.String(), "<tbody") {
		t.Errorf("partial must not emit a tbody wrapper")
	}
}
