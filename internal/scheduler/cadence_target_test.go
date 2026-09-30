package scheduler

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func cadenceFloat(v float64) *float64 { return &v }

func cadenceProject(name string, target *float64) database.Project {
	ns := "cadence"
	pin := 21600
	return database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
		Weight: 1, Priority: 5, CooldownS: 21600, CooldownPinS: &pin,
		DecayRate: 1, Enabled: true, NamespaceID: &ns,
		CreatedAt:        time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		TargetRunsPerDay: target,
	}
}

func TestCadenceTarget_BelowTargetOutranksLaneMeetingTarget(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	projects := []database.Project{
		cadenceProject("a-meeting", cadenceFloat(4)),
		cadenceProject("z-below", cadenceFloat(4)),
	}
	ns := []database.Namespace{{ID: "cadence", Weight: 100, Reserved: 100, HardCap: 100, Enabled: true}}
	last := map[string]time.Time{
		"a-meeting": now.Add(-7 * time.Hour),
		"z-below":   now.Add(-7 * time.Hour),
	}
	p := NewMultiPoolPacker(100, 1, nil)
	p.SetCadenceRates(map[string]float64{"a-meeting": 4, "z-below": 1})
	got := p.Pack(projects, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), last, nil, now)
	if len(got.Projects) != 1 || got.Projects[0].Name != "z-below" {
		t.Fatalf("selected = %#v, want only z-below (below target outranks meeting target)", got.Projects)
	}
}

func TestCadenceTarget_AbsentTargetPreservesCurrentOrdering(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	// No override and no cooldown pin means no cadence opinion. A cadence-rate
	// snapshot must therefore produce the same winner as the pre-feature path.
	a := cadenceProject("a-current-first", nil)
	z := cadenceProject("z-current-second", nil)
	a.CooldownPinS = nil
	z.CooldownPinS = nil
	projects := []database.Project{z, a}
	ns := []database.Namespace{{ID: "cadence", Weight: 100, Reserved: 100, HardCap: 100, Enabled: true}}
	last := map[string]time.Time{
		a.Name: now.Add(-7 * time.Hour),
		z.Name: now.Add(-7 * time.Hour),
	}
	pack := func(rates map[string]float64) []PackedProject {
		p := NewMultiPoolPacker(100, 1, nil)
		p.SetCadenceRates(rates)
		return p.Pack(projects, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), last, nil, now).Projects
	}
	baseline := pack(nil)
	withRates := pack(map[string]float64{a.Name: 0, z.Name: 999})
	if len(baseline) != 1 || len(withRates) != 1 || baseline[0].Name != withRates[0].Name {
		t.Fatalf("absent targets changed ordering: baseline=%#v withRates=%#v", baseline, withRates)
	}
}

func TestEffectiveCadenceTarget_DerivedThenOverrideThenOptOut(t *testing.T) {
	p := cadenceProject("lane", nil)
	if got, ok := effectiveCadenceTarget(p); !ok || got != 4 {
		t.Fatalf("derived target = (%v,%v), want (4,true)", got, ok)
	}
	p.TargetRunsPerDay = cadenceFloat(3)
	if got, ok := effectiveCadenceTarget(p); !ok || got != 3 {
		t.Fatalf("override target = (%v,%v), want (3,true)", got, ok)
	}
	p.TargetRunsPerDay = cadenceFloat(0)
	if _, ok := effectiveCadenceTarget(p); ok {
		t.Fatal("explicit zero target must mean no cadence opinion")
	}
}

// TestCadenceTarget_CooldownModeAdmissionUnchangedByBoost is the ORDERING-ONLY
// invariant the feature's own comment claims (SCHED-GAP-1668): the cadence
// boost may reorder candidates, it may never widen admission. Every existing
// cooldown, namespace, budget, load and concurrency gate still decides who is
// admitted.
//
// The test drives the REAL selection path (MultiPoolPacker.Pack) twice — once
// with no achieved-rate snapshot and once with a deficit snapshot that boosts
// the lane — and requires the admitted set to be identical, with a
// cooldown-mode lane inside its cooldown still refused. Two non-vacuity guards
// are built in: the baseline must admit exactly one lane (so "unchanged" is not
// two empty results), and a second arm with both lanes outside their cooldown
// must show the SAME snapshot flipping the winner (so the equality cannot hold
// because the boost never fired).
//
// Clock seam (SCHED-GAP-169): the instant under test is an explicit value
// threaded through Pack's `now` parameter — the same value evaluate() takes
// from Loop.SetClock — so the test reads no wall clock at all.
func TestCadenceTarget_CooldownModeAdmissionUnchangedByBoost(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	pin := 21600 // 6h durable pin → derived target 4 runs/day
	nsID := "cadence"
	createdAt := now.Add(-72 * time.Hour).Format(time.RFC3339)

	// a-cold-deficit: operator-pinned lane (so it HAS a cadence target) that
	// has achieved nothing in the window → the snapshot boosts it.
	cold := database.Project{
		Name: "a-cold-deficit", RepoURL: "local://a-cold-deficit", Workdir: "/tmp/a-cold-deficit",
		Weight: 1, Priority: 5, CooldownS: pin, CooldownPinS: &pin,
		DecayRate: 1, Enabled: true, NamespaceID: &nsID, CreatedAt: createdAt,
	}
	// z-eligible: no override and no pin → no cadence opinion, never boosted.
	eligible := database.Project{
		Name: "z-eligible", RepoURL: "local://z-eligible", Workdir: "/tmp/z-eligible",
		Weight: 1, Priority: 5, CooldownS: pin,
		DecayRate: 1, Enabled: true, NamespaceID: &nsID, CreatedAt: createdAt,
	}
	projects := []database.Project{cold, eligible}
	// targetless is the same cold lane with its cadence opinion REMOVED (no
	// override, no pin) — the control that switches the boost off while every
	// other field, gate and workload stays identical.
	targetless := cold
	targetless.CooldownPinS = nil
	ns := []database.Namespace{{
		ID: "cadence", Weight: 100, Reserved: 100, HardCap: 100, Enabled: true,
		AdmissionMode: database.AdmissionModeCooldown,
	}}

	// admitted runs one real Pack with global concurrency 1 (exactly one
	// admission) and returns the admitted names in selection order. `achieved`
	// is the trailing-window snapshot: nil reads as zero achieved runs, which
	// is exactly the deficit case for the pinned lane.
	admitted := func(candidates []database.Project, last map[string]time.Time, achieved map[string]float64) []string {
		p := NewMultiPoolPacker(100, 1, nil)
		p.SetCadenceRates(achieved)
		got := p.Pack(candidates, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), last, nil, now)
		names := make([]string, 0, len(got.Projects))
		for _, pp := range got.Projects {
			names = append(names, pp.Name)
		}
		return names
	}

	// --- Arm A: the cooldown gate refuses the lane, boost or no boost -----
	insideCooldown := map[string]time.Time{
		cold.Name:     now.Add(-1 * time.Hour), // inside its 6h cooldown
		eligible.Name: now.Add(-7 * time.Hour), // eligible
	}
	baseline := admitted([]database.Project{targetless, eligible}, insideCooldown, nil)
	if len(baseline) != 1 || baseline[0] != eligible.Name {
		t.Fatalf("baseline admitted = %v, want exactly [%s] (non-vacuity guard)", baseline, eligible.Name)
	}
	boosted := admitted(projects, insideCooldown, nil)
	if len(boosted) != len(baseline) || boosted[0] != baseline[0] {
		t.Fatalf("cadence boost changed admission: %v -> %v", baseline, boosted)
	}
	if boosted[0] == cold.Name {
		t.Fatal("a cooldown-mode lane inside its cooldown was admitted by the cadence boost")
	}
	// Same answer when the achieved-rate snapshot is installed explicitly.
	if got := admitted(projects, insideCooldown, map[string]float64{cold.Name: 0}); len(got) != 1 || got[0] != eligible.Name {
		t.Fatalf("explicit snapshot admitted = %v, want [%s]", got, eligible.Name)
	}

	// --- Arm B: the same target decides ORDER alone when nothing gates ----
	// Both lanes outside their cooldown: no gate is in play, so the winner is
	// decided by urgency. The targetless control must yield the eligible lane
	// (older last tick = higher organic urgency) and the cadence target must
	// FLIP it to the deficit lane — proof that the boost is live through this
	// exact Pack path and that its only effect is ordering.
	bothEligible := map[string]time.Time{
		cold.Name:     now.Add(-7 * time.Hour),
		eligible.Name: now.Add(-8 * time.Hour), // older → wins the unboosted ordering
	}
	if got := admitted([]database.Project{targetless, eligible}, bothEligible, nil); len(got) != 1 || got[0] != eligible.Name {
		t.Fatalf("no-opinion selection = %v, want [%s] (older last tick wins)", got, eligible.Name)
	}
	if got := admitted(projects, bothEligible, nil); len(got) != 1 || got[0] != cold.Name {
		t.Fatalf("below-target selection = %v, want [%s] — the cadence target must decide the order", got, cold.Name)
	}

	// The boost itself: a below-target lane is lifted, a lane meeting its
	// target (or holding no cadence opinion) is left exactly where it was.
	const base = 100.0
	if up := cadenceAdjustedUrgency(base, cold, 0); up <= base {
		t.Fatalf("below-target lane urgency = %v, want > %v", up, base)
	}
	if up := cadenceAdjustedUrgency(base, cold, 4); up != base {
		t.Fatalf("lane meeting its target urgency = %v, want unchanged %v", up, base)
	}
	if up := cadenceAdjustedUrgency(base, eligible, 0); up != base {
		t.Fatalf("lane without a cadence opinion urgency = %v, want unchanged %v", up, base)
	}
}

// TestCadenceWindow_SevenDayHorizon pins the achieved-rate horizon itself.
// Seven days is the documented number (the /api/v1/cadence row and the API's
// own window_days both say seven), and it is what makes a weekly lane
// measurable while smoothing one-day tick-duration noise — a silent change here
// would move every achieved rate on the wire.
func TestCadenceWindow_SevenDayHorizon(t *testing.T) {
	if CadenceWindow != 7*24*time.Hour {
		t.Fatalf("CadenceWindow = %v, want 7d", CadenceWindow)
	}
	// The boost must stay strictly inside the starvation guarantee's shadow:
	// below starvationBoostUrgency (the hard fairness floor, 1e12) so a
	// starving lane always wins, and above the bump tier so a cadence-deficit
	// lane outranks ordinary bumped work.
	if cadenceBoostUrgency <= bumpBoostUrgency {
		t.Fatalf("cadenceBoostUrgency %v must exceed bumpBoostUrgency %v", cadenceBoostUrgency, bumpBoostUrgency)
	}
	if cadenceBoostUrgency+1e5 >= starvationBoostUrgency {
		t.Fatalf("cadence boost ceiling %v must stay below starvationBoostUrgency %v", cadenceBoostUrgency+1e5, starvationBoostUrgency)
	}
}
