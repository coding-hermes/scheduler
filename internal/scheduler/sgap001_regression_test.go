package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// Regression tests for S-GAP-001 (2026-08-04).
//
// Fix A — selection starvation: urgency-greedy packing let the prio-10
// cohort permanently monopolize all slots; 16 enabled projects were never
// picked. Any eligible project whose last tick attempt is older than its
// starvation window (cooldown<=3600s → 60min; else 3× cooldown) now gets a
// massive urgency boost in BOTH the namespace pack and the flat fallback.
//
// Fix B — spawn-failure backoff: consecutive spawn failures multiply the
// effective cooldown by 2^(failures-1), capped at 2h, making >50 consecutive
// failures per day impossible.
//
// Reopen (2026-08-05): the flat 1e12 boost tied every starving project, so
// the priority-desc tie-break kept the prio-5 tier at zero spawn attempts
// for 25-34h live. The boost is now monotonic in starvation age
// (starvationBoostUrgencyFor) — most-starved first, regardless of priority.
// The MultiStarved tests below pin that ordering.

// prodUrgencyCalc mirrors the daemon's live urgency calculator
// (--min-interval 30s, max 24h, 10 levels).
func prodUrgencyCalc() *scheduler.UrgencyCalculator {
	return scheduler.NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
}

// setConsecutiveFailures sets the backoff counter directly (simulating prior
// spawn failures) and fails the test on error.
func setConsecutiveFailures(t *testing.T, db *sql.DB, name string, n int) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET consecutive_failures = ? WHERE name = ?`, n, name); err != nil {
		t.Fatalf("set consecutive_failures=%d for %s: %v", n, name, err)
	}
}

// setLastCompleted rewrites projects.last_tick_completed (the flat packer's
// attempt clock) directly.
func setLastCompleted(t *testing.T, db *sql.DB, name string, ts time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		ts.Format(time.RFC3339), name); err != nil {
		t.Fatalf("set last_tick_completed for %s: %v", name, err)
	}
}

func packNames(result scheduler.PackResult) []string {
	names := make([]string, 0, len(result.Projects))
	for _, p := range result.Projects {
		names = append(names, p.Name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestSGAP001_Starvation_NamespacePack is THE acceptance test for criterion
// (a): a prio-10 project ticking every ~15 min and a prio-5 project (900s
// cooldown) whose last attempt was 61 min ago compete for ONE free slot —
// the starved prio-5 project MUST be selected. Before the fix, urgency
// (≈330 vs ≈12) made this impossible.
func TestSGAP001_Starvation_NamespacePack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "hot-prio10", "coding-hermes", 10, 10, 900, 1.0)
	mustCreateProjectInNS(t, db, "starved-prio5", "coding-hermes", 10, 5, 900, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"hot-prio10":    now.Add(-16 * time.Minute), // past its 900s cooldown — eligible
		"starved-prio5": now.Add(-61 * time.Minute), // past cooldown AND past the 60-min window
	}

	// One free slot, like the live daemon's freed-slot evaluations.
	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "starved-prio5" {
		t.Errorf("Pack selected %q, want %q — starvation boost did not fire; "+
			"the prio-10 cohort still monopolizes the slot (S-GAP-001 regression)", got[0], "starved-prio5")
	}
}

// TestSGAP001_StarvationBoundary pins the guarantee window: a project whose
// last attempt was 59 min ago (< 60-min window) must NOT be boosted — normal
// greedy urgency order is preserved and the prio-10 project wins the slot.
func TestSGAP001_StarvationBoundary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "hot-prio10", "coding-hermes", 10, 10, 900, 1.0)
	mustCreateProjectInNS(t, db, "recent-prio5", "coding-hermes", 10, 5, 900, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"hot-prio10":   now.Add(-16 * time.Minute),
		"recent-prio5": now.Add(-59 * time.Minute), // inside the 60-min window — not starving
	}

	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "hot-prio10" {
		t.Errorf("Pack selected %q, want %q — 59-min-old project must NOT be boosted; "+
			"normal urgency order must be preserved inside the window", got[0], "hot-prio10")
	}
}

// TestSGAP001_Backoff_NamespacePack covers criterion (b): with
// consecutive_failures=4 and cooldown 900 the effective cooldown is
// 900×2^3 = 7200s (capped at 2h). A last attempt 10 min ago → skipped; a
// last attempt 2h+ ago → selected (and starving-boosted, which is fine —
// the backoff gate, not the boost, is what's under test).
func TestSGAP001_Backoff_NamespacePack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "flaky", "coding-hermes", 10, 10, 900, 1.0)
	setConsecutiveFailures(t, db, "flaky", 4)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	mp := scheduler.NewMultiPoolPacker(100, 6, nil)

	// Case A: failed 10 min ago → effective cooldown 7200s not elapsed → skip.
	resA := mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-10 * time.Minute)}, nil, now)
	if got := packNames(resA); len(got) != 0 {
		t.Errorf("10 min after failure: Pack selected %v, want none — "+
			"backoff (eff. cooldown 7200s) must skip the project", got)
	}

	// Case B: failed 2h+ ago → backoff elapsed → selected (starving-boosted).
	resB := mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-(2*time.Hour + time.Minute))}, nil, now)
	gotB := packNames(resB)
	if !containsName(gotB, "flaky") {
		t.Errorf("2h+ after failure: Pack selected %v, want flaky — "+
			"backoff must release the project once the effective cooldown elapses", gotB)
	}
}

// TestSGAP001_BackoffRamp pins the full exponential ramp at the pack level:
// cf=1 → 900s cooldown (recent attempt blocks), cf=2 → 1800s, cf=3 → 3600s.
// This is what makes >50 consecutive failures/day arithmetically impossible.
func TestSGAP001_BackoffRamp(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "flaky", "coding-hermes", 10, 10, 900, 1.0)

	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	now := time.Now().UTC()
	mp := scheduler.NewMultiPoolPacker(100, 6, nil)

	// cf=1, attempt 10 min ago (600s < 900s) → still blocked by base cooldown.
	setConsecutiveFailures(t, db, "flaky", 1)
	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	res := mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-10 * time.Minute)}, nil, now)
	if got := packNames(res); len(got) != 0 {
		t.Errorf("cf=1, 10min ago: selected %v, want none (base cooldown 900s still active)", got)
	}

	// cf=1, attempt 16 min ago (960s > 900s) → eligible (first failure adds nothing).
	res = mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-16 * time.Minute)}, nil, now)
	if got := packNames(res); !containsName(got, "flaky") {
		t.Errorf("cf=1, 16min ago: selected %v, want flaky (cf=1 keeps base cooldown)", got)
	}

	// cf=2, attempt 20 min ago (1200s < 1800s) → blocked by 2x backoff.
	setConsecutiveFailures(t, db, "flaky", 2)
	projects, err = database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	res = mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-20 * time.Minute)}, nil, now)
	if got := packNames(res); len(got) != 0 {
		t.Errorf("cf=2, 20min ago: selected %v, want none (backoff 1800s still active)", got)
	}

	// cf=3, attempt 30 min ago (1800s < 3600s) → blocked by 4x backoff.
	setConsecutiveFailures(t, db, "flaky", 3)
	projects, err = database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	res = mp.Pack(projects, namespaces, prodUrgencyCalc(),
		map[string]time.Time{"flaky": now.Add(-30 * time.Minute)}, nil, now)
	if got := packNames(res); len(got) != 0 {
		t.Errorf("cf=3, 30min ago: selected %v, want none (backoff 3600s still active)", got)
	}
}

// TestSGAP001_Starvation_FlatPick proves the boost also applies in the flat
// DB-backed packer (Packer.Pick) — the two selection paths must not diverge.
func TestSGAP001_Starvation_FlatPick(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "hot-prio10", 10, 10, 900, 1.0)
	mustCreateProjectAt(t, db, "starved-prio5", 10, 5, 900, 1.0)

	now := time.Now().UTC()
	setLastCompleted(t, db, "hot-prio10", now.Add(-16*time.Minute))
	setLastCompleted(t, db, "starved-prio5", now.Add(-61*time.Minute))

	p := scheduler.NewPacker(db, prodUrgencyCalc(), 100, 1, nil)
	picked, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(picked) != 1 {
		t.Fatalf("flat Pick selected %d projects, want exactly 1 (maxConcurrent=1)", len(picked))
	}
	if picked[0].Name != "starved-prio5" {
		t.Errorf("flat Pick selected %q, want %q — starvation boost missing in flat path",
			picked[0].Name, "starved-prio5")
	}
}

// TestSGAP001_Backoff_FlatPick proves the backoff also applies in the flat
// packer: cf=4 (eff. cooldown 7200s) with an attempt 10 min ago → skipped.
func TestSGAP001_Backoff_FlatPick(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "flaky", 10, 10, 900, 1.0)
	setConsecutiveFailures(t, db, "flaky", 4)

	now := time.Now().UTC()
	setLastCompleted(t, db, "flaky", now.Add(-10*time.Minute))

	p := scheduler.NewPacker(db, prodUrgencyCalc(), 100, 6, nil)
	picked, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(picked) != 0 {
		t.Errorf("flat Pick selected %d projects, want 0 — backoff must skip (eff. cooldown 7200s)", len(picked))
	}

	// 2h+ after the failure the project is eligible again.
	setLastCompleted(t, db, "flaky", now.Add(-(2*time.Hour + time.Minute)))
	picked, err = p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(picked) != 1 || picked[0].Name != "flaky" {
		t.Errorf("flat Pick 2h+ after failure selected %v, want [flaky]", picked)
	}
}

// TestSGAP001_MultiStarved_MostStarvedWins_NamespacePack is THE acceptance
// test for the 2026-08-05 reopen: TWO starved projects of different priority
// plus a hot prio-10 compete for ONE slot — the MOST-starved project must
// win regardless of priority. With the original flat boost (1e12 for all)
// the two starved projects tied and the priority-desc tie-break handed the
// slot to starved-prio10 — the exact bug that kept the prio-5 tier at zero
// spawn attempts for 25-34h live.
func TestSGAP001_MultiStarved_MostStarvedWins_NamespacePack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "hot-prio10", "coding-hermes", 10, 10, 900, 1.0)
	mustCreateProjectInNS(t, db, "starved-prio10", "coding-hermes", 10, 10, 900, 1.0)
	mustCreateProjectInNS(t, db, "starved-prio5", "coding-hermes", 10, 5, 900, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"hot-prio10":     now.Add(-16 * time.Minute), // past cooldown, inside window — not starving
		"starved-prio10": now.Add(-65 * time.Minute), // starving (65min > 60min window)
		"starved-prio5":  now.Add(-10 * time.Hour),   // starving ~9x longer
	}

	// One free slot, like the live daemon's freed-slot evaluations.
	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "starved-prio5" {
		t.Errorf("Pack selected %q, want %q — most-starved project must win the slot "+
			"regardless of priority (S-GAP-001 reopen regression: flat boost ties let "+
			"the prio-10 cohort outvote longer-starved prio-5 projects)", got[0], "starved-prio5")
	}
}

// TestSGAP001_MultiStarved_OrderingIsAgeNotPriority pins the ordering key
// among starved projects: a 4-minute age gap (65min vs 61min) is decisive.
// The older-starved project wins — the sort key among the boosted cohort is
// starvation age, so any wrong formulation that ignores age (flat boost,
// priority-weighted boost) reintroduces the tie this test guards.
func TestSGAP001_MultiStarved_OrderingIsAgeNotPriority(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))
	mustCreateProjectInNS(t, db, "starved-prio10", "coding-hermes", 10, 10, 900, 1.0)
	mustCreateProjectInNS(t, db, "starved-prio5", "coding-hermes", 10, 5, 900, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"starved-prio10": now.Add(-65 * time.Minute), // older by 4 minutes
		"starved-prio5":  now.Add(-61 * time.Minute),
	}

	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "starved-prio10" {
		t.Errorf("Pack selected %q, want %q — among starved projects the OLDER "+
			"last attempt (65min > 61min) must decide; ordering is starvation age",
			got[0], "starved-prio10")
	}
}

// TestSGAP001_MultiStarved_MostStarvedWins_FlatPick is the flat-path twin of
// TestSGAP001_MultiStarved_MostStarvedWins_NamespacePack via the DB-backed
// Packer.Pick — the age-monotonic boost must hold in both selection paths.
func TestSGAP001_MultiStarved_MostStarvedWins_FlatPick(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "hot-prio10", 10, 10, 900, 1.0)
	mustCreateProjectAt(t, db, "starved-prio10", 10, 10, 900, 1.0)
	mustCreateProjectAt(t, db, "starved-prio5", 10, 5, 900, 1.0)

	now := time.Now().UTC()
	setLastCompleted(t, db, "hot-prio10", now.Add(-16*time.Minute))
	setLastCompleted(t, db, "starved-prio10", now.Add(-65*time.Minute))
	setLastCompleted(t, db, "starved-prio5", now.Add(-10*time.Hour))

	p := scheduler.NewPacker(db, prodUrgencyCalc(), 100, 1, nil)
	picked, err := p.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(picked) != 1 {
		t.Fatalf("flat Pick selected %d projects, want exactly 1 (maxConcurrent=1)", len(picked))
	}
	if picked[0].Name != "starved-prio5" {
		t.Errorf("flat Pick selected %q, want %q — age-monotonic starvation boost "+
			"missing in flat path (S-GAP-001 reopen regression)", picked[0].Name, "starved-prio5")
	}
}

// TestSGAP001_ConsecutiveFailuresRoundTrip proves the new column survives
// the DB layer (migration default 0 + explicit value read back through the
// Project struct the packers consume).
func TestSGAP001_ConsecutiveFailuresRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateProjectAt(t, db, "roundtrip", 10, 5, 900, 1.0)

	p, err := database.GetProject(ctx, db, "roundtrip")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.ConsecutiveFailures != 0 {
		t.Errorf("fresh project consecutive_failures = %d, want 0 (migration default)", p.ConsecutiveFailures)
	}

	setConsecutiveFailures(t, db, "roundtrip", 7)
	p, err = database.GetProject(ctx, db, "roundtrip")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.ConsecutiveFailures != 7 {
		t.Errorf("consecutive_failures = %d, want 7 after UPDATE", p.ConsecutiveFailures)
	}

	list, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 1 || list[0].ConsecutiveFailures != 7 {
		t.Errorf("ListProjects consecutive_failures = %+v, want [{7}]", list)
	}
}
