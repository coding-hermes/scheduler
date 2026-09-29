package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// SCHED-GAP-1670 — SetCooldownPin is the path a fleet.toml import uses, and it
// must snap a live cooldown UP to the pin it just recorded. Before the fix the
// import path deliberately skipped the snap the API PUT path applies, which is
// how a lane whose file carries cooldown_s = 604800 stayed live at 86400 (and
// how warpfs stayed at 900 against a 21600 pin, provenance
// fleet-toml-import).

func newPinTestDB(t *testing.T, name string, liveS int) *sql.DB {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := sampleProject(name)
	p.CooldownS = liveS
	if err := CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return db
}

func TestSCHEDGAP1670_SetCooldownPinSnapsLiveCooldownUp(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		liveS    int
		pinS     int
		wantLive int
		wantSnap int // snappedFrom: 0 = the snap did not fire
	}{
		{
			name:     "weekly pin over a daily live value",
			liveS:    86400,
			pinS:     604800,
			wantLive: 604800,
			wantSnap: 86400,
		},
		{
			name:     "6h pin over wake-residue 900",
			liveS:    900,
			pinS:     21600,
			wantLive: 21600,
			wantSnap: 900,
		},
		{
			name:     "at the pin is a no-op",
			liveS:    21600,
			pinS:     21600,
			wantLive: 21600,
			wantSnap: 0,
		},
		{
			name:     "a pin below a slower live value never lowers it",
			liveS:    604800,
			pinS:     21600,
			wantLive: 604800,
			wantSnap: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newPinTestDB(t, "lane", tc.liveS)

			snappedFrom, err := SetCooldownPin(ctx, db, "lane", tc.pinS, CooldownPinImportBy)
			if err != nil {
				t.Fatalf("SetCooldownPin: %v", err)
			}
			if snappedFrom != tc.wantSnap {
				t.Errorf("snappedFrom = %d, want %d", snappedFrom, tc.wantSnap)
			}

			p, err := GetProject(ctx, db, "lane")
			if err != nil {
				t.Fatalf("GetProject: %v", err)
			}
			if p.CooldownS != tc.wantLive {
				t.Errorf("cooldown_s = %d, want %d", p.CooldownS, tc.wantLive)
			}
			if p.CooldownPinS == nil || *p.CooldownPinS != tc.pinS {
				t.Errorf("cooldown_pin_s = %v, want %d", p.CooldownPinS, tc.pinS)
			}
			if p.CooldownPinBy != CooldownPinImportBy {
				t.Errorf("cooldown_pin_by = %q, want %q", p.CooldownPinBy, CooldownPinImportBy)
			}
		})
	}
}

func TestSCHEDGAP1670_SetCooldownPinRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	db := newPinTestDB(t, "lane", 21600)

	if _, err := SetCooldownPin(ctx, db, "lane", 0, CooldownPinImportBy); err == nil {
		t.Error("SetCooldownPin(pin=0) = nil error, want an error")
	}
	if _, err := SetCooldownPin(ctx, db, "missing", 21600, CooldownPinImportBy); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("SetCooldownPin(unknown lane) = %v, want ErrProjectNotFound", err)
	}
}
