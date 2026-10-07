package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-225 — review-lane reserved slots. Every scenario runs the LIVE
// packer paths against a saturated admission window built from THIS PASS's
// picks (no running set): the review lane sorts AFTER the code lanes (all
// candidates share urgency/priority/last-tick, so the pack walks name-ASC
// and alice/bob/carol precede ciere-review) — exactly the starvation shape
// this row closes, because the legacy greedy loops break at the cap before
// they ever reach the review lane. The setup helper restores the flag
// default (2) via t.Cleanup so the package-level knob can never leak into
// another test's assertions.

const gap225ReviewLane = "ciere-review"

// gap225SetReserve arms the knob for one test and restores the default on
// cleanup (SetReviewLaneReservedSlots is a package-level last-writer-wins
// value — see review_reserve.go).
func gap225SetReserve(t *testing.T, n int) {
	t.Helper()
	scheduler.SetReviewLaneReservedSlots(n)
	t.Cleanup(func() {
		scheduler.SetReviewLaneReservedSlots(scheduler.ReviewLaneReservedSlotsFlagDefault)
	})
}

func TestSCHEDGAP225_FlatFallbackPicksReviewLaneUnderReserve(t *testing.T) {
	gap225SetReserve(t, 1)
	db := newTestDB(t)
	ctx := context.Background()

	for _, n := range []string{"alice", "bob", "carol"} {
		mustCreateProjectAt(t, db, n, 10, 5, 0, 1.0)
	}
	mustCreateProjectAt(t, db, gap225ReviewLane, 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	// Cap 3, reserve 1, nothing running: the code lanes stop at the
	// (cap − reserve) ceiling of 2 and the review lane takes the reserved
	// third slot. Legacy selection packs alice/bob/carol and starves it.
	calc := defaultUrgencyCalc()
	mp := scheduler.NewMultiPoolPacker(100, 3, nil)
	res := mp.Pack(projects, nil, calc, nil, nil, time.Now())

	packed := map[string]bool{}
	for _, p := range res.Projects {
		packed[p.Name] = true
	}
	if len(res.Projects) != 3 {
		t.Fatalf("packed %d projects, want 3 (full cap): %v", len(res.Projects), res.Projects)
	}
	if !packed[gap225ReviewLane] {
		t.Fatalf("review lane not packed — the reserve did not hold its slot: %v", res.Projects)
	}
	if packed["carol"] {
		t.Fatalf("code lane packed past the reserved ceiling (carol ran, review starved first): %v", res.Projects)
	}
}

func TestSCHEDGAP225_NoReserveStarvesReviewLaneUnderSaturation(t *testing.T) {
	// The control arm: reserve disarmed (0) — the legacy behavior where the
	// window closes on code lanes and the review lane never packs.
	// RED-proofs the mechanism: without this test a reserve that admits
	// EVERYTHING would pass.
	gap225SetReserve(t, 0)
	db := newTestDB(t)
	ctx := context.Background()

	for _, n := range []string{"alice", "bob", "carol"} {
		mustCreateProjectAt(t, db, n, 10, 5, 0, 1.0)
	}
	mustCreateProjectAt(t, db, gap225ReviewLane, 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	calc := defaultUrgencyCalc()
	mp := scheduler.NewMultiPoolPacker(100, 3, nil)
	res := mp.Pack(projects, nil, calc, nil, nil, time.Now())

	for _, p := range res.Projects {
		if p.Name == gap225ReviewLane {
			t.Fatalf("review lane packed with the reserve disarmed — window closed before it: %v", res.Projects)
		}
	}
	if len(res.Projects) != 3 {
		t.Fatalf("no reserve must pack the legacy 3 code lanes, got %v", res.Projects)
	}
}

func TestSCHEDGAP225_OverdueRescueUnblockedByReserve(t *testing.T) {
	// GAP-011: the overdue force-select outranks the reserve — an overdue
	// CODE lane is force-selected even while the band is armed (the
	// overdue branch reads the full cap). The review lane here is merely
	// starved-boosted, not overdue, and still claims the band.
	gap225SetReserve(t, 2)
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "code-due", 10, 9, 300, 1.0)
	mustCreateProjectAt(t, db, "alice", 10, 5, 300, 1.0)
	mustCreateProjectAt(t, db, gap225ReviewLane, 10, 5, 300, 1.0)

	now := time.Now()
	stale := now.Add(-6 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'code-due'`, stale); err != nil {
		t.Fatalf("set overdue last_tick_completed: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name IN ('alice', 'ciere-review')`, recent); err != nil {
		t.Fatalf("set recent last_tick_completed: %v", err)
	}

	calc := defaultUrgencyCalc()
	pk := scheduler.NewPacker(db, calc, 100, 4, nil)
	packed, err := pk.Pick(now, nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}

	var gotOverdue, gotReview bool
	for _, p := range packed {
		switch p.Name {
		case "code-due":
			gotOverdue = true
		case gap225ReviewLane:
			gotReview = true
		}
	}
	if !gotOverdue {
		t.Fatalf("overdue code lane not force-selected — the GAP-011 rescue regressed under the reserve: %v", packed)
	}
	if !gotReview {
		t.Fatalf("review lane not admitted into the reserved band: %v", packed)
	}
}

func TestSCHEDGAP225_ReviewLanePacksFreelyBelowCeiling(t *testing.T) {
	// The reserve is a floor, not a boost: with room below the ceiling a
	// review lane competes unchanged. Cap 4, configured reserve 2 but only
	// ONE waiting review lane → the dynamic reserve is 1, so the code
	// ceiling is 3 and all four lanes pack.
	gap225SetReserve(t, 2)
	db := newTestDB(t)
	ctx := context.Background()

	for _, n := range []string{"alice", "bob", "carol"} {
		mustCreateProjectAt(t, db, n, 10, 5, 0, 1.0)
	}
	mustCreateProjectAt(t, db, gap225ReviewLane, 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	calc := defaultUrgencyCalc()
	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	res := mp.Pack(projects, nil, calc, nil, nil, time.Now())

	if len(res.Projects) != 4 {
		t.Fatalf("packed %d projects below the ceiling, want all 4: %v", len(res.Projects), res.Projects)
	}
}

func TestSCHEDGAP225_NamespacePathReservesSlotUnderSaturation(t *testing.T) {
	// The LIVE namespace-mode path (Phase 2): code lanes saturate the
	// general window; the review lane takes the reserved slot.
	gap225SetReserve(t, 1)
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("satellites", 10, 1, 100, true))
	mustCreateNamespace(t, db, makeNamespace("foremen", 10, 1, 100, true))

	for _, n := range []string{"alice", "bob", "carol"} {
		mustCreateProjectInNS(t, db, n, "foremen", 10, 5, 0, 1.0)
	}
	mustCreateProjectInNS(t, db, gap225ReviewLane, "satellites", 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	calc := defaultUrgencyCalc()
	mp := scheduler.NewMultiPoolPacker(100, 3, nil)
	res := mp.Pack(projects, namespaces, calc, nil, nil, time.Now())

	packed := map[string]bool{}
	for _, p := range res.Projects {
		packed[p.Name] = true
	}
	if len(res.Projects) != 3 {
		t.Fatalf("namespace path packed %d projects, want 3 (full cap): %v", len(res.Projects), res.Projects)
	}
	if !packed[gap225ReviewLane] {
		t.Fatalf("namespace path starved the review lane — the reserve did not hold a slot: %v", res.Projects)
	}
	if packed["carol"] {
		t.Fatalf("namespace path packed code lanes past the reserved ceiling: %v", res.Projects)
	}
}

func TestSCHEDGAP225_ReserveAtLeastAsLargeAsCapNeverStarvesCodeLanes(t *testing.T) {
	// A pathological reserve >= the cap must not lock code lanes out
	// entirely: the per-lane ceiling clamps the effective reserve to
	// cap-1, so the first code lane always fits, and the dynamic reserve
	// decays to 0 once the review lane packs — the rest of the window
	// reopens to code lanes.
	gap225SetReserve(t, 4)
	db := newTestDB(t)
	ctx := context.Background()

	for _, n := range []string{"alice", "bob", "carol"} {
		mustCreateProjectAt(t, db, n, 10, 5, 0, 1.0)
	}
	mustCreateProjectAt(t, db, gap225ReviewLane, 10, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	calc := defaultUrgencyCalc()
	mp := scheduler.NewMultiPoolPacker(100, 4, nil)
	res := mp.Pack(projects, nil, calc, nil, nil, time.Now())

	var code, review int
	for _, p := range res.Projects {
		if p.Name == gap225ReviewLane {
			review++
		} else {
			code++
		}
	}
	if code == 0 {
		t.Fatalf("reserve >= cap starved every code lane — the clamp must keep a code slot: %v", res.Projects)
	}
	if review != 1 {
		t.Fatalf("review lane packed %d times, want exactly 1: %v", review, res.Projects)
	}
	if len(res.Projects) != 4 {
		t.Fatalf("packed %d projects, want 4 (full cap; dynamic reserve decays after the review lane packs): %v", len(res.Projects), res.Projects)
	}
}

func TestSCHEDGAP225_IsReviewLaneSuffixMatch(t *testing.T) {
	for _, name := range []string{"ciere-review", "x-review", "a-b-review"} {
		if !scheduler.IsReviewLane(name) {
			t.Errorf("IsReviewLane(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"review-bot", "myproj-reviews", "review", "reviewer"} {
		if scheduler.IsReviewLane(name) {
			t.Errorf("IsReviewLane(%q) = true, want false", name)
		}
	}
}
