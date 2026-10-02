package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-138 — the lane onboarding shape gate.
//
// Three defects arrived together with the freshly registered `trouble` family
// (2026-09-17): a retired driver command (refused since SCHED-GAP-150), an
// off-convention sync workdir (~/.hermes/stand-in/pm/trouble-sync instead of
// ~/.hermes/sync-workdirs/trouble-sync) and an ENABLED row with no pacing
// policy at all (adaptive_cooldown=0, floor=0, ceiling=0). This file covers the
// write path that must refuse (or repair) those shapes from here on, and pins
// the constants the gate shares with ops/check-fleet-invariants.py.
//
// Every test drives the REAL handlers through the shared API test stack
// (newAPITestServer: in-memory DB + armed operator gate), so a regression in
// the wiring — not just in the pure rules — fails here.

// gap138LaneBody is the minimal create body for one lane.
func gap138LaneBody(name, workdir string, extra map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"name":     name,
		"repo_url": "local:/tmp/" + name,
		"workdir":  workdir,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// gap138SeedLane inserts a project row DIRECTLY through the DB layer — the
// documented bypass (the fleet.toml loader, the reconciler and the cascade all
// write this way), which lets a test plant the legacy shapes the API gate must
// grandfather instead of creating them through the very handlers under test.
func gap138SeedLane(t *testing.T, db *sql.DB, p database.Project) *database.Project {
	t.Helper()
	if p.Weight == 0 {
		p.Weight = 10
	}
	if p.Priority == 0 {
		p.Priority = 5
	}
	if p.CooldownS == 0 {
		p.CooldownS = 900
	}
	if p.DecayRate == 0 {
		p.DecayRate = 1.0
	}
	if p.Model == "" {
		p.Model = "test"
	}
	if p.Provider == "" {
		p.Provider = "test"
	}
	if err := database.CreateProject(context.Background(), db, &p); err != nil {
		t.Fatalf("seed lane %s: %v", p.Name, err)
	}
	got, err := database.GetProject(context.Background(), db, p.Name)
	if err != nil {
		t.Fatalf("read back seeded lane %s: %v", p.Name, err)
	}
	return got
}

// TestSCHEDGAP138_CreateRejectsOffConventionWorkdir drives every satellite
// family through POST /api/v1/projects with the workdir the family must not
// use, and proves no row is written. The -sync case is the MEASURED defect:
// the lane was registered under stand-in/pm, the directory the -qa family uses.
func TestSCHEDGAP138_CreateRejectsOffConventionWorkdir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	cases := []struct {
		name      string // lane name (distinct per case, so no case can be masked by a 409)
		workdir   string // the off-convention workdir offered
		canonical string // the path the refusal must name
	}{
		// The MEASURED defect: the sync lane was registered under stand-in/pm.
		{"tr1-sync", filepath.Join(home, "stand-in", "pm", "tr1-sync"),
			filepath.Join(home, "sync-workdirs", "tr1-sync")},
		{"tr2-sync", "/tmp/tr2-sync",
			filepath.Join(home, "sync-workdirs", "tr2-sync")},
		// A sync lane's workdir is named after the LANE, never the primary.
		{"tr3-sync", filepath.Join(home, "sync-workdirs", "tr3"),
			filepath.Join(home, "sync-workdirs", "tr3-sync")},
		{"tr4-qa", filepath.Join(home, "sync-workdirs", "tr4-qa"),
			filepath.Join(home, "stand-in", "pm", "tr4")},
		// -qa / -pm / -dogfood share the PRIMARY's stand-in dir, so the lane's
		// own name as the leaf is off-convention even under the right root.
		{"tr5-qa", filepath.Join(home, "stand-in", "pm", "tr5-qa"),
			filepath.Join(home, "stand-in", "pm", "tr5")},
		{"tr6-pm", "/tmp/tr6-pm",
			filepath.Join(home, "stand-in", "pm", "tr6")},
		{"tr7-dogfood", filepath.Join(home, "stand-in", "dogfood", "tr7-dogfood"),
			filepath.Join(home, "stand-in", "dogfood", "tr7")},
	}

	for i, tc := range cases {
		status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody(tc.name, tc.workdir, nil))
		if status != http.StatusBadRequest {
			t.Errorf("case %d: POST %s workdir=%q: status = %d, want 400 (body %v)",
				i, tc.name, tc.workdir, status, body)
			continue
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, "off-convention") {
			t.Errorf("case %d: error must say 'off-convention', got %q", i, msg)
		}
		if !strings.Contains(msg, tc.canonical) {
			t.Errorf("case %d: error must name the canonical workdir %q, got %q", i, tc.canonical, msg)
		}
		if _, err := database.GetProject(ctx, a.db, tc.name); err == nil {
			t.Errorf("case %d: project %q was created despite the off-convention workdir", i, tc.name)
		}
	}
}

// TestSCHEDGAP138_CreateAcceptsCanonicalWorkdirs is the acceptance control: the
// canonical path for every family — both measured variants of the -qa and -pm
// families included — is created 201, and a primary/root lane stays
// unconstrained by this gate (it may live anywhere).
func TestSCHEDGAP138_CreateAcceptsCanonicalWorkdirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	accepted := []struct{ name, workdir string }{
		{"ok-sync", filepath.Join(home, "sync-workdirs", "ok-sync")},
		{"ok-qa", filepath.Join(home, "stand-in", "pm", "ok")},
		{"ok2-qa", filepath.Join(home, "stand-in", "qa", "ok2")},
		{"ok-pm", filepath.Join(home, "stand-in", "pm", "ok")},
		{"ok2-pm", filepath.Join(home, "stand-in", "pm-lane", "ok2")},
		{"ok-dogfood", filepath.Join(home, "stand-in", "dogfood", "ok")},
		// Trailing slash / dot segments normalize to the same path.
		{"ok3-sync", filepath.Join(home, "sync-workdirs", "ok3-sync") + string(filepath.Separator)},
		// A primary lane is not constrained by the family convention.
		{"ok-primary", "/tmp/ok-primary"},
	}

	for i, tc := range accepted {
		status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody(tc.name, tc.workdir, nil))
		if status != http.StatusCreated {
			t.Errorf("case %d: POST %s workdir=%q: status = %d, want 201 (body %v)",
				i, tc.name, tc.workdir, status, body)
			continue
		}
		stored, err := database.GetProject(ctx, a.db, tc.name)
		if err != nil {
			t.Errorf("case %d: read back %s: %v", i, tc.name, err)
			continue
		}
		if filepath.Clean(stored.Workdir) != filepath.Clean(tc.workdir) {
			t.Errorf("case %d: stored workdir = %q, want %q", i, stored.Workdir, tc.workdir)
		}
	}
}

// TestSCHEDGAP138_UpdateRejectsOffConventionWorkdir proves the PUT path refuses
// a workdir move off the lane's convention and leaves the stored row untouched,
// then accepts a move to another canonical variant.
func TestSCHEDGAP138_UpdateRejectsOffConventionWorkdir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	canonical := filepath.Join(home, "sync-workdirs", "move-sync")
	gap138SeedLane(t, a.db, database.Project{
		Name: "move-sync", RepoURL: "local:/tmp/move-sync", Workdir: canonical,
		Enabled: true, AdaptiveCooldown: false, CooldownFloorS: 43200, CooldownCeilingS: 345600,
	})

	status, body := a.do(t, "PUT", "/api/v1/projects/move-sync", map[string]interface{}{
		"workdir": "/tmp/move-sync",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("PUT off-convention workdir: status = %d, want 400 (body %v)", status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, canonical) {
		t.Errorf("error must name the canonical workdir %q, got %q", canonical, msg)
	}
	got, err := database.GetProject(ctx, a.db, "move-sync")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Workdir != canonical {
		t.Errorf("workdir = %q, want unchanged %q", got.Workdir, canonical)
	}

	status, body = a.do(t, "PUT", "/api/v1/projects/move-sync", map[string]interface{}{
		"workdir": filepath.Join(home, "sync-workdirs", "move-sync"),
	})
	if status != http.StatusOK {
		t.Fatalf("PUT canonical workdir: status = %d, want 200 (body %v)", status, body)
	}
	got, err = database.GetProject(ctx, a.db, "move-sync")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Workdir != canonical {
		t.Errorf("workdir = %q, want %q", got.Workdir, canonical)
	}
}

// TestSCHEDGAP138_CreateRejectsUnarmedEnabledSatellite is the onboarding
// refusal: an ENABLED satellite that would be registered with no pacing policy
// (no adaptive cooldown, floor=0, ceiling=0) is refused before any DB write,
// and the message names the family pin so the fix is one number. The controls
// prove the gate is not a blanket "no satellite creates".
func TestSCHEDGAP138_CreateRejectsUnarmedEnabledSatellite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	// One lane per family: the refusal must name THAT family's pin.
	for _, tc := range []struct {
		name, workdir string
		pin           int
	}{
		{"bare-sync", filepath.Join(home, "sync-workdirs", "bare-sync"), 21600},
		{"bare-qa", filepath.Join(home, "stand-in", "pm", "bare"), 21600},
		{"bare-pm", filepath.Join(home, "stand-in", "pm-lane", "bare"), 86400},
		{"bare-dogfood", filepath.Join(home, "stand-in", "dogfood", "bare"), 259200},
	} {
		t.Run("reject_unarmed_enabled_"+tc.name, func(t *testing.T) {
			status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody(tc.name, tc.workdir,
				map[string]interface{}{"enabled": true}))
			if status != http.StatusBadRequest {
				t.Fatalf("POST enabled unarmed %s: status = %d, want 400 (body %v)", tc.name, status, body)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, "unarmed") {
				t.Errorf("error must say 'unarmed', got %q", msg)
			}
			if want := "cooldown_floor_s=" + strconv.Itoa(tc.pin); !strings.Contains(msg, want) {
				t.Errorf("error must name %s, got %q", want, msg)
			}
			if _, err := database.GetProject(ctx, a.db, tc.name); err == nil {
				t.Errorf("project %q was created despite being enabled+unarmed", tc.name)
			}
		})
	}

	controls := []struct {
		sub  string
		name string
		body map[string]interface{}
	}{
		// An explicit floor is the family-pin shape the reconciler writes.
		{"control_explicit_floor_accepted", "armed-sync", map[string]interface{}{
			"enabled": true, "cooldown_floor_s": 43200, "cooldown_ceiling_s": 345600,
		}},
		// adaptive_cooldown=true is a pacing policy (the fleet-wide invariant
		// gate forbids it on satellites — check 5b — so the family floor above
		// is the shape this gate recommends, but it is not a 400 here).
		{"control_adaptive_accepted", "adaptive-sync", map[string]interface{}{
			"enabled": true, "adaptive_cooldown": true,
		}},
		// A DISABLED row may be provisioned unarmed: create never auto-enables,
		// and the reconciler provisions parked rows then enables them.
		{"control_disabled_unarmed_accepted", "parked-sync", nil},
	}
	for _, tc := range controls {
		t.Run(tc.sub, func(t *testing.T) {
			workdir := filepath.Join(home, "sync-workdirs", tc.name)
			status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody(tc.name, workdir, tc.body))
			if status != http.StatusCreated {
				t.Fatalf("POST %s: status = %d, want 201 (body %v)", tc.name, status, body)
			}
			if _, err := database.GetProject(ctx, a.db, tc.name); err != nil {
				t.Errorf("control project %q was not created: %v", tc.name, err)
			}
		})
	}
}

// TestSCHEDGAP138_UpdateEnableTransitionArmsSatellite covers the one path that
// REPAIRS instead of refusing: enabling a satellite lane that carries no pacing
// policy applies the family pin in the same write. The caller is machinery —
// satellite-coverage-reconcile.py enables lanes with a bare {"enabled": true}
// and never reads the response status — so a refusal here would be a silent
// coverage loss. The audit event makes the repair visible.
func TestSCHEDGAP138_UpdateEnableTransitionArmsSatellite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	gap138SeedLane(t, a.db, database.Project{
		Name: "arm-sync", RepoURL: "local:/tmp/arm-sync",
		Workdir: filepath.Join(home, "sync-workdirs", "arm-sync"),
		Enabled: false, // unarmed + parked, the shape the reconciler provisions
	})

	status, body := a.do(t, "PUT", "/api/v1/projects/arm-sync", map[string]interface{}{"enabled": true})
	if status != http.StatusOK {
		t.Fatalf("PUT enabled=true: status = %d, want 200 (body %v)", status, body)
	}
	got, err := database.GetProject(ctx, a.db, "arm-sync")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !got.Enabled {
		t.Fatal("lane was not enabled")
	}
	if got.CooldownFloorS != 21600 {
		t.Errorf("cooldown_floor_s = %d, want the -sync family pin 21600", got.CooldownFloorS)
	}
	if got.CooldownCeilingS != 8*21600 {
		t.Errorf("cooldown_ceiling_s = %d, want the derived cap %d (8 × floor)", got.CooldownCeilingS, 8*21600)
	}
	if got.AdaptiveCooldown {
		t.Error("auto-arm must not switch adaptive cooldown on — satellites are paced by their family floor")
	}
	var events int
	if err := a.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE message LIKE 'lane auto-armed on enable: arm-sync%'`).Scan(&events); err != nil {
		t.Fatalf("count auto-arm events: %v", err)
	}
	if events != 1 {
		t.Errorf("auto-arm audit events = %d, want 1", events)
	}

	t.Run("explicit_pin_in_same_body_wins", func(t *testing.T) {
		gap138SeedLane(t, a.db, database.Project{
			Name: "arm2-sync", RepoURL: "local:/tmp/arm2-sync",
			Workdir: filepath.Join(home, "sync-workdirs", "arm2-sync"),
			Enabled: false,
		})
		status, body := a.do(t, "PUT", "/api/v1/projects/arm2-sync", map[string]interface{}{
			"enabled": true, "cooldown_floor_s": 21600,
		})
		if status != http.StatusOK {
			t.Fatalf("PUT enable + explicit floor: status = %d, want 200 (body %v)", status, body)
		}
		got, err := database.GetProject(ctx, a.db, "arm2-sync")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if got.CooldownFloorS != 21600 {
			t.Errorf("cooldown_floor_s = %d, want the caller's explicit 21600 (repair must not overwrite a pin)", got.CooldownFloorS)
		}
	})

	t.Run("no_repair_when_row_is_not_the_enable_transition", func(t *testing.T) {
		gap138SeedLane(t, a.db, database.Project{
			Name: "live-sync", RepoURL: "local:/tmp/live-sync",
			Workdir: filepath.Join(home, "sync-workdirs", "live-sync"),
			Enabled: true, CooldownFloorS: 999,
		})
		status, body := a.do(t, "PUT", "/api/v1/projects/live-sync", map[string]interface{}{"enabled": true})
		if status != http.StatusOK {
			t.Fatalf("PUT enabled=true (already enabled): status = %d, want 200 (body %v)", status, body)
		}
		got, err := database.GetProject(ctx, a.db, "live-sync")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if got.CooldownFloorS != 999 {
			t.Errorf("cooldown_floor_s = %d, want untouched 999 (a no-op re-enable is not an onboarding write)", got.CooldownFloorS)
		}
	})
}

// TestSCHEDGAP138_UpdateRejectsExplicitDisarm proves a PUT cannot remove the
// last pacing policy of an enabled satellite: zeroing the bounds on a
// non-adaptive lane is refused, and the stored row keeps its pin. The controls
// show the rule is precise — a lane still armed through its ceiling, and a
// DISABLED lane, are both free to zero a bound.
func TestSCHEDGAP138_UpdateRejectsExplicitDisarm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	gap138SeedLane(t, a.db, database.Project{
		Name: "disarm-sync", RepoURL: "local:/tmp/disarm-sync",
		Workdir: filepath.Join(home, "sync-workdirs", "disarm-sync"),
		Enabled: true, CooldownFloorS: 43200,
	})
	status, body := a.do(t, "PUT", "/api/v1/projects/disarm-sync", map[string]interface{}{
		"cooldown_floor_s": 0,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("PUT floor=0 on an enabled unarmed-after write: status = %d, want 400 (body %v)", status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "unarmed") {
		t.Errorf("error must say 'unarmed', got %q", msg)
	}
	got, err := database.GetProject(ctx, a.db, "disarm-sync")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownFloorS != 43200 {
		t.Errorf("cooldown_floor_s = %d, want unchanged 43200", got.CooldownFloorS)
	}

	t.Run("control_still_armed_through_ceiling", func(t *testing.T) {
		gap138SeedLane(t, a.db, database.Project{
			Name: "ceil-sync", RepoURL: "local:/tmp/ceil-sync",
			Workdir: filepath.Join(home, "sync-workdirs", "ceil-sync"),
			Enabled: true, CooldownCeilingS: 345600,
		})
		status, body := a.do(t, "PUT", "/api/v1/projects/ceil-sync", map[string]interface{}{
			"cooldown_floor_s": 0,
		})
		if status != http.StatusOK {
			t.Fatalf("PUT floor=0 on a ceiling-armed lane: status = %d, want 200 (body %v)", status, body)
		}
	})

	t.Run("control_disabled_lane", func(t *testing.T) {
		gap138SeedLane(t, a.db, database.Project{
			Name: "off-sync", RepoURL: "local:/tmp/off-sync",
			Workdir: filepath.Join(home, "sync-workdirs", "off-sync"),
			Enabled: false, CooldownFloorS: 43200,
		})
		status, body := a.do(t, "PUT", "/api/v1/projects/off-sync", map[string]interface{}{
			"cooldown_floor_s": 0,
		})
		if status != http.StatusOK {
			t.Fatalf("PUT floor=0 on a DISABLED lane: status = %d, want 200 (body %v)", status, body)
		}
	})
}

// TestSCHEDGAP138_LegacyUnarmedRowStaysUpdatable is the grandparent control: a
// row that is ALREADY enabled-and-unarmed (29 such lanes live in the fleet) can
// still take an unrelated write. This gate refuses the write that installs the
// shape, not every write to a legacy row.
func TestSCHEDGAP138_LegacyUnarmedRowStaysUpdatable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	gap138SeedLane(t, a.db, database.Project{
		Name: "legacy-sync", RepoURL: "local:/tmp/legacy-sync",
		Workdir: filepath.Join(home, "sync-workdirs", "legacy-sync"),
		Enabled: true, // enabled with no pacing policy — the legacy shape
	})
	status, body := a.do(t, "PUT", "/api/v1/projects/legacy-sync", map[string]interface{}{"weight": 20})
	if status != http.StatusOK {
		t.Fatalf("PUT weight on a legacy unarmed row: status = %d, want 200 (body %v)", status, body)
	}
	got, err := database.GetProject(ctx, a.db, "legacy-sync")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Weight != 20 {
		t.Errorf("weight = %d, want 20", got.Weight)
	}
	if !got.Enabled {
		t.Error("row was disabled by an unrelated write")
	}
}

// TestSCHEDGAP138_RetiredCommandRouteRefusedOnBothPaths re-proves the
// SCHED-GAP-150 refusal through the onboarding gate's own code path — the exact
// composition the `trouble` family arrived in (satellite name + canonical
// workdir + armed, so only the command is wrong) on CREATE and on UPDATE.
func TestSCHEDGAP138_RetiredCommandRouteRefusedOnBothPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	const driver = "sync-scheduler-tick.sh"
	command := "bash /home/kara/.hermes/scripts/" + driver

	t.Run("create", func(t *testing.T) {
		status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody(
			"retired-onboard-sync", filepath.Join(home, "sync-workdirs", "retired-onboard-sync"),
			map[string]interface{}{
				"enabled": true, "adaptive_cooldown": true, "command": command,
			}))
		if status != http.StatusBadRequest {
			t.Fatalf("POST with retired driver: status = %d, want 400 (body %v)", status, body)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, driver) {
			t.Errorf("error must name the retired driver %q, got %q", driver, msg)
		}
		if _, err := database.GetProject(ctx, a.db, "retired-onboard-sync"); err == nil {
			t.Error("project was created despite the retired driver")
		}
	})

	t.Run("update", func(t *testing.T) {
		gap138SeedLane(t, a.db, database.Project{
			Name: "retired-onboard2-sync", RepoURL: "local:/tmp/retired-onboard2-sync",
			Workdir: filepath.Join(home, "sync-workdirs", "retired-onboard2-sync"),
			Enabled: true, CooldownFloorS: 43200,
		})
		status, body := a.do(t, "PUT", "/api/v1/projects/retired-onboard2-sync", map[string]interface{}{
			"command": command,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("PUT with retired driver: status = %d, want 400 (body %v)", status, body)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, driver) {
			t.Errorf("error must name the retired driver %q, got %q", driver, msg)
		}
		got, err := database.GetProject(ctx, a.db, "retired-onboard2-sync")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if got.Command != "" {
			t.Errorf("command = %q, want unchanged empty string", got.Command)
		}
	})
}

// TestSCHEDGAP138_FamilyConstantsParity pins the two constants this gate shares
// with the fleet: for the four families this onboarding gate polices, the Go
// family cadence pins must equal the matching entries of SATELLITE_FAMILY_PINS
// in ops/check-fleet-invariants.py (check 5e, the live backstop — the canonical
// matrix Bane ratified 2026-09-29, SCHED-GAP-1675, which carries five further
// families this gate does not onboard), and the family key sets of the pins and
// the workdir roots must cover the satellite suffix vocabulary exactly — adding
// a suffix without its pin/roots would leave the gate silently unarmed for that
// family.
//
// Parsed from source text on purpose: the Go vars are unexported and the Python
// constants live in a script, so this is the assertion that fails when either
// side is edited alone (same shape as
// TestCheckFleetInvariants_RetiredDriversMatchPythonGate).
func TestSCHEDGAP138_FamilyConstantsParity(t *testing.T) {
	goSrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "internal", "api", "lane_onboarding.go"))
	if err != nil {
		t.Fatalf("read lane_onboarding.go: %v", err)
	}
	pySrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "ops", "check-fleet-invariants.py"))
	if err != nil {
		t.Fatalf("read check-fleet-invariants.py: %v", err)
	}
	suffixSrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "internal", "api", "server_projects.go"))
	if err != nil {
		t.Fatalf("read server_projects.go: %v", err)
	}

	goPins := intMapIn(t, string(goSrc), `(?s)satelliteFamilyPins\s*=\s*map\[string\]int\{(.*?)\n\}`)
	pyPins := intMapIn(t, string(pySrc), `(?s)SATELLITE_FAMILY_PINS\s*=\s*\{(.*?)\}`)
	want := map[string]int{"qa": 21600, "pm": 86400, "sync": 21600, "dogfood": 259200}

	if len(goPins) != len(want) {
		t.Fatalf("internal/api/lane_onboarding.go pins %d families (%v), want %d", len(goPins), goPins, len(want))
	}
	for family, pin := range want {
		if goPins[family] != pin {
			t.Errorf("go pin %s = %d, want %d", family, goPins[family], pin)
		}
		if pyPins[family] != pin {
			t.Errorf("ops/check-fleet-invariants.py pin %s = %d, want %d", family, pyPins[family], pin)
		}
	}

	roots := stringSliceMapKeysIn(t, string(goSrc), `(?s)satelliteWorkdirRoots\s*=\s*map\[string\]\[\]string\{(.*?)\n\}`)
	for family := range want {
		if !roots[family] {
			t.Errorf("satelliteWorkdirRoots has no entry for family %q", family)
		}
	}
	if len(roots) != len(want) {
		t.Errorf("satelliteWorkdirRoots covers %d families, want %d", len(roots), len(want))
	}

	// Since SCHED-GAP-1696 server_projects.go no longer holds a literal: it
	// derives the cascade vocabulary via scheduler.LaneRoleSuffixes(). Assert
	// the derivation is intact AND parse the canonical suffix list from
	// internal/scheduler/lane_class.go, which is the single shared table the
	// admission law and the cascade must both use.
	if !strings.Contains(string(suffixSrc), "satelliteLaneSuffixes = scheduler.LaneRoleSuffixes()") {
		t.Errorf("server_projects.go no longer derives satelliteLaneSuffixes from scheduler.LaneRoleSuffixes() — the cascade and the admission law would read different vocabularies")
	}
	classSrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "internal", "scheduler", "lane_class.go"))
	if err != nil {
		t.Fatalf("read lane_class.go: %v", err)
	}
	suffixes := quotedNamesIn(t, string(classSrc), `(?s)laneRoleSuffixes\s*=\s*\[\]string\{(.*?)\n\}`)
	// SUBSET semantics (INT-CI-173): the canonical vocabulary (SCHED-GAP-1675
	// matrix) is a SUPERSET of the four families this gate polices. The
	// invariant that matters is directional: every policed family must have a
	// suffix (the gate must stay armed); extra suffixes for families the gate
	// does not onboard are expected, not an error.
	if len(suffixes) < len(want) {
		t.Fatalf("laneRoleSuffixes carries %d suffixes (%v), cannot cover the %d policed families", len(suffixes), suffixes, len(want))
	}
	// Directional coverage (INT-CI-173): every POLICED family must appear in
	// the canonical suffix table. Suffixes beyond the policed four (perf,
	// releng, review, docs, readme — SCHED-GAP-1675 matrix) belong to families
	// this gate does not onboard and are expected here without pins/roots.
	policed := map[string]bool{}
	for family := range want {
		policed["-"+family] = true
	}
	seen := map[string]bool{}
	for _, suffix := range suffixes {
		seen[suffix] = true
	}
	for family := range want {
		if !seen["-"+family] {
			t.Errorf("canonical laneRoleSuffixes has no \"-%s\" entry — the onboarding gate would be unarmed for policed family %q", family, family)
		}
	}
	for _, suffix := range suffixes {
		if policed[suffix] {
			continue
		}
		if roots[strings.TrimPrefix(suffix, "-")] {
			t.Errorf("suffix %q is outside the policed families but satelliteWorkdirRoots carries an entry for it — extend the gate or drop the roots entry", suffix)
		}
	}
}

// stringSliceMapKeysIn extracts the top-level KEYS of a
// `map[string][]string{...}` literal. The key regex requires the following
// brace, so the root paths inside each value list can never be mistaken for
// keys (which is why intMapIn cannot read this block).
func stringSliceMapKeysIn(t *testing.T, src, pattern string) map[string]bool {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("pattern %q not found in source", pattern)
	}
	out := map[string]bool{}
	for _, key := range regexp.MustCompile(`"([a-z0-9-]+)"\s*:\s*\{`).FindAllStringSubmatch(m[1], -1) {
		out[key[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("pattern %q matched a block with no keys", pattern)
	}
	return out
}

func intMapIn(t *testing.T, src, pattern string) map[string]int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("pattern %q not found in source", pattern)
	}
	out := map[string]int{}
	for _, kv := range regexp.MustCompile(`"([a-z0-9-]+)"\s*:\s*(\d+)`).FindAllStringSubmatch(m[1], -1) {
		n, err := strconv.Atoi(kv[2])
		if err != nil {
			t.Fatalf("pattern %q: value %q is not an int", pattern, kv[2])
		}
		out[kv[1]] = n
	}
	if len(out) == 0 {
		t.Fatalf("pattern %q matched a block with no key/value pairs", pattern)
	}
	return out
}
