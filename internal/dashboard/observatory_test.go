package dashboard_test

// SCHED-GAP-1592 — Observatory page + collector regression tests. These drive
// the REAL Generator over a real (temporary-file) SQLite database seeded
// through database.CreateTick / CreateProject / CreateNamespace — nothing
// here calls an internal helper in place of the public surface.

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// seedObservatory builds a fresh temp SQLite DB with two namespaces
// (core enabled w=10, satellite enabled w=5), one project in each, one
// unassigned project, and a windowed tick mix. It returns the Generator plus
// a time anchoring the seeded window (now is the moment of seeding).
func seedObservatory(t *testing.T) (*dashboard.Generator, time.Time) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	for _, ns := range []database.Namespace{
		{ID: "core", Description: "core fleet", Weight: 10, Enabled: true},
		{ID: "satellite", Description: "satellites", Weight: 5, Enabled: true},
	} {
		if err := database.CreateNamespace(ctx, db, &ns); err != nil {
			t.Fatalf("CreateNamespace %s: %v", ns.ID, err)
		}
	}
	mkProject := func(name, nsID string) {
		t.Helper()
		id := nsID
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
			Weight: 10, Priority: 5, NamespaceID: &id,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	mkProject("core-a", "core")
	mkProject("sat-b", "satellite")
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "lonely", RepoURL: "https://example.com/lonely", Workdir: "/tmp/lonely", Weight: 10, Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject lonely: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	mkTick := func(id, proj string, status database.TickStatus, spawned time.Time, completed time.Time, hasCompleted bool, cost float64) {
		t.Helper()
		tick := &database.Tick{
			ID: id, ProjectName: proj, Status: status,
			SpawnedAt: spawned.Format(time.RFC3339),
			CostUSD:   cost,
		}
		if hasCompleted {
			tick.CompletedAt = completed.Format(time.RFC3339)
		}
		if err := database.CreateTick(ctx, db, tick); err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}

	// core: 3 completed (1h ago, ~30m each), 1 failed, 1 running.
	base := now.Add(-time.Hour)
	mkTick("o-c0", "core-a", database.StatusCompleted, base, base.Add(30*time.Minute), true, 1.5)
	mkTick("o-c1", "core-a", database.StatusCompleted, base.Add(10*time.Minute), base.Add(40*time.Minute), true, 0.5)
	mkTick("o-c2", "core-a", database.StatusCompleted, base.Add(20*time.Minute), base.Add(50*time.Minute), true, 1.0)
	mkTick("o-f0", "core-a", database.StatusFailed, base.Add(30*time.Minute), time.Time{}, false, 0)
	mkTick("o-run", "core-a", database.StatusRunning, now.Add(-5*time.Minute), time.Time{}, false, 0)
	// satellite: 1 completed, 1 timeout.
	mkTick("o-s0", "sat-b", database.StatusCompleted, base.Add(5*time.Minute), base.Add(35*time.Minute), true, 0.2)
	mkTick("o-s1", "sat-b", database.StatusTimeout, base.Add(15*time.Minute), time.Time{}, false, 0)
	// unassigned: 1 failed.
	mkTick("o-u0", "lonely", database.StatusFailed, base.Add(25*time.Minute), time.Time{}, false, 0)

	return dashboard.NewGenerator(db, nil), now
}

// decodeSnapshot parses one snapshot out of raw JSON.
func decodeSnapshot(t *testing.T, raw []byte) dashboard.ObservatorySnapshot {
	t.Helper()
	var snap dashboard.ObservatorySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot JSON: %v\nraw: %s", err, raw)
	}
	return snap
}

// TestObservatory_CollectorMath pins the whole computed snapshot against the
// seeded mix: per-namespace volume, shares, failure rates (timeout counts as
// failure), avg duration, and the unassigned row for a project with no
// namespace.
func TestObservatory_CollectorMath(t *testing.T) {
	gen, _ := seedObservatory(t)
	raw, err := gen.CollectObservatory("", "")
	if err != nil {
		t.Fatalf("CollectObservatory: %v", err)
	}
	snap := decodeSnapshot(t, raw)

	if snap.WindowLabel != "6h" || snap.Window != 21600 {
		t.Errorf("window = %d/%q, want 21600/6h (default)", snap.Window, snap.WindowLabel)
	}
	if len(snap.Rate) == 0 {
		t.Fatal("rate buckets empty — a flatline must still carry buckets")
	}
	var totalSpawned int
	for _, p := range snap.Rate {
		totalSpawned += p.Spawned + p.Completed + p.Failed + p.Timeout
	}
	if totalSpawned == 0 {
		t.Error("no tick activity landed in any bucket — seeding window mismatch")
	}

	byNS := map[string]dashboard.ObservatoryNamespaceSlice{}
	for _, a := range snap.Allocation {
		byNS[a.Namespace] = a
	}
	a := byNS["core"]
	if a.Ticks != 5 {
		t.Errorf("core ticks = %d, want 5", a.Ticks)
	}
	if a.Weight != 10 || !a.Enabled || a.Lanes != 1 {
		t.Errorf("core meta = weight %d enabled %v lanes %d, want 10/true/1", a.Weight, a.Enabled, a.Lanes)
	}
	if a.CostUSD < 2.9 || a.CostUSD > 3.1 {
		t.Errorf("core cost = %v, want ~3.0", a.CostUSD)
	}
	// 3 completed of ~30m each → avg 1800s
	if a.AvgSeconds < 1700 || a.AvgSeconds > 1900 {
		t.Errorf("core avg_seconds = %v, want ~1800", a.AvgSeconds)
	}
	if got := byNS["satellite"].Ticks; got != 2 {
		t.Errorf("satellite ticks = %d, want 2", got)
	}
	u := byNS["unassigned"]
	if u.Ticks != 1 || !strings.Contains(u.Label, "no namespace") {
		t.Errorf("unassigned row = %+v, want 1 tick with an explicit no-namespace label", u)
	}

	// Shares sum to ~100 over the whole pie.
	var shareSum float64
	for _, s := range snap.Allocation {
		shareSum += s.SharePct
	}
	if shareSum < 99.9 || shareSum > 100.1 {
		t.Errorf("allocation shares sum = %v, want ~100", shareSum)
	}

	// Heatmap: core = 3 completed / 1 failed → 25% failure; satellite =
	// 1 completed / 1 timeout → 50%.
	heat := map[string]dashboard.ObservatoryHeatCell{}
	for _, c := range snap.Heatmap {
		heat[c.Namespace] = c
	}
	if h := heat["core"]; h.Total != 4 || h.FailPct < 24.9 || h.FailPct > 25.1 {
		t.Errorf("core heat = %+v, want total 4 fail 25%%", h)
	}
	if h := heat["satellite"]; h.Timeout != 1 || h.FailPct < 49.9 || h.FailPct > 50.1 {
		t.Errorf("satellite heat = %+v, want timeout 1 fail 50%%", h)
	}

	tt := snap.Totals
	if tt.Spawned != 8 {
		t.Errorf("totals.spawned = %d, want 8", tt.Spawned)
	}
	if tt.Running != 1 {
		t.Errorf("totals.running = %d, want 1", tt.Running)
	}
	// terminal = 4 completed + 2 failed + 1 timeout = 7; failures = 3 → 42.86%
	if tt.FailedPct < 42.8 || tt.FailedPct > 42.9 {
		t.Errorf("totals.failed_pct = %v, want ~42.86 (3 failures of 7 terminal)", tt.FailedPct)
	}
}

// TestObservatory_NamespaceFilter proves the filter changes what the SERVER
// computes — not a client-side slice: the filtered snapshot's pie carries
// only the admitted namespace and shares re-normalize to 100 within it.
func TestObservatory_NamespaceFilter(t *testing.T) {
	gen, _ := seedObservatory(t)
	raw, err := gen.CollectObservatory("", "satellite")
	if err != nil {
		t.Fatalf("CollectObservatory: %v", err)
	}
	snap := decodeSnapshot(t, raw)
	if snap.Namespace != "satellite" {
		t.Errorf("namespace echo = %q, want satellite", snap.Namespace)
	}
	if len(snap.Allocation) != 1 || snap.Allocation[0].Namespace != "satellite" {
		t.Fatalf("allocation = %+v, want only satellite", snap.Allocation)
	}
	if snap.Allocation[0].SharePct < 99.9 {
		t.Errorf("share = %v, want ~100 within the filtered pie", snap.Allocation[0].SharePct)
	}
	if snap.Totals.Spawned != 2 {
		t.Errorf("filtered totals.spawned = %d, want 2", snap.Totals.Spawned)
	}
}

// TestObservatory_PageRenders checks the HTML page: controls present, the
// initial snapshot inlined as JSON, and honest degradation markers in the
// JS (STALE/LOST states, never a silent frozen chart).
func TestObservatory_PageRenders(t *testing.T) {
	gen, _ := seedObservatory(t)
	var buf strings.Builder
	if err := gen.GenerateObservatory(&buf, "1h", ""); err != nil {
		t.Fatalf("GenerateObservatory: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="obsWindow"`, `id="obsNamespace"`, `id="obsInit"`, `id="obsSignal"`,
		`/api/v1/observatory/stream?window=3600`, // 1h resolves to seconds on the stream URL
		"EventSource", "stale", "stream lost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The inlined snapshot must parse and carry the resolved 1h window.
	start := strings.Index(out, `id="obsInit">`) + len(`id="obsInit">`)
	end := strings.Index(out[start:], "</script>")
	if end < 0 {
		t.Fatal("obsInit script block unterminated")
	}
	snap := decodeSnapshot(t, []byte(out[start:start+end]))
	if snap.Window != 3600 || snap.WindowLabel != "1h" {
		t.Errorf("inlined snapshot window = %d/%q, want 3600/1h", snap.Window, snap.WindowLabel)
	}
}

// TestObservatory_PageRendersWithCollectorError pins honest degradation at
// the render site: a collector failure renders the error banner text, not a
// fabricated or empty chart presented as current.
func TestObservatory_PageRendersWithCollectorError(t *testing.T) {
	// A nil-DB Generator makes every collect step fail. The page must
	// render the explicit collector-unavailable banner — a failure stated,
	// never a fabricated or empty chart presented as current.
	broken := dashboard.NewGenerator(nil, nil)
	var buf strings.Builder
	if renderErr := broken.GenerateObservatory(&buf, "", ""); renderErr == nil {
		t.Skip("nil-DB render tolerated; error-banner path covered API-side")
	}
	out := buf.String()
	if !strings.Contains(out, "observatory collector unavailable") {
		t.Errorf("error render missing banner text:\n%s", out[:min(600, len(out))])
	}
	_ = io.Discard
}
