package scheduler

// SCHED-GAP-143 — regression battery for the GATEWAY-DRAIN error class.
//
// RULE PINNED HERE: a tick refused by the gateway's DRAIN response
// (HTTP 503, body {"error":"Gateway is draining"}) is TRANSPORT-CLASS — the
// work never reached the lane, so it must never be accounted as a lane
// failure. Measured twice before this file existed:
//
//   - 2026-09-16 (SCHED-GAP-134): a gateway restart left 503 drain replies
//     for ~15h; the ~700 refused rows fed the 100-tick auto-disable windows
//     and mass-disabled six top foreman lanes (hermes-dagger, hermes-canopy,
//     crier, bunker, chimera-v2, heading) at exactly 90/100 — 100% of the
//     counted failures gateway-side.
//   - 2026-09-17: ALL 45 failed ticks in the day were drain 503s from two
//     gateway restarts (01:06, 04:08) — zero lane-caused failures.
//
// TRANSPORT-CLASS MARKER (acceptance 1b): the codebase had no marker FIELD on
// ticks — the class existed only as harnessFailure() (alert_escalation.go,
// SCHED-GAP-134) re-deriving it from ticks.error on every read. This task adds
// the canonical field: ticks.failure_reason (migration v33), stamped by
// lifecycle.Complete with failureReasonClass(): "" = not transport-class
// (project-side, and every legacy row) | "gateway_drain" | "gateway_transport".
// The drain refusal gets the specific token "gateway_drain" so the class is
// checkable in SQL (LIKE '%gateway_drain%') instead of by prose parsing. These
// tests assert BOTH layers agree: the SQL marker AND harnessFailure().
//
// HARNESS: the shared gateway stub (newResumeGateway, schedgap091_resume_test.go)
// is extended with setGatewayDraining(true): the gateway stays ALIVE on /health
// (200) but refuses /v1/responses with the real 503 drain body. /health staying
// healthy is not a shortcut — it is the measured production shape: evaluate()'s
// liveness ping would otherwise pause every spawn, and the 09-17 fleet would
// have had no refused tick rows at all. Every test drives the real path
// (Loop.evaluate → SlotPool → Spawner → gateway → lifecycle.Complete) and
// asserts on DB rows, never on log text.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gap143LaneFailureText is a project-side (lane-caused) failure. It is the
// negative control for every transport-class assertion: this text must still
// count against the lane (failure counter + auto-disable breaker).
const gap143LaneFailureText = "tick timeout after 7200s: no board progress"

// gap143SetEligible arms a project for the next evaluate() pass: a small
// cooldown and an attempt clock 1h in the past. It deliberately does NOT touch
// consecutive_failures — tests 1/3/4 assert on that counter across ticks.
func gap143SetEligible(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	gap143SetLastCompleted(t, db, name, time.Hour)
	if _, err := db.Exec(`UPDATE projects SET cooldown_s = 60 WHERE name = ?`, name); err != nil {
		t.Fatalf("set cooldown_s for %s: %v", name, err)
	}
}

// gap143SetLastCompleted rewrites the attempt clock (the cooldown baseline)
// to `age` in the past. Used between ticks: every terminal tick re-stamps
// last_tick_completed, so the next evaluate() is otherwise inside cooldown
// (long ticks with small cooldowns: cooldown_s=60, tick≈3.5s → eligible
// immediately; escalations to 7200s need a matching age).
func gap143SetLastCompleted(t *testing.T, db *sql.DB, name string, age time.Duration) {
	t.Helper()
	past := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`, past, name); err != nil {
		t.Fatalf("set last_tick_completed for %s: %v", name, err)
	}
}

// gap143RunEvalTick runs one evaluation cycle and waits for the tick it
// spawned to reach a terminal state, returning its id. want is the number of
// terminal rows expected for the project AFTER this cycle.
func gap143RunEvalTick(t *testing.T, l *Loop, db *sql.DB, project string, want int) string {
	t.Helper()
	l.evaluate()
	return gap143AwaitTerminalTicks(t, db, project, want, 30*time.Second)[want-1]
}

// gap143AwaitTerminalTicks polls until the project has `want` terminal tick
// rows (failed/timeout/completed) and returns their ids oldest-first.
func gap143AwaitTerminalTicks(t *testing.T, db *sql.DB, project string, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ids := gap143TerminalTickIDs(t, db, project)
		if len(ids) >= want {
			return ids
		}
		if time.Now().After(deadline) {
			t.Fatalf("project %s reached %d terminal ticks, want %d within %v", project, len(ids), want, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// gap143TerminalTickIDs lists terminal tick ids for a project, oldest-first
// (spawned_at is RFC3339 UTC, so string ordering is chronological).
func gap143TerminalTickIDs(t *testing.T, db *sql.DB, project string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT id FROM ticks WHERE project_name = ? AND status IN ('completed','failed','timeout')
		 ORDER BY spawned_at`, project)
	if err != nil {
		t.Fatalf("query terminal ticks for %s: %v", project, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan tick id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter terminal ticks: %v", err)
	}
	return ids
}

// gap143TickRow reads the terminal row fields this battery asserts on:
// status, error text, and the SCHED-GAP-143 transport-class marker.
func gap143TickRow(t *testing.T, db *sql.DB, tickID string) (status, errText, failureReason string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT status, COALESCE(error, ''), COALESCE(failure_reason, '') FROM ticks WHERE id = ?`,
		tickID).Scan(&status, &errText, &failureReason); err != nil {
		t.Fatalf("query tick row %s: %v", tickID, err)
	}
	return status, errText, failureReason
}

// gap143ConsecutiveFailures mirrors the external-package helper of the same
// purpose (getConsecutiveFailures in schedgap137a_completion_reset_test.go) —
// this file lives in package scheduler so it can drive Loop internals.
func gap143ConsecutiveFailures(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var cf int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, name).Scan(&cf); err != nil {
		t.Fatalf("query consecutive_failures for %s: %v", name, err)
	}
	return cf
}

// gap143SetConsecutiveFailures pre-arms the failure counter.
func gap143SetConsecutiveFailures(t *testing.T, db *sql.DB, name string, n int) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET consecutive_failures = ? WHERE name = ?`, n, name); err != nil {
		t.Fatalf("set consecutive_failures=%d for %s: %v", n, name, err)
	}
}

// gap143ProjectEnabled reads the auto-disable outcome the breaker writes.
func gap143ProjectEnabled(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&enabled); err != nil {
		t.Fatalf("query enabled for %s: %v", name, err)
	}
	return enabled == 1
}

// gap143Cooldown reads the live cooldown + no-progress streak (the adaptive
// cooldown state the drain window must not distort).
func gap143Cooldown(t *testing.T, db *sql.DB, name string) (cooldownS, streak int) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT cooldown_s, no_progress_ticks FROM projects WHERE name = ?`, name).
		Scan(&cooldownS, &streak); err != nil {
		t.Fatalf("query cooldown state for %s: %v", name, err)
	}
	return cooldownS, streak
}

// gap143PollCooldown re-reads the adaptive-cooldown row every 20ms until it
// matches (wantCooldownS, wantStreak) or the 2s deadline expires, returning
// the LAST OBSERVED values either way.
//
// The adaptive-cooldown persist lands AFTER the ticks row reaches its
// terminal state (the same completion path writes both), so a synchronous
// read races the persist and can observe the pre-recovery value on a loaded
// host — FND-002 (load-only flake surface; 15eec5c CI-004 race-detector
// run), the same class as FND-001's async slot release. Bounded: a real
// regression never passes — the caller still fails, naming the last-observed
// numbers instead of "timeout".
func gap143PollCooldown(t *testing.T, db *sql.DB, name string, wantCooldownS, wantStreak int) (lastCooldownS, lastStreak int) {
	t.Helper()
	const wait = 2 * time.Second
	deadline := time.Now().Add(wait)
	for {
		lastCooldownS, lastStreak = gap143Cooldown(t, db, name)
		if lastCooldownS == wantCooldownS && lastStreak == wantStreak {
			return lastCooldownS, lastStreak
		}
		if time.Now().After(deadline) {
			return lastCooldownS, lastStreak
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gap143InsertFailedTick records a terminal failed tick row directly. Used to
// fill an auto-disable window without paying the gateway retry backoff
// (1 + gatewayRetryMaxAttempts POSTs, ≈3.5s) per row.
func gap143InsertFailedTick(t *testing.T, db *sql.DB, tickID, project, errText, failureReason string, at time.Time) {
	t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, outcome, error, failure_reason, spawned_at, completed_at, created_at)
		 VALUES (?, ?, 'failed', 'failed', ?, ?, ?, ?, ?)`,
		tickID, project, errText, failureReason, ts, ts, ts); err != nil {
		t.Fatalf("insert failed tick %s: %v", tickID, err)
	}
}

// gap143HighEventCount counts HIGH events for a project on a component.
func gap143HighEventCount(t *testing.T, db *sql.DB, component, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE severity = ? AND component = ? AND json_extract(details,'$.project') = ?`,
		string(SeverityHigh), component, project).Scan(&n); err != nil {
		t.Fatalf("count HIGH %s events for %s: %v", component, project, err)
	}
	return n
}

// gap143DrainLoop builds the loop every test drives: the real spawner wired to
// the shared gateway stub with exec fallback disabled (so a refused tick can
// never be rescued by a local process and silently pass).
func gap143DrainLoop(t *testing.T, db *sql.DB, gw *resumeGateway) *Loop {
	t.Helper()
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.noDeliver = true
	gw.wire(l)
	return l
}

// TestSCHEDGAP143_DrainTickRecordedAsTransportClass — acceptance 1.
//
// One real drain-killed tick, driven end to end (evaluate → slot pool →
// gateway 503 → lifecycle.Complete), must be:
//
//	(a) recorded as a tick row for the project,
//	(b) marked TRANSPORT-CLASS — ticks.failure_reason = "gateway_drain"
//	    (migration v33) AND the shipped classifier harnessFailure() agreeing,
//	(c) accounted without touching projects.consecutive_failures, so a
//	    gateway restart can never look like a lane that keeps failing.
//
// Pre-fix behaviour caught: the row still lands (a) but failure_reason is
// absent (b) and consecutive_failures becomes 1 (c) — the bump that fed the
// 2026-09-16 mass-disable.
func TestSCHEDGAP143_DrainTickRecordedAsTransportClass(t *testing.T) {
	db := newTestDB(t)
	const project = "gap143-drain-class"
	mustCreateProjectINFRA012(t, db, project)
	gap143SetEligible(t, db, project)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)
	gw.setGatewayDraining(true)

	tickID := gap143RunEvalTick(t, l, db, project, 1)

	// The refusal really was the path taken: no spawn was accepted.
	if got := gw.spawns.Load(); got != 0 {
		t.Fatalf("gateway accepted %d spawns while draining — the refusal path was not exercised", got)
	}

	status, errText, failureReason := gap143TickRow(t, db, tickID)
	if status != string(TickFailed) {
		t.Errorf("(a) tick %s status = %q, want failed", tickID, status)
	}
	if !strings.Contains(errText, "503") || !strings.Contains(strings.ToLower(errText), "draining") {
		t.Errorf("(a) tick %s error = %q, want the real 503 drain refusal text", tickID, errText)
	}

	// (b) transport-class, at both layers.
	if failureReason != FailureReasonGatewayDrain {
		t.Errorf("(b) ticks.failure_reason = %q, want %q (transport-class marker, migration v33)",
			failureReason, FailureReasonGatewayDrain)
	}
	if !harnessFailure(errText) {
		t.Errorf("(b) harnessFailure(%q) = false, want true — the canonical SCHED-GAP-134 classifier must agree with the marker", errText)
	}

	// (c) a refused tick is not a lane failure.
	if cf := gap143ConsecutiveFailures(t, db, project); cf != 0 {
		t.Errorf("(c) consecutive_failures = %d after a drain-killed tick, want 0 (transport-class must not bump the lane counter)", cf)
	}
}

// TestSCHEDGAP143_AutoDisableDoesNotFireOnDrain — acceptance 2.
//
// A lane that accumulates 10 drain-killed ticks in the auto-disable window
// (rate 1.0 ≥ the 0.5 threshold, sample ≥ min_ticks) must stay ENABLED: none
// of those ticks reached it. The same pass must still disable a lane whose 10
// failures are project-side — the harness exclusion may never blunt the
// breaker (that is the positive control, and without it this test could pass
// vacuously on a min_ticks that skips every sample).
//
// Two of the ten rows are produced END TO END by the real drain path (so the
// fixture carries production's own text); the remaining eight reuse that exact
// captured text, because each real drain tick costs 1 + gatewayRetryMaxAttempts
// POSTs with backoff.
func TestSCHEDGAP143_AutoDisableDoesNotFireOnDrain(t *testing.T) {
	db := newTestDB(t)
	const (
		drainProj = "gap143-autodisable-drain"
		laneProj  = "gap143-autodisable-lane"
	)
	mustCreateProjectINFRA012(t, db, drainProj)
	mustCreateProjectINFRA012(t, db, laneProj)
	gap143SetEligible(t, db, drainProj)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)
	gw.setGatewayDraining(true)

	// Two real drain-killed ticks.
	gap143RunEvalTick(t, l, db, drainProj, 1)
	gap143SetEligible(t, db, drainProj)
	tickID := gap143RunEvalTick(t, l, db, drainProj, 2)

	_, realText, realReason := gap143TickRow(t, db, tickID)
	if realReason != FailureReasonGatewayDrain {
		t.Fatalf("real drain tick %s failure_reason = %q, want %q — fixture text would be unrepresentative",
			tickID, realReason, FailureReasonGatewayDrain)
	}

	// AC: pre-arm the failure counter (the breaker must not care).
	gap143SetConsecutiveFailures(t, db, drainProj, 10)

	// Eight more drain rows (identical text) → 10 drain-killed in the window.
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 8; i++ {
		gap143InsertFailedTick(t, db, "gap143-synth-drain-"+string(rune('a'+i)), drainProj,
			realText, FailureReasonGatewayDrain, base.Add(time.Duration(i)*time.Minute))
	}
	// Positive control: a lane failing on its own terms.
	gap143SetConsecutiveFailures(t, db, laneProj, 10)
	for i := 0; i < 10; i++ {
		gap143InsertFailedTick(t, db, "gap143-synth-lane-"+string(rune('a'+i)), laneProj,
			gap143LaneFailureText, "", base.Add(time.Duration(i)*time.Minute))
	}

	// Drive the evaluator exactly as the eval cycle does (tick_process.go):
	// policy from the loop, escalator constructed per pass.
	l.SetAutoDisablePolicy(0.5, 100, 5)
	l.mu.RLock()
	policy := l.autoDisablePolicy
	l.mu.RUnlock()
	esc := NewAlertEscalator(db, l.events, policy)
	if err := esc.CheckFailureRateAutoDisable(context.Background()); err != nil {
		t.Fatalf("CheckFailureRateAutoDisable: %v", err)
	}

	if !gap143ProjectEnabled(t, db, drainProj) {
		t.Errorf("%s was auto-disabled on 10 drain-killed ticks — a gateway restart must never disable a lane", drainProj)
	}
	if n := gap143HighEventCount(t, db, "auto-disable", drainProj); n != 0 {
		t.Errorf("auto-disable HIGH events for %s = %d, want 0", drainProj, n)
	}
	// Positive control — same window, same threshold, lane-side failures.
	if gap143ProjectEnabled(t, db, laneProj) {
		t.Errorf("%s stayed enabled on 10 project-side failures — the harness exclusion must not blunt the breaker", laneProj)
	}
	if n := gap143HighEventCount(t, db, "auto-disable", laneProj); n != 1 {
		t.Errorf("auto-disable HIGH events for %s = %d, want 1", laneProj, n)
	}
}

// TestSCHEDGAP143_SuccessfulTickResetsConsecutiveFailures — acceptance 3.
//
// Pins the SCHED-GAP-137a reset (lifecycle.Complete clears consecutive_failures
// on TickCompleted) ACROSS a drain window, not just after a clean run: the
// counter must survive the refused ticks (they neither bump nor clear it —
// being refused says nothing about the lane) and then be cleared by the next
// successful tick.
func TestSCHEDGAP143_SuccessfulTickResetsConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	const project = "gap143-reset-after-drain"
	mustCreateProjectINFRA012(t, db, project)
	gap143SetEligible(t, db, project)
	gap143SetConsecutiveFailures(t, db, project, 5)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)

	// Drain window: the refusal must leave the pre-existing 5 alone.
	gw.setGatewayDraining(true)
	drainTick := gap143RunEvalTick(t, l, db, project, 1)
	if status, _, reason := gap143TickRow(t, db, drainTick); status != string(TickFailed) || reason != FailureReasonGatewayDrain {
		t.Fatalf("drain tick %s = (%s, %s), want (failed, %s)", drainTick, status, reason, FailureReasonGatewayDrain)
	}
	if cf := gap143ConsecutiveFailures(t, db, project); cf != 5 {
		t.Errorf("consecutive_failures = %d after the drain tick, want 5 (untouched: neither bumped nor cleared)", cf)
	}

	// Drain over: one successful tick end to end.
	gw.setGatewayDraining(false)
	gap143SetEligible(t, db, project)
	okTick := gap143RunEvalTick(t, l, db, project, 2)
	if status, _, _ := gap143TickRow(t, db, okTick); status != string(TickCompleted) {
		t.Fatalf("recovery tick %s status = %s, want completed", okTick, status)
	}
	if got := gw.spawns.Load(); got != 1 {
		t.Errorf("gateway accepted %d spawns, want 1 (the recovery tick)", got)
	}
	if cf := gap143ConsecutiveFailures(t, db, project); cf != 0 {
		t.Errorf("consecutive_failures = %d after a successful tick following the drain window, want 0 (SCHED-GAP-137a reset)", cf)
	}
}

// TestSCHEDGAP143_DrainWindowDoesNotParkEscalatedCooldown — acceptance 4.
//
// The drain-then-recover safety net: a lane already parked at its escalated
// cooldown (floor 900 → ceiling 7200, streak at the threshold) must not be
// pushed further by refusals, and the first tick that lands real work must
// snap it back to the floor — never leave it parked at the ceiling because the
// drain window ate the recovery signal.
func TestSCHEDGAP143_DrainWindowDoesNotParkEscalatedCooldown(t *testing.T) {
	db := newTestDB(t)
	const project = "gap143-cooldown-recover"

	// A board with one OPEN row, observed as two before this tick: the lane
	// closed net work → adaptive cooldown's documented speed-up signal.
	// (Commits cannot be used here — the gateway stub does no repo work.)
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"),
		[]byte("{\"id\":\"GAP143-ROW-1\",\"status\":\"pending\",\"title\":\"open work\"}\n"), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}

	mustCreateProjectINFRA012(t, db, project)
	if _, err := db.Exec(
		`UPDATE projects SET workdir = ?, adaptive_cooldown = 1, cooldown_floor_s = 900,
		     cooldown_ceiling_s = 7200, no_progress_threshold = 10, no_progress_ticks = 10,
		     cooldown_s = 7200, board_open_seen = 2, board_rows_seen = 2, admission_mode = 'cooldown'
		 WHERE name = ?`, wd, project); err != nil {
		t.Fatalf("arm adaptive cooldown for %s: %v", project, err)
	}
	// cooldown_s is 7200 (parked at the ceiling) → the attempt clock must be
	// older than that for the next evaluate() to admit the lane at all.
	gap143SetLastCompleted(t, db, project, 3*time.Hour)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)

	// Drain window: the refusal must not escalate, and must not consume the
	// recovery signal either (spawn-failure paths never reach adaptiveCooldown).
	gw.setGatewayDraining(true)
	drainTick := gap143RunEvalTick(t, l, db, project, 1)
	if status, _, reason := gap143TickRow(t, db, drainTick); status != string(TickFailed) || reason != FailureReasonGatewayDrain {
		t.Fatalf("drain tick %s = (%s, %s), want (failed, %s)", drainTick, status, reason, FailureReasonGatewayDrain)
	}
	if cd, streak := gap143Cooldown(t, db, project); cd != 7200 || streak != 10 {
		t.Fatalf("after the drain tick cooldown_s = %d / streak = %d, want 7200 / 10 (a refusal must touch neither)", cd, streak)
	}

	// Recovery: a successful tick that closed net board work → floor restore.
	gw.setGatewayDraining(false)
	gap143SetLastCompleted(t, db, project, 3*time.Hour)
	okTick := gap143RunEvalTick(t, l, db, project, 2)
	if status, _, _ := gap143TickRow(t, db, okTick); status != string(TickCompleted) {
		t.Fatalf("recovery tick %s status = %s, want completed", okTick, status)
	}
	if _, _, reason := gap143TickRow(t, db, okTick); reason != "" {
		t.Errorf("recovery tick %s failure_reason = %q, want empty (a successful tick is not transport-class)", okTick, reason)
	}
	// The adaptive-cooldown persist is async relative to the ticks row's
	// terminal state — poll until it lands instead of racing it (FND-002,
	// load-only flake). Bounded: on deadline expiry the LAST observed values
	// are asserted, so a real regression still names the bad number.
	cd, streak := gap143PollCooldown(t, db, project, 900, 0)
	if cd != 900 {
		t.Errorf("cooldown_s = %d after the recovery tick (last observed), want 900 (floor) — the drain window must not park the lane at the ceiling %d", cd, 7200)
	}
	if streak != 0 {
		t.Errorf("no_progress_ticks = %d after the recovery tick (last observed), want 0 (streak reset)", streak)
	}
}

// TestFND002_DrainCooldownReadIsBounded pins the load-bearing property of the
// FND-002 conversion directly: after a drain-then-recover sequence, the
// bounded poll observes the adaptive cooldown restored to the floor (900) and
// the no-progress streak reset (0). Same fixture shape as the acceptance test
// above (parked lane at ceiling 7200 / streak 10, drain refusal, recovery
// tick), but asserts ONLY on the poll outcome.
func TestFND002_DrainCooldownReadIsBounded(t *testing.T) {
	db := newTestDB(t)
	const project = "gap143-fnd002-bounded"

	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"),
		[]byte("{\"id\":\"GAP143-ROW-1\",\"status\":\"pending\",\"title\":\"open work\"}\n"), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}

	mustCreateProjectINFRA012(t, db, project)
	if _, err := db.Exec(
		`UPDATE projects SET workdir = ?, adaptive_cooldown = 1, cooldown_floor_s = 900,
		     cooldown_ceiling_s = 7200, no_progress_threshold = 10, no_progress_ticks = 10,
		     cooldown_s = 7200, board_open_seen = 2, board_rows_seen = 2, admission_mode = 'cooldown'
		 WHERE name = ?`, wd, project); err != nil {
		t.Fatalf("arm adaptive cooldown for %s: %v", project, err)
	}
	gap143SetLastCompleted(t, db, project, 3*time.Hour)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)

	// Drain window refusal (synchronous, must not move the parked values)…
	gw.setGatewayDraining(true)
	drainTick := gap143RunEvalTick(t, l, db, project, 1)
	if status, _, reason := gap143TickRow(t, db, drainTick); status != string(TickFailed) || reason != FailureReasonGatewayDrain {
		t.Fatalf("drain tick %s = (%s, %s), want (failed, %s)", drainTick, status, reason, FailureReasonGatewayDrain)
	}
	if cd, streak := gap143Cooldown(t, db, project); cd != 7200 || streak != 10 {
		t.Fatalf("after the drain tick cooldown_s = %d / streak = %d, want 7200 / 10", cd, streak)
	}

	// …then recovery: the bounded poll must observe floor 900 / streak 0.
	gw.setGatewayDraining(false)
	gap143SetLastCompleted(t, db, project, 3*time.Hour)
	okTick := gap143RunEvalTick(t, l, db, project, 2)
	if status, _, _ := gap143TickRow(t, db, okTick); status != string(TickCompleted) {
		t.Fatalf("recovery tick %s status = %s, want completed", okTick, status)
	}
	cd, streak := gap143PollCooldown(t, db, project, 900, 0)
	if cd != 900 || streak != 0 {
		t.Errorf("after the recovery tick the bounded poll observed cooldown_s = %d / no_progress_ticks = %d, want 900 / 0", cd, streak)
	}
}
