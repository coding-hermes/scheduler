package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// queueParityConfig is the resolved interval range both surfaces derive their
// urgency calculator from — the same shape main.go snapshots into
// api.ResolvedConfig at daemon boot.
var queueParityConfig = api.ResolvedConfig{
	MinInterval: "30s",
	MaxInterval: "24h",
	NumLevels:   10,
}

// queueParityCalc builds the calculator from the resolved config exactly the
// way the API server does (newUrgencyCalculatorFromConfig: time.ParseDuration
// of the two resolved interval strings, then scheduler.NewUrgencyCalculator).
func queueParityCalc(t *testing.T) *scheduler.UrgencyCalculator {
	t.Helper()
	minI, err := time.ParseDuration(queueParityConfig.MinInterval)
	if err != nil {
		t.Fatalf("parse min interval %q: %v", queueParityConfig.MinInterval, err)
	}
	maxI, err := time.ParseDuration(queueParityConfig.MaxInterval)
	if err != nil {
		t.Fatalf("parse max interval %q: %v", queueParityConfig.MaxInterval, err)
	}
	return scheduler.NewUrgencyCalculator(minI, maxI, queueParityConfig.NumLevels)
}

// apiQueueRow mirrors the JSON shape of one /api/v1/queue entry.
type apiQueueRow struct {
	Project   string  `json:"project"`
	Urgency   float64 `json:"urgency"`
	Weight    int     `json:"weight"`
	Priority  int     `json:"priority"`
	CooldownS int     `json:"cooldown_s"`
	Enabled   bool    `json:"enabled"`
}

// parityProject is one fixture project: the engine inputs the queue ranks on
// (priority, decay_rate, created_at, last_tick_completed) plus the latest tick
// spawned_at the RETIRED dashboard formula used to rank on instead.
type parityProject struct {
	name         string
	priority     int
	decayRate    float64
	createdAgo   time.Duration // before the frozen instant
	lastDoneAgo  time.Duration // before the frozen instant; 0 = never completed
	hasLastDone  bool
	lastSpawnAgo time.Duration // latest rows in the ticks table; 0 = none
	hasTick      bool
	enabled      bool
}

// seedParityProject inserts the fixture row and stamps the exact engine
// timestamps (whole seconds, so the RFC3339 round trip is lossless).
func seedParityProject(t *testing.T, db *sql.DB, instant time.Time, p parityProject) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      p.name,
		RepoURL:   "https://example.com/" + p.name,
		Workdir:   "/tmp/" + p.name,
		Weight:    10,
		Priority:  p.priority,
		CooldownS: 900,
		DecayRate: p.decayRate,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", p.name, err)
	}

	lastDone := ""
	if p.hasLastDone {
		lastDone = instant.Add(-p.lastDoneAgo).Format(time.RFC3339)
	}
	if _, err := db.Exec(
		`UPDATE projects SET created_at = ?, last_tick_completed = ?, decay_rate = ?, enabled = ? WHERE name = ?`,
		instant.Add(-p.createdAgo).Format(time.RFC3339), lastDone, p.decayRate, boolToInt(p.enabled), p.name,
	); err != nil {
		t.Fatalf("stamp %s engine inputs: %v", p.name, err)
	}
	if p.hasTick {
		if err := database.CreateTick(context.Background(), db, &database.Tick{
			ID:          p.name + "-tick",
			ProjectName: p.name,
			Status:      database.StatusCompleted,
			SpawnedAt:   instant.Add(-p.lastSpawnAgo).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("CreateTick %s: %v", p.name, err)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fetchAPIQueue drives the real HTTP surface (GET /api/v1/queue) against the
// fixture, on the given clock and resolved config, and returns the rows in
// wire order.
func fetchAPIQueue(t *testing.T, db *sql.DB, clk clock.Clock, cfg *api.ResolvedConfig) []apiQueueRow {
	t.Helper()
	srv := api.NewServer(db, nil)
	srv.SetClock(clk)
	if cfg != nil {
		srv.SetResolvedConfig(*cfg)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/queue = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Queue []apiQueueRow `json:"queue"`
		Count int           `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /api/v1/queue: %v (body %q)", err, rec.Body.String())
	}
	if payload.Count != len(payload.Queue) {
		t.Errorf("/api/v1/queue count = %d but returned %d rows", payload.Count, len(payload.Queue))
	}
	return payload.Queue
}

var queueRowLink = regexp.MustCompile(`<a href="/projects/([^"]+)">`)

// queueTableOrder returns the project names of the rendered /queue page in
// document order — i.e. exactly the order an operator reads off the screen.
func queueTableOrder(t *testing.T, page string) []string {
	t.Helper()
	start := strings.Index(page, "<tbody>")
	end := strings.Index(page, "</tbody>")
	if start < 0 || end < start {
		head := page
		if len(head) > 400 {
			head = head[:400]
		}
		t.Fatalf("queue page has no usable <tbody> table body; page starts: %q", head)
	}
	matches := queueRowLink.FindAllStringSubmatch(page[start:end], -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// dashboardQueue builds the dashboard generator the way main.go does (same
// calculator, same clock) and returns both the slices that feed the ordering
// and the order the HTML actually renders.
func dashboardQueue(t *testing.T, db *sql.DB, clk clock.Clock, calc *scheduler.UrgencyCalculator) ([]QueueEntry, []string) {
	t.Helper()
	g := NewGenerator(db, calc)
	g.SetClock(clk)
	data, err := g.queueEntries(context.Background())
	if err != nil {
		t.Fatalf("queueEntries: %v", err)
	}
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	return data.Entries, queueTableOrder(t, buf.String())
}

func queueNames(rows []apiQueueRow) []string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Project)
	}
	return names
}

func entryNames(entries []QueueEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// retiredDashboardUrgency reproduces the formula this row removed from
// /queue — a fixed multiplier on priority, then a linear ramp from the latest
// TICK's spawned_at (falling back to the flat multiplier with no tick). It
// exists only to prove the fixture is non-vacuous: the retired formula's
// order must differ from the agreed one, otherwise the test would pass even
// with the two formulas still split.
func retiredDashboardUrgency(p parityProject) float64 {
	if !p.hasTick {
		return float64(p.priority) * 10.0
	}
	return float64(p.priority) * (1 + p.lastSpawnAgo.Hours())
}

func orderNamesByScore(names []string, score func(string) float64) []string {
	out := append([]string(nil), names...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && score(out[j]) > score(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestQueueSurface_HTMLUrgencyEqualsAPIUrgency is the SCHED-GAP-174 parity
// gate. Both queue surfaces are built over ONE fixture and ONE frozen instant
// and must agree per project on the urgency value and on the rendered order:
//
//	dashboard /queue (HTML row order + the slices feeding it) == /api/v1/queue
//
// The fixture is deliberately the shape the bug was measured on: a project
// whose tick history is stale but whose engine score is high, a project with
// decay_rate < 1, a project that has never completed a tick (created_at
// fallback), and a disabled project that both surfaces must ignore.
func TestQueueSurface_HTMLUrgencyEqualsAPIUrgency(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// One frozen decision instant for BOTH surfaces: each ranks through its
	// own clock seam, so "the same instant" is only testable when they share
	// the clock.
	instant := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFixed(instant)

	fixture := []parityProject{
		// Overdue high-priority project: engine score is dominated by elapsed
		// time since the completed tick.
		{name: "hot", priority: 10, decayRate: 1.0, createdAgo: 7 * 24 * time.Hour, lastDoneAgo: 3 * time.Hour, hasLastDone: true, lastSpawnAgo: 3 * time.Hour, hasTick: true, enabled: true},
		// Just-completed project: barely past its 30s interval.
		{name: "fresh", priority: 10, decayRate: 1.0, createdAgo: 7 * 24 * time.Hour, lastDoneAgo: 30 * time.Second, hasLastDone: true, lastSpawnAgo: 30 * time.Second, hasTick: true, enabled: true},
		// decay_rate 0.5: the retired dashboard formula ignored decay entirely.
		{name: "decaying", priority: 5, decayRate: 0.5, createdAgo: 30 * 24 * time.Hour, lastDoneAgo: 12 * time.Hour, hasLastDone: true, lastSpawnAgo: 12 * time.Hour, hasTick: true, enabled: true},
		// Never completed a tick: urgency falls back to created_at.
		{name: "never-ran", priority: 7, decayRate: 1.0, createdAgo: time.Hour, hasLastDone: false, hasTick: false, enabled: true},
		// Stale tick history: the retired formula ranked this one FIRST, the
		// engine ranks it last. This row is why the two surfaces disagreed.
		{name: "stale-ticks", priority: 2, decayRate: 1.0, createdAgo: 60 * 24 * time.Hour, lastDoneAgo: 30 * time.Minute, hasLastDone: true, lastSpawnAgo: 400 * time.Hour, hasTick: true, enabled: true},
		// Disabled: neither surface may list it.
		{name: "paused", priority: 10, decayRate: 1.0, createdAgo: 7 * 24 * time.Hour, lastDoneAgo: 3 * time.Hour, hasLastDone: true, hasTick: false, enabled: false},
	}
	for _, p := range fixture {
		seedParityProject(t, db, instant, p)
	}

	calc := queueParityCalc(t)
	apiRows := fetchAPIQueue(t, db, clk, &queueParityConfig)
	entries, htmlOrder := dashboardQueue(t, db, clk, calc)

	if len(apiRows) != 5 {
		t.Fatalf("/api/v1/queue returned %d rows, want the 5 enabled fixture projects (got %v)", len(apiRows), queueNames(apiRows))
	}
	if len(entries) != len(apiRows) {
		t.Fatalf("dashboard /queue has %d entries, /api/v1/queue has %d (%v vs %v)", len(entries), len(apiRows), entryNames(entries), queueNames(apiRows))
	}

	// (a) Per project: the urgency value the dashboard orders by equals the
	// value the API reports, within float64 rounding. Both surfaces compute
	// from one calculator over one instant, so this is exact arithmetic —
	// the bound only tolerates representation differences.
	dashUrgency := make(map[string]float64, len(entries))
	for _, e := range entries {
		dashUrgency[e.Name] = e.Urgency
	}
	for _, row := range apiRows {
		du, ok := dashUrgency[row.Project]
		if !ok {
			t.Errorf("dashboard /queue is missing project %q (API has it at urgency %v)", row.Project, row.Urgency)
			continue
		}
		if diff := math.Abs(du - row.Urgency); diff > 1e-9*math.Max(1, math.Abs(row.Urgency)) {
			t.Errorf("urgency mismatch for %s: dashboard /queue = %.10f, /api/v1/queue = %.10f (diff %g)",
				row.Project, du, row.Urgency, diff)
		}
	}
	apiNames := make(map[string]bool, len(apiRows))
	for _, row := range apiRows {
		apiNames[row.Project] = true
	}
	for name := range dashUrgency {
		if !apiNames[name] {
			t.Errorf("dashboard /queue lists %q but /api/v1/queue does not", name)
		}
	}

	// (b) Order: the slices feeding the dashboard, the HTML it renders, and
	// the API array are one ordering.
	apiOrder := queueNames(apiRows)
	if got := entryNames(entries); !equalStrings(got, apiOrder) {
		t.Errorf("dashboard entry order = %v, /api/v1/queue order = %v", got, apiOrder)
	}
	if !equalStrings(htmlOrder, apiOrder) {
		t.Errorf("/queue HTML row order = %v, /api/v1/queue order = %v", htmlOrder, apiOrder)
	}

	// (c) Non-vacuity: the fixture must be one the OLD dashboard formula
	// answered differently, otherwise this test could pass with the two
	// formulas still split. The engine order must also be urgency-descending
	// (not merely priority-descending), which the fixture mixes (priority 7
	// outranks priority 5 and priority 2 outranks nothing).
	oldOrder := orderNamesByScore(entryNames(entries), func(name string) float64 {
		for _, p := range fixture {
			if p.name == name {
				return retiredDashboardUrgency(p)
			}
		}
		return 0
	})
	if equalStrings(oldOrder, apiOrder) {
		t.Fatalf("fixture is degenerate: the retired dashboard formula produces the same order as /api/v1/queue (%v) — this test could not detect the split surfaces", apiOrder)
	}
	if apiOrder[0] != "hot" {
		t.Errorf("first queue row = %q, want %q (highest engine urgency: priority 10 overdue by 3h)", apiOrder[0], "hot")
	}
	if last := apiOrder[len(apiOrder)-1]; last != "stale-ticks" {
		t.Errorf("last queue row = %q, want %q (priority 2, recently completed despite a 400h-old tick row)", last, "stale-ticks")
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Urgency > entries[i-1].Urgency {
			t.Errorf("dashboard queue not urgency-descending at %d: %s(%.6f) > %s(%.6f)",
				i, entries[i].Name, entries[i].Urgency, entries[i-1].Name, entries[i-1].Urgency)
		}
	}
	// The retired formula ranked stale-ticks first; the engine ranks it last.
	if oldOrder[0] != "stale-ticks" {
		t.Fatalf("fixture drift: the retired formula no longer ranks stale-ticks first (got %v) — re-derive the fixture before trusting the parity assertions", oldOrder)
	}
}

// TestQueueSurface_PriorityOnlyFallbackParity pins the other half of the
// contract: when NO calculator is configured (a daemon with an unparseable
// interval range), the API scores priority-only — and the dashboard must
// mirror that fallback rather than invent its own number, or the surfaces
// would silently split again on exactly the configuration where nobody is
// watching.
func TestQueueSurface_PriorityOnlyFallbackParity(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	instant := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFixed(instant)

	// Distinct priorities: the fallback score IS the priority, so a tie would
	// make the ordering claim meaningless.
	fixture := []parityProject{
		{name: "p9", priority: 9, decayRate: 1.0, createdAgo: 48 * time.Hour, lastDoneAgo: time.Hour, hasLastDone: true, hasTick: true, lastSpawnAgo: time.Hour, enabled: true},
		{name: "p5", priority: 5, decayRate: 1.0, createdAgo: 48 * time.Hour, lastDoneAgo: 30 * time.Minute, hasLastDone: true, hasTick: true, lastSpawnAgo: 30 * time.Minute, enabled: true},
		{name: "p2", priority: 2, decayRate: 1.0, createdAgo: 48 * time.Hour, lastDoneAgo: 20 * time.Hour, hasLastDone: true, hasTick: true, lastSpawnAgo: 20 * time.Hour, enabled: true},
	}
	for _, p := range fixture {
		seedParityProject(t, db, instant, p)
	}

	apiRows := fetchAPIQueue(t, db, clk, nil) // no SetResolvedConfig → nil calculator
	entries, htmlOrder := dashboardQueue(t, db, clk, nil)

	apiOrder := queueNames(apiRows)
	if got := entryNames(entries); !equalStrings(got, apiOrder) {
		t.Errorf("fallback: dashboard entry order = %v, /api/v1/queue order = %v", got, apiOrder)
	}
	if !equalStrings(htmlOrder, apiOrder) {
		t.Errorf("fallback: /queue HTML row order = %v, /api/v1/queue order = %v", htmlOrder, apiOrder)
	}
	for _, row := range apiRows {
		if row.Urgency != float64(row.Priority) {
			t.Fatalf("/api/v1/queue fallback urgency for %s = %v, want priority %d — the API fallback changed, re-derive this test", row.Project, row.Urgency, row.Priority)
		}
	}
	for _, e := range entries {
		if e.Urgency != float64(e.Priority) {
			t.Errorf("fallback: dashboard urgency for %s = %v, want priority-only %d", e.Name, e.Urgency, e.Priority)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
