package config

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-141 durability tests. The blocking-gate knobs a lane is admitted
// by — admission_mode and (new) board_ownership — obey the SCHED-GAP-025/121
// law: an operator pin is durable only when it exists in BOTH stores (the
// live SQLite row AND fleet.toml), because the loader re-pins from the file
// on every start. These tests pin the two halves of that law.

// T-141-D1: a fleet.toml entry carrying admission_mode + board_ownership
// re-pins BOTH values over DB drift on the next load (restart durability).
func TestSCHEDGAP141_FleetPinRepinsOverDBDrift(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{Projects: []ProjectDef{{
		Name:           "lane",
		RepoURL:        "local://lane",
		Workdir:        "/tmp/lane",
		AdmissionMode:  database.AdmissionModeCooldown,
		BoardOwnership: database.BoardOwnershipShared,
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	p, err := database.GetProject(ctx, db, "lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.AdmissionMode != database.AdmissionModeCooldown || p.BoardOwnership != database.BoardOwnershipShared {
		t.Fatalf("T-141-D1-0 FAIL: fleet.toml keys not stored at creation (mode=%q ownership=%q)", p.AdmissionMode, p.BoardOwnership)
	}

	// Simulate DB drift: a hand-edit or an older API flip.
	if err := database.UpdateProject(ctx, db, "lane", database.ProjectUpdates{
		AdmissionMode:  strPtr(database.AdmissionModeTasks),
		BoardOwnership: strPtr(database.BoardOwnershipAuto),
	}); err != nil {
		t.Fatalf("UpdateProject (drift): %v", err)
	}

	// Restart: the loader re-pins from fleet.toml.
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	p, err = database.GetProject(ctx, db, "lane")
	if err != nil {
		t.Fatalf("GetProject (re-pin): %v", err)
	}
	if p.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-141-D1a FAIL: admission_mode drift survived the restart: %q", p.AdmissionMode)
	}
	if p.BoardOwnership != database.BoardOwnershipShared {
		t.Errorf("T-141-D1b FAIL: board_ownership drift survived the restart: %q", p.BoardOwnership)
	}
}

// T-141-D2: a keyless fleet.toml entry never reverts an API-assigned
// value (a DB-only pin survives the restart instead of being silently
// normalized), and validation rejects unknown ownership values.
func TestSCHEDGAP141_KeylessEntryPreservesAPIValue(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{Projects: []ProjectDef{{
		Name:    "lane2",
		RepoURL: "local://lane2",
		Workdir: "/tmp/lane2",
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// API/DB-only pin (no fleet.toml key).
	if err := database.UpdateProject(ctx, db, "lane2", database.ProjectUpdates{
		BoardOwnership: strPtr(database.BoardOwnershipShared),
	}); err != nil {
		t.Fatalf("UpdateProject (db-only pin): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}
	p, err := database.GetProject(ctx, db, "lane2")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.BoardOwnership != database.BoardOwnershipShared {
		t.Errorf("T-141-D2a FAIL: keyless fleet.toml entry reverted a DB-only board_ownership pin: %q", p.BoardOwnership)
	}

	if err := database.UpdateProject(ctx, db, "lane2", database.ProjectUpdates{
		BoardOwnership: strPtr("foreign"),
	}); err == nil {
		t.Errorf("T-141-D2b FAIL: UpdateProject accepted board_ownership=%q", "foreign")
	}
}

func strPtr(s string) *string { return &s }
