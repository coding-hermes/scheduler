package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SimFixture creates a clean set of test projects for simulation testing.
// Projects are designed to exercise: concurrency cap, weight budget packing,
// priority decay/starvation, cooldown throttling, and disabled exclusion.
type SimFixture struct {
	db *sql.DB
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk clockSeam
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

		// Medium weight: fill remaining budget (20 each, 4 = 80).
		{Name: "medium-alpha", Weight: 20, Priority: 7, CooldownS: 2, Enabled: true},
		{Name: "medium-beta", Weight: 20, Priority: 6, CooldownS: 2, Enabled: true},
		{Name: "medium-gamma", Weight: 20, Priority: 5, CooldownS: 2, Enabled: true},
		{Name: "medium-delta", Weight: 20, Priority: 3, CooldownS: 2, Enabled: true},

		// Lightweight: test concurrency cap (5).
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
// When withBoards is true, each project also gets its own workdir containing a
// dummy .coding-hermes/board/tasks.jsonl, so the adaptive-cooldown board-row
// signal (countBoardRows / board_rows_seen) works in dry-runs.
func (sf *SimFixture) Setup(projects []SimProject) error {
	// DOGFOOD-021: wipe CHILD rows before the parent row, inside ONE
	// transaction. ticks.project_name carries a FOREIGN KEY to projects(name)
	// and InitDB enforces PRAGMA foreign_keys=ON, so deleting projects first
	// fails with "FOREIGN KEY constraint failed (787)" whenever the target DB
	// already holds tick history (--sim-setup against an existing DB file),
	// FATALing the boot. SCHED-GAP-019's `rm -f <rundir>/*.db` workaround is no
	// longer required. Reordering alone would still leave a window in which a
	// concurrent writer inserts a child row between the two DELETEs, so both
	// statements run in a single transaction.
	tx, err := sf.db.Begin()
	if err != nil {
		return fmt.Errorf("clear sim state: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	if _, err := tx.Exec(`DELETE FROM ticks`); err != nil {
		return fmt.Errorf("clear ticks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM projects`); err != nil {
		return fmt.Errorf("clear projects: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("clear sim state: commit: %w", err)
	}

	now := sf.clock().Now().Format(time.RFC3339)
	for _, p := range projects {
		workdir := "/tmp/sim"
		// Per-project workdir with a dummy board: gives the adaptive engine a
		// real board file to observe (baseline → growth → progress reset).
		if bd, err := ensureSimBoard(p.Name); err == nil {
			workdir = bd
		}
		_, err := sf.db.Exec(`
			INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, enabled, adaptive_cooldown, created_at, updated_at)
			VALUES (?, 'local:/sim', ?, ?, ?, ?, 1.0, ?, 1, ?, ?)
		`, p.Name, workdir, p.Weight, p.Priority, p.CooldownS, p.Enabled, now, now)
		if err != nil {
			return fmt.Errorf("insert %s: %w", p.Name, err)
		}
	}
	log.Printf("SIM-SETUP: %d test projects inserted (budget=100, max_concurrent=8)", len(projects))
	return nil
}

// ensureSimBoard creates /tmp/sim-boards/<name>/.coding-hermes/board/tasks.jsonl
// with a couple of dummy rows, returning the workdir path.
func ensureSimBoard(project string) (string, error) {
	dir := filepath.Join("/tmp", "sim-boards", project, ".coding-hermes", "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	board := filepath.Join(dir, "tasks.jsonl")
	if _, err := os.Stat(board); os.IsNotExist(err) {
		rows := fmt.Sprintf("{\"id\": %q-1, \"title\": \"sim seed row 1\", \"status\": \"todo\"}\n"+
			"{\"id\": %q-2, \"title\": \"sim seed row 2\", \"status\": \"todo\"}\n", project, project)
		if err := os.WriteFile(board, []byte(rows), 0o644); err != nil {
			return "", err
		}
	}
	return filepath.Dir(filepath.Dir(dir)), nil // .../<project> (strip .coding-hermes/board)
}

// SimRunner runs multi-tick simulations and collects statistics.
type SimRunner struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk         clockSeam
	loop        *Loop
	fixture     *SimFixture
	idleRate    float64
	successRate float64
}

// simTickIDPrefix is the ID prefix of every tick RunMultiTick spawns
// (sim-tick<NN>-<project>-<HHMMSS>). RunBulkSim's IDs ("sim-<project>-...")
// deliberately do not match, so the authoritative totals below count only
// this runner's rows.
const simTickIDPrefix = "sim-tick"

// NewSimRunner creates a runner bound to an existing loop.
func NewSimRunner(loop *Loop, fixture *SimFixture) *SimRunner {
	return &SimRunner{
		loop:    loop,
		fixture: fixture,
	}
}

// SetIdleRate sets the fraction of completed sim ticks with zero commits
// (--sim-idle). Applied when RunMultiTick enables simulation — after the
// sim spawner exists, so it actually sticks.
func (sr *SimRunner) SetIdleRate(rate float64) {
	sr.idleRate = rate
}

// SetSuccessRate sets the simulated success fraction for RunMultiTick
// (mirrors SetIdleRate). Must be called before RunMultiTick: RunMultiTick
// applies it to the loop's sim spawner when it enables simulation. Values
// are clamped to (0, 1]; out-of-range values keep the 0.85 default so a
// misconfigured flag can never simulate a 0%-success fleet.
func (sr *SimRunner) SetSuccessRate(rate float64) {
	if rate <= 0 || rate > 1 {
		return
	}
	sr.successRate = rate
}

// RunMultiTick runs N evaluation ticks in fast-forward mode.
// Each tick simulates a 60s advancement with cooldown decay.
// Returns per-tick statistics.
func (sr *SimRunner) RunMultiTick(ctx context.Context, tickCount int) (*SimReport, error) {
	projects := sr.fixture.TestProjects()
	if err := sr.fixture.Setup(projects); err != nil {
		return nil, err
	}

	if sr.successRate > 0 {
		sr.loop.SetSimulation(sr.successRate)
	} else {
		sr.loop.SetSimulation(0.85)
	}
	sr.loop.SetSimIdleRate(sr.idleRate)
	report := &SimReport{
		TickCount: tickCount,
		Budget:    100,
		MaxConcur: 8,
		Projects:  len(projects),
		Enabled:   countEnabled(projects),
	}

	start := sr.clock().Now()
	for tick := 1; tick <= tickCount; tick++ {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}

		tickReport := sr.runOneTick(tick)
		report.Ticks = append(report.Ticks, tickReport)

		// Advance simulated time so cooldowns expire between ticks.
		sr.clock().Sleep(time.Duration(sr.fixture.TestProjects()[0].CooldownS) * time.Second)
	}

	report.Elapsed = sr.clock().Since(start)

	// SCHED-GAP-1630: the completion goroutines of the LAST batch may still
	// be pending on their 50-250ms sim sleeps, so per-tick snapshots taken
	// 200ms after each batch under-count. The report must be authoritative
	// at return — settle the outstanding completion timers (bounded by the
	// context on both clocks), then re-read the totals straight from
	// SQLite's GROUP BY ticks.status over this runner's rows so the report
	// can never disagree with its own DB.
	sr.settleSimWork(ctx)
	report.TotalSpawned, report.TotalCompleted, report.TotalFailed, report.TotalTimeout =
		sr.dbStatusTotals()
	// Per-batch budget use is a packer decision known only in-process — it
	// has no DB row to re-read — so it stays a snapshot sum.
	for _, tr := range report.Ticks {
		report.TotalBudgetUsed += tr.BudgetUsed
	}
	report.AvgPerTick = float64(report.TotalSpawned) / float64(tickCount)

	return report, nil
}

// settleSimWork waits until every simulated tick spawned during the run has
// written its outcome row, bounded by ctx (and a hard deadline as backstop).
//
// Proof of settling: Spawn inserts each row SYNCHRONOUSLY with the
// transitional 'running' status; the completion goroutine later moves its row
// to a terminal status with a single UPDATE. A row is therefore 'running'
// exactly while its outcome write is in flight, so zero running rows among
// this runner's IDs proves every spawned tick has settled — on the wall
// clock and on a simulated clock alike, with no clock-specific waiting.
func (sr *SimRunner) settleSimWork(ctx context.Context) {
	const (
		poll    = 20 * time.Millisecond
		maxWait = 30 * time.Second
	)
	// All reads/waits go through the component clock (SCHED-GAP-169 static
	// guard). On a SimClock the poll is a virtual sleep; on the wall clock
	// it is the real 20ms.
	clk := sr.clock()
	deadline := clk.Now().Add(maxWait)
	for {
		if ctx.Err() != nil {
			return
		}
		if sr.runningCount() == 0 {
			return
		}
		if clk.Now().After(deadline) {
			return // bounded; DB totals stay authoritative even if late
		}
		clk.Sleep(poll)
	}
}

// runningCount counts this runner's tick rows still in the transitional
// 'running' status (see settleSimWork for why that is the settle oracle).
func (sr *SimRunner) runningCount() int {
	var n int
	if err := sr.loop.db.QueryRow(`
		SELECT COUNT(*) FROM ticks
		WHERE id LIKE ? || '%' AND status = ?
	`, simTickIDPrefix, string(TickRunning)).Scan(&n); err != nil {
		return 0
	}
	return n
}

// dbStatusTotals reads the authoritative tick-status totals for this
// runner's rows: one GROUP BY over the DB the report must agree with.
func (sr *SimRunner) dbStatusTotals() (spawned, completed, failed, timeout int) {
	rows, err := sr.loop.db.Query(`
		SELECT status, COUNT(*) FROM ticks
		WHERE id LIKE ? || '%'
		GROUP BY status
	`, simTickIDPrefix)
	if err != nil {
		return 0, 0, 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			continue
		}
		spawned += count
		switch status {
		case string(TickCompleted):
			completed += count
		case string(TickFailed):
			failed += count
		case string(TickTimeout):
			timeout += count
		}
	}
	_ = rows.Err()
	return spawned, completed, failed, timeout
}

func (sr *SimRunner) runOneTick(tickNum int) SimTickReport {
	tr := SimTickReport{Tick: tickNum}

	now := sr.clock().Now()
	packed, err := sr.loop.packer.Pick(now, nil)
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
	sr.clock().Sleep(200 * time.Millisecond)

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
	TickCount       int
	Budget          int
	MaxConcur       int
	Projects        int
	Enabled         int
	Elapsed         time.Duration
	Ticks           []SimTickReport
	TotalSpawned    int
	TotalCompleted  int
	TotalFailed     int
	TotalTimeout    int
	TotalBudgetUsed int
	AvgPerTick      float64
}

// SimTickReport holds one tick's statistics.
type SimTickReport struct {
	Tick           int
	Selected       int
	Spawned        int
	BudgetUsed     int
	Completed      int
	Failed         int
	Timeout        int
	PriorityPicked []int
	NamesPicked    []string
	Error          string
}

// Summary returns a human-readable summary of the simulation.
func (r *SimReport) Summary() string {
	// SCHED-GAP-1630: a zero-tick or zero-spawn report must render sanely —
	// no integer divide-by-zero (TickCount=0), no NaN success rate (0/0).
	budgetPerTick := 0
	if r.TickCount > 0 {
		budgetPerTick = r.TotalBudgetUsed / r.TickCount
	}
	successRate := 0.0
	if r.TotalSpawned > 0 {
		successRate = float64(r.TotalCompleted) / float64(r.TotalSpawned) * 100
	}
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
		r.AvgPerTick, budgetPerTick,
		r.TotalSpawned, r.TotalCompleted, r.TotalFailed, r.TotalTimeout,
		successRate)

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

// SetClock installs the clock this SimFixture reads and waits on (SCHED-GAP-169).
// nil keeps the wall clock.
func (sf *SimFixture) SetClock(c clock.Clock) { sf.clk.Set(c) }

// clock returns the component's clock, never nil.
func (sf *SimFixture) clock() clock.Clock { return sf.clk.Get() }

// SetClock installs the clock this runner reads and waits on (SCHED-GAP-169).
// nil keeps the owning loop's clock (and, without one, the wall clock).
func (sr *SimRunner) SetClock(c clock.Clock) { sr.clk.Set(c) }

// clock returns the runner's clock: its own if installed, else the owning
// loop's, else the wall clock.
func (sr *SimRunner) clock() clock.Clock {
	if sr.clk.Installed() {
		return sr.clk.Get()
	}
	if sr.loop != nil {
		return sr.loop.clock()
	}
	return clock.Real()
}
