package scheduler

import (
	"sort"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1696 — the ADMISSION LAW, derived in ONE place.
//
// LAW: a FOREMAN lane carries admission_mode=tasks; EVERYTHING ELSE carries
// cooldown. A lane that OWNS SATELLITES is a foreman even when it carries a
// parent for org nesting (logsey/pulse/lore/digest under h3; release-engineer
// under coding-hermes-scheduler). The only satellite classes are the role-name
// suffixes and non-owner parented lanes.
//
// The class rule here is IDENTICAL to the shipped fleet_runrate_audit.py
// (§3/§3b/§3c) so the boundary guard and the watchdog cannot disagree:
//
//	sat          name ends in a role suffix
//	owners       a lane whose "<name><suffix>" is ALSO a project
//	fore         not sat and an owner
//	solo         not sat and not an owner and has no parent  (a primary)
//	parented_sat not sat and not an owner and has a parent
//	is_foreman   fore OR solo

// LaneClassForeman / LaneClassSatellite are the two lane classes.
const (
	LaneClassForeman   = "foreman"
	LaneClassSatellite = "satellite"
)

// laneRoleSuffixes is the ONE satellite role-suffix table (SCHED-GAP-1696). It
// unifies the previously-duplicated lists: the reporter-class table in
// nowork.go, the pause/resume cascade table in server_projects.go and the
// dashboard's inferred-parent table.
var laneRoleSuffixes = []string{
	"-qa", "-pm", "-sync", "-dogfood", "-perf",
	"-releng", "-review", "-docs", "-readme",
}

// LaneRoleSuffixes returns the canonical satellite role-suffix list (a copy —
// callers must not mutate the shared table).
func LaneRoleSuffixes() []string {
	out := make([]string, len(laneRoleSuffixes))
	copy(out, laneRoleSuffixes)
	return out
}

// IsRoleSuffixLane reports whether the name ends in a satellite role suffix.
func IsRoleSuffixLane(name string) bool {
	for _, s := range laneRoleSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// ownsSatelliteName reports whether "<name><suffix>" is also a project — the
// audit's owner test (a lane owns the satellite NAMED after it).
func ownsSatelliteName(name string, all map[string]bool) bool {
	for _, s := range laneRoleSuffixes {
		if all[name+s] {
			return true
		}
	}
	return false
}

// LaneOwnsSatellite reports whether "<name><suffix>" is also a project — the
// audit's owner test, exported so the API boundary check and the boot pass use
// the SAME derivation as the count.
func LaneOwnsSatellite(name string, all map[string]bool) bool {
	return ownsSatelliteName(name, all)
}

// LaneClass derives foreman|satellite from a lane's name, its parent reference
// and whether it owns satellites (the audit's `owners` test). Satellite iff the
// name ends in a role suffix, OR the lane has a parent AND does not own
// satellites; foreman otherwise (role-less root primaries, and parented lanes
// that DO own satellites).
func LaneClass(name, parent string, ownsSatellites bool) string {
	if IsRoleSuffixLane(name) {
		return LaneClassSatellite
	}
	if ownsSatellites {
		return LaneClassForeman
	}
	if parent == "" {
		return LaneClassForeman // solo primary
	}
	return LaneClassSatellite // parented, ownerless
}

// ExpectedAdmission is the lane-level admission_mode a class MUST carry:
// foreman → tasks, satellite → cooldown.
func ExpectedAdmission(class string) string {
	if class == LaneClassForeman {
		return database.AdmissionModeTasks
	}
	return database.AdmissionModeCooldown
}

// AdmissionLawViolations returns the ENABLED lanes that break the admission law
// — the exact set fleet_runrate_audit.py §3/§3b/§3c reports, so /api/v1/status
// reads the same number the watchdog does:
//
//	not_tasks    — a foreman whose LANE-LEVEL mode is not "tasks"
//	fore_wrong_ns — a foreman whose NAMESPACE mode is not "tasks"
//	sat_on_tasks — a satellite whose LANE-LEVEL mode is "tasks"
//
// nsAdmission maps namespace id → admission_mode. Uses the LANE-LEVEL mode (the
// column that beats both the lane's cooldown and its namespace), matching the
// audit — never the resolved mode. Sorted for deterministic output.
func AdmissionLawViolations(projects []database.Project, nsAdmission map[string]string) []string {
	all := make(map[string]bool, len(projects))
	for _, p := range projects {
		all[p.Name] = true
	}
	owns := make(map[string]bool, len(projects))
	for _, p := range projects {
		owns[p.Name] = ownsSatelliteName(p.Name, all)
	}

	var out []string
	for _, p := range projects {
		if !p.Enabled {
			continue
		}
		nsID := ""
		if p.NamespaceID != nil {
			nsID = *p.NamespaceID
		}
		cls := LaneClass(p.Name, p.Parent, owns[p.Name])
		if cls == LaneClassForeman {
			// not_tasks: lane-level mode must be tasks.
			if p.AdmissionMode != database.AdmissionModeTasks {
				out = append(out, p.Name)
				continue
			}
			// fore_wrong_ns: the namespace must be tasks-mode.
			if nsAdmission[nsID] != database.AdmissionModeTasks {
				out = append(out, p.Name)
			}
			continue
		}
		// sat_on_tasks: a satellite must never carry lane-level tasks.
		if p.AdmissionMode == database.AdmissionModeTasks {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}
