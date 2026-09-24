package dashboard_test

// SCHED-GAP-1596 — Fleet Tape tests. The page's contract, in order:
//   1. it renders from REAL seeded data (no invented figures),
//   2. it never references the wedged endpoints (/api/v1/status,
//      /api/v1/queue) and adds no poll against them,
//   3. its htmx fragment is rows-only and byte-matches the page's rows,
//   4. the derived-quote math (Δ, $/hour, settle, symbols, sparklines) is
//      correct on known seeds.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// mustSeedTick creates a tick row with full quote inputs (status, outcome,
// cost, spawn time) — mustCreateTick above seeds bare completed ticks only.
func mustSeedTick(t *testing.T, db *sql.DB, id, project string, spawnedAt time.Time, status database.TickStatus, outcome database.TickOutcome, cost float64) {
	t.Helper()
	tk := &database.Tick{
		ID:          id,
		ProjectName: project,
		Status:      status,
		SpawnedAt:   spawnedAt.UTC().Format(time.RFC3339),
		CostUSD:     cost,
	}
	if outcome != "" {
		tk.Outcome = outcome
	}
	if status == database.StatusCompleted {
		tk.CompletedAt = spawnedAt.UTC().Add(20 * time.Minute).Format(time.RFC3339)
	}
	if err := database.CreateTick(context.Background(), db, tk); err != nil {
		t.Fatalf("CreateTick %s: %v", id, err)
	}
}

// seedTapeFleet: one live lane with a real 24h/prior-24h history and one
// disabled lane that never ticked. Alpha: 2 completed ticks in the last 24h
// ($1.00 + $6.00) and 1 completed tick 30h ago → move ▲100%, $7/24h.
func seedTapeFleet(t *testing.T, db *sql.DB) {
	t.Helper()
	mustCreateProject(t, db, "alpha", 10, 5)
	mustCreateProject(t, db, "dead-lane", 10, 2)
	if _, err := db.ExecContext(context.Background(), `UPDATE projects SET enabled=0 WHERE name='dead-lane'`); err != nil {
		t.Fatalf("disable dead-lane: %v", err)
	}
	now := time.Now()
	mustSeedTick(t, db, "t-a1", "alpha", now.Add(-3*time.Hour), database.StatusCompleted, database.OutcomeCommitted, 1.00)
	mustSeedTick(t, db, "t-a2", "alpha", now.Add(-9*time.Hour), database.StatusCompleted, database.OutcomeCommitted, 6.00)
	mustSeedTick(t, db, "t-a0", "alpha", now.Add(-30*time.Hour), database.StatusCompleted, database.OutcomeCommitted, 0.50)
}

// hangingEndpoints are the two API routes that currently wedge; the tape
// must not reference either (a reference invites a browser poll that hangs).
const hangingEndpoints = "/api/v1/status"

var tapeForbidden = []string{hangingEndpoints, "/api/v1/queue"}

func TestGenerateTape_RendersRealSeededData(t *testing.T) {
	db := newTestDB(t)
	seedTapeFleet(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateTape(&buf); err != nil {
		t.Fatalf("GenerateTape: %v", err)
	}
	out := buf.String()

	// ── Data honesty: no reference to a hanging endpoint, anywhere. ──
	for _, bad := range tapeForbidden {
		if strings.Contains(out, bad) {
			t.Errorf("tape references wedged endpoint %q — the data path must never hang behind it", bad)
		}
	}

	// ── Index bar (fleet as one instrument). ──
	for _, want := range []string{
		"HERMES-X",
		"2 lanes open", // 1 enabled + 1 disabled
		"ticks 24h",    // index row labels
		"▲ 100.0%",     // 2 ticks vs 1 prior — one-decimal index move
		"$7",           // notional: 1.00+6.00 = $7 (commaGroup format)
		"100.0%",       // fleet settle: 2/2 completed
	} {
		if !strings.Contains(out, want) {
			t.Errorf("index bar missing %q", want)
		}
	}

	// ── Market board: real per-lane quotes. ──
	// alpha → HERCA symbol, drill-down link, $/hour from real cost.
	if !strings.Contains(out, `href="/projects/alpha"`) {
		t.Errorf("board row must link to the lane detail page")
	}
	if !strings.Contains(out, "ALPHA") {
		t.Errorf("expected ticker symbol ALPHA for alpha")
	}
	if !strings.Contains(out, "$0.292/h") { // $7.00 / 24h, sub-dollar precision
		t.Errorf("expected $/hour quote $0.292/h from the seeded $7.00")
	}
	if !strings.Contains(out, "▲ 100%") { // lane move, whole-percent
		t.Errorf("expected lane move ▲ 100%%")
	}
	if !strings.Contains(out, "0.08") { // 2 ticks / 24h
		t.Errorf("expected ticks/h 0.08")
	}
	// Sparkline: 14-bucket SVG polyline rendered from real tick times.
	if !strings.Contains(out, `viewBox="0 0 100 22"`) || !strings.Contains(out, `<polyline points="`) {
		t.Errorf("expected sparkline polylines on the board")
	}
	// States: live lane OPEN, disabled lane SUSPENDED with a dimmed row.
	if !strings.Contains(out, `class="t-pill live">OPEN<`) {
		t.Errorf("expected an OPEN state pill for alpha")
	}
	if !strings.Contains(out, `class="t-pill halt">SUSPENDED<`) {
		t.Errorf("expected a SUSPENDED state pill for dead-lane")
	}
	if !strings.Contains(out, `<tr class="halt">`) {
		t.Errorf("expected the suspended lane's row to carry the halt class")
	}

	// ── Structure parity with the approved mockup. ──
	for _, want := range []string{`class="idx"`, `class="tape"`, `class="tape-board"`, "Fleet board — 24h", `class="reel"`} {
		if !strings.Contains(out, want) {
			t.Errorf("mockup structure missing: %q", want)
		}
	}

	// ── Nav: reachable from the sidebar, marked active on its own page. ──
	if !strings.Contains(out, `href="/tape" class="active"`) {
		t.Errorf("expected the sidebar entry for /tape to render active")
	}

	// ── Feed wiring: SSE + shared auto-refresh fallback, no private timer. ──
	for _, want := range []string{
		"/api/v1/events/stream", // real push feed
		"EventSource",
		"tape-sse",              // SSE pokes this throttled body event
		"autorefresh from:body", // fallback cadence = the shared layout event
		`hx-get="/tape"`,        // htmx refreshes the rows fragment, not the page
	} {
		if !strings.Contains(out, want) {
			t.Errorf("feed wiring missing: %q", want)
		}
	}
}

// TestGenerateTapeRows_FragmentIsRowsOnly mirrors the existing htmx fragment
// tests: the HX-Request render is tbody children only — no page chrome, no
// nested tbody (the page's tbody swaps innerHTML).
func TestGenerateTapeRows_FragmentIsRowsOnly(t *testing.T) {
	db := newTestDB(t)
	seedTapeFleet(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateTapeRows(&buf); err != nil {
		t.Fatalf("GenerateTapeRows: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "<tbody") {
		t.Errorf("rows fragment must NOT emit a tbody wrapper (htmx swaps innerHTML); got: %q", snippet(out, "tbody"))
	}
	if strings.Contains(out, "<!DOCTYPE html>") || strings.Contains(out, "<title>") {
		t.Errorf("rows fragment must not contain page chrome")
	}
	if !strings.Contains(out, "<tr") || !strings.Contains(out, `href="/projects/alpha"`) {
		t.Errorf("rows fragment missing seeded board rows")
	}
	for _, bad := range tapeForbidden {
		if strings.Contains(out, bad) {
			t.Errorf("rows fragment references wedged endpoint %q", bad)
		}
	}
}

// TestGenerateTape_FragmentMatchesPageRows pins the two render paths to one
// markup source: the fragment must appear verbatim inside the full page.
func TestGenerateTape_FragmentMatchesPageRows(t *testing.T) {
	db := newTestDB(t)
	seedTapeFleet(t, db)
	gen := dashboard.NewGenerator(db, nil)

	var page, frag strings.Builder
	if err := gen.GenerateTape(&page); err != nil {
		t.Fatalf("GenerateTape: %v", err)
	}
	if err := gen.GenerateTapeRows(&frag); err != nil {
		t.Fatalf("GenerateTapeRows: %v", err)
	}
	if frag.Len() == 0 {
		t.Fatal("rows fragment is empty")
	}
	if !strings.Contains(page.String(), frag.String()) {
		t.Errorf("page rows and htmx fragment diverged — the two paths must render identical markup")
	}
}

// TestGenerateTape_EmptyDatabase renders honestly with no lanes: zero counts,
// flat index, no rows.
func TestGenerateTape_EmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	var buf strings.Builder
	if err := gen.GenerateTape(&buf); err != nil {
		t.Fatalf("GenerateTape: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "0 lanes open") {
		t.Errorf("expected 0 lanes open on an empty fleet")
	}
	if strings.Contains(out, "<tr class=") {
		t.Errorf("empty fleet must render no board rows")
	}
	for _, bad := range tapeForbidden {
		if strings.Contains(out, bad) {
			t.Errorf("tape references wedged endpoint %q", bad)
		}
	}
}

// TestTapeSymbol pins the deterministic symbol derivation.
func TestTapeSymbol(t *testing.T) {
	cases := map[string]string{
		"hermes-canopy":       "HERMY", // first-4 + last-1 compression (herm + y)
		"coding-hermes-tools": "CODIS",
		"h3":                  "H3",
		"ai-plays-poke":       "AIPLE", // AIPLAYSPOKE → first4 + last1
		"__":                  "???",   // nothing alphanumeric → never empty
	}
	for in, want := range cases {
		if got := dashboard.TapeSymbolForTest(in); got != want {
			t.Errorf("TapeSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTapeMoveAndIndexLabels pin the Δ quote math on known pairs.
func TestTapeMoveAndIndexLabels(t *testing.T) {
	cases := []struct {
		curr, prior int64
		label, cls  string
	}{
		{308, 211, "▲ 46.0%", "up"}, // the mockup's index move
		{2, 1, "▲ 100.0%", "up"},
		{1, 2, "▼ 50.0%", "down"},
		{5, 0, "NEW", "up"},
		{0, 0, "— flat", "flat"},
	}
	for _, c := range cases {
		label, cls := dashboard.TapeIndexMoveForTest(c.curr, c.prior)
		if label != c.label || cls != c.cls {
			t.Errorf("tapeIndexMove(%d,%d) = (%q,%q), want (%q,%q)", c.curr, c.prior, label, cls, c.label, c.cls)
		}
	}
}
