package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestDailyCooldownResidueCheck_CadenceAndEvent pins the SCHED-GAP-1670
// daily residue report: the read-only census runs on the 24h gate, names
// residue lanes in a cooldown-residue event, and never mutates the rows.
func TestDailyCooldownResidueCheck_CadenceAndEvent(t *testing.T) {
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// One residue lane (the measured python-audit shape). Seeded via a
	// direct pin UPDATE — the drift's actual origin (migration backfill /
	// external tooling); the SetCooldownPin path itself snaps and cannot
	// create residue anymore.
	p := &database.Project{Name: "hot-lane", Enabled: true, RepoURL: "local:/tmp/h", Workdir: "/tmp/h", Weight: 10, Priority: 5, CooldownS: 900}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET cooldown_pin_s = 86400, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = '2026-09-28T12:00:00Z' WHERE name = 'hot-lane'`); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	loop := NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	defer loop.Stop()

	// First pass at t0: the gate starts at zero time, so the report is due
	// immediately and advances the gate.
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	loop.dailyCooldownResidueCheck(t0)

	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM events WHERE component = 'cooldown-residue'`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("cooldown-residue events after first pass = %d, want 1", n)
	}
	var severity, message, details string
	if err := db.QueryRow(`SELECT severity, message, details FROM events WHERE component = 'cooldown-residue'`).Scan(&severity, &message, &details); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if severity != string(SeverityMedium) {
		t.Errorf("event severity = %q, want %q", severity, SeverityMedium)
	}
	if !contains(message, "1 enabled lane") {
		t.Errorf("event message %q does not state the residue count", message)
	}
	// The lane names ride the details JSON ("lanes": [...]) — the report
	// must NAME the residue lanes, not just count them.
	for _, want := range []string{"hot-lane", "live=900s", "pin=86400s"} {
		if !contains(details, want) {
			t.Errorf("event details %q does not name %q", details, want)
		}
	}

	// Same-day pass: the 24h gate must suppress a duplicate report.
	loop.dailyCooldownResidueCheck(t0.Add(30 * time.Second))
	if err := db.QueryRow(`SELECT COUNT(1) FROM events WHERE component = 'cooldown-residue'`).Scan(&n); err != nil {
		t.Fatalf("count events (same-day): %v", err)
	}
	if n != 1 {
		t.Errorf("cooldown-residue events after same-day pass = %d, want 1 (the 24h gate must dedupe)", n)
	}

	// Next-day pass: the gate turns over and a second report lands.
	loop.dailyCooldownResidueCheck(t0.Add(24 * time.Hour))
	if err := db.QueryRow(`SELECT COUNT(1) FROM events WHERE component = 'cooldown-residue'`).Scan(&n); err != nil {
		t.Fatalf("count events (next-day): %v", err)
	}
	if n != 2 {
		t.Errorf("cooldown-residue events after next-day pass = %d, want 2", n)
	}

	// Read-only contract: the census never reconciles the row.
	got, err := database.GetProject(ctx, db, "hot-lane")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownS != 900 {
		t.Errorf("live cooldown = %d, want 900 — the daily report is read-only by contract", got.CooldownS)
	}
}

// TestResidueReportOnce_CleanFleet pins the clean arm: zero residue emits
// no cooldown-residue event (the clean log line is enough).
func TestResidueReportOnce_CleanFleet(t *testing.T) {
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	defer loop.Stop()

	loop.residueReportOnce(context.Background())

	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM events WHERE component = 'cooldown-residue'`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("cooldown-residue events on a clean fleet = %d, want 0", n)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
