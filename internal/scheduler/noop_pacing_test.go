package scheduler_test

// NOOP-PACING regression tests (the task-router-foreman spin).
//
// Measured defect (live, 2026-11-06): task-router-foreman runs
// admission_mode='tasks' with cooldown_s = cooldown_pin_s = cooldown_floor_s =
// 21600s and a board holding 78 pending rows. In one calendar day it was
// spawned 76 times; 63 ticks recorded outcome='dry_run' with commits=0 and
// files_changed=0, and 50 of the 75 inter-spawn gaps were under 5 minutes
// (min 2.0 min, mean 13.0 min). projects.no_progress_ticks stayed 0 the whole
// day, so nothing charged the lane: the SCHED-GAP-214 stand-down only fires on
// last_tick_status='failed', and a dry_run completes CLEANLY ('completed').
// The only spacing left was the 60s SCHED-GAP-136 pacing floor, so the lane
// re-admitted on its next eval — forever — because 'tasks' kept finding work
// it never did.
//
// The rule these pin: consecutive no-artifact ticks on a tasks-admitted lane
// space the next admission by the lane's own cooldown_floor_s, doubling per
// consecutive no-op tick up to 8x the floor. The first no-op already costs the
// full floor, so a lane that produced nothing can never be respawned two
// minutes later. Any artifact resets the streak and restores the plain waiver,
// and the pause is bounded, so the lane is paced — never abandoned.
//
// These drive the REAL MultiPoolPacker.Pack (the live primary admission path —
// loop.go's evaluate()). On the pre-fix tree the first test fails: the lane is
// selected 2 minutes after a no-op tick.

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// noopLane is the measured lane's shape: tasks admission, 6h cooldown/pin/floor.
func noopLane(t *testing.T, name string, streak int) database.Project {
	t.Helper()
	wd := t.TempDir()
	writeModeBoard(t, wd,
		`{"id":"REAL-1","status":"pending"}`,
		`{"id":"REAL-2","status":"pending"}`,
		`{"id":"NEVER-DONE","status":"pending","perpetual":true}`,
	)
	p := modeProject(name, "coding-hermes", wd, "")
	p.CooldownS = 21600
	p.CooldownFloorS = 21600
	p.CooldownCeilingS = 172800
	p.NoProgressTicks = streak
	return p
}

// T-NOOP-1: ONE no-op tick, board full of work, last tick 2 minutes ago —
// exactly the measured churn interval. The lane must be DEFERRED for the
// cooldown floor (6h). Before the fix it is selected.
func TestTasksMode_NoopStreakDefersNextSpawn(t *testing.T) {
	p := noopLane(t, "spin-lane", 1)
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"spin-lane": now.Add(-2 * time.Minute)})
	if modeSelected(t, res, "spin-lane") {
		t.Fatalf("T-NOOP-1 FAIL: a tasks lane with 1 consecutive no-op tick and a 21600s floor was re-spawned 2 minutes later — the no-op streak must pace the next admission by cooldown_floor_s")
	}
}

// T-NOOP-2: the pause is a PACE, not a park. Once the floor has elapsed the
// lane admits again on the ordinary tasks waiver — a no-op streak must never
// abandon a lane that still has work.
func TestTasksMode_NoopStreakAdmitsAfterFloorElapses(t *testing.T) {
	p := noopLane(t, "spin-lane-2", 1)
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"spin-lane-2": now.Add(-7 * time.Hour)})
	if !modeSelected(t, res, "spin-lane-2") {
		t.Fatalf("T-NOOP-2 FAIL: the 6h no-op floor elapsed (last tick 7h ago) and the lane still has pending work — it must be admitted, not abandoned")
	}
}

// T-NOOP-3: the streak must not leak onto a productive lane. Same lane, same
// 2-minute gap, streak 0 (the last tick landed a code commit) → admitted by
// the normal tasks waiver, exactly as before the fix.
func TestTasksMode_NoOpStreakZeroKeepsPlainWaiver(t *testing.T) {
	p := noopLane(t, "productive-lane", 0)
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"productive-lane": now.Add(-2 * time.Minute)})
	if !modeSelected(t, res, "productive-lane") {
		t.Fatalf("T-NOOP-3 FAIL: streak=0 (last tick produced work) must keep the plain tasks waiver — pending work still admits immediately")
	}
}

// T-NOOP-4: a growing streak outlasts the first floor. At streak 2 the lane
// owes 12h (2x floor); a 7h-old tick is still inside the pause.
func TestTasksMode_NoopStreakGrowsPastFirstFloor(t *testing.T) {
	p := noopLane(t, "spin-lane-4", 2)
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"spin-lane-4": now.Add(-7 * time.Hour)})
	if modeSelected(t, res, "spin-lane-4") {
		t.Fatalf("T-NOOP-4 FAIL: at streak=2 the pause must be 2x the 21600s floor (12h); a 7h-old tick is still inside it")
	}
}
