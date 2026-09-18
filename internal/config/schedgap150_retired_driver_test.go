package config

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-150 retired-driver guard, config side.
//
// 130 legacy `command` rows were cleared on 2026-09-17 (pm 42 / qa 31 / sync 26
// / dogfood 31) so that a re-enable cannot resurrect the retired dagger-era
// drivers. The API write path closes half of that door (createProject /
// updateProject reject the 5 names — internal/api/server_projects_test.go).
// The CONFIG LOADER is the other enable path and had no guard at all:
// fleet.toml re-pins `enabled` unconditionally, so a relic row (a retired
// command, enabled=0) that is also listed in fleet.toml came back ENABLED with
// its retired command intact — the exact forbidden state, reachable at every
// daemon boot.
//
// These tests pin the invariant on the loader path:
//
//	no project row may be ENABLED while its `command` names a retired driver.
//
// The contract asserted here is precise about what the loader owns: the loader
// must never WRITE enabled=1 onto a row whose command drives a retired driver
// (TestSCHEDGAP150_EnablePathRefusesRetiredDriverCommand). A row that was
// already enabled before the guard existed is NOT auto-disabled by the loader —
// that would silently stop a live lane — it is left alone and named by the
// fleet-wide gate in ops/check-fleet-invariants.py (check #4). The invariant
// sweep test below measures exactly that distinction.
//
// Boundaries NOT asserted here, and why:
//   - The API write path is pinned in internal/api/server_projects_test.go.
//     This file deliberately does not duplicate it.
//   - The fleet-wide gate itself is pinned by tests/
//     test_check_fleet_invariants_retired_drivers.py and by the api package's
//     Go<->Python parity test.

// schedgap150RetiredDrivers is the brief's list, spelled out independently of
// the production var so a rename or a dropped name fails loudly instead of
// comparing a list to itself.
var schedgap150RetiredDrivers = []string{
	"pm-standin-tick.sh",
	"qa-scheduler-tick.sh",
	"sync-scheduler-tick.sh",
	"dogfood-scheduler-tick.sh",
	"dagger-role-tick.sh",
}

// seedSchedgap150Project inserts a project row directly, bypassing the API gate
// — the only way to build the legacy relic state the guard exists to stop.
func seedSchedgap150Project(t *testing.T, db *sql.DB, name, command string, enabled bool) {
	t.Helper()
	p := &database.Project{
		Name: name, RepoURL: "local://" + name, Workdir: "/tmp/" + name,
		Weight: 10, Priority: 5, CooldownS: 7200, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: enabled, Command: command,
	}
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("seed project %q: %v", name, err)
	}
}

// enableEveryProjectToml is a fleet.toml that asks for every seeded lane to be
// enabled — the restart that used to flip a relic back on.
func enableEveryProjectToml(names ...string) *FleetConfig {
	enabled := true
	cfg := &FleetConfig{}
	for _, n := range names {
		cfg.Projects = append(cfg.Projects, ProjectDef{
			Name: n, RepoURL: "local://" + n, Workdir: "/tmp/" + n, Enabled: &enabled,
		})
	}
	return cfg
}

// TestSCHEDGAP150_EnablePathRefusesRetiredDriverCommand drives the loader's
// enable path once per retired driver: the row stays disabled and the boot
// still succeeds (a relic must not wedge the daemon).
//
// RED-proof: delete the retiredDriverInCommand guard in loader.go and every
// subtest here reports "enabled despite the retired driver".
func TestSCHEDGAP150_EnablePathRefusesRetiredDriverCommand(t *testing.T) {
	for _, driver := range schedgap150RetiredDrivers {
		t.Run(driver, func(t *testing.T) {
			db, err := database.InitDB(":memory:")
			if err != nil {
				t.Fatalf("InitDB: %v", err)
			}
			defer db.Close()
			ctx := context.Background()

			name := "relic-" + strings.TrimSuffix(driver, ".sh")
			seedSchedgap150Project(t, db, name, driver, false)

			if err := ApplyFleetConfig(ctx, db, enableEveryProjectToml(name)); err != nil {
				t.Fatalf("T-150-E1 FAIL: boot errored on a relic row (a legacy command must not wedge the daemon): %v", err)
			}

			got, err := database.GetProject(ctx, db, name)
			if err != nil {
				t.Fatalf("GetProject: %v", err)
			}
			if got.Enabled {
				t.Errorf("T-150-E2 FAIL: loader enabled a project whose command drives the retired driver %s", driver)
			}
			// The guard must refuse the ENABLE only — it must not rewrite or
			// clear the operator's command behind their back.
			if got.Command != driver {
				t.Errorf("T-150-E3 FAIL: loader rewrote the stored command: got %q want %q", got.Command, driver)
			}
		})
	}
}

// TestSCHEDGAP150_EnablePathStillEnablesCleanProjects is the control: the guard
// is not a blanket enable-reject. A lane with no custom command, and a lane
// with a live (non-retired) executor, must both be enabled by the same loader
// run that refuses the relics.
func TestSCHEDGAP150_EnablePathStillEnablesCleanProjects(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	seedSchedgap150Project(t, db, "clean-default", "", false)
	seedSchedgap150Project(t, db, "clean-executor", "/usr/local/bin/foreman-tick.sh", false)

	if err := ApplyFleetConfig(ctx, db, enableEveryProjectToml("clean-default", "clean-executor")); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	for _, name := range []string{"clean-default", "clean-executor"} {
		got, err := database.GetProject(ctx, db, name)
		if err != nil {
			t.Fatalf("GetProject(%s): %v", name, err)
		}
		if !got.Enabled {
			t.Errorf("T-150-C1 FAIL: the guard refused to enable the clean project %q — it must reject retired drivers, not all commands", name)
		}
	}
}

// TestSCHEDGAP150_SeededFleetInvariantSweep is the guard as a test over a
// seeded DB: a mixed fleet (relics + real lanes, some already enabled) is put
// through a full loader boot, and the invariant is asserted over EVERY row.
//
// The sweep is scoped to the rows the LOADER produced enabled — the rows that
// were already enabled before the boot keep their state (the loader never
// disables; see the file comment). A run that leaves a loader-enabled row
// carrying a retired driver fails here.
func TestSCHEDGAP150_SeededFleetInvariantSweep(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Five relics, disabled: the 2026-09-17 cleanup left nothing behind, so
	// these are hand-built to prove the door is shut.
	relics := make([]string, 0, len(schedgap150RetiredDrivers))
	clean := make([]string, 0, 3)
	for _, driver := range schedgap150RetiredDrivers {
		name := "sweep-relic-" + strings.TrimSuffix(driver, ".sh")
		seedSchedgap150Project(t, db, name, driver, false)
		relics = append(relics, name)
	}
	// Real lanes, disabled: must come up enabled.
	for _, name := range []string{"sweep-lane-a", "sweep-lane-b", "sweep-lane-c"} {
		seedSchedgap150Project(t, db, name, "", false)
		clean = append(clean, name)
	}

	all := append(append([]string{}, relics...), clean...)
	if err := ApplyFleetConfig(ctx, db, enableEveryProjectToml(all...)); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	seen := 0
	for _, p := range projects {
		seen++
		if !p.Enabled {
			continue
		}
		for _, driver := range schedgap150RetiredDrivers {
			if strings.Contains(p.Command, driver) {
				t.Errorf("T-150-S1 FAIL: enabled project %q carries the retired driver %s (command %q)", p.Name, driver, p.Command)
			}
		}
	}
	if seen != len(all) {
		t.Errorf("T-150-S2 FAIL: swept %d rows, seeded %d — the sweep missed rows", seen, len(all))
	}

	// Each real lane must actually be enabled, or the sweep above is vacuous.
	for _, name := range clean {
		got, err := database.GetProject(ctx, db, name)
		if err != nil {
			t.Fatalf("GetProject(%s): %v", name, err)
		}
		if !got.Enabled {
			t.Errorf("T-150-S3 FAIL: real lane %q was not enabled — the sweep asserts nothing about clean lanes", name)
		}
	}
}

// TestSCHEDGAP150_RetiredDriverListNames pins the 5 names themselves, so a
// dropped or renamed entry fails here rather than passing a self-comparison.
func TestSCHEDGAP150_RetiredDriverListNames(t *testing.T) {
	if len(retiredDriverScripts) != len(schedgap150RetiredDrivers) {
		t.Fatalf("T-150-L1 FAIL: retired driver list has %d names (%v), want %d", len(retiredDriverScripts), retiredDriverScripts, len(schedgap150RetiredDrivers))
	}
	for i, want := range schedgap150RetiredDrivers {
		if retiredDriverScripts[i] != want {
			t.Errorf("T-150-L2 FAIL: retired driver %d = %q, want %q", i, retiredDriverScripts[i], want)
		}
	}
	// The matcher must find each name inside a realistic command line, and
	// must not fire on a command that merely resembles one.
	for _, driver := range schedgap150RetiredDrivers {
		cmd := "/usr/bin/bash /home/kara/.hermes/scripts/" + driver + " --foreman helix"
		if got := retiredDriverInCommand(cmd); got != driver {
			t.Errorf("T-150-L3 FAIL: retiredDriverInCommand(%q) = %q, want %q", cmd, got, driver)
		}
	}
	for _, clean := range []string{"", "hermes chat -q 'tick'", "/usr/local/bin/foreman-tick.sh", "python3 -m dogfood"} {
		if got := retiredDriverInCommand(clean); got != "" {
			t.Errorf("T-150-L4 FAIL: retiredDriverInCommand(%q) = %q, want no match", clean, got)
		}
	}
}

// TestSCHEDGAP150_RetiredDriverListParityWithAPIGate pins the config mirror to
// the canonical api list by parsing both SOURCES. The two Go lists cannot be
// one definition: internal/api imports internal/scheduler, which imports
// internal/config, so config -> api is an import cycle. Parsing the source is
// the same technique the api package uses to pin its list to the Python gate
// (TestCheckFleetInvariants_RetiredDriversMatchPythonGate), and it fails the
// moment either list is edited alone.
func TestSCHEDGAP150_RetiredDriverListParityWithAPIGate(t *testing.T) {
	apiSrc, err := os.ReadFile(filepath.Join("..", "..", "internal", "api", "retired_drivers.go"))
	if err != nil {
		t.Fatalf("read internal/api/retired_drivers.go: %v", err)
	}
	apiList := schedgap150QuotedNamesIn(t, string(apiSrc),
		`(?s)retiredDriverScripts\s*=\s*\[\]string\{(.*?)\}`)

	if len(apiList) != len(schedgap150RetiredDrivers) {
		t.Fatalf("T-150-P1 FAIL: internal/api/retired_drivers.go lists %d drivers (%v), want %d", len(apiList), apiList, len(schedgap150RetiredDrivers))
	}
	for i := range apiList {
		if apiList[i] != retiredDriverScripts[i] {
			t.Errorf("T-150-P2 FAIL: retired driver lists diverged at %d:\n  config = %v\n  api    = %v", i, retiredDriverScripts, apiList)
			break
		}
	}
}

// schedgap150QuotedNamesIn extracts the double-quoted strings inside the first
// match of pattern in src.
func schedgap150QuotedNamesIn(t *testing.T, src, pattern string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("pattern %q not found in source", pattern)
	}
	matches := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(matches))
	for _, q := range matches {
		out = append(out, q[1])
	}
	return out
}
