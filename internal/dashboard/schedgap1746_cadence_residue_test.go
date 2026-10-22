package dashboard_test

// SCHED-GAP-1746: cadence (configured vs achieved starts/day, SOL-CADENCE /
// SCHED-GAP-1668) and the cooldown-pin residue detector (SCHED-GAP-1670)
// render on the dashboard. Both facts existed only on REST/MCP surfaces
// (/api/v1/cadence, /api/v1/cooldown/residue); the board row demands an
// operator-visible page whose template literally renders the residue
// endpoint route, so `/cadence` serves one Cadence & Residue page:
//
//   - the cadence table (target/source/achieved/deficit) from the SAME
//     database resolution the API endpoint uses (database.ListProjects +
//     LoadCadenceRates + EffectiveCadenceTarget — no second formula);
//   - the residue detector rows from database.ListCooldownResidue;
//   - an explicit link to /api/v1/cooldown/residue (the row's grep
//     contract — the template contains the literal route, and the link
//     must never be dead);
//   - the empty-data arms render honest explicit states, never empty
//     tables presented as complete views.

import (
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/dashboard"
)

// TestSCHEDGAP1746_CadenceResiduePageRendersBothDatasources drives the board
// acceptance: a template renders the residue detector, the cadence table
// shows real handler data, and the /api/v1/cooldown/residue link is present.
func TestSCHEDGAP1746_CadenceResiduePageRendersBothDatasources(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	// pinned: cooldown pin 86400 (target 1/day derived) with the live
	// cooldown ALREADY snapped to the pin — a clean cadence row, no residue.
	mustCreateProject(t, db, "cad-pinned", 10, 5)
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 86400,
		cooldown_pin_by = 'fleet-toml-import', cooldown_s = 86400
		WHERE name = 'cad-pinned'`); err != nil {
		t.Fatalf("pin cad-pinned: %v", err)
	}

	// residue: live 900 strictly below pin 86400 — the detector's row.
	mustCreateProject(t, db, "cad-residue", 10, 5)
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 86400,
		cooldown_pin_by = 'fleet-toml-import', cooldown_s = 900
		WHERE name = 'cad-residue'`); err != nil {
		t.Fatalf("pin cad-residue: %v", err)
	}

	// cad-disabled: an explicit zero override (target_source=disabled) —
	// appears with no fabricated target, per the API's own contract.
	mustCreateProject(t, db, "cad-disabled", 10, 5)
	if _, err := db.Exec(`UPDATE projects SET target_runs_per_day = 0
		WHERE name = 'cad-disabled'`); err != nil {
		t.Fatalf("disable cad-disabled: %v", err)
	}

	// Two in-window spawned ticks for cad-residue → achieved = 2/7 per day.
	mustCreateTick(t, db, "cad-tick-1", "cad-residue", now.Add(-24*time.Hour))
	mustCreateTick(t, db, "cad-tick-2", "cad-residue", now.Add(-48*time.Hour))

	gen := dashboard.NewGenerator(db, nil)
	gen.SetClock(clock.NewFixed(now))

	var buf strings.Builder
	if err := gen.GenerateCadence(&buf); err != nil {
		t.Fatalf("GenerateCadence: %v", err)
	}
	out := buf.String()

	// Board acceptance 1: the template literally renders the residue route.
	for _, want := range []string{
		"Cadence", "/api/v1/cooldown/residue",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cadence page missing %q", want)
		}
	}

	// Cadence rows carry real handler data: both lanes listed with their
	// derived source; the disabled lane keeps its explicit source token.
	for _, want := range []string{"cad-pinned", "cad-residue", "cad-disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("cadence table missing lane %q", want)
		}
	}
	if !strings.Contains(out, "cooldown_pin") {
		t.Errorf("cadence table does not render the cooldown_pin source token")
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("cadence table does not render the disabled source token")
	}

	// The residue detector renders its lane: live 900 below pin 86400.
	if !strings.Contains(out, "900") || !strings.Contains(out, "86400") {
		t.Errorf("residue section missing live/pin values (900 / 86400)")
	}
	if !strings.Contains(out, "fleet-toml-import") {
		t.Errorf("residue row missing the pin provenance (pin_by)")
	}

	// The page exposes the cadence endpoint too (both REST surfaces linked).
	if !strings.Contains(out, "/api/v1/cadence") {
		t.Errorf("cadence page does not link /api/v1/cadence")
	}

	// The page is a full page (title + sidebar), not a fragment.
	for _, scaffold := range []string{"<!DOCTYPE html>", "Coding Hermes Fleet"} {
		if !strings.Contains(out, scaffold) {
			t.Errorf("cadence page missing scaffold %q", scaffold)
		}
	}
}

// TestSCHEDGAP1746_ResidueFragmentAndEmptyArms pins the htmx fragment
// contract and both honest-empty arms (no residue rows / no projects).
func TestSCHEDGAP1746_ResidueFragmentAndEmptyArms(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)
	gen.SetClock(clock.NewFixed(time.Now().UTC()))

	// Empty fleet: full page renders the honest "no lanes" line and still
	// carries the residue endpoint link (never a dead link).
	var buf strings.Builder
	if err := gen.GenerateCadence(&buf); err != nil {
		t.Fatalf("GenerateCadence (empty fleet): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No lanes configured") {
		t.Errorf("empty fleet must render the explicit no-lanes line")
	}
	if !strings.Contains(out, "/api/v1/cooldown/residue") {
		t.Errorf("empty fleet page lost the residue endpoint link")
	}

	// A fleet with lanes but NO residue: the detector section renders its
	// explicit clean state instead of an empty table.
	mustCreateProject(t, db, "cad-clean", 10, 5)
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 86400,
		cooldown_pin_by = 'fleet-toml-import', cooldown_s = 86400
		WHERE name = 'cad-clean'`); err != nil {
		t.Fatalf("pin cad-clean: %v", err)
	}

	var buf2 strings.Builder
	if err := gen.GenerateCadence(&buf2); err != nil {
		t.Fatalf("GenerateCadence (no residue): %v", err)
	}
	out2 := buf2.String()
	if !strings.Contains(out2, "No residue") {
		t.Errorf("clean fleet must render the explicit no-residue line")
	}

	// The htmx fragment: residue rows only, never a full page (it swaps
	// into its own poller — the /health and /remote fragment contract).
	mustCreateProject(t, db, "cad-hot", 10, 5)
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 3600,
		cooldown_pin_by = 'api', cooldown_s = 600
		WHERE name = 'cad-hot'`); err != nil {
		t.Fatalf("pin cad-hot: %v", err)
	}

	var frag strings.Builder
	if err := gen.GenerateCadenceResidueRows(&frag); err != nil {
		t.Fatalf("GenerateCadenceResidueRows: %v", err)
	}
	fout := frag.String()
	for _, want := range []string{"cad-hot", "600", "3600", "api"} {
		if !strings.Contains(fout, want) {
			t.Errorf("residue fragment missing %q", want)
		}
	}
	if strings.Contains(fout, "<!DOCTYPE html>") {
		t.Errorf("residue fragment must be a fragment, not a full page")
	}
	if strings.Contains(fout, "cad-clean") {
		t.Errorf("residue fragment must NOT render: clean lane cad-clean appeared (row leakage into the fragment)")
	}
}

/*
ch:trace row=SCHED-GAP-1746 spec=docs/api.md#cadence wave=none:serial-worker test=internal/dashboard/schedgap1746_cadence_residue_test.go doc=internal/dashboard/templates/cadence.html prompt=none:foreman-dispatch-brief evidence=none:pending-commit witness=none:worker-local verdict=none:foreman-judge commit=none:pending-implementation memory=none:not-written
STATUS: complete
TIER: T3 — invoked go test ./internal/dashboard -count=1 (PASS); full page, fragment, empty-data and residue-link assertions observed.
EVIDENCE: commit:none:pending-commit (implementation + tests + this marker will be committed together on wt/SCHED-GAP-1746; SHA bound by foreman trace follow-up)
*/
