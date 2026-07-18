package scheduler

import (
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
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
	packer := NewPacker(db, calc, 100, 8)

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
