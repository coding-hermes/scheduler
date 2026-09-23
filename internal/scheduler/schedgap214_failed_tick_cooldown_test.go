package scheduler

// SCHED-GAP-214 — a FAILED tick consumes its cooldown.
//
// THE MEASURED DEFECT. Project crier, 2026-09-16 hour 18:00-19:00 local: 91
// DISTINCT tick ids, 91/91 failed, spaced ~9 SECONDS apart, every one ending
// "gateway unreachable and exec fallback disabled ... invalid_request_error:
// Gateway is draining" (scheduler.db, verified 2026-09-22). Fleet-wide the
// same string accounts for 827 failed ticks in 7 days across 9 lanes. The
// post-failure spawn gap measured 5s-9s against a 21600s cooldown pin.
//
// THE MECHANISM (every step file:line-cited):
//  1. All nine storm lanes are TASKS-mode (ns.admission_mode='tasks' in the
//     live scheduler.db: crier, bunker, heading, chimera-v2, hermes-canopy,
//     9router, off-by-one, gitreins, hermes-dagger).
//  2. The SCHED-GAP-124 tasks waiver (packer.go / packer_select.go /
//     multipool_packer.go tasksAdmissionDue branches) ignores the cooldown
//     pin whenever open board work exists — the ONLY spacing was the 5s eval
//     debounce (loop.go Run slot-freed coalescing).
//  3. SCHED-GAP-143 deliberately leaves consecutive_failures at 0 for a
//     transport-class failure, so the SCHED-GAP-133 FailureBackoff gate
//     (which keys on consecutive_failures > 1) never fired either.
//  4. The gateway-health gate (SCHED-GAP-170) was NOT ARMED during the storm
//     (scheduler.log: the first "GATEWAY-HEALTH-GATE: armed" line is
//     2026/09/19 09:06:02) — the defer-not-drop admission gate that now
//     prevents most of this class did not exist yet.
//
// THE FIX: projects.last_tick_status (migration v38), stamped by
// lifecycle.Complete for every terminal outcome. The tasks waiver consults
// it (lastTickStatusFailed, admission_mode.go): after a FAILED tick the
// waiver stands down and the lane paces on its FULL effective cooldown —
// the same arithmetic cooldown mode applies (effectiveCooldown, packer.go).
// A 21600s pin therefore spaces two failed-tick attempts by 21600s: within
// a drain window a lane attempts at most once per cooldown.
//
// Backward-compat constraint from the row: a COMPLETED last tick keeps the
// waiver fully intact (only the FAILED branch changes); a legacy pre-v38
// row reads "" and behaves as before.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// gap214BoardWithWork creates the board file tasksAdmissionDue needs: the
// lane owns its board and holds open work (the waiver's precondition).
func gap214BoardWithWork(t *testing.T) string {
	t.Helper()
	return admitWorkdirWithBoard(t,
		`{"id": "GAP214-1", "status": "pending", "title": "storm work"}`,
	)
}

// gap214StampTerminal writes the project state a terminal tick leaves
// behind: last_tick_completed + last_tick_status (mirrors the UPDATE
// lifecycle.Complete performs; the end-to-end tests below also exercise
// that real write path).
func gap214StampTerminal(t *testing.T, db *sql.DB, name string, status string, completedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE projects SET last_tick_completed = ?, last_tick_status = ? WHERE name = ?`,
		completedAt.UTC().Format(time.RFC3339), status, name); err != nil {
		t.Fatalf("stamp terminal state for %s: %v", name, err)
	}
}

// gap214InsertLane seeds one enabled tasks-mode project in the given
// namespace with the given cooldown, board workdir, and last-tick state.
// When lastStatus != "" it also inserts the terminal tick row itself —
// the packers' lastCompleted map keys off ticks.completed_at (evalContext),
// so a lane with a last tick in the live DB always has one.
func gap214InsertLane(t *testing.T, db *sql.DB, name, nsID, workdir string, cooldownS int, lastStatus string, lastCompleted time.Time) {
	t.Helper()
	admitInsertProject(t, db, admitProjectSpec{
		Name:      name,
		NS:        nsID,
		CooldownS: cooldownS,
		Workdir:   workdir,
	})
	if lastStatus != "" {
		gap214StampTerminal(t, db, name, lastStatus, lastCompleted)
		ts := lastCompleted.UTC().Format(time.RFC3339)
		// outcome uses the ticks CHECK vocabulary (completed → committed).
		outcome := "dry_run"
		switch lastStatus {
		case database.LastStatusCompleted:
			outcome = "committed"
		case database.LastStatusFailed:
			outcome = "failed"
		case database.LastStatusTimeout:
			outcome = "timeout"
		case database.LastStatusDeferred:
			outcome = "deferred"
		}
		if _, err := db.Exec(
			`INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			name+"-prev", name, lastStatus, outcome, ts, ts, ts); err != nil {
			t.Fatalf("insert terminal tick row for %s: %v", name, err)
		}
	}
}

// gap214CountTicks counts the sim tick rows a project accumulated.
func gap214CountTicks(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND id LIKE 'sim-%'`, project).
		Scan(&n); err != nil {
		t.Fatalf("count ticks for %s: %v", project, err)
	}
	return n
}

// gap214NewTasksLoop builds a simulation-mode loop over a tasks-mode
// namespace at the pinned clock (the admission_decision_test harness —
// the selection decision is deterministic at one constructed instant).
// The namespace row is created HERE, before any lane that references it
// (projects.namespace_id is a live FK in the test schema).
func gap214NewTasksLoop(t *testing.T, db *sql.DB, nsID string, now time.Time) *Loop {
	t.Helper()
	admitInsertNamespace(t, db, nsID, 0, "tasks")
	// namespaceMode=true routes evaluate() through the multi-pool packer —
	// the path the live fleet uses (all nine storm lanes are namespaced).
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4, true)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(now))
	return l
}

// TestSchedGap214_FailedTickConsumesCooldown — acceptance 1 + 2 + 4.
//
// A tasks-mode lane with open board work whose last tick FAILED does NOT
// re-admit on the next eval (the pre-fix behavior produced the measured
// 9-second cadence): it waits its full effective cooldown. One eval pass
// at T+9s (the storm cadence) must select NOTHING; a pass at T+cooldown+1s
// must select the lane again. This is the synthetic gateway outage: one
// failure injection, at most one spawn per cooldown.
func TestSchedGap214_FailedTickConsumesCooldown(t *testing.T) {
	const (
		project = "gap214-storm-lane"
		nsID    = "gap214-ns"
	)
	// The live storm lanes carry a 21600s (6h) pin; the storm cadence
	// measured 9s. Use the real pin to prove the 6h cooldown is honoured.
	const cooldownS = 21600
	// A lane whose last tick (a synthetic gateway-outage failure) landed
	// 9 seconds before the pinned evaluation instant — the measured storm
	// spacing. Pre-fix this pass re-spawns the lane (the defect); post-fix
	// the waiver is stood down.
	stormCadence := 9 * time.Second
	now := fixedEvalNow()
	lastFailed := now.Add(-stormCadence)

	db := newTestDB(t)
	l := gap214NewTasksLoop(t, db, nsID, now) // namespace row exists first (FK)
	wd := gap214BoardWithWork(t)
	gap214InsertLane(t, db, project, nsID, wd, cooldownS, database.LastStatusFailed, lastFailed)

	// Pass 1 at T+9s (the storm cadence): the failed tick consumed its
	// cooldown — the lane must NOT be selected again.
	l.evaluate()
	if got := gap214CountTicks(t, db, project); got != 0 {
		t.Fatalf("post-failure pass at T+%v selected %d tick(s) for %s — the failed tick did not consume its cooldown (SCHED-GAP-214: pre-fix this is the 91-ticks-in-90s storm)",
			stormCadence, got, project)
	}

	// Pass 2 at T+cooldown+1s: the cooldown has elapsed and the lane is
	// admitted again — the pin delays, it never abandons. A fresh Loop on
	// the SAME db (namespace row already exists — the ns insert is
	// idempotent-guarded by reusing the loop's db).
	l2 := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4, true)
	l2.SetSimulation(1.0)
	l2.SetClock(clock.NewFixed(now.Add(time.Duration(cooldownS)*time.Second + time.Second)))
	l2.evaluate()
	if got := gap214CountTicks(t, db, project); got != 1 {
		t.Fatalf("post-cooldown pass selected %d tick(s) for %s, want 1 — the cooldown must delay, never abandon the lane",
			got, project)
	}
}

// TestSchedGap214_CompletedTickKeepsWaiver — backward-compat constraint.
//
// A lane whose last tick COMPLETED keeps the SCHED-GAP-124 waiver fully
// intact: pending board work still waives the cooldown pin (only the FAILED
// branch changes). The same T+9s pass that the failed lane fails to get
// MUST admit the completed lane.
func TestSchedGap214_CompletedTickKeepsWaiver(t *testing.T) {
	const (
		project = "gap214-healthy-lane"
		nsID    = "gap214-ns2"
	)
	const cooldownS = 21600
	now := fixedEvalNow()
	lastCompleted := now.Add(-9 * time.Second) // same 9s-young state

	db := newTestDB(t)
	l := gap214NewTasksLoop(t, db, nsID, now) // namespace row exists first (FK)
	wd := gap214BoardWithWork(t)
	gap214InsertLane(t, db, project, nsID, wd, cooldownS, database.LastStatusCompleted, lastCompleted)

	l.evaluate()
	if got := gap214CountTicks(t, db, project); got != 1 {
		t.Fatalf("completed-lane pass selected %d tick(s), want 1 — the SCHED-GAP-124 waiver must stay intact for a completed last tick (SCHED-GAP-214 changes ONLY the failed branch)",
			got)
	}
}

// TestSchedGap214_TimeoutAndLegacyKeepWaiver pins the remaining statuses:
// a TIMEOUT last tick (own "no timeout backoff" contract) and a legacy ""
// row (pre-v38) both keep the pre-214 waiver behavior.
func TestSchedGap214_TimeoutAndLegacyKeepWaiver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"timeout", database.LastStatusTimeout},
		{"legacy-empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				project = "gap214-waiver-keep"
				nsID    = "gap214-ns3"
			)
			now := fixedEvalNow()
			db := newTestDB(t)
			l := gap214NewTasksLoop(t, db, nsID, now) // namespace row exists first (FK)
			wd := gap214BoardWithWork(t)
			status := tc.status
			if status != "" {
				gap214InsertLane(t, db, project, nsID, wd, 21600, status, now.Add(-9*time.Second))
			} else {
				// Legacy row: no last_tick_status, but last_tick_completed
				// is fresh — exactly the pre-v38 on-disk shape.
				admitInsertProject(t, db, admitProjectSpec{
					Name: project, NS: nsID, CooldownS: 21600, Workdir: wd,
					Last: now.Add(-9 * time.Second),
				})
			}

			l.evaluate()
			if got := gap214CountTicks(t, db, project); got != 1 {
				t.Fatalf("%s: pass selected %d tick(s), want 1 — only the FAILED branch changes", tc.name, got)
			}
		})
	}
}

// TestSchedGap214_StampRecordsTerminalStatus — the diagnostic field is
// really written by the LIVE completion path: a drain-killed tick driven
// end to end (evaluate → slot pool → gateway 503 → lifecycle.Complete)
// leaves projects.last_tick_status='failed', and a completed one leaves
// 'completed'. This is the stamp the admission gate reads.
func TestSchedGap214_StampRecordsTerminalStatus(t *testing.T) {
	db := newTestDB(t)
	const project = "gap214-stamp-drain"
	mustCreateProjectINFRA012(t, db, project)
	gap143SetEligible(t, db, project)

	gw := newResumeGateway(t)
	l := gap143DrainLoop(t, db, gw)
	gw.setGatewayDraining(true)

	tickID := gap143RunEvalTick(t, l, db, project, 1)
	_ = tickID

	var status string
	if err := db.QueryRow(`SELECT last_tick_status FROM projects WHERE name = ?`, project).Scan(&status); err != nil {
		t.Fatalf("read last_tick_status: %v", err)
	}
	if status != database.LastStatusFailed {
		t.Errorf("last_tick_status = %q after a drain-killed tick, want %q", status, database.LastStatusFailed)
	}

	// And the recovery: a completed tick re-stamps the field.
	gw.setGatewayDraining(false)
	gap143SetEligible(t, db, project)
	if _, err := db.Exec(`UPDATE projects SET last_tick_status = '' WHERE name = ?`, project); err != nil {
		t.Fatalf("reset status: %v", err)
	}
	gap143RunEvalTick(t, l, db, project, 2)
	if err := db.QueryRow(`SELECT last_tick_status FROM projects WHERE name = ?`, project).Scan(&status); err != nil {
		t.Fatalf("read last_tick_status post-success: %v", err)
	}
	if status != database.LastStatusCompleted {
		t.Errorf("last_tick_status = %q after a successful tick, want %q", status, database.LastStatusCompleted)
	}
}

// TestSchedGap214_DrainStormAtMostOneSpawnPerCooldown — acceptance 4, the
// synthetic gateway outage driven END TO END through the real admission
// path (evaluate → namespace packer → waiver gate). Two lanes, both
// storm-shaped (tasks mode, open work, 6h pin, failed last tick 9s old),
// both must be rejected on the storm-cadence pass; a healthy control lane
// on the same pass must still be admitted (the gate is per-lane, never a
// fleet-wide pause).
func TestSchedGap214_DrainStormAtMostOneSpawnPerCooldown(t *testing.T) {
	const nsID = "gap214-ns4"
	now := fixedEvalNow()

	db := newTestDB(t)
	l := gap214NewTasksLoop(t, db, nsID, now) // namespace row exists first (FK)
	wd1 := gap214BoardWithWork(t)
	wd2 := gap214BoardWithWork(t)
	wdOK := gap214BoardWithWork(t)
	gap214InsertLane(t, db, "gap214-storm-1", nsID, wd1, 21600, database.LastStatusFailed, now.Add(-9*time.Second))
	gap214InsertLane(t, db, "gap214-storm-2", nsID, wd2, 21600, database.LastStatusFailed, now.Add(-9*time.Second))
	gap214InsertLane(t, db, "gap214-healthy", nsID, wdOK, 21600, database.LastStatusCompleted, now.Add(-9*time.Second))

	l.evaluate()

	if got := gap214CountTicks(t, db, "gap214-storm-1"); got != 0 {
		t.Errorf("storm lane 1 spawned %d tick(s) at the 9s cadence, want 0", got)
	}
	if got := gap214CountTicks(t, db, "gap214-storm-2"); got != 0 {
		t.Errorf("storm lane 2 spawned %d tick(s) at the 9s cadence, want 0", got)
	}
	if got := gap214CountTicks(t, db, "gap214-healthy"); got != 1 {
		t.Errorf("healthy control lane spawned %d tick(s), want 1 — the post-failure gate must never pause a healthy lane", got)
	}
}

// TestSchedGap214_ADMITReasonFailedCooldown — the diagnostics contract
// (design constraint 3): a deferred storm-cadence candidate reports
// reason=failed_cooldown with a cooldown_remaining_s countdown, and the
// admission counter carries it so /api/v1/status admission_counters shows
// the post-failure cooldown consumption.
func TestSchedGap214_ADMITReasonFailedCooldown(t *testing.T) {
	const (
		project = "gap214-admit-lane"
		nsID    = "gap214-ns5"
	)
	now := fixedEvalNow()

	db := newTestDB(t)
	l := gap214NewTasksLoop(t, db, nsID, now) // namespace row exists first (FK)
	wd := gap214BoardWithWork(t)
	gap214InsertLane(t, db, project, nsID, wd, 21600, database.LastStatusFailed, now.Add(-9*time.Second))
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines(project)
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for %s = %d, want exactly 1: %v", project, len(lines), lines)
	}
	line := lines[0]
	admitAssertLineShape(t, line)
	if got := admitField(line, "reason"); got != AdmissionReasonFailedCooldown {
		t.Errorf("reason = %q, want %q (the post-failure stand-down must be a named admission reason): %s",
			got, AdmissionReasonFailedCooldown, line)
	}
	if _, ok := admitFloat(t, line, "cooldown_remaining_s"); !ok {
		t.Errorf("failed_cooldown line missing cooldown_remaining_s: %s", line)
	}

	counters := l.AdmissionCounters()
	if counters[AdmissionReasonFailedCooldown] != 1 {
		t.Errorf("admission_counters[%s] = %d, want 1 — the counter feeds /api/v1/status",
			AdmissionReasonFailedCooldown, counters[AdmissionReasonFailedCooldown])
	}
}

// TestSchedGap214_EligibilityMirror — GAP-050: a project the packer skips
// on the post-failure stand-down is never counted eligible by the
// zero-select mirror.
func TestSchedGap214_EligibilityMirror(t *testing.T) {
	const (
		project = "gap214-mirror-lane"
		nsID    = "gap214-ns6"
	)
	now := fixedEvalNow()

	db := newTestDB(t)
	admitInsertNamespace(t, db, nsID, 0, "tasks") // namespace row exists first (FK)
	wd := gap214BoardWithWork(t)
	gap214InsertLane(t, db, project, nsID, wd, 21600, database.LastStatusFailed, now.Add(-9*time.Second))
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4, true)
	l.SetClock(clock.NewFixed(now))

	if got := l.countEligibleProjects(now, map[string]bool{}); got != 0 {
		t.Errorf("countEligibleProjects = %d, want 0 — the storm lane is deferred by the post-failure cooldown, the mirror must agree (GAP-050)", got)
	}
}
