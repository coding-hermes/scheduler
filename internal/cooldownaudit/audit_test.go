package cooldownaudit

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func pinPtr(v int) *int { return &v }

// TestDetectResidue_ClassBoundary pins the classifier's definition of the
// residue class one lane shape at a time: below the pin fires, at/above the pin
// does not (the pin is a floor, not a cap), and a disabled or pin-less lane is
// not scheduled against a pin at all.
func TestDetectResidue_ClassBoundary(t *testing.T) {
	tests := []struct {
		name string
		lane Lane
		want []Finding
	}{
		{
			name: "below-fast live value against a 24h pin is residue",
			lane: Lane{Name: "python-audit-lint", CooldownS: 900, PinS: pinPtr(86400), Enabled: true},
			want: []Finding{{Name: "python-audit-lint", LiveS: 900, PinS: 86400, DeltaS: 85500, PinSource: PinSourceDB}},
		},
		{
			name: "below a non-canonical weekly pin is residue too",
			lane: Lane{Name: "bunker-perf", CooldownS: 86400, PinS: pinPtr(604800), Enabled: true},
			want: []Finding{{Name: "bunker-perf", LiveS: 86400, PinS: 604800, DeltaS: 518400, PinSource: PinSourceDB}},
		},
		{
			name: "at the pin is clean",
			lane: Lane{Name: "h3", CooldownS: 43200, PinS: pinPtr(43200), Enabled: true},
			want: []Finding{},
		},
		{
			name: "above the pin is clean — the pin is a floor, never a cap",
			lane: Lane{Name: "operator-slowdown", CooldownS: 604800, PinS: pinPtr(21600), Enabled: true},
			want: []Finding{},
		},
		{
			name: "a disabled lane is not scheduled, so not residue",
			lane: Lane{Name: "paused-lane", CooldownS: 900, PinS: pinPtr(21600), Enabled: false},
			want: []Finding{},
		},
		{
			name: "a lane with no pin cannot be below it",
			lane: Lane{Name: "unpinned", CooldownS: 900, Enabled: true},
			want: []Finding{},
		},
		{
			name: "the fleet.toml pin source is carried into the finding",
			lane: Lane{Name: "seed-only", CooldownS: 86400, PinS: pinPtr(604800), Enabled: true, PinSource: PinSourceFleetToml},
			want: []Finding{{Name: "seed-only", LiveS: 86400, PinS: 604800, DeltaS: 518400, PinSource: PinSourceFleetToml}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectResidue([]Lane{tc.lane})
			if len(got) != len(tc.want) {
				t.Fatalf("DetectResidue(%+v) = %+v, want %+v", tc.lane, got, tc.want)
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], tc.want[i]) {
					t.Errorf("finding %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDetectResidue_MeasuredFleetShape is the non-vacuity anchor: the exact
// eleven-lane measurement of 2026-09-28, reduced to three of its rows plus one
// clean lane. A detector that silently stopped firing fails here — this is the
// fixture the daily schedule exists to alert on.
func TestDetectResidue_MeasuredFleetShape(t *testing.T) {
	lanes := []Lane{
		{Name: "python-audit-typing", CooldownS: 900, PinS: pinPtr(86400), Enabled: true},
		{Name: "warpfs", CooldownS: 900, PinS: pinPtr(21600), Enabled: true},
		{Name: "bunker-perf", CooldownS: 86400, PinS: pinPtr(604800), Enabled: true},
		{Name: "h3", CooldownS: 43200, PinS: pinPtr(43200), Enabled: true},
		{Name: "crier", CooldownS: 21600, PinS: pinPtr(21600), Enabled: true},
	}

	got := DetectResidue(lanes)
	if len(got) != 3 {
		t.Fatalf("DetectResidue(measured fleet shape) = %d finding(s), want 3: %+v", len(got), got)
	}
	wantNames := []string{"bunker-perf", "python-audit-typing", "warpfs"}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("finding %d = %q, want %q (output must be name-sorted)", i, got[i].Name, name)
		}
	}
	if got[0].DeltaS != 518400 {
		t.Errorf("delta for %s = %d, want 518400 (604800-86400)", got[0].Name, got[0].DeltaS)
	}
}

// TestApplyFleetPins_DBIsAuthority — the seed fills only a lane whose row has
// no pin, and the DB pin is never displaced by the file value.
func TestApplyFleetPins_DBIsAuthority(t *testing.T) {
	lanes := []Lane{
		{Name: "db-pinned", CooldownS: 900, PinS: pinPtr(21600), Enabled: true, PinSource: PinSourceDB},
		{Name: "seed-only", CooldownS: 86400, Enabled: true},
		{Name: "unknown", CooldownS: 900, Enabled: true},
		{Name: "zero-seed", CooldownS: 900, Enabled: true},
	}
	pins := map[string]int{"db-pinned": 604800, "seed-only": 604800, "zero-seed": 0}

	out := ApplyFleetPins(lanes, pins)

	if out[0].PinS == nil || *out[0].PinS != 21600 || out[0].PinSource != PinSourceDB {
		t.Errorf("db-pinned lane = %+v, want the DB pin 21600 with source %q (the seed's 604800 must not win)",
			out[0], PinSourceDB)
	}
	if out[1].PinS == nil || *out[1].PinS != 604800 || out[1].PinSource != PinSourceFleetToml {
		t.Errorf("seed-only lane = %+v, want the seed pin 604800 with source %q", out[1], PinSourceFleetToml)
	}
	if out[2].PinS != nil {
		t.Errorf("unknown lane = %+v, want no pin (no seed entry, no DB pin)", out[2])
	}
	if out[3].PinS != nil {
		t.Errorf("zero-seed lane = %+v, want no pin (0 is not a value a pin can hold)", out[3])
	}
	if lanes[1].PinS != nil {
		t.Errorf("ApplyFleetPins mutated its input: lanes[1].PinS = %v", *lanes[1].PinS)
	}

	// A residue row reached only through the seed still classifies.
	got := DetectResidue(out)
	if len(got) != 2 {
		t.Fatalf("DetectResidue(seeded) = %+v, want 2 findings (db-pinned and seed-only)", got)
	}
	if got[1].PinSource != PinSourceFleetToml {
		t.Errorf("finding %+v, want pin_source=%q", got[1], PinSourceFleetToml)
	}
}

func TestApplyFleetPins_EmptyPinSetReturnsCopy(t *testing.T) {
	lanes := []Lane{{Name: "a", CooldownS: 900, Enabled: true}}
	out := ApplyFleetPins(lanes, nil)
	if len(out) != 1 || out[0].Name != "a" {
		t.Fatalf("ApplyFleetPins(lanes, nil) = %+v, want the lanes unchanged", out)
	}
	if out[0].PinS != nil {
		t.Errorf("lane = %+v, want no pin invented from an empty seed", out[0])
	}
}

func TestFleetPins_ParsesSeedBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	seed := `
[scheduler]
blackout_windows = [{ start = "01:00", end = "04:00", multiplier = 2.0 }]

[[namespaces]]
id = "coding-hermes"
weight = 10

[[projects]]
name = "bunker-perf"
workdir = "/srv/lanes/bunker-perf"
cooldown_s = 604800
enabled = true

[[projects]]
name = "keyless"
workdir = "/srv/lanes/keyless"
enabled = true

[[projects]]
name = "zeroed"
workdir = "/srv/lanes/zeroed"
cooldown_s = 0
enabled = true
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	pins, err := FleetPins(path)
	if err != nil {
		t.Fatalf("FleetPins: %v", err)
	}
	want := map[string]int{"bunker-perf": 604800}
	if !reflect.DeepEqual(pins, want) {
		t.Errorf("FleetPins = %v, want %v (keyless and zero-cadence entries carry no pin)", pins, want)
	}
}

func TestFleetPins_MissingAndUnparseableAreErrors(t *testing.T) {
	if _, err := FleetPins(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Error("FleetPins(absent) = nil error, want an error (a missing seed must never read as an empty pin set)")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[[projects]\nname = "), 0o644); err != nil {
		t.Fatalf("write bad seed: %v", err)
	}
	if _, err := FleetPins(bad); err == nil {
		t.Error("FleetPins(unparseable) = nil error, want an error")
	}
}

// TestLoadLanes_ReadsPinsAndEnabledFromTheDB drives the SQL read the detector
// uses end to end: rows created through the database package are read back
// read-only, and the classifier sees exactly the residue row.
func TestLoadLanes_ReadsPinsAndEnabledFromTheDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scheduler.db")
	write, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	rows := []*database.Project{
		{Name: "residue", RepoURL: "https://example.invalid/residue", Workdir: "/tmp/lanes/residue", Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1, Model: "m", Provider: "p", Enabled: true},
		{Name: "clean", RepoURL: "https://example.invalid/clean", Workdir: "/tmp/lanes/clean", Weight: 10, Priority: 5, CooldownS: 21600, DecayRate: 1, Model: "m", Provider: "p", Enabled: true},
		{Name: "paused", RepoURL: "https://example.invalid/paused", Workdir: "/tmp/lanes/paused", Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1, Model: "m", Provider: "p", Enabled: false},
	}
	for _, p := range rows {
		if err := database.CreateProject(ctx, write, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.Name, err)
		}
	}
	if _, err := database.SetCooldownPin(ctx, write, "residue", 86400, database.CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin(residue): %v", err)
	}
	if _, err := database.SetCooldownPin(ctx, write, "clean", 21600, database.CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin(clean): %v", err)
	}
	if _, err := database.SetCooldownPin(ctx, write, "paused", 86400, database.CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin(paused): %v", err)
	}
	// The residue MECHANISM, reproduced: after the pins landed, a plain
	// cooldown write (the wake-PUT path, database.UpdateProject — which has no
	// pin guard) drops two lanes below their pin. SetCooldownPin itself now
	// snaps UP, so it cannot be used to build the fixture.
	wake := 900
	for _, name := range []string{"residue", "paused"} {
		if err := database.UpdateProject(ctx, write, name, database.ProjectUpdates{CooldownS: &wake}); err != nil {
			t.Fatalf("UpdateProject(%s): %v", name, err)
		}
	}
	if err := write.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	db, err := database.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	lanes, err := LoadLanes(ctx, db)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if len(lanes) != 3 {
		t.Fatalf("LoadLanes = %d lane(s), want 3", len(lanes))
	}
	findings := DetectResidue(lanes)
	if len(findings) != 1 || findings[0].Name != "residue" {
		t.Fatalf("DetectResidue(loaded lanes) = %+v, want exactly the residue row", findings)
	}
	if findings[0].LiveS != 900 || findings[0].PinS != 86400 {
		t.Errorf("finding = %+v, want live 900 pin 86400", findings[0])
	}
}

func TestReport_StatesScannedCount(t *testing.T) {
	clean := Report(nil, 0)
	if !strings.Contains(clean, "scanned 0 enabled lane") {
		t.Errorf("clean report = %q, want it to state the scanned count", clean)
	}
	alert := Report([]Finding{{Name: "warpfs", LiveS: 900, PinS: 21600, DeltaS: 20700, PinSource: PinSourceDB}}, 12)
	if !strings.Contains(alert, "ALERT: 1 enabled lane(s)") || !strings.Contains(alert, "warpfs") {
		t.Errorf("alert report = %q, want the ALERT line and the lane name", alert)
	}
	if !strings.Contains(alert, "fleet-cooldown-policy.py --apply") {
		t.Errorf("alert report = %q, want the named mutating remedy", alert)
	}
}
