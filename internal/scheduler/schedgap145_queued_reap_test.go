package scheduler

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-145 (2026-09-17): the fleet DB held 7 rows
// with status='queued', session_id NULL and spawned_at from 2026-09-16T20:17 /
// 2026-09-17T01:10 (gitreins-poc, warpfs, off-by-one, 9router,
// coding-hermes-scheduler, duckbrain, heading) — rows a previous process
// enqueued and never dispatched. They inflated the queue reading, and the
// in-flight dedup (`status IN ('queued','running')`) refused to re-spawn those
// projects, so they were unschedulable indefinitely. Startup must mark a
// queued row older than 2x tick_timeout terminal, and must NOT touch a younger
// one (a live process owns its own rows).

// schedgap145TickTimeout is the per-tick deadline used by every test here, so
// the reap window is unambiguous: 2x = 60m.
const schedgap145TickTimeout = 30 * time.Minute

// schedgap145ReapWindow is 2x the tick timeout above — the documented boundary
// between "leftover" and "possibly live".
const schedgap145ReapWindow = 2 * schedgap145TickTimeout

// seedQueuedTickState inserts one 'queued' tick row with an explicit
// spawned_at (the measurement the reaper ages on). created_at mirrors it, so a
// bug that reached for created_at instead would not hide.
func seedQueuedTickState(t *testing.T, db *sql.DB, tickID, project string, spawnedAt time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid)
		 VALUES (?, ?, 'queued', ?, ?, 0)`,
		tickID, project,
		spawnedAt.UTC().Format(time.RFC3339), spawnedAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert queued tick %s: %v", tickID, err)
	}
}

// schedgap145NewLoop builds a loop with the pinned tick timeout and the given
// DB — the same construction the daemon performs at boot (NewLoop +
// SetTickTimeout from --tick-timeout).
func schedgap145NewLoop(t *testing.T, db *sql.DB) *Loop {
	t.Helper()
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetTickTimeout(schedgap145TickTimeout)
	return loop
}

// schedgap145InFlightCount mirrors the scheduler's own in-flight dedup/admission
// query (`loop.go` SpawnNow + evaluate, `session_resume.go` namespace
// admission): queued OR running rows for a project/namespace. This is the
// counter a stale queued row actually inflates.
func schedgap145InFlightCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE status IN ('queued','running')`).Scan(&n); err != nil {
		t.Fatalf("query in-flight count: %v", err)
	}
	return n
}

// schedgap145APIActiveTicksCount is the EXACT query internal/api's
// countActiveTicks serves as `active_ticks` (internal/api/server_helpers.go:
// `SELECT COUNT(*) FROM ticks WHERE status = 'running'`), reported by
// /api/v1/health and /api/v1/status — the field the ops drain script polls.
func schedgap145APIActiveTicksCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n); err != nil {
		t.Fatalf("query api active_ticks: %v", err)
	}
	return n
}

// schedgap145APIQueuedCount is internal/api's queued metric
// (server_metrics.go: `SELECT COUNT(*) FROM ticks WHERE status = 'queued'`).
func schedgap145APIQueuedCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE status = 'queued'`).Scan(&n); err != nil {
		t.Fatalf("query api queued count: %v", err)
	}
	return n
}

// schedgap145StaleFixtureNames are the seven projects measured on 2026-09-17.
func schedgap145StaleFixtureNames() []string {
	return []string{
		"gitreins-poc", "warpfs", "off-by-one", "9router",
		"coding-hermes-scheduler", "duckbrain", "heading",
	}
}

// seedSchedgap145StaleFleet seeds the measured 7-row fixture and returns the
// tick ids in fixture order.
func seedSchedgap145StaleFleet(t *testing.T, db *sql.DB) []string {
	t.Helper()
	now := time.Now()
	ids := make([]string, 0, len(schedgap145StaleFixtureNames()))
	for _, name := range schedgap145StaleFixtureNames() {
		mustCreateProjectINFRA012(t, db, name)
		id := name + "-stale-queued"
		seedQueuedTickState(t, db, id, name, now.Add(-schedgap145ReapWindow-time.Hour))
		ids = append(ids, id)
	}
	return ids
}

// assertSchedgap145Reaped pins the terminal shape of a reaped queued row:
// status='timeout', completed_at stamped (GAP-045), outcome left NULL (the
// CHECK constraint only allows committed/dry_run/failed/timeout).
func assertSchedgap145Reaped(t *testing.T, db *sql.DB, tickID string) {
	t.Helper()
	if got := tickStatusOf(t, db, tickID); got != "timeout" {
		t.Errorf("reaped queued tick %s status = %q, want timeout", tickID, got)
	}
	if outcome := tickOutcomeOf(t, db, tickID); outcome.Valid {
		t.Errorf("reaped queued tick %s outcome = %q, want NULL (CHECK constraint rejects a 'queued_reaped' style value)", tickID, outcome.String)
	}
	if got := tickCompletedAtOf(t, db, tickID); !got.Valid || got.String == "" {
		t.Errorf("reaped queued tick %s completed_at = %v, want stamped (GAP-045: reaped rows must be terminal)", tickID, got)
	}
}

// assertSchedgap145LeftQueued pins a row the reaper must not touch: still
// 'queued', no completed_at, no outcome — a live process owns it.
func assertSchedgap145LeftQueued(t *testing.T, db *sql.DB, tickID string) {
	t.Helper()
	if got := tickStatusOf(t, db, tickID); got != "queued" {
		t.Errorf("fresh queued tick %s status = %q, want queued — younger than 2x tick_timeout must never be reaped", tickID, got)
	}
	if got := tickCompletedAtOf(t, db, tickID); got.Valid && got.String != "" {
		t.Errorf("fresh queued tick %s completed_at = %q, want NULL (row must stay live)", tickID, got.String)
	}
	if outcome := tickOutcomeOf(t, db, tickID); outcome.Valid {
		t.Errorf("fresh queued tick %s outcome = %q, want NULL", tickID, outcome.String)
	}
}

// TestSCHED145_StaleQueuedRowsReaped (AC1, and the AC5 mutation discriminator):
// a queued row older than 2x tick_timeout is reaped on the startup path, while
// a fresh row in the SAME call is left alone — so dropping the age filter from
// the reap query turns this test red (the fresh control row gets reaped too).
func TestSCHED145_StaleQueuedRowsReaped(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	staleIDs := make([]string, 0, 7)
	for i, name := range schedgap145StaleFixtureNames() {
		mustCreateProjectINFRA012(t, db, name)
		id := name + "-stale"
		// Staggered ages, all comfortably past the window (mirrors the real
		// fixture: 2026-09-16T20:17 through 2026-09-17T01:10).
		seedQueuedTickState(t, db, id, name, now.Add(-schedgap145ReapWindow-time.Duration(i+1)*time.Hour))
		staleIDs = append(staleIDs, id)
	}

	// Fresh control row: enqueued moments ago, must survive the same reap.
	mustCreateProjectINFRA012(t, db, "live-process-project")
	seedQueuedTickState(t, db, "live-fresh", "live-process-project", now.Add(-time.Minute))

	if got := schedgap145InFlightCount(t, db); got != len(staleIDs)+1 {
		t.Fatalf("pre-reap in-flight ticks = %d, want %d (fixture premise)", got, len(staleIDs)+1)
	}

	loop := schedgap145NewLoop(t, db)
	if n := loop.reapStaleQueuedRows(); n != len(staleIDs) {
		t.Errorf("reapStaleQueuedRows() = %d, want %d (only the rows older than %v)", n, len(staleIDs), schedgap145ReapWindow)
	}

	for _, id := range staleIDs {
		assertSchedgap145Reaped(t, db, id)
	}
	assertSchedgap145LeftQueued(t, db, "live-fresh")
}

// TestSCHED145_FreshQueuedRowLeftAlone (AC2): the age boundary. 59m-old stays
// queued, 61m-old is reaped — 2x tick_timeout is the documented cut.
func TestSCHED145_FreshQueuedRowLeftAlone(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "boundary-proj")
	now := time.Now()

	// Just inside the window: 1 minute younger than 2x tick_timeout.
	seedQueuedTickState(t, db, "boundary-young", "boundary-proj", now.Add(-(schedgap145ReapWindow - time.Minute)))
	// Just outside it.
	seedQueuedTickState(t, db, "boundary-old", "boundary-proj", now.Add(-(schedgap145ReapWindow + time.Minute)))

	loop := schedgap145NewLoop(t, db)
	if n := loop.reapStaleQueuedRows(); n != 1 {
		t.Errorf("reapStaleQueuedRows() = %d, want 1 (only the row past 2x tick_timeout)", n)
	}
	assertSchedgap145LeftQueued(t, db, "boundary-young")
	assertSchedgap145Reaped(t, db, "boundary-old")

	// The reaper must be a no-op on a second call (idempotent startup).
	if n := loop.reapStaleQueuedRows(); n != 0 {
		t.Errorf("second reapStaleQueuedRows() = %d, want 0 (nothing left to reap)", n)
	}
	assertSchedgap145LeftQueued(t, db, "boundary-young")
}

// TestSCHED145_ReapedRowsLeaveNoActiveOrInFlightTicks (AC3): the counters that
// read the queue must no longer see the reaped rows. The stale rows inflate the
// scheduler's in-flight (`queued` OR `running`) dedup/admission count and the
// API's `queued` metric; the API's `active_ticks` helper counts `running` rows
// only, so it is reported too and pinned at 0 both before and after (honest
// scope: active_ticks never counted queued rows — the ticket's premise is only
// true of the in-flight/queued counters).
func TestSCHED145_ReapedRowsLeaveNoActiveOrInFlightTicks(t *testing.T) {
	db := newTestDB(t)
	ids := seedSchedgap145StaleFleet(t, db)

	if got := schedgap145InFlightCount(t, db); got != len(ids) {
		t.Fatalf("pre-reap in-flight count = %d, want %d (fixture premise: stale queued rows ARE counted by the dedup/admission query)", got, len(ids))
	}
	if got := schedgap145APIQueuedCount(t, db); got != len(ids) {
		t.Fatalf("pre-reap /api/v1/metrics queued = %d, want %d (fixture premise: the queue reading was inflated)", got, len(ids))
	}
	if got := schedgap145APIActiveTicksCount(t, db); got != 0 {
		t.Fatalf("pre-reap active_ticks = %d, want 0 (the API helper counts running rows only)", got)
	}

	loop := schedgap145NewLoop(t, db)
	loop.reapStaleQueuedRows()

	if got := schedgap145InFlightCount(t, db); got != 0 {
		t.Errorf("post-reap in-flight count = %d, want 0 — a reaped queued row must not block the project's next spawn", got)
	}
	if got := schedgap145APIQueuedCount(t, db); got != 0 {
		t.Errorf("post-reap /api/v1/metrics queued = %d, want 0 — the queue must not read as live work", got)
	}
	if got := schedgap145APIActiveTicksCount(t, db); got != 0 {
		t.Errorf("post-reap active_ticks = %d, want 0", got)
	}
}

// TestSCHED145_SevenStaleRowsFixtureReportsZeroInFlight (AC4, the original
// symptom): the measured 7-row fixture, reaped through the exact startup
// sequence Run() executes (cleanDanglingOnStartup then reapStaleQueuedRows —
// ordering pinned by TestSCHED145_StartupReapIsWiredIntoRun), leaves the
// in-flight count at 0.
//
// Run() itself is not driven here because it blocks on the eval loop and its
// first evaluation would spawn/fail ticks for the fixture projects, adding
// noise this assertion cannot tolerate; the source-level test below is what
// keeps the two calls and their order honest.
func TestSCHED145_SevenStaleRowsFixtureReportsZeroInFlight(t *testing.T) {
	db := newTestDB(t)
	ids := seedSchedgap145StaleFleet(t, db)
	if len(ids) != 7 {
		t.Fatalf("fixture holds %d rows, want 7 (2026-09-17 measurement)", len(ids))
	}
	if got := schedgap145InFlightCount(t, db); got != 7 {
		t.Fatalf("pre-startup in-flight count = %d, want 7 (the measured symptom)", got)
	}

	loop := schedgap145NewLoop(t, db)
	// Startup sequence, same order as Loop.Run.
	loop.cleanDanglingOnStartup()
	loop.reapStaleQueuedRows()

	if got := schedgap145InFlightCount(t, db); got != 0 {
		t.Errorf("in-flight count after startup = %d, want 0 (SCHED-GAP-145: 7 stale queued rows blocked their projects and read as a live queue)", got)
	}
	for _, id := range ids {
		assertSchedgap145Reaped(t, db, id)
	}
}

// TestSCHED145_StartupReapIsWiredIntoRun (AC6): the reap must be invoked at
// startup, in Run(), AFTER cleanDanglingOnStartup and BEFORE the initial
// evaluation is triggered. A helper nobody calls is the classic
// unwired-implementation failure mode, so this pins the call site in source.
func TestSCHED145_StartupReapIsWiredIntoRun(t *testing.T) {
	src, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatalf("read loop.go: %v", err)
	}
	text := string(src)

	runIdx := strings.Index(text, "func (l *Loop) Run() {")
	if runIdx < 0 {
		t.Fatal("Loop.Run not found in loop.go")
	}
	cleanIdx := strings.Index(text, "l.cleanDanglingOnStartup()")
	reapIdx := strings.Index(text, "l.reapStaleQueuedRows()")
	firstEvalIdx := strings.Index(text, "Fire initial evaluation so the fleet starts immediately")

	if cleanIdx < 0 {
		t.Fatal("cleanDanglingOnStartup call site missing from loop.go")
	}
	if reapIdx < 0 {
		t.Fatal("reapStaleQueuedRows() is never called — SCHED-GAP-145 reap is unwired")
	}
	if reapIdx < runIdx {
		t.Error("reapStaleQueuedRows() is called outside Loop.Run (before it in source order)")
	}
	if reapIdx < cleanIdx {
		t.Error("reapStaleQueuedRows() runs BEFORE cleanDanglingOnStartup — the startup reap order changed")
	}
	if firstEvalIdx > 0 && reapIdx > firstEvalIdx {
		t.Error("reapStaleQueuedRows() runs after the initial evaluation trigger — stale queued rows would already have been deduped against")
	}
}

// TestSCHED145_ReapWindowFollowsConfiguredTickTimeout: the window is
// 2 x the CONFIGURED tick timeout (SetTickTimeout), not a hardcoded age — a
// daemon booted with a longer deadline must not reap rows its predecessor is
// still legitimately holding.
func TestSCHED145_ReapWindowFollowsConfiguredTickTimeout(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "cfg-proj")

	// Age that is stale under the default 30m deadline (2x = 60m) but well
	// inside the window of a 2h deadline (2x = 4h).
	seedQueuedTickState(t, db, "cfg-mid", "cfg-proj", time.Now().Add(-90*time.Minute))

	longLoop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	longLoop.SetTickTimeout(2 * time.Hour)
	if n := longLoop.reapStaleQueuedRows(); n != 0 {
		t.Errorf("reapStaleQueuedRows() with a 2h tick timeout = %d, want 0 (90m is inside the 4h window)", n)
	}
	assertSchedgap145LeftQueued(t, db, "cfg-mid")

	shortLoop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	shortLoop.SetTickTimeout(schedgap145TickTimeout)
	if n := shortLoop.reapStaleQueuedRows(); n != 1 {
		t.Errorf("reapStaleQueuedRows() with a 30m tick timeout = %d, want 1 (90m is past the 60m window)", n)
	}
	assertSchedgap145Reaped(t, db, "cfg-mid")
}
