package scheduler

// SCHED-GAP-1765 — the default family template is a PARTITION of the role
// suffix table, not a second vocabulary.
//
// The onboarding template (scheduler.DefaultFamilyRoles) and the on-demand
// remainder (scheduler.OnDemandFamilyRoles) must together cover laneRoleSuffixes
// exactly once. That is the property that makes the smaller default safe to
// live next to the unchanged admission law: a suffix added to the canonical
// table can never end up in NEITHER set (silently unmintable at onboarding AND
// invisible to the on-demand path), and a default role can never be dropped
// from the table it is supposed to be drawn from.
//
// The handler-level acceptance cell (posting a family body and counting the
// rows it writes) lives in internal/api/schedgap1765_family_template_test.go;
// these cells pin the rule itself, where a drift is cheapest to see.

import (
	"reflect"
	"strings"
	"testing"
)

// canonicalRoleSuffixes is the expected content of laneRoleSuffixes, written
// out here so that DELETING a suffix from the table fails a test instead of
// silently shrinking both derived sets.
var canonicalRoleSuffixes = []string{
	"-qa", "-pm", "-sync", "-dogfood", "-perf", "-releng", "-review", "-docs", "-readme",
}

// TestLaneClass_DefaultFamilyTemplatePartitionsSuffixTable is the invariant the
// two derived lists rest on: default ∪ on-demand == the canonical table, with
// no duplicate on either side and no overlap between them.
func TestLaneClass_DefaultFamilyTemplatePartitionsSuffixTable(t *testing.T) {
	table := LaneRoleSuffixes()
	if got, want := table, canonicalRoleSuffixes; !reflect.DeepEqual(got, want) {
		t.Fatalf("LaneRoleSuffixes() = %v, want %v — the canonical table moved; update the template partition deliberately", got, want)
	}

	def := DefaultFamilyRoles()
	onDemand := OnDemandFamilyRoles()

	// The union is the table, exactly once each.
	seen := map[string]int{}
	for _, s := range append(append([]string{}, def...), onDemand...) {
		seen[s]++
	}
	for _, s := range table {
		switch seen[s] {
		case 1:
			// covered exactly once: good
		case 0:
			t.Errorf("%q is in laneRoleSuffixes but in NEITHER the default template nor the on-demand set — it would be unmintable", s)
		default:
			t.Errorf("%q appears %d times across the default and on-demand sets — the sets must be disjoint", s, seen[s])
		}
	}
	for s := range seen {
		if !strings.Contains(strings.Join(table, " ")+" ", s+" ") {
			t.Errorf("%q is in the template sets but not in laneRoleSuffixes — a role suffix must be drawn FROM the table", s)
		}
	}
	if len(def)+len(onDemand) != len(table) {
		t.Errorf("default (%d) + on-demand (%d) != canonical table (%d)", len(def), len(onDemand), len(table))
	}

	// The on-demand complement preserves the table's order, so the API can
	// report it without re-sorting and a reader can map it back by eye.
	lastIdx := -1
	for _, s := range onDemand {
		idx := -1
		for i, c := range table {
			if c == s {
				idx = i
				break
			}
		}
		if idx < lastIdx {
			t.Errorf("on-demand set out of table order at %q (%d after %d)", s, idx, lastIdx)
		}
		lastIdx = idx
	}

	// IsDefaultFamilyRole agrees with the list, exactly (no prefix matching:
	// "-qa" is a role, "qa" and "-qa2" are not).
	for _, s := range table {
		want := false
		for _, d := range def {
			if d == s {
				want = true
			}
		}
		if got := IsDefaultFamilyRole(s); got != want {
			t.Errorf("IsDefaultFamilyRole(%q) = %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "qa", "-qa2", "-foreman", "-ops"} {
		if IsDefaultFamilyRole(s) {
			t.Errorf("IsDefaultFamilyRole(%q) = true — the comparison must be exact", s)
		}
	}
}

// TestLaneClass_DefaultFamilyIsForemanPlusQaSyncDocs pins the owner's
// 2026-11-16 ruling literally: four lanes at onboarding — the foreman (the
// project row itself) plus -qa, -sync and -docs — and the six retired-ish
// roles minted only on demand.
func TestLaneClass_DefaultFamilyIsForemanPlusQaSyncDocs(t *testing.T) {
	if got, want := DefaultFamilyRoles(), []string{"-qa", "-sync", "-docs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultFamilyRoles() = %v, want %v", got, want)
	}
	if got, want := DefaultFamilyLaneNames("auger"), []string{"auger", "auger-qa", "auger-sync", "auger-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultFamilyLaneNames(\"auger\") = %v, want %v", got, want)
	}

	// The smaller template must stay SMALLER: a default equal to the whole
	// table is the regression this row exists to prevent.
	full := len(LaneRoleSuffixes()) + 1 // +1 for the foreman row
	if got := len(DefaultFamilyLaneNames("x")); got >= full {
		t.Errorf("default family is %d of %d lanes — onboarding must not mint the full role set", got, full)
	}
	if got, want := len(DefaultFamilyLaneNames("x")), 4; got != want {
		t.Errorf("default family = %d lanes, want %d (foreman + qa + sync + docs)", got, want)
	}

	// The six on-demand roles are named explicitly, so a future edit that
	// promotes one of them into the default set fails HERE with the role name.
	for _, s := range []string{"-pm", "-dogfood", "-perf", "-releng", "-review", "-readme"} {
		if IsDefaultFamilyRole(s) {
			t.Errorf("%q must be minted on demand, not at onboarding", s)
		}
		found := false
		for _, d := range OnDemandFamilyRoles() {
			if d == s {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from OnDemandFamilyRoles() — it would be impossible to mint", s)
		}
	}
}

// TestLaneClass_DefaultFamilyRolesAreCopies guards the shared table: these
// accessors hand out copies, so a caller appending to one cannot corrupt the
// template for the next request on the same server.
func TestLaneClass_DefaultFamilyRolesAreCopies(t *testing.T) {
	first := DefaultFamilyRoles()
	for i := range first {
		first[i] = "-corrupted"
	}
	if got, want := DefaultFamilyRoles(), []string{"-qa", "-sync", "-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mutating a returned slice changed the template: %v", got)
	}

	onDemand := OnDemandFamilyRoles()
	for i := range onDemand {
		onDemand[i] = "-corrupted"
	}
	if got := len(OnDemandFamilyRoles()); got != len(canonicalRoleSuffixes)-len(DefaultFamilyRoles()) {
		t.Errorf("on-demand set is %d roles after mutation, want the table minus the default set", got)
	}

	names := DefaultFamilyLaneNames("p")
	for i := range names {
		names[i] = "corrupted"
	}
	if got := DefaultFamilyLaneNames("q")[0]; got != "q" {
		t.Errorf("DefaultFamilyLaneNames is not fresh per call: %v", got)
	}

	table := LaneRoleSuffixes()
	for i := range table {
		table[i] = "-corrupted"
	}
	if got, want := LaneRoleSuffixes(), canonicalRoleSuffixes; !reflect.DeepEqual(got, want) {
		t.Errorf("mutating LaneRoleSuffixes() changed the canonical table: %v", got)
	}
}
