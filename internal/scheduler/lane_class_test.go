package scheduler

import (
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func mkLane(name, parent, mode, ns string, enabled bool) database.Project {
	nsid := ns
	return database.Project{
		Name:          name,
		Parent:        parent,
		AdmissionMode: mode,
		NamespaceID:   &nsid,
		Enabled:       enabled,
	}
}

// SCHED-GAP-1696 criterion 4 (ownership edge) + the class rule.
func TestLaneClass_Derivation(t *testing.T) {
	cases := []struct {
		name, parent string
		owns         bool
		want         string
	}{
		{"auger-qa", "auger", true, LaneClassSatellite}, // role suffix wins
		{"coding-hermes-scheduler-dogfood", "coding-hermes-scheduler", true, LaneClassSatellite},
		{"auger", "", true, LaneClassForeman},         // owner, no suffix
		{"solo-primary", "", false, LaneClassForeman}, // solo primary
		{"h3-shim", "h3", false, LaneClassSatellite},  // parented, ownerless
		// OWNERSHIP EDGE: a parented lane that OWNS satellites is a foreman.
		{"release-engineer", "coding-hermes-scheduler", true, LaneClassForeman},
		{"logsey", "h3", true, LaneClassForeman},
	}
	for _, c := range cases {
		if got := LaneClass(c.name, c.parent, c.owns); got != c.want {
			t.Errorf("LaneClass(%q, parent=%q, owns=%v) = %q, want %q", c.name, c.parent, c.owns, got, c.want)
		}
	}
}

func TestExpectedAdmission(t *testing.T) {
	if got := ExpectedAdmission(LaneClassForeman); got != database.AdmissionModeTasks {
		t.Errorf("foreman → %q, want tasks", got)
	}
	if got := ExpectedAdmission(LaneClassSatellite); got != database.AdmissionModeCooldown {
		t.Errorf("satellite → %q, want cooldown", got)
	}
}

// Criterion 5: the count reads 0 on a clean fleet (foreman=tasks in a tasks ns,
// satellite=cooldown).
func TestAdmissionLawViolations_CleanReadsZero(t *testing.T) {
	projects := []database.Project{
		mkLane("auger", "", "tasks", "coding-hermes", true), // foreman, tasks, tasks ns
		mkLane("auger-qa", "auger", "cooldown", "qa", true), // satellite, cooldown
		mkLane("auger-pm", "auger", "cooldown", "pm", true),
	}
	nsAdm := map[string]string{"coding-hermes": "tasks", "qa": "cooldown", "pm": "cooldown"}
	if v := AdmissionLawViolations(projects, nsAdm); len(v) != 0 {
		t.Fatalf("clean fleet must read 0, got %v", v)
	}
}

// §3b: a satellite carrying lane-level tasks is a violation.
func TestAdmissionLawViolations_SatelliteOnTasks(t *testing.T) {
	projects := []database.Project{
		mkLane("auger", "", "tasks", "coding-hermes", true),
		mkLane("auger-qa", "auger", "tasks", "coding-hermes", true), // VIOLATION
	}
	nsAdm := map[string]string{"coding-hermes": "tasks"}
	v := AdmissionLawViolations(projects, nsAdm)
	if len(v) != 1 || v[0] != "auger-qa" {
		t.Fatalf("want [auger-qa], got %v", v)
	}
}

// §3: a foreman whose lane-level mode is not tasks is a violation.
func TestAdmissionLawViolations_ForemanNotTasks(t *testing.T) {
	projects := []database.Project{
		mkLane("auger", "", "cooldown", "coding-hermes", true), // VIOLATION
		mkLane("auger-qa", "auger", "cooldown", "qa", true),
	}
	nsAdm := map[string]string{"coding-hermes": "tasks", "qa": "cooldown"}
	v := AdmissionLawViolations(projects, nsAdm)
	if len(v) != 1 || v[0] != "auger" {
		t.Fatalf("want [auger], got %v", v)
	}
}

// §3c: a foreman in a non-tasks namespace is a violation.
func TestAdmissionLawViolations_ForemanWrongNamespace(t *testing.T) {
	projects := []database.Project{
		mkLane("auger", "", "tasks", "qa", true), // VIOLATION (ns not tasks)
		mkLane("auger-qa", "auger", "cooldown", "qa", true),
	}
	nsAdm := map[string]string{"qa": "cooldown"}
	v := AdmissionLawViolations(projects, nsAdm)
	if len(v) != 1 || v[0] != "auger" {
		t.Fatalf("want [auger], got %v", v)
	}
}

// Disabled lanes never count.
func TestAdmissionLawViolations_DisabledIgnored(t *testing.T) {
	projects := []database.Project{
		mkLane("auger", "", "cooldown", "qa", false), // disabled violation-shaped
	}
	nsAdm := map[string]string{"qa": "cooldown"}
	if v := AdmissionLawViolations(projects, nsAdm); len(v) != 0 {
		t.Fatalf("disabled lane counted: %v", v)
	}
}

// The ownership edge drives the class, not the bare parent: a parented lane
// that owns satellites is a foreman (so tasks is its expected mode).
func TestAdmissionLawViolations_OwnershipEdgeForeman(t *testing.T) {
	projects := []database.Project{
		mkLane("release-engineer", "coding-hermes-scheduler", "tasks", "coding-hermes", true),
		mkLane("release-engineer-qa", "release-engineer", "cooldown", "qa", true),
	}
	nsAdm := map[string]string{"coding-hermes": "tasks", "qa": "cooldown"}
	if v := AdmissionLawViolations(projects, nsAdm); len(v) != 0 {
		t.Fatalf("parented-but-owns lane must be a compliant foreman, got %v", v)
	}
}
