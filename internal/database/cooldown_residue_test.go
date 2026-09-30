package database

import (
	"context"
	"testing"
)

// TestSetCooldownPin_SnapsLiveUp is the SCHED-GAP-1670 core proof: a pin
// import RAISES the live cooldown to the pin in the SAME write when the
// live value sits below — a fleet.toml entry carrying 604800 can never
// leave the row live at 900.
func TestSetCooldownPin_SnapsLiveUp(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	p := &Project{Name: "warpfs-probe", Enabled: true, RepoURL: "local:/tmp/aa", Workdir: "/tmp/aa", Weight: 10, Priority: 5, CooldownS: 900}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := SetCooldownPin(ctx, db, "warpfs-probe", 604800, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin: %v", err)
	}
	got, err := GetProject(ctx, db, "warpfs-probe")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownS < 604800 {
		t.Errorf("PIN-SNAP BROKEN: fleet.toml carried cooldown_s=604800 but the row ended live at %d — an import must snap the live cooldown UP to the pin (SCHED-GAP-1670)", got.CooldownS)
	}
	if got.CooldownPinS == nil || *got.CooldownPinS != 604800 {
		t.Errorf("pin stamp: cooldown_pin_s = %v, want 604800", got.CooldownPinS)
	}
	if got.CooldownPinBy != CooldownPinImportBy {
		t.Errorf("pin provenance: cooldown_pin_by = %q, want %q", got.CooldownPinBy, CooldownPinImportBy)
	}
}

// TestSetCooldownPin_NoForcedSlowdown pins the floor direction: a pin at or
// below the live cooldown never lowers the live value.
func TestSetCooldownPin_NoForcedSlowdown(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	p := &Project{Name: "fast-lane", Enabled: true, RepoURL: "local:/tmp/p", Workdir: "/tmp/p", Weight: 10, Priority: 5, CooldownS: 21600}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetCooldownPin(ctx, db, "fast-lane", 900, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin: %v", err)
	}
	got, err := GetProject(ctx, db, "fast-lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownS != 21600 {
		t.Errorf("live cooldown = %d, want 21600 — a pin below the live value is a floor, not a forced slowdown", got.CooldownS)
	}
	if got.CooldownPinS == nil || *got.CooldownPinS != 900 {
		t.Errorf("pin stamp: cooldown_pin_s = %v, want 900", got.CooldownPinS)
	}
}

// TestSetCooldownPin_RefusesLowering is the direct-caller guard: even a
// caller that skips the loader's equal-or-higher check cannot lower an
// existing pin through SetCooldownPin.
func TestSetCooldownPin_RefusesLowering(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	p := &Project{Name: "pinned-lane", Enabled: true, RepoURL: "local:/tmp/p", Workdir: "/tmp/p", Weight: 10, Priority: 5, CooldownS: 86400}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetCooldownPin(ctx, db, "pinned-lane", 86400, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin (initial): %v", err)
	}
	if err := SetCooldownPin(ctx, db, "pinned-lane", 900, CooldownPinImportBy); err == nil {
		t.Fatal("SetCooldownPin with a LOWER pin must refuse, got nil error")
	}
	got, err := GetProject(ctx, db, "pinned-lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownPinS == nil || *got.CooldownPinS != 86400 {
		t.Errorf("pin = %v, want 86400 (the lowering import must not move it)", got.CooldownPinS)
	}
	if got.CooldownS != 86400 {
		t.Errorf("live cooldown = %d, want 86400", got.CooldownS)
	}
}

// TestSetCooldownPin_MissingProject pins the not-found error mapping the
// loader surfaces on a torn fleet.toml.
func TestSetCooldownPin_MissingProject(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	err = SetCooldownPin(context.Background(), db, "ghost", 900, CooldownPinImportBy)
	if err == nil {
		t.Fatal("SetCooldownPin on a missing project must fail")
	}
}

// TestCooldownResidue_DetectionAndListing covers the SCHED-GAP-1670
// detector: only ENABLED lanes with pin > live residue, disabled and clean
// rows excluded, and the exact JSON field values the endpoint serves.
func TestCooldownResidue_DetectionAndListing(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// The residue row: live below pin (the 2026-09-28 python-audit shape).
	// Seeded via a direct pin UPDATE — the drift's actual origin (migration
	// backfill / external tooling). The SetCooldownPin path itself snaps
	// and can no longer create residue.
	residue := &Project{Name: "aa-hot-lane", Enabled: true, RepoURL: "local:/tmp/aa", Workdir: "/tmp/aa", Weight: 10, Priority: 5, CooldownS: 900}
	if err := CreateProject(ctx, db, residue); err != nil {
		t.Fatalf("CreateProject (residue): %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 86400, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = '2026-09-28T12:00:00Z' WHERE name = 'aa-hot-lane'`); err != nil {
		t.Fatalf("seed residue pin: %v", err)
	}
	// The clean row: live at its pin.
	clean := &Project{Name: "bb-clean-lane", Enabled: true, RepoURL: "local:/tmp/p", Workdir: "/tmp/p", Weight: 10, Priority: 5, CooldownS: 21600}
	if err := CreateProject(ctx, db, clean); err != nil {
		t.Fatalf("CreateProject (clean): %v", err)
	}
	if err := SetCooldownPin(ctx, db, "bb-clean-lane", 21600, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin (clean): %v", err)
	}
	// The hot-but-unpinned row: live at 900 with NO pin at all — a bare
	// cooldown_s value is not a pin, so there is nothing to residue against.
	plain := &Project{Name: "cc-plain-lane", Enabled: true, RepoURL: "local:/tmp/cc", Workdir: "/tmp/cc", Weight: 10, Priority: 5, CooldownS: 900}
	if err := CreateProject(ctx, db, plain); err != nil {
		t.Fatalf("CreateProject (plain): %v", err)
	}
	// The disabled-residue row: same defect, but the lane is off.
	disabled := &Project{Name: "dd-off-lane", Enabled: true, RepoURL: "local:/tmp/dd", Workdir: "/tmp/dd", Weight: 10, Priority: 5, CooldownS: 900}
	if err := CreateProject(ctx, db, disabled); err != nil {
		t.Fatalf("CreateProject (disabled): %v", err)
	}
	if err := SetCooldownPin(ctx, db, "dd-off-lane", 86400, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin (disabled): %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET enabled = 0 WHERE name = 'dd-off-lane'`); err != nil {
		t.Fatalf("disable dd-off-lane: %v", err)
	}

	has, err := DetectCooldownResidue(ctx, db)
	if err != nil {
		t.Fatalf("DetectCooldownResidue: %v", err)
	}
	if !has {
		t.Fatal("DetectCooldownResidue = false, want true (aa-hot-lane is live below its pin)")
	}

	lanes, err := ListCooldownResidue(ctx, db)
	if err != nil {
		t.Fatalf("ListCooldownResidue: %v", err)
	}
	if len(lanes) != 1 {
		t.Fatalf("residue lanes = %d (%v), want exactly [aa-hot-lane]", len(lanes), lanes)
	}
	got := lanes[0]
	if got.Name != "aa-hot-lane" {
		t.Errorf("residue name = %q, want aa-hot-lane", got.Name)
	}
	if got.CooldownS != 900 {
		t.Errorf("residue cooldown_s = %d, want 900", got.CooldownS)
	}
	if got.PinS != 86400 {
		t.Errorf("residue pin_s = %d, want 86400", got.PinS)
	}
	if got.PinBy != CooldownPinImportBy {
		t.Errorf("residue pin_by = %q, want %q", got.PinBy, CooldownPinImportBy)
	}

	// Clean sweep: snapping the lane up clears the detector.
	if err := SetCooldownPin(ctx, db, "aa-hot-lane", 86400, CooldownPinImportBy); err != nil {
		t.Fatalf("SetCooldownPin (reconcile): %v", err)
	}
	has, err = DetectCooldownResidue(ctx, db)
	if err != nil {
		t.Fatalf("DetectCooldownResidue (after): %v", err)
	}
	if has {
		t.Error("DetectCooldownResidue = true after reconciliation, want false")
	}
}
