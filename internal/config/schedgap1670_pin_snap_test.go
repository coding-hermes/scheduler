package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSchedGap1670_ImportSnapsLiveUpToPin is the fleet.toml-import arm of
// the pin-snap proof (SCHED-GAP-1670): a lane whose fleet.toml carries an
// explicit cooldown_s=604800 while its DB row sits live at a LOWER value
// must end the import at live >= 604800. This is the exact measured shape
// on warpfs (cooldown_pin_by=fleet-toml-import, live 900 vs a 604800 pin).
//
// AC RED-proof contract: this test FAILS with a message naming the
// invariant when the snap is removed from the import path (see the report's
// RED transcript — the breakeven mutation deletes the snap from
// SetCooldownPin's UPDATE).
func TestSchedGap1670_ImportSnapsLiveUpToPin(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Boot 1: the seed row lands at 900 (an old, already-stale value) WITH
	// its operator pin stamped at seed time.
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[[projects]]
name = "warpfs-lane"
repo_url = "local:/tmp/warpfs"
workdir = "/tmp/warpfs"
cooldown_s = 900
enabled = true
`), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	cfg, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (seed): %v", err)
	}

	// The operator's real intent arrives: the file is raised to 604800.
	if err := os.WriteFile(tomlPath, []byte(`
[[projects]]
name = "warpfs-lane"
repo_url = "local:/tmp/warpfs"
workdir = "/tmp/warpfs"
cooldown_s = 604800
enabled = true
`), 0o644); err != nil {
		t.Fatalf("rewrite fleet.toml: %v", err)
	}
	cfg2, err := LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig (raised): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg2); err != nil {
		t.Fatalf("ApplyFleetConfig (raised): %v", err)
	}

	p, err := database.GetProject(ctx, db, "warpfs-lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownPinS == nil || *p.CooldownPinS != 604800 {
		t.Errorf("pin import: cooldown_pin_s = %v, want 604800 (the raised toml value must stamp the pin)", p.CooldownPinS)
	}
	if p.CooldownS < 604800 {
		t.Errorf("PIN-SNAP BROKEN: fleet.toml carries cooldown_s=604800 but the row ended live at %d — a fleet.toml import must snap the live cooldown UP to the pin so no lane is live BELOW its own pin (SCHED-GAP-1670)", p.CooldownS)
	}
	if p.CooldownPinBy != database.CooldownPinImportBy {
		t.Errorf("pin provenance: cooldown_pin_by = %q, want %q", p.CooldownPinBy, database.CooldownPinImportBy)
	}
}
