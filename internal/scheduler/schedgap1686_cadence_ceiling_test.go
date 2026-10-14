package scheduler

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ceilingLane builds an enabled lane in namespace nsID. parent is the lane's
// project root (Parent field): "" makes the lane its own root; a non-empty
// parent points at a primary that must be present in the project list for the
// family grouping to count the lane as "not the sole runnable lane".
func ceilingLane(name, parent, nsID string, cooldownS int) database.Project {
	return database.Project{
		Name: name, RepoURL: "local://" + name, Workdir: "/tmp/" + name,
		Weight: 1, Priority: 5, CooldownS: cooldownS,
		DecayRate: 1, Enabled: true, NamespaceID: &nsID, Parent: parent,
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

// ceilingPrimary builds an enabled root lane with no namespace — a project
// primary that exists only so a satellite's family has more than one enabled
// lane (and therefore is NOT the sole runnable lane of its project).
func ceilingPrimary(name string) database.Project {
	return database.Project{
		Name: name, RepoURL: "local://" + name, Workdir: "/tmp/" + name,
		Weight: 1, Priority: 5, CooldownS: 86400,
		DecayRate: 1, Enabled: true,
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

func ceilingNamespace(id string) database.Namespace {
	return database.Namespace{ID: id, Weight: 100, Reserved: 100, HardCap: 100, Enabled: true}
}

// ceilingPack runs one Pack over the given projects in namespace nsID with the
// trailing-window completed-tick count injected, and returns the packed lane
// names (in selection order).
func ceilingPack(t *testing.T, projects []database.Project, nsID string, tickCount int, now time.Time) []string {
	t.Helper()
	p := NewMultiPoolPacker(100, 100, nil)
	p.SetNamespaceTickCounts(map[string]int{nsID: tickCount})
	got := p.Pack(projects, []database.Namespace{ceilingNamespace(nsID)},
		NewUrgencyCalculator(time.Minute, time.Hour, 10), nil, nil, now)
	names := make([]string, 0, len(got.Projects))
	for _, pp := range got.Projects {
		names = append(names, pp.Name)
	}
	return names
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// A1 — a synthetic namespace with 10 lanes at 24h cooldown cannot exceed
// 15 ticks in any 24h window (without the trigger clause). Implied demand is
// 10 * (24h / 24h) = 10, ceiling 15. The packer defers lanes once the
// trailing count plus the lanes already selected this cycle reaches 15, so
// tickCount + packed never exceeds 15.
func TestCadenceCeiling_NamespaceCannotExceedImpliedDemand(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const nsID = "satellite"
	var projects []database.Project
	for i := 0; i < 10; i++ {
		prim := fmt.Sprintf("proj-%d", i)
		lane := fmt.Sprintf("proj-%d-pm", i)
		projects = append(projects, ceilingPrimary(prim), ceilingLane(lane, prim, nsID, 86400))
	}

	onlyLanes := func(names []string) []string {
		var out []string
		for _, n := range names {
			if strings.HasSuffix(n, "-pm") {
				out = append(out, n)
			}
		}
		return out
	}

	// tickCount = completed ticks already in the trailing 24h window. The
	// ceiling (15) bounds tickCount + packed, never exceeding it.
	cases := []struct {
		tickCount  int
		wantPacked int
	}{
		{0, 10}, // 0 + 10 = 10 <= 15 — every lane fits
		{5, 10}, // 5 + 10 = 15 — exactly at the ceiling, all fit
		{6, 9},  // 6 + 9 = 15
		{10, 5}, // 10 + 5 = 15
		{14, 1}, // 14 + 1 = 15 — only one more fits
		{15, 0}, // already at the ceiling — nothing packs
		{20, 0}, // over the ceiling — nothing packs
	}
	for _, c := range cases {
		got := onlyLanes(ceilingPack(t, projects, nsID, c.tickCount, now))
		if len(got) != c.wantPacked {
			t.Errorf("tickCount=%d packed %d lanes (total %d), want %d (total %d) — ceiling must cap at 15",
				c.tickCount, len(got), c.tickCount+len(got), c.wantPacked, c.tickCount+c.wantPacked)
		}
	}
	// A lane is never dropped or penalized by the ceiling — once the count
	// falls back under the ceiling the same lanes are admitted unchanged.
	if got := onlyLanes(ceilingPack(t, projects, nsID, 0, now)); len(got) != 10 {
		t.Fatalf("re-admission after ceiling packed %d lanes, want 10", len(got))
	}
}

// A2 — the ceiling event names the boost mechanism that caused the excess.
// A bump-boosted lane deferred at the ceiling must produce a CEILING log line
// naming boost=bump (not a bare organic line).
func TestCadenceCeiling_EventNamesBoostCause(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const nsID = "satellite"
	primary := ceilingPrimary("proj")
	bumped := ceilingLane("proj-pm", "proj", nsID, 86400)
	bumped.BumpActive = true
	bumped.BumpCooldownS = 3600 // active bump → the bump boost sets the urgency
	// Keep the lane OUT of the starvation window so the bump boost (not the
	// starvation guarantee) is the mechanism the ceiling names.
	bumped.CreatedAt = now.Add(-30 * time.Minute).Format(time.RFC3339)

	projects := []database.Project{primary, bumped}
	ns := []database.Namespace{ceilingNamespace(nsID)}

	p := NewMultiPoolPacker(100, 100, nil)
	p.SetNamespaceTickCounts(map[string]int{nsID: 100}) // far over the ceiling

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	got := p.Pack(projects, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), nil, nil, now)
	log.SetOutput(old)

	if contains(namesOf(got.Projects), "proj-pm") {
		t.Fatalf("bumped lane was packed despite the ceiling: %v", namesOf(got.Projects))
	}
	out := buf.String()
	if !strings.Contains(out, "CEILING:") {
		t.Fatalf("no CEILING log line emitted: %q", out)
	}
	if !strings.Contains(out, "boost=bump") {
		t.Fatalf("CEILING log does not name the bump mechanism: %q", out)
	}
	if strings.Contains(out, "boost=organic") {
		t.Fatalf("CEILING log misattributes the bump as organic: %q", out)
	}
}

// A3 — the trigger clause: a lane that is the ONLY runnable lane of its
// enabled project is still packed while its namespace is at ceiling.
func TestCadenceCeiling_TriggerClauseSoleRunnableLaneStillPacks(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const nsID = "satellite"
	// "solo" is a root lane (Parent="") with no satellites — its project's
	// only runnable lane. Deferring it would starve the project entirely.
	solo := ceilingLane("solo", "", nsID, 86400)
	// "multi-pm" is a satellite of "multi" (also enabled) — NOT the sole
	// runnable lane, so the ceiling may defer it.
	multi := ceilingPrimary("multi")
	multiPm := ceilingLane("multi-pm", "multi", nsID, 86400)

	projects := []database.Project{solo, multi, multiPm}
	got := ceilingPack(t, projects, nsID, 100, now) // 100 ticks: far over ceiling

	if !contains(got, "solo") {
		t.Fatalf("sole runnable lane 'solo' was deferred at ceiling: packed = %v", got)
	}
	if contains(got, "multi-pm") {
		t.Fatalf("non-sole lane 'multi-pm' was packed despite the ceiling: %v", got)
	}
}

// namesOf renders PackedProjects as their names (test convenience).
func namesOf(ps []PackedProject) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// TestCadenceCeiling_UnarmedIsNoOp is the fail-open regression guard: with no
// trailing-window counts injected (nil map), the ceiling must NOT defer
// anything — even a namespace whose lanes have such long cooldowns that the
// implied ceiling is below the lane count. This pins that the feature only
// ever activates once the loop arms it.
func TestCadenceCeiling_UnarmedIsNoOp(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const nsID = "satellite"
	var projects []database.Project
	for i := 0; i < 10; i++ {
		prim := fmt.Sprintf("proj-%d", i)
		lane := fmt.Sprintf("proj-%d-pm", i)
		// 48h cooldown → implied demand 5, ceiling 7.5 (< 10 lanes), so an
		// armed ceiling WOULD defer. Unarmed, it must not.
		projects = append(projects, ceilingPrimary(prim), ceilingLane(lane, prim, nsID, 172800))
	}
	p := NewMultiPoolPacker(100, 100, nil) // no SetNamespaceTickCounts
	got := p.Pack(projects, []database.Namespace{ceilingNamespace(nsID)},
		NewUrgencyCalculator(time.Minute, time.Hour, 10), nil, nil, now)
	packedLanes := 0
	for _, pp := range got.Projects {
		if strings.HasSuffix(pp.Name, "-pm") {
			packedLanes++
		}
	}
	if packedLanes != 10 {
		t.Fatalf("unarmed pack deferred %d of 10 lanes (packed=%d) — the ceiling must be a no-op until armed",
			10-packedLanes, packedLanes)
	}
}

// --- pure helper unit tests -------------------------------------------------

func TestCadenceCeiling_ImpliedDemandAndCeiling(t *testing.T) {
	lanes := []database.Project{
		{CooldownS: 86400}, {CooldownS: 86400}, {CooldownS: 43200}, {CooldownS: 0},
	}
	// 86400/86400 + 86400/86400 + 86400/43200 + 86400/1 = 1 + 1 + 2 + 86400.
	want := 1.0 + 1.0 + 2.0 + 86400.0
	if got := cadenceImpliedDemand(lanes, CadenceCeilingWindow); got != want {
		t.Fatalf("implied demand = %v, want %v", got, want)
	}
	if got := cadenceCeilingFor(10); got != 15 {
		t.Fatalf("ceiling for implied 10 = %v, want 15", got)
	}
}

func TestCadenceCeiling_DefersAtBoundary(t *testing.T) {
	// The ceiling defers once tickCount + selectedCount reaches the ceiling.
	if cadenceCeilingDefers(14, 0, 15) {
		t.Fatal("14 ticks, 0 selected must not defer (below ceiling)")
	}
	if !cadenceCeilingDefers(15, 0, 15) {
		t.Fatal("15 ticks, 0 selected must defer (at ceiling)")
	}
	if !cadenceCeilingDefers(14, 1, 15) {
		t.Fatal("14 ticks + 1 selected must defer (reaches the ceiling)")
	}
	if !cadenceCeilingDefers(0, 16, 15) {
		t.Fatal("0 ticks + 16 selected must defer (over ceiling)")
	}
}

func TestCadenceCeiling_ProjectRootResolution(t *testing.T) {
	parentOf := map[string]string{
		"proj":     "",
		"proj-pm":  "proj",
		"proj-qa":  "proj",
		"proj-2":   "proj-qa", // arbitrary depth
		"dangling": "ghost",   // parent not in snapshot → its own root
		"self":     "self",    // self-link cycle
	}
	cases := map[string]string{
		"proj":     "proj",
		"proj-pm":  "proj",
		"proj-qa":  "proj",
		"proj-2":   "proj",
		"dangling": "dangling",
		"self":     "self",
		"absent":   "absent", // not in the map at all → its own root
	}
	for name, want := range cases {
		if got := projectRootName(name, parentOf); got != want {
			t.Errorf("projectRootName(%q) = %q, want %q", name, got, want)
		}
	}
}
