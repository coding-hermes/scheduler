package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// SimFixture creates a clean set of test projects for simulation testing.
// Projects are designed to exercise: concurrency cap, weight budget packing,
// priority decay/starvation, cooldown throttling, and disabled exclusion.
type SimFixture struct {
	db *sql.DB
}

// NewSimFixture creates a fixture on the given database.
func NewSimFixture(db *sql.DB) *SimFixture {
	return &SimFixture{db: db}
}

// SimProject defines a test project.
type SimProject struct {
	Name      string
	Weight    int
	Priority  float64
	CooldownS int
	Enabled   bool
}

// TestProjects returns a carefully designed set that exercises all scheduler edges.
// Cooldowns are set short for multi-tick simulation (1-5s).
func (sf *SimFixture) TestProjects() []SimProject {
	return []SimProject{
		// Heavyweight: exhaust budget fast (35 each, 2 = 70 of 100).
		{Name: "heavy-alpha", Weight: 35, Priority: 9, CooldownS: 3, Enabled: true},
		{Name: "heavy-beta", Weight: 35, Priority: 8, CooldownS: 3, Enabled: true},
		{Name: "heavy-gamma", Weight: 35, Priority: 2, CooldownS: 3, Enabled: true}, // starvation test

		// Medium weight: fill remaining budget (20 each, 5 = 100).
		{Name: "medium-alpha", Weight: 20, Priority: 7, CooldownS: 2, Enabled: true},
		{Name: "medium-beta", Weight: 20, Priority: 6, CooldownS: 2, Enabled: true},
		{Name: "medium-gamma", Weight: 20, Priority: 5, CooldownS: 2, Enabled: true},
		{Name: "medium-delta", Weight: 20, Priority: 3, CooldownS: 2, Enabled: true},

		// Lightweight: test concurrency cap (8).
		{Name: "light-alpha", Weight: 5, Priority: 9, CooldownS: 1, Enabled: true},
		{Name: "light-beta", Weight: 5, Priority: 8, CooldownS: 1, Enabled: true},
		{Name: "light-gamma", Weight: 5, Priority: 4, CooldownS: 1, Enabled: true},
		{Name: "light-delta", Weight: 5, Priority: 2, CooldownS: 1, Enabled: true},
		{Name: "light-epsilon", Weight: 5, Priority: 1, CooldownS: 1, Enabled: true},

		// Disabled: should never be picked.
		{Name: "ghost-project", Weight: 10, Priority: 5, CooldownS: 60, Enabled: false},
	}
}

// Setup wipes the projects table and inserts the test fixture.
func (sf *SimFixture) Setup(projects []SimProject) error {
	if _, err := sf.db.Exec(`DELETE FROM projects`); err != nil {
		return fmt.Errorf("clear projects: %w", err)
	}
	// Also clear old ticks.
	if _, err := sf.db.Exec(`DELETE FROM ticks`); err != nil {
		return fmt.Errorf("clear ticks: %w", err)
	}

	now := time.Now().Format(time.RFC3339)
	for _, p := range projects {
		_, err := sf.db.Exec(`
			INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, enabled, created_at, updated_at)
			VALUES (?, 'local:/sim', '/tmp/sim', ?, ?, ?, 1.0, ?, ?, ?)
		`, p.Name, p.Weight, p.Priority, p.CooldownS, p.Enabled, now, now)
		if err != nil {
			return fmt.Errorf("insert %s: %w", p.Name, err)
		}
	}
	log.Printf("SIM-SETUP: %d test projects inserted (budget=100, max_concurrent=8)", len(projects))
	return nil
}

// SimRunner runs multi-tick simulations and collects statistics.
type SimRunner struct {
	loop     *Loop
	fixture  *SimFixture
	projects []SimProject
}

// NewSimRunner creates a runner bound to an existing loop.
func NewSimRunner(loop *Loop, fixture *SimFixture) *SimRunner {
	return &SimRunner{
		loop:    loop,
		fixture: fixture,
	}
}

// RunMultiTick runs N evaluation ticks in fast-forward mode.
// Each tick simulates a 60s advancement with cooldown decay.
// Returns per-tick statistics.
func (sr *SimRunner) RunMultiTick(ctx context.Context, tickCount int) (*SimReport, error) {
	projects := sr.fixture.TestProjects()
	if err := sr.fixture.Setup(projects); err != nil {
		return nil, err
	}

	sr.loop.SetSimulation(0.85)
	report := &SimReport{
		TickCount: tickCount,
		Budget:    100,
		MaxConcur: 8,
		Projects:  len(projects),
		Enabled:   countEnabled(projects),
	}

	start := time.Now()
	for tick := 1; tick <= tickCount; tick++ {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}

		tickReport := sr.runOneTick(tick)
		report.Ticks = append(report.Ticks, tickReport)

		// Advance simulated time so cooldowns expire between ticks.
		time.Sleep(time.Duration(sr.fixture.TestProjects()[0].CooldownS) * time.Second)
	}

	report.Elapsed = time.Since(start)

	// Collect aggregate stats.
	for _, tr := range report.Ticks {
		report.TotalSpawned += tr.Spawned
		report.TotalCompleted += tr.Completed
		report.TotalFailed += tr.Failed
		report.TotalTimeout += tr.Timeout
		report.TotalBudgetUsed += tr.BudgetUsed
	}
	report.AvgPerTick = float64(report.TotalSpawned) / float64(tickCount)

	return report, nil
}

func (sr *SimRunner) runOneTick(tickNum int) SimTickReport {
	tr := SimTickReport{Tick: tickNum}

	now := time.Now()
	packed, err := sr.loop.packer.Pick(now)
	if err != nil {
		tr.Error = err.Error()
		return tr
	}

	tr.Selected = len(packed)
	for _, p := range packed {
		tr.BudgetUsed += p.Weight
		tickID := fmt.Sprintf("sim-tick%d-%s-%s", tickNum, p.Name, now.Format("150405"))

		if _, err := sr.loop.simSpawner.Spawn(p, tickID); err != nil {
			tr.Error = fmt.Sprintf("spawn %s: %v", p.Name, err)
			return tr
		}
		tr.Spawned++

		// Record which priority levels were picked.
		tr.PriorityPicked = append(tr.PriorityPicked, int(p.Priority))
		tr.NamesPicked = append(tr.NamesPicked, p.Name)
	}

	// Wait for instantaneous simulated completions.
	time.Sleep(200 * time.Millisecond)

	// Count outcomes from this tick's batch.
	rows, _ := sr.loop.db.Query(`
		SELECT status, COUNT(*) FROM ticks 
		WHERE id LIKE 'sim-tick' || ? || '-%' 
		GROUP BY status
	`, tickNum)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			var count int
			rows.Scan(&status, &count)
			switch status {
			case "completed":
				tr.Completed += count
			case "failed":
				tr.Failed += count
			case "timeout":
				tr.Timeout += count
			}
		}
	}

	return tr
}

// SimReport holds the full simulation result.
type SimReport struct {
	TickCount      int
	Budget         int
	MaxConcur      int
	Projects       int
	Enabled        int
	Elapsed        time.Duration
	Ticks          []SimTickReport
	TotalSpawned   int
	TotalCompleted int
	TotalFailed    int
	TotalTimeout   int
	TotalBudgetUsed int
	AvgPerTick     float64
}

// SimTickReport holds one tick's statistics.
type SimTickReport struct {
	Tick          int
	Selected      int
	Spawned       int
	BudgetUsed    int
	Completed     int
	Failed        int
	Timeout       int
	PriorityPicked []int
	NamesPicked   []string
	Error         string
}

// Summary returns a human-readable summary of the simulation.
func (r *SimReport) Summary() string {
	s := fmt.Sprintf(`
========== SIMULATION REPORT ==========
Ticks:       %d (%.1fs real time)
Projects:    %d total, %d enabled
Budget:      %d  |  Max concurrent: %d

Per tick:    avg %.1f projects, avg %d budget used
Total:       %d spawned, %d completed, %d failed, %d timeout
Success rate: %.1f%%

Priority spread by tick:
`, r.TickCount, r.Elapsed.Seconds(), r.Projects, r.Enabled, r.Budget, r.MaxConcur,
		r.AvgPerTick, r.TotalBudgetUsed/r.TickCount,
		r.TotalSpawned, r.TotalCompleted, r.TotalFailed, r.TotalTimeout,
		float64(r.TotalCompleted)/float64(r.TotalSpawned)*100)

	for _, t := range r.Ticks {
		s += fmt.Sprintf("  tick %2d: %d projects [%v]  budget=%d/100\n",
			t.Tick, t.Selected, t.NamesPicked, t.BudgetUsed)
	}
	return s
}

func countEnabled(projects []SimProject) int {
	n := 0
	for _, p := range projects {
		if p.Enabled {
			n++
		}
	}
	return n
}
