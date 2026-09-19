package scheduler

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestSimSetupDebug(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	fixture := NewSimFixture(db)
	projects := fixture.TestProjects()
	if err := fixture.Setup(projects); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Verify the DB.
	var count int
	db.QueryRow("SELECT COUNT(*) FROM projects WHERE enabled=1").Scan(&count)
	t.Logf("DB has %d enabled projects", count)
	db.QueryRow("SELECT COUNT(*) FROM ticks WHERE status='running'").Scan(&count)
	t.Logf("DB has %d running ticks", count)

	calc := NewUrgencyCalculator(5*time.Minute, 4*time.Hour, 10)
	packer := NewPacker(db, calc, 100, 8, nil)

	// Check what urgency looks like for two projects.
	now := time.Now()
	for _, name := range []string{"heavy-alpha", "light-epsilon", "light-alpha"} {
		var priority float64
		var decayRate float64
		var lastStr, createdStr string
		db.QueryRow(`SELECT priority, decay_rate, COALESCE(last_tick_completed,''), created_at FROM projects WHERE name=?`, name).
			Scan(&priority, &decayRate, &lastStr, &createdStr)
		var last *time.Time
		if lastStr != "" {
			lt, _ := time.Parse(time.RFC3339, lastStr)
			last = &lt
		}
		created, _ := time.Parse(time.RFC3339, createdStr)
		u := calc.ComputeUrgency(priority, decayRate, now, last, created)
		interval := calc.ComputeInterval(priority)
		t.Logf("  %s: priority=%.0f interval=%v urgency=%.4f", name, priority, interval, u)
	}

	packed, err := packer.Pick(now, nil)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}

	t.Logf("Packed: %d projects", len(packed))
	for _, p := range packed {
		t.Logf("  %s (w=%d p=%.0f u=%.2f)", p.Name, p.Weight, p.Priority, p.Urgency)
	}

	if len(packed) == 0 {
		t.Error("expected at least 1 packed project")
	}
}

// TestSimSetup_TolerantOfPreExistingTicks is the DOGFOOD-021 regression test:
// SimFixture.Setup must tolerate a database that already holds tick history
// referencing a project row — the `--sim-setup` invocation against an existing
// DB file.
//
// ticks.project_name carries a FOREIGN KEY to projects(name) and InitDB turns
// PRAGMA foreign_keys=ON, so wiping the parent table before its children made
// Setup return "clear projects: constraint failed: FOREIGN KEY constraint
// failed (787)" and FATAL the boot. The workaround was deleting the DB file
// first (SCHED-GAP-019: `rm -f <rundir>/*.db <rundir>/*.db-*`); this test pins
// that the workaround is no longer needed.
func TestSimSetup_TolerantOfPreExistingTicks(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// Premise of the regression: with FK enforcement off, the pre-fix ordering
	// is harmless and this test could not fail. Assert the premise so the test
	// goes loud instead of vacuous if the pragma ever stops being applied.
	var fkEnabled int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fkEnabled != 1 {
		t.Fatalf("foreign_keys = %d, want 1 (this test's premise)", fkEnabled)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	// Pre-existing parent row plus a child row referencing it: the shape a
	// prior run's tick history leaves behind.
	if _, err := db.Exec(`
		INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, enabled, created_at, updated_at)
		VALUES ('sim-preexisting', 'local:/sim', '/tmp/sim', 10, 5, 60, 1.0, 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("insert pre-existing project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO ticks (id, project_name, status, spawned_at, completed_at, created_at)
		VALUES ('sim-tick-preexisting-1', 'sim-preexisting', 'failed', ?, ?, ?)
	`, now, now, now); err != nil {
		t.Fatalf("insert pre-existing tick: %v", err)
	}

	fixture := NewSimFixture(db)
	if err := fixture.Setup(fixture.TestProjects()); err != nil {
		t.Fatalf("Setup with pre-existing tick rows: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE enabled=1`).Scan(&count); err != nil {
		t.Fatalf("count enabled projects: %v", err)
	}
	if count < 12 {
		t.Errorf("enabled projects = %d, want >= 12", count)
	}

	// The stale parent and its child rows are both gone — Setup is a clean
	// wipe, not a partial one.
	var stale int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE name='sim-preexisting'`).Scan(&stale); err != nil {
		t.Fatalf("count pre-existing project: %v", err)
	}
	if stale != 0 {
		t.Errorf("pre-existing project rows = %d, want 0", stale)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&remaining); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if remaining != 0 {
		t.Errorf("ticks rows = %d, want 0 (child rows wiped)", remaining)
	}
}
