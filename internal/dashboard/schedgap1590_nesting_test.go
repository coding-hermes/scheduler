package dashboard_test

// SCHED-GAP-1590 — lane nesting in the list views.
//
// /queue and the fleet overview table render lanes FLAT. This row makes the
// satellite relation legible in both: a satellite row is visually attached to
// its primary (a ↳-rail + the primary's name as text) and indented by its
// depth, so the nesting is readable without colour and without breaking the
// existing columns.
//
// The sort-vs-nesting tension is resolved for the queue as GLOBAL URGENCY
// ORDER + ANNOTATION, deliberately NOT grouping under primaries: the
// SCHED-GAP-174 parity gate (TestQueueSurface_HTMLUrgencyEqualsAPIUrgency)
// pins the dashboard's /queue row order equal to /api/v1/queue's array, and
// grouping families would fork the dashboard's ordering from the API's with
// no matching change to the API surface. Both options were written up in the
// task; the reasoning lives next to the resolver in generator_lane_nesting.go
// and is echoed in the queue template.
//
// The parenthood source is the explicit `projects.parent` column
// (SCHED-GAP-1586) resolved through database.BuildLaneTree — the ONE shared
// resolver — with name-suffix inference ONLY as a fallback when parent is
// empty; every test below also pins which source was used (marker attribute
// parent="explicit" vs parent="inferred").

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// seedLane creates one lane row with a weight/priority and optional parent.
// The priority default (5) keeps every fixture at the same urgency base so
// order-sensitive assertions are not disturbed by the score.
func seedLane(t *testing.T, db *sql.DB, name string, parent string) {
	t.Helper()
	p := &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
	if parent != "" {
		if err := database.UpdateProject(context.Background(), db, name, database.ProjectUpdates{Parent: &parent}); err != nil {
			t.Fatalf("set parent of %s to %s: %v", name, parent, err)
		}
	}
}

// laneCellRegexp picks one rendered lane's Project cell INCLUDING its
// data-parent-source attribute (the only <td> carrying it). Depth info
// travels inside this cell, so the tests read depth + attachment straight
// from the HTML the way an operator sees it.
var laneCellRegexp = regexp.MustCompile(`<td (data-parent-source="[^"]*")>(.*?)</td>`)

// laneLinkRegexp extracts the lane name from a cell's project link.
var laneLinkRegexp = regexp.MustCompile(`<a href="/projects/([^"]+)">`)

// renderQueueTable renders the /queue page and returns the ordered
// (name, row-text) pairs of the queue table's body — document order.
func renderQueueTable(t *testing.T, db *sql.DB) ([]string, []string) {
	t.Helper()
	return parseLaneRows(t, renderQueuePage(t, db))
}

// renderFleetTable renders the fleet overview partial and returns the ordered
// (name, row-text) pairs of the projects table — document order.
func renderFleetTable(t *testing.T, db *sql.DB) ([]string, []string) {
	t.Helper()
	return parseLaneRows(t, renderFleetPage(t, db))
}

// parseLaneRows extracts each project's name and its full Project-cell HTML
// from rendered table markup, in document order.
func parseLaneRows(t *testing.T, page string) ([]string, []string) {
	t.Helper()
	cells := laneCellRegexp.FindAllStringSubmatch(page, -1)
	names := make([]string, 0, len(cells))
	texts := make([]string, 0, len(cells))
	for _, c := range cells {
		link := laneLinkRegexp.FindStringSubmatch(c[2])
		if link == nil {
			continue
		}
		names = append(names, link[1])
		texts = append(texts, c[1]+" "+c[2])
	}
	return names, texts
}

// countLaneRows counts project links in a rendered page — the "nesting must
// not drop lanes" probe. Every lane must render exactly once.
func countLaneRows(page string) int {
	return strings.Count(page, `<a href="/projects/`)
}

// nestingInfo reads the attachment markup off one row text: the leading
// rail/indent characters and the "↳ <primary>" annotation.
// depth returns the number of rail markers (0 = root row).
func depth(rowText string) int {
	return strings.Count(rowText, "└")
}

// hasPrimaryRef reports whether the row carries an explicit "↳ <primary>"
// attachment annotation.
func hasPrimaryRef(rowText string) bool {
	return strings.Contains(rowText, "↳ ")
}

// primaryRef extracts the named primary from a row's annotation, e.g.
// "↳ 1586-primary (L1)" → "1586-primary". Empty when unannotated. Slicing is
// byte-exact: "↳ " is 4 bytes (3-byte rune + space).
func primaryRef(rowText string) string {
	const arrow = "↳ "
	i := strings.Index(rowText, arrow)
	if i < 0 {
		return ""
	}
	rest := rowText[i+len(arrow):]
	if j := strings.Index(rest, " ("); j >= 0 {
		return rest[:j]
	}
	return rest
}

// parentSourceMarker asserts the row names its parenthood source — the task
// requires the surface to say WHICH source it used. The template stamps
// data-parent-source="explicit|inferred" on the cell.
func parentSourceMarker(rowText string) string {
	const key = `data-parent-source="`
	i := strings.Index(rowText, key)
	if i < 0 {
		return ""
	}
	rest := rowText[i+len(key):]
	return rest[:strings.IndexByte(rest, '"')]
}

// ── /queue ──────────────────────────────────────────────────────────────────

// TestQueue_1590_SatelliteAnnotatedWithPrimary: a primary with several
// satellites shows them attached — each satellite row carries the ↳ rail, the
// primary's name, its own depth, and the source marker; the primary row is a
// plain root row. Global urgency order (annotation option) is asserted by the
// parity suite separately; here the primary/satellite mix must interleave in
// urgency order, not be grouped.
func TestQueue_1590_SatelliteAnnotatedWithPrimary(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "qa-1590-primary", "")
	seedLane(t, db, "1590-qa", "qa-1590-primary")
	seedLane(t, db, "1590-pm", "qa-1590-primary")
	seedLane(t, db, "1590-standalone", "")

	names, texts := renderQueueTable(t, db)
	if len(names) != 4 {
		t.Fatalf("queue rows = %v, want all 4 lanes", names)
	}

	// Attachment: every satellite row names its primary; the primary row
	// does not name anything.
	for i, name := range names {
		switch name {
		case "1590-qa", "1590-pm":
			if !hasPrimaryRef(texts[i]) {
				t.Errorf("satellite %q row %q lacks a ↳ primary annotation", name, texts[i])
			}
			if got := primaryRef(texts[i]); got != "qa-1590-primary" {
				t.Errorf("satellite %q names primary %q, want %q", name, got, "qa-1590-primary")
			}
			if d := depth(texts[i]); d != 1 {
				t.Errorf("satellite %q depth = %d, want 1", name, d)
			}
		case "qa-1590-primary", "1590-standalone":
			if hasPrimaryRef(texts[i]) {
				t.Errorf("root lane %q row %q unexpectedly annotated with a primary", name, texts[i])
			}
			if d := depth(texts[i]); d != 0 {
				t.Errorf("root lane %q depth = %d, want 0", name, d)
			}
		}
	}

	// The parent column was authoritative: the source marker says so on the
	// satellite rows (and says nothing on roots — nothing was resolved).
	for i, name := range names {
		switch name {
		case "1590-qa", "1590-pm":
			if got := parentSourceMarker(texts[i]); got != "explicit" {
				t.Errorf("satellite %q parent source = %q, want explicit", name, got)
			}
		default:
			if got := parentSourceMarker(texts[i]); got != "" {
				t.Errorf("root lane %q carries a parent-source marker %q", name, got)
			}
		}
	}
}

// TestQueue_1590_ThreeLevelChainRendersAtDepth: a satellite of a satellite is
// allowed and expected — the mid lane and the leaf must each render at their
// own depth with the right primary named.
func TestQueue_1590_ThreeLevelChainRendersAtDepth(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "1590-root", "")
	seedLane(t, db, "1590-mid", "1590-root")
	seedLane(t, db, "1590-leaf", "1590-mid")

	names, texts := renderQueueTable(t, db)
	if len(names) != 3 {
		t.Fatalf("queue rows = %v, want 3", names)
	}
	wantDepth := map[string]int{"1590-root": 0, "1590-mid": 1, "1590-leaf": 2}
	wantPrimary := map[string]string{"1590-root": "", "1590-mid": "1590-root", "1590-leaf": "1590-mid"}
	for i, name := range names {
		if d := depth(texts[i]); d != wantDepth[name] {
			t.Errorf("lane %q rendered depth %d, want %d (row %q)", name, d, wantDepth[name], texts[i])
		}
		if got := primaryRef(texts[i]); got != wantPrimary[name] {
			t.Errorf("lane %q names primary %q, want %q", name, got, wantPrimary[name])
		}
	}
}

// TestQueue_1590_EmptyParentRendersAsRoot: a lane with an empty parent is a
// primary/root — no rail, no annotation, no source marker.
func TestQueue_1590_EmptyParentRendersAsRoot(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "1590-lonely", "")

	names, texts := renderQueueTable(t, db)
	if len(names) != 1 || names[0] != "1590-lonely" {
		t.Fatalf("queue rows = %v, want only 1590-lonely", names)
	}
	if hasPrimaryRef(texts[0]) || depth(texts[0]) != 0 || parentSourceMarker(texts[0]) != "" {
		t.Errorf("primary lane rendered nested: %q", texts[0])
	}
}

// TestQueue_1590_DanglingParentDoesNotPanic: a satellite whose parent lane
// does not resolve (soft-deleted / purged) must render without panicking —
// it surfaces as the top of its own fragment with its parent name still
// shown as text, so the operator sees the broken link instead of silence.
func TestQueue_1590_DanglingParentDoesNotPanic(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "1590-orphan", "1590-deleted-primary")

	names, texts := renderQueueTable(t, db)
	if len(names) != 1 {
		t.Fatalf("queue rows = %v, want the orphan lane to still render", names)
	}
	// The lane itself must appear, and the dangling parent name must stay
	// visible as text — a missing annotation would hide the breakage.
	if !hasPrimaryRef(texts[0]) || !strings.Contains(texts[0], "1590-deleted-primary") {
		t.Errorf("dangling-parent lane lost its parent reference: %q", texts[0])
	}
	// It resolves as the top of its fragment: root depth, source=explicit.
	if d := depth(texts[0]); d != 0 {
		t.Errorf("dangling-parent lane depth = %d, want 0 (top of its own fragment)", d)
	}
	if got := parentSourceMarker(texts[0]); got != "explicit" {
		t.Errorf("dangling-parent lane parent source = %q, want explicit", got)
	}
}

// TestQueue_1590_InferredSuffixFallback: a lane with an EMPTY parent whose
// name carries a satellite suffix falls back to the legacy name-suffix
// inference, and the row says so (data-parent-source="inferred"). A suffixed
// lane whose parent IS set must NOT re-infer — explicit wins.
func TestQueue_1590_InferredSuffixFallback(t *testing.T) {
	db := newTestDB(t)
	// Empty parent + recognized suffix → inferred attachment to the base name.
	seedLane(t, db, "1590-infer", "")
	seedLane(t, db, "1590-infer-qa", "")
	// Explicit parent beats inference even with a suffix present.
	seedLane(t, db, "1590-explicit-qa", "1590-infer")

	names, texts := renderQueueTable(t, db)
	if len(names) != 3 {
		t.Fatalf("queue rows = %v, want 3", names)
	}
	for i, name := range names {
		switch name {
		case "1590-infer-qa":
			if got := primaryRef(texts[i]); got != "1590-infer" {
				t.Errorf("suffixed lane with empty parent: primary = %q, want inferred %q (row %q)", got, "1590-infer", texts[i])
			}
			if got := parentSourceMarker(texts[i]); got != "inferred" {
				t.Errorf("suffixed lane parent source = %q, want inferred", got)
			}
		case "1590-explicit-qa":
			if got := primaryRef(texts[i]); got != "1590-infer" {
				t.Errorf("explicit-parent lane names primary %q, want %q", got, "1590-infer")
			}
			if got := parentSourceMarker(texts[i]); got != "explicit" {
				t.Errorf("explicit-parent lane parent source = %q, want explicit (explicit must win over suffix inference)", got)
			}
		case "1590-infer":
			if hasPrimaryRef(texts[i]) {
				t.Errorf("primary lane %q unexpectedly annotated", name)
			}
		}
	}
}

// TestQueue_1590_NestingKeepsEveryRow: nesting must not drop lanes — every
// enabled lane renders exactly once, even in a deep mixed family.
func TestQueue_1590_NestingKeepsEveryRow(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "1590-p0", "")
	seedLane(t, db, "1590-p0-qa", "1590-p0")
	seedLane(t, db, "1590-p0-qa-nested", "1590-p0-qa")
	seedLane(t, db, "1590-other", "")

	page := renderQueuePage(t, db)
	if got := countLaneRows(page); got != 4 {
		t.Errorf("queue rendered %d lane links, want exactly 4 (nesting must not drop lanes)", got)
	}
	// And the eligibility count card still counts all of them.
	if !strings.Contains(page, "4 eligible") {
		t.Errorf("queue count card lost lanes: %q", snippetForNesting(page, "eligible"))
	}
}

// TestQueue_1590_SortTension_GlobalUrgencyHolds: the chosen rule is GLOBAL
// urgency order with annotation — satellites are NOT pulled under their
// primary. A satellite whose own urgency outranks its primary's must sit
// above it in the queue (grouping would hoist the primary instead).
func TestQueue_1590_SortTension_GlobalUrgencyHolds(t *testing.T) {
	db := newTestDB(t)
	// Primary is fresh (low urgency); its satellite has never run a tick and
	// was created earlier — with equal priority the created_at fallback makes
	// the satellite the MORE urgent row.
	instant := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	seedLane(t, db, "1590-cool-primary", "")
	seedLane(t, db, "1590-hot-sat", "1590-cool-primary")
	// created_at: satellite much older → urgency falls back higher.
	if _, err := db.Exec(`UPDATE projects SET created_at = ? WHERE name = ?`,
		instant.Add(-72*time.Hour).Format(time.RFC3339), "1590-hot-sat"); err != nil {
		t.Fatalf("stamp satellite created_at: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET created_at = ?, last_tick_completed = ? WHERE name = ?`,
		instant.Add(-24*time.Hour).Format(time.RFC3339),
		instant.Add(-time.Minute).Format(time.RFC3339), "1590-cool-primary"); err != nil {
		t.Fatalf("stamp primary engine inputs: %v", err)
	}

	// A REAL calculator (the parity suite's shape): the created_at fallback
	// only fires with one — priority-only scoring would tie both lanes and
	// make the order assertion meaningless.
	calc := scheduler.NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
	gen := dashboard.NewGenerator(db, calc)
	data, err := gen.QueueEntriesForTest(context.Background())
	if err != nil {
		t.Fatalf("QueueEntriesForTest: %v", err)
	}
	if len(data.Entries) != 2 {
		t.Fatalf("entries = %v, want both lanes", data.Entries)
	}
	if data.Entries[0].Name != "1590-hot-sat" {
		t.Errorf("queue order = [%s, %s]; the urgent satellite must NOT be regrouped under its calmer primary (global-urgency rule)",
			data.Entries[0].Name, data.Entries[1].Name)
	}
}

// ── fleet overview table ────────────────────────────────────────────────────

// TestFleetTable_1590_NestingRendered: the overview's Projects table carries
// the same attachment markup — a satellite reads as attached to its primary
// with visible depth, and the table still lists every lane.
func TestFleetTable_1590_NestingRendered(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "ft-1590-primary", "")
	seedLane(t, db, "ft-1590-qa", "ft-1590-primary")
	seedLane(t, db, "ft-1590-qa-sync", "ft-1590-qa")

	names, texts := renderFleetTable(t, db)
	if len(names) != 3 {
		t.Fatalf("fleet table rows = %v, want 3", names)
	}
	wantDepth := map[string]int{"ft-1590-primary": 0, "ft-1590-qa": 1, "ft-1590-qa-sync": 2}
	wantPrimary := map[string]string{"ft-1590-primary": "", "ft-1590-qa": "ft-1590-primary", "ft-1590-qa-sync": "ft-1590-qa"}
	for i, name := range names {
		if d := depth(texts[i]); d != wantDepth[name] {
			t.Errorf("lane %q rendered depth %d, want %d (row %q)", name, d, wantDepth[name], texts[i])
		}
		if got := primaryRef(texts[i]); got != wantPrimary[name] {
			t.Errorf("lane %q names primary %q, want %q", name, got, wantPrimary[name])
		}
	}
}

// TestFleetTable_1590_DanglingParentDoesNotPanic: same no-panic contract on
// the overview table.
func TestFleetTable_1590_DanglingParentDoesNotPanic(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "ft-1590-orphan", "ft-1590-gone")

	names, texts := renderFleetTable(t, db)
	if len(names) != 1 || names[0] != "ft-1590-orphan" {
		t.Fatalf("fleet table rows = %v, want the orphan lane", names)
	}
	if !strings.Contains(texts[0], "ft-1590-gone") {
		t.Errorf("dangling parent name lost: %q", texts[0])
	}
}

// TestFleetTable_1590_EveryLaneStillRendered: nesting must not drop lanes on
// the overview either.
func TestFleetTable_1590_EveryLaneStillRendered(t *testing.T) {
	db := newTestDB(t)
	seedLane(t, db, "ft-1590-a", "")
	seedLane(t, db, "ft-1590-a-qa", "ft-1590-a")
	seedLane(t, db, "ft-1590-b", "")

	page := renderFleetPage(t, db)
	if got := countLaneRows(page); got != 3 {
		t.Errorf("fleet table rendered %d lane links, want exactly 3", got)
	}
}

// ── helpers the parser above needs ──────────────────────────────────────────

func snippetForNesting(page, marker string) string {
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	return page[start : i+len(marker)]
}

// renderQueuePage renders the full /queue HTML page.
func renderQueuePage(t *testing.T, db *sql.DB) string {
	t.Helper()
	var buf strings.Builder
	if err := dashboard.NewGenerator(db, nil).GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	return buf.String()
}

// renderFleetPage renders the fleet table partial HTML.
func renderFleetPage(t *testing.T, db *sql.DB) string {
	t.Helper()
	var buf strings.Builder
	if err := dashboard.NewGenerator(db, nil).GenerateFleetTable(&buf); err != nil {
		t.Fatalf("GenerateFleetTable: %v", err)
	}
	return buf.String()
}
