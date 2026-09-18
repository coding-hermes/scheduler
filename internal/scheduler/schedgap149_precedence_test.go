package scheduler

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-149 precedence pin: the admission-mode resolution law is
// project > namespace > cooldown default.
//
// admissionModeFor is the single definition the packers, the ADMIT decider and
// the post-tick paths all read, and admissionModeForProject is a second
// implementation of the SAME law for callers that have a database handle but
// no packer maps. Until now the law was only ever exercised end-to-end through
// the multi-pool packer (internal/scheduler/admission_mode_test.go, T-MODE-3/4),
// which covers the project-beats-namespace rung and an EXPLICIT cooldown
// namespace — never the fall-through rung itself: the cooldown default that
// applies when neither level names a mode, or when the namespace id is unknown
// to the map.
//
// That rung is the one the 2026-09-17/18 config work leans on. The operator
// flipped the qa/pm/dogfood/sync namespaces to "cooldown"; a lane whose
// namespace row or fleet.toml entry goes missing must resolve to cooldown
// (cron pacing), never to tasks (back-to-back spawning) — an unknown namespace
// has no claim on the work-driven waiver.
//
// The tests below pin both implementations against the same inputs, so the two
// copies cannot drift apart silently.

// TestSCHEDGAP149_AdmissionModePrecedence is the table-driven resolution law.
func TestSCHEDGAP149_AdmissionModePrecedence(t *testing.T) {
	const (
		tasks    = database.AdmissionModeTasks
		cooldown = database.AdmissionModeCooldown
	)

	cases := []struct {
		name    string
		project string // projects.admission_mode ("" = inherit)
		nsID    string // projects.namespace_id
		nsMode  string // namespaces.admission_mode for nsID ("" = no default)
		want    string
		// wantNSAbsent is the resolved mode when the namespace is NOT present
		// in the map (or the map is nil) — the rung the resolution falls to
		// when the namespace default is simply unavailable. It equals want
		// whenever the namespace contributes nothing either way.
		wantNSAbsent string
	}{
		{
			name:    "project tasks beats namespace cooldown",
			project: tasks, nsID: "qa", nsMode: cooldown, want: tasks, wantNSAbsent: tasks,
		},
		{
			name:    "project cooldown beats namespace tasks",
			project: cooldown, nsID: "foremen", nsMode: tasks, want: cooldown, wantNSAbsent: cooldown,
		},
		{
			name:    "namespace tasks applies when the project inherits",
			project: "", nsID: "foremen", nsMode: tasks, want: tasks, wantNSAbsent: cooldown,
		},
		{
			name:    "namespace cooldown applies when the project inherits",
			project: "", nsID: "qa", nsMode: cooldown, want: cooldown, wantNSAbsent: cooldown,
		},
		{
			name:    "cooldown default when neither level names a mode",
			project: "", nsID: "satellite", nsMode: "", want: cooldown, wantNSAbsent: cooldown,
		},
		{
			name:    "cooldown default when the namespace is unknown to the map",
			project: "", nsID: "no-such-namespace", nsMode: "", want: cooldown, wantNSAbsent: cooldown,
		},
		{
			name:    "cooldown default when the project has no namespace at all",
			project: "", nsID: "", nsMode: "", want: cooldown, wantNSAbsent: cooldown,
		},
		{
			name:    "a nonsense project mode falls through to the namespace",
			project: "turbo", nsID: "foremen", nsMode: tasks, want: tasks, wantNSAbsent: cooldown,
		},
		{
			name:    "a nonsense project mode falls through to the cooldown default",
			project: "turbo", nsID: "satellite", nsMode: "", want: cooldown, wantNSAbsent: cooldown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// nsModes is the per-namespace map the packers build, keyed by
			// namespace id. A namespace carrying no default is simply absent
			// (equivalently present with an empty value) — both must fall
			// through, so both shapes are asserted.
			absent := map[string]string{}
			if tc.nsMode != "" {
				absent[tc.nsID] = tc.nsMode
			}
			if got := admissionModeFor(tc.project, tc.nsID, absent); got != tc.want {
				t.Errorf("T-149-PR1 FAIL (namespace absent from the map): admissionModeFor(%q, %q) = %q, want %q",
					tc.project, tc.nsID, got, tc.want)
			}

			present := map[string]string{tc.nsID: tc.nsMode}
			if got := admissionModeFor(tc.project, tc.nsID, present); got != tc.want {
				t.Errorf("T-149-PR2 FAIL (namespace present with an empty value): admissionModeFor(%q, %q) = %q, want %q",
					tc.project, tc.nsID, got, tc.want)
			}

			// A nil map is the "no namespace state read at all" shape (the
			// post-tick path builds its map from whatever namespaces it
			// managed to read). It must behave exactly like an empty map —
			// never like a namespace that carries a default.
			if got := admissionModeFor(tc.project, tc.nsID, nil); got != tc.wantNSAbsent {
				t.Errorf("T-149-PR3 FAIL (nil map): admissionModeFor(%q, %q) = %q, want %q",
					tc.project, tc.nsID, got, tc.wantNSAbsent)
			}
		})
	}
}

// TestSCHEDGAP149_AdmissionModePrecedenceMatchesDBResolver pins the two
// implementations of the law together over the same seeded state: the
// map-based admissionModeFor and the DB-backed admissionModeForProject must
// agree on every rung, including the cooldown default.
//
// This is the contract the config loader feeds. The loader pins the same two
// columns (projects.admission_mode, namespaces.admission_mode) from
// fleet.toml — see TestSCHEDGAP149_FilePinsRepinOverDBDrift in
// internal/config — so a loader that stopped pinning a project override, or a
// resolver that read the rungs in the wrong order, shows up here as a
// disagreement between the two resolvers over real rows.
func TestSCHEDGAP149_AdmissionModePrecedenceMatchesDBResolver(t *testing.T) {
	const (
		tasks    = database.AdmissionModeTasks
		cooldown = database.AdmissionModeCooldown
	)

	cases := []struct {
		name    string
		nsID    string
		nsMode  string
		project string
		want    string
	}{
		{name: "project override wins", nsID: "ns-pr-a", nsMode: cooldown, project: tasks, want: tasks},
		{name: "project override wins against a tasks namespace", nsID: "ns-pr-b", nsMode: tasks, project: cooldown, want: cooldown},
		{name: "namespace default applies", nsID: "ns-pr-c", nsMode: tasks, project: "", want: tasks},
		{name: "cooldown default when both are empty", nsID: "ns-pr-d", nsMode: "", project: "", want: cooldown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.InitDB(":memory:")
			if err != nil {
				t.Fatalf("InitDB: %v", err)
			}
			defer db.Close()
			ctx := context.Background()

			enabled := true
			ns := &database.Namespace{
				ID: tc.nsID, Weight: 100, Reserved: 1, HardCap: 100,
				Enabled: true, MaxConcurrent: 4, AdmissionMode: tc.nsMode,
			}
			if err := database.CreateNamespace(ctx, db, ns); err != nil {
				t.Fatalf("CreateNamespace: %v", err)
			}
			nsID := tc.nsID
			p := &database.Project{
				Name: "pr-lane-" + tc.nsID, RepoURL: "local://pr", Workdir: t.TempDir(),
				Weight: 10, Priority: 5, CooldownS: 7200, DecayRate: 1.0,
				Model: "m", Provider: "p", Enabled: enabled,
				NamespaceID: &nsID, AdmissionMode: tc.project,
			}
			if err := database.CreateProject(ctx, db, p); err != nil {
				t.Fatalf("CreateProject: %v", err)
			}

			want := tc.want
			fromMap := admissionModeFor(tc.project, tc.nsID, map[string]string{tc.nsID: tc.nsMode})
			if fromMap != want {
				t.Errorf("T-149-DBR1 FAIL: admissionModeFor = %q, want %q", fromMap, want)
			}
			fromDB := admissionModeForProject(db, p.Name)
			if fromDB != want {
				t.Errorf("T-149-DBR2 FAIL: admissionModeForProject = %q, want %q", fromDB, want)
			}
			if fromDB != fromMap {
				t.Errorf("T-149-DBR3 FAIL: the two resolvers disagree over the same rows: map-based %q, DB-backed %q", fromMap, fromDB)
			}
		})
	}
}
