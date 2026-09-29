package config

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1670 — the pin-snap must hold on the fleet.toml IMPORT path.
//
// The accepting criterion, verbatim: "a fleet.toml with cooldown_s=604800 for
// a lane whose pin is 604800 must yield live cooldown 604800, not 86400 or
// 900". Before the fix the loader recorded the pin and deliberately left the
// live cooldown alone (database.SetCooldownPin: "WITHOUT touching the live
// cooldown_s (unlike the API PUT path, which snaps the cooldown up)"), which is
// how warpfs sat at live 900 against a 21600 pin with provenance
// fleet-toml-import.
//
// These tests drive the REAL path — a fleet.toml on disk, LoadFleetConfig, then
// ApplyFleetConfig, the same two calls a daemon boot makes.

// bootFromToml runs the boot path a restart runs over the given file.
func bootFromToml(t *testing.T, db *sql.DB, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := ApplyFleetConfig(context.Background(), db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
}

// wakeLane creates the lane row and then drops its live cooldown the way the
// wake-PUT path does (database.UpdateProject carries no pin guard) — the
// mechanism that produces residue in the field.
func wakeLane(t *testing.T, db *sql.DB, name string, liveS int) {
	t.Helper()
	ctx := context.Background()
	p := &database.Project{
		Name: name, RepoURL: "local:/tmp/" + name, Workdir: "/tmp/" + name,
		Weight: 10, Priority: 5, CooldownS: liveS, DecayRate: 1,
		Model: "m", Provider: "p", Enabled: true,
	}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject(%s): %v", name, err)
	}
	if err := database.UpdateProject(ctx, db, name, database.ProjectUpdates{CooldownS: &liveS}); err != nil {
		t.Fatalf("UpdateProject(%s): %v", name, err)
	}
}

func TestSCHEDGAP1670_FleetTomlImportSnapsLiveCooldownUpToThePin(t *testing.T) {
	ctx := context.Background()
	// The two live values the acceptance criterion names: a daily cadence and
	// a wake-residue 900, both against a file that declares the weekly pin.
	for _, liveS := range []int{86400, 900} {
		t.Run(fmt.Sprintf("live_%d", liveS), func(t *testing.T) {
			db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
			if err != nil {
				t.Fatalf("InitDB: %v", err)
			}
			defer db.Close()

			wakeLane(t, db, "bunker-perf", liveS)
			bootFromToml(t, db, t.TempDir(), `
[[projects]]
name = "bunker-perf"
repo_url = "local:/tmp/bunker-perf"
workdir = "/tmp/bunker-perf"
cooldown_s = 604800
enabled = true
`)

			p, err := database.GetProject(ctx, db, "bunker-perf")
			if err != nil {
				t.Fatalf("GetProject: %v", err)
			}
			if p.CooldownS != 604800 {
				t.Errorf("live cooldown after the fleet.toml import = %d, want 604800 (a lane whose file carries cooldown_s=604800 must not end up live at 86400 or 900)", p.CooldownS)
			}
			if p.CooldownPinS == nil || *p.CooldownPinS != 604800 {
				t.Errorf("cooldown_pin_s = %v, want 604800", p.CooldownPinS)
			}
			if p.CooldownPinBy != database.CooldownPinImportBy {
				t.Errorf("cooldown_pin_by = %q, want %q", p.CooldownPinBy, database.CooldownPinImportBy)
			}
		})
	}
}

// The snap is one-directional: a file value below the live cooldown is recorded
// as the pin and never pulls the live value down (SCHED-GAP-219's durability —
// the DB is the authority for an API value the file never carried).
func TestSCHEDGAP1670_FleetTomlImportNeverLowersLiveCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	wakeLane(t, db, "slow-lane", 604800)
	bootFromToml(t, db, t.TempDir(), `
[[projects]]
name = "slow-lane"
repo_url = "local:/tmp/slow-lane"
workdir = "/tmp/slow-lane"
cooldown_s = 900
enabled = true
`)

	p, err := database.GetProject(ctx, db, "slow-lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownS != 604800 {
		t.Errorf("live cooldown = %d, want 604800 (a lower toml value must never pull a live cadence down)", p.CooldownS)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 900 {
		t.Errorf("cooldown_pin_s = %v, want 900 (imported, not applied)", p.CooldownPinS)
	}
}

// The documented BOUNDARY of the fix, pinned so a future "enforce the pin on
// every boot" change is a deliberate decision rather than an accident: when the
// pin is ALREADY on the row and the file carries the same value, the loader
// does not re-write the row (SCHED-GAP-219: the DB is the cooldown authority,
// and the skip is what keeps an API write durable across restarts). A live
// cooldown lowered AFTER its pin landed is therefore not healed at boot — it is
// reported by the scheduled read-only detector (internal/cooldownaudit) and
// corrected by the operator's policy run.
func TestSCHEDGAP1670_LiveCooldownLoweredAfterItsPinLandedIsNotRewrittenAtBoot(t *testing.T) {
	ctx := context.Background()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	dir := t.TempDir()
	tomlFile := `
[[projects]]
name = "residue-after-pin"
repo_url = "local:/tmp/residue-after-pin"
workdir = "/tmp/residue-after-pin"
cooldown_s = 7200
enabled = true
`
	// First boot: the seed creates the row and records the pin.
	bootFromToml(t, db, dir, tomlFile)
	// The wake PUT happens afterwards (this is the residue mechanism).
	wake := 900
	if err := database.UpdateProject(ctx, db, "residue-after-pin", database.ProjectUpdates{CooldownS: &wake}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	// Second boot over the UNCHANGED file.
	bootFromToml(t, db, dir, tomlFile)

	p, err := database.GetProject(ctx, db, "residue-after-pin")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 7200 {
		t.Fatalf("cooldown_pin_s = %v, want 7200", p.CooldownPinS)
	}
	if p.CooldownS != 900 {
		t.Errorf("live cooldown = %d, want 900 left untouched: an equal pin does not re-write the row (SCHED-GAP-219 durability), so this class is the detector's job, not the loader's", p.CooldownS)
	}
}
