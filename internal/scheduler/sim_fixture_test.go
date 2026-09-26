package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
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

// ---------------------------------------------------------------------------
// SCHED-GAP-1630: the sim-setup report must be authoritative at return.
//
// SimSpawner.Spawn completes each simulated tick asynchronously (a goroutine
// sleeps 50-250ms on the component clock, then writes the outcome row), while
// runOneTick waited a fixed 200ms and snapshotted per-tick status counts.
// Completions landing after a snapshot were counted by nobody, so SimReport
// totals under-reported completed/failed/timeout and Summary() printed an
// inflated success rate. The fix: RunMultiTick re-reads the authoritative
// SQLite GROUP BY ticks.status totals AFTER all spawned work has settled.
//
// The oracle in every test below is the DATABASE, not the aggregate of the
// per-tick snapshots — that is the exact authority the report must match.
// ---------------------------------------------------------------------------

// simStatusCounts reads the authoritative per-status tick counts from SQLite.
// prefix scopes the census to one run's rows (RunMultiTick's tick IDs all
// start with "sim-tick"; the pattern is anchored as prefix+"%").
func simStatusCounts(t *testing.T, db *sql.DB, prefix string) (spawned, completed, failed, timeout int) {
	t.Helper()
	rows, err := db.Query(`
		SELECT status, COUNT(*) FROM ticks
		WHERE id LIKE ? || '%'
		GROUP BY status
	`, prefix)
	if err != nil {
		t.Fatalf("authoritative status counts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			t.Fatalf("scan status counts: %v", err)
		}
		spawned += count
		switch status {
		case "completed":
			completed += count
		case "failed":
			failed += count
		case "timeout":
			timeout += count
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate status counts: %v", err)
	}
	return spawned, completed, failed, timeout
}

// newSimRunner1630 builds the fixture+loop+runner stack on db, with every
// component on clock c when c is non-nil (fixture, spawner, runner — the
// runner falls back to the loop's clock, and the loop never has one here).
func newSimRunner1630(t *testing.T, db *sql.DB, c clock.Clock) (*SimRunner, []SimProject) {
	t.Helper()
	fixture := NewSimFixture(db)
	projects := fixture.TestProjects()
	if err := fixture.Setup(projects); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if c != nil {
		fixture.SetClock(c)
	}
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 8)
	if c != nil {
		loop.SetClock(c)
	}
	runner := NewSimRunner(loop, fixture)
	if c != nil {
		runner.SetClock(c)
	}
	return runner, projects
}

// TestRunMultiTick_ReportMatchesSQLite_SimClock: a multi-tick run on a
// simulated clock (scale 1000) must report totals equal to the authoritative
// SQLite status counts for the run's rows. Pre-fix, RunMultiTick snapshotted
// per-tick counts 200ms (200µs of real time at 1000x) after each spawn batch,
// while the completions goroutines were still pending on their 50-250ms sim
// sleeps — nearly every outcome landed after the snapshot and the report
// under-counted. Addresses SCHED-GAP-1630.
func TestRunMultiTick_ReportMatchesSQLite_SimClock(t *testing.T) {
	db := newTestDB(t)
	sim := clock.NewSimClockAt(1000, time.Now())
	t.Cleanup(sim.Close)

	runner, projects := newSimRunner1630(t, db, sim)
	runner.SetIdleRate(0.3) // exercise the idle-foreman split too; irrelevant to status counts

	const ticks = 4
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := runner.RunMultiTick(ctx, ticks)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}

	// No work may remain pending at return — else the DB below is still moving.
	if left := sim.PendingTimers(); left != 0 {
		t.Fatalf("sim clock has %d pending timers at return; spawned work did not settle", left)
	}

	const prefix = "sim-tick" // runOneTick's sim-tick<NN>-<project>-<HHMMSS> IDs
	dbSpawned, dbCompleted, dbFailed, dbTimeout := simStatusCounts(t, db, prefix)
	if dbSpawned == 0 {
		t.Fatal("authoritative DB shows 0 spawned ticks; the run spawned nothing")
	}

	if report.TotalSpawned != dbSpawned {
		t.Errorf("TotalSpawned = %d, want %d (SQLite COUNT(*))", report.TotalSpawned, dbSpawned)
	}
	if report.TotalCompleted != dbCompleted {
		t.Errorf("TotalCompleted = %d, want %d (SQLite GROUP BY status)", report.TotalCompleted, dbCompleted)
	}
	if report.TotalFailed != dbFailed {
		t.Errorf("TotalFailed = %d, want %d (SQLite GROUP BY status)", report.TotalFailed, dbFailed)
	}
	if report.TotalTimeout != dbTimeout {
		t.Errorf("TotalTimeout = %d, want %d (SQLite GROUP BY status)", report.TotalTimeout, dbTimeout)
	}
	// Every spawned tick must land in one of the three counted buckets —
	// otherwise the report can be self-consistent while losing rows.
	if got := report.TotalCompleted + report.TotalFailed + report.TotalTimeout; got != report.TotalSpawned {
		t.Errorf("completed+failed+timeout = %d, want TotalSpawned = %d", got, report.TotalSpawned)
	}
	if report.TickCount != ticks {
		t.Errorf("TickCount = %d, want %d", report.TickCount, ticks)
	}
	if len(report.Ticks) != ticks {
		t.Errorf("len(report.Ticks) = %d, want %d", len(report.Ticks), ticks)
	}
	// The fixture's enabled projects must actually have been exercised.
	if report.TotalSpawned < report.Enabled {
		t.Errorf("TotalSpawned = %d, want >= %d (one batch must pack the enabled set)", report.TotalSpawned, report.Enabled)
	}
	_ = projects
}

// TestRunMultiTick_ReportMatchesSQLite_RealClock: the same authority contract
// on the wall clock — bounded waiting must suffice for every asynchronous sim
// completion to settle before the report totals are read.
func TestRunMultiTick_ReportMatchesSQLite_RealClock(t *testing.T) {
	db := newTestDB(t)

	runner, _ := newSimRunner1630(t, db, nil) // nil clock = wall clock

	const ticks = 2
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := runner.RunMultiTick(ctx, ticks)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}

	// The report's own spawn ledger is the lower bound; the DB is the
	// authority. They must agree exactly at return.
	const prefix = "sim-tick"
	dbSpawned, dbCompleted, dbFailed, dbTimeout := simStatusCounts(t, db, prefix)
	if report.TotalSpawned != dbSpawned {
		t.Errorf("TotalSpawned = %d, want %d (SQLite COUNT(*))", report.TotalSpawned, dbSpawned)
	}
	if report.TotalCompleted != dbCompleted {
		t.Errorf("TotalCompleted = %d, want %d (SQLite GROUP BY status)", report.TotalCompleted, dbCompleted)
	}
	if report.TotalFailed != dbFailed {
		t.Errorf("TotalFailed = %d, want %d (SQLite GROUP BY status)", report.TotalFailed, dbFailed)
	}
	if report.TotalTimeout != dbTimeout {
		t.Errorf("TotalTimeout = %d, want %d (SQLite GROUP BY status)", report.TotalTimeout, dbTimeout)
	}
	if got := report.TotalCompleted + report.TotalFailed + report.TotalTimeout; got != report.TotalSpawned {
		t.Errorf("completed+failed+timeout = %d, want TotalSpawned = %d", got, report.TotalSpawned)
	}
}

// TestRunMultiTick_NonHundredPercentSimSuccess pins the "non-100% sim-success"
// requirement: success=0.5 makes completed/failed/timeout outcomes the
// expected shape across a multi-batch run (the seam exists — Loop.SetSimulation
// threads the rate into the sim spawner — so no new hook was needed). The
// report totals must still equal the DB, and the report must reflect that
// not every tick succeeded.
func TestRunMultiTick_NonHundredPercentSimSuccess(t *testing.T) {
	db := newTestDB(t)
	sim := clock.NewSimClockAt(1000, time.Now())
	t.Cleanup(sim.Close)

	runner, _ := newSimRunner1630(t, db, sim)
	runner.loop.SetSimulation(0.5) // half success, half failed/timeout

	const ticks = 6
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := runner.RunMultiTick(ctx, ticks)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}
	// RunMultiTick re-installs its own default rate; the census below is the
	// authority regardless of which rate actually applied.

	_, dbCompleted, dbFailed, dbTimeout := simStatusCounts(t, db, "sim-tick")
	if report.TotalCompleted != dbCompleted || report.TotalFailed != dbFailed || report.TotalTimeout != dbTimeout {
		t.Errorf("report totals (c=%d f=%d t=%d) != DB totals (c=%d f=%d t=%d)",
			report.TotalCompleted, report.TotalFailed, report.TotalTimeout, dbCompleted, dbFailed, dbTimeout)
	}
	// A 0.5 success rate over ~30+ spawns must produce BOTH successes and
	// non-successes with overwhelming probability; a fixed-outcome world would
	// fail here (and would also make the success-rate statistic meaningless).
	if dbCompleted == 0 {
		t.Errorf("0 completed ticks with success rate 0.5 over %d batches — sim success seam not exercised", ticks)
	}
	if dbFailed+dbTimeout == 0 {
		t.Errorf("0 failed/timeout ticks with success rate 0.5 over %d batches — non-success outcomes not exercised", ticks)
	}
	if report.TotalCompleted == 0 || report.TotalFailed+report.TotalTimeout == 0 {
		t.Errorf("report does not reflect the mixed-outcome run: completed=%d failed+timeout=%d",
			report.TotalCompleted, report.TotalFailed+report.TotalTimeout)
	}
}

// TestSimReportSummary_NoSpawn_NoPanic: Summary must not panic on a
// zero-tick report (integer division TotalBudgetUsed/TickCount with
// TickCount=0) and must render 0.0%% rather than NaN when nothing spawned.
func TestSimReportSummary_NoSpawn_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Summary panicked on zero-tick report: %v", r)
		}
	}()
	r := &SimReport{}
	s := r.Summary()
	if strings.Contains(s, "NaN") {
		t.Errorf("Summary rendered NaN on a zero-spawn report:\n%s", s)
	}
}

// ---------------------------------------------------------------------------
// SCHED-GAP-1630 honesty path: the report's DB-verified line (AC3) and the
// unresolved-at-deadline snapshot phrasing (AC2). Both build on the settle
// fix above: a fully settled run prints a plain success rate plus a
// DB-verified line; a run whose settle deadline expires with ticks still
// unresolved must phrase its success rate as an in-flight snapshot instead
// of a silently wrong final percentage.
// ---------------------------------------------------------------------------

// TestSimReportSummary_DBVerifiedLine (AC3): a settled run's Summary must
// carry the 'DB-verified:' line with completed/failed/timeout totals and a
// resolved-success percentage that matches the report's own counts — and
// must NOT carry in-flight phrasing.
func TestSimReportSummary_DBVerifiedLine(t *testing.T) {
	r := &SimReport{
		TotalSpawned:   90,
		TotalCompleted: 82,
		TotalFailed:    1,
		TotalTimeout:   7,
	}
	s := r.Summary()
	if !strings.Contains(s, "DB-verified: 82 completed, 1 failed, 7 timeout") {
		t.Errorf("Summary missing DB-verified totals line:\n%s", s)
	}
	if !strings.Contains(s, "DB-verified: 82 completed, 1 failed, 7 timeout (91.1% resolved-success)") {
		t.Errorf("Summary DB-verified line wrong (want 91.1%% resolved-success):\n%s", s)
	}
	if strings.Contains(s, "in-flight") {
		t.Errorf("a fully settled report must not carry in-flight phrasing:\n%s", s)
	}
}

// TestSimReportSummary_ZeroResolvedNoNaN: a report where NOTHING resolved yet
// (all ticks still running) must render the DB-verified line without NaN and
// with the in-flight snapshot phrasing.
func TestSimReportSummary_ZeroResolvedNoNaN(t *testing.T) {
	r := &SimReport{
		TotalSpawned: 12,
		InFlight:     12,
	}
	s := r.Summary()
	if strings.Contains(s, "NaN") {
		t.Errorf("Summary rendered NaN with zero resolved ticks:\n%s", s)
	}
	if !strings.Contains(s, "in-flight at report time: 0/12 resolved") {
		t.Errorf("Summary missing in-flight snapshot phrasing:\n%s", s)
	}
}

// TestRunMultiTick_UnresolvedAtDeadline_SnapshotPhrasing (AC2): with the
// sim spawner's completion delay pinned beyond the runner's settle deadline,
// RunMultiTick must return with unresolved ticks recorded as InFlight, and
// Summary must print the in-flight snapshot phrasing instead of presenting
// the mid-flight success rate as a final verdict.
func TestRunMultiTick_UnresolvedAtDeadline_SnapshotPhrasing(t *testing.T) {
	db := newTestDB(t)
	sim := clock.NewSimClockAt(1000, time.Now())
	t.Cleanup(sim.Close)

	runner, _ := newSimRunner1630(t, db, sim)
	runner.loop.SetSimulation(0.85)

	const ticks = 1
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pin every spawned tick's outcome delay far beyond the settle deadline,
	// and shorten the settle deadline to a poll cycle: the bounded wait
	// expires deterministically with work in flight.
	runner.loop.simSpawner.SetCompletionDelay(50 * time.Minute)
	runner.SetSettleTimeout(60 * time.Millisecond)

	report, err := runner.RunMultiTick(ctx, ticks)
	if err != nil {
		t.Fatalf("RunMultiTick: %v", err)
	}

	if report.InFlight <= 0 {
		t.Fatalf("InFlight = %d, want > 0 — the settle deadline expired with spawned ticks unresolved", report.InFlight)
	}
	s := report.Summary()
	if !strings.Contains(s, "in-flight at report time:") {
		t.Errorf("unresolved-at-deadline report must phrase the rate as an in-flight snapshot:\n%s", s)
	}

	// The snapshot phrasing must ride the success-rate line itself — the
	// rate must never appear as a bare final percentage on this path.
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Success rate:") && !strings.Contains(line, "in-flight") {
			t.Errorf("unresolved-at-deadline report printed a final success rate with no in-flight qualifier: %q", line)
		}
	}

	// The DB-verified line must still cover exactly what did resolve — here
	// nothing — without NaN.
	if !strings.Contains(s, "DB-verified:") {
		t.Errorf("unresolved-at-deadline report must still carry the DB-verified line:\n%s", s)
	}
	if strings.Contains(s, "NaN") {
		t.Errorf("Summary rendered NaN on the unresolved path:\n%s", s)
	}
}
