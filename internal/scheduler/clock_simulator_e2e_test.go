package scheduler

// SCHED-GAP-169 — the test-time simulator, end to end.
//
// WHAT THIS FILE PROVES: a scheduler scenario that structurally needs LONG
// wall-clock waits (a lane parked at a 2h adaptive-cooldown ceiling, then
// recovering) runs on a virtual clock instead — same production code path
// (Loop.evaluate → SlotPool → Spawner → gateway → lifecycle.Complete), same
// DB assertions, but the two hours of cooldown elapse in a clock Advance
// instead of a real sleep.
//
// The scenario is the REWIRED shape of
// TestSCHEDGAP143_DrainWindowDoesNotParkEscalatedCooldown (kept verbatim in
// schedgap143_drain_class_test.go, unchanged on the wall clock, as the
// real-clock control): a drain refusal must not escalate, and a recovery tick
// must restore the cooldown floor. What is added here is the part that could
// only ever be asserted by waiting before SCHED-GAP-169 existed: after the
// recovery the lane is at the floor again, so a SECOND lane-cooldown expiry
// two hours later is reached by advancing the virtual clock, and the lane is
// admitted again — no real time passes.
//
// ENV: SCHEDULER_TIME_MODE=sim SCHEDULER_TIME_SCALE=1000 makes this test drive
// the env-configured simulator (the acceptance command). With no env it still
// installs its own 1000x simulator, so the test is fast in every run and the
// production default (RealClock) is never what is being measured here.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// schedulerSimClock returns the simulator this test runs on. When the process
// environment asks for sim mode (SCHEDULER_TIME_MODE=sim) the env-configured
// clock is used verbatim — same factory the daemon boots with — otherwise a
// 1000x simulator is installed directly so the test never sleeps for real.
func schedulerSimClock(t *testing.T) *clock.SimClock {
	t.Helper()
	if os.Getenv(clock.EnvMode) == clock.ModeSim {
		c, err := clock.FromEnv()
		if err != nil {
			t.Fatalf("%s=sim but clock.FromEnv() failed: %v", clock.EnvMode, err)
		}
		sim, ok := c.(*clock.SimClock)
		if !ok {
			t.Fatalf("FromEnv() with %s=sim returned %T, want *clock.SimClock", clock.EnvMode, c)
		}
		t.Cleanup(sim.Close)
		return sim
	}
	sim := clock.NewSimClockAt(1000, time.Now())
	t.Cleanup(sim.Close)
	return sim
}

// TestClockSimulator_E2E_DrainWindowAndTwoHourCooldownExpiry is the acceptance
// E2E. Real wall time budget: 5s (asserted).
func TestClockSimulator_E2E_DrainWindowAndTwoHourCooldownExpiry(t *testing.T) {
	realStart := time.Now()

	db := newTestDB(t)
	const project = "gap169-sim-cooldown"

	// Same fixture as the SCHED-GAP-143 control: one open board row (net board
	// work closed by the next tick → the adaptive cooldown's speed-up signal),
	// parked at the 7200s ceiling with a 900s floor.
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"),
		[]byte("{\"id\":\"GAP169-ROW-1\",\"status\":\"pending\",\"title\":\"open work\"}\n"), 0o644); err != nil {
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

	// ── The seam under test: every wait and instant below goes virtual. ──
	sim := schedulerSimClock(t)
	l.SetClock(sim)
	if got := l.clock(); got != clock.Clock(sim) {
		t.Fatalf("Loop.SetClock did not install the simulator: loop clock is %T", got)
	}

	// 1. Drain window — a refused tick must not escalate and must not consume
	//    the recovery signal (the wall-clock control asserts the same rows).
	gw.setGatewayDraining(true)
	drainTick := gap143RunEvalTick(t, l, db, project, 1)
	if status, _, reason := gap143TickRow(t, db, drainTick); status != string(TickFailed) || reason != FailureReasonGatewayDrain {
		t.Fatalf("drain tick %s = (%s, %s), want (failed, %s)", drainTick, status, reason, FailureReasonGatewayDrain)
	}
	if cd, streak := gap143Cooldown(t, db, project); cd != 7200 || streak != 10 {
		t.Fatalf("after the drain tick cooldown_s = %d / streak = %d, want 7200 / 10", cd, streak)
	}

	// 2. Recovery — a successful tick that closed net board work restores the
	//    floor, exactly as on the wall clock.
	gw.setGatewayDraining(false)
	gap143SetLastCompleted(t, db, project, 3*time.Hour)
	okTick := gap143RunEvalTick(t, l, db, project, 2)
	if status, _, _ := gap143TickRow(t, db, okTick); status != string(TickCompleted) {
		t.Fatalf("recovery tick %s status = %s, want completed", okTick, status)
	}
	cd, streak := gap143Cooldown(t, db, project)
	if cd != 900 || streak != 0 {
		t.Fatalf("after the recovery tick cooldown_s = %d / streak = %d, want 900 / 0", cd, streak)
	}

	// 3. THE PART THAT USED TO COST REAL HOURS. The lane now sits at its 900s
	//    floor; make the next admission a full adaptive-cooldown window away
	//    and drive the clock forward instead of sleeping. Before SCHED-GAP-169
	//    this assertion could only be made by waiting 2h (or by hand-editing
	//    last_tick_completed, which tests the decision but never the clock).
	if _, err := db.Exec(`UPDATE projects SET cooldown_s = 7200 WHERE name = ?`, project); err != nil {
		t.Fatalf("park the lane at the ceiling: %v", err)
	}
	virtualBefore := l.clock().Now()
	if _, err := db.Exec(
		`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		virtualBefore.UTC().Format(time.RFC3339), project); err != nil {
		t.Fatalf("stamp last_tick_completed at the virtual instant: %v", err)
	}
	// One second inside the window: still in cooldown → no third tick.
	sim.Advance(7199 * time.Second)
	l.evaluate()
	if ids := gap143TerminalTickIDs(t, db, project); len(ids) != 2 {
		t.Fatalf("2h-minus-1s of virtual cooldown admitted a tick: got %d terminal rows, want 2", len(ids))
	}
	// One second past it: eligible again — admitted on the virtual timeline.
	sim.Advance(2 * time.Second)
	third := gap143RunEvalTick(t, l, db, project, 3)
	if third == "" {
		t.Fatal("the lane was not admitted after the virtual cooldown window elapsed")
	}
	if got := l.clock().Now().Sub(virtualBefore); got != 7201*time.Second {
		t.Fatalf("virtual elapsed = %v, want exactly 7201s (the clock, not the wall, decided)", got)
	}
	if got := l.clock().Since(virtualBefore); got < 2*time.Hour {
		t.Fatalf("virtual age of the cooldown = %v, want >= 2h", got)
	}

	// 4. The budget: this whole 2h+ scenario must cost a fraction of that in
	//    real time — the acceptance bound is 5s.
	if elapsed := time.Since(realStart); elapsed > 5*time.Second {
		t.Fatalf("simulated E2E took %v of REAL time, want < 5s", elapsed)
	}
}

// TestClockSimulator_E2E_RealClockIsTheDefault is the production guard: a Loop
// that was never handed a clock must read the WALL clock, and its waits must
// really wait. Without this, a mis-injection that left a production component
// on a simulator (or vice versa) would be invisible.
func TestClockSimulator_E2E_RealClockIsTheDefault(t *testing.T) {
	l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
	defer l.Stop()

	if _, isReal := l.clock().(clock.RealClock); !isReal {
		t.Fatalf("a freshly built Loop reads %T, want the wall clock (clock.RealClock)", l.clock())
	}
	// A real 30ms wait must actually cost ~30ms: a simulator with the driver on
	// would return far sooner, and a frozen clock would never return at all.
	start := time.Now()
	l.clock().Sleep(30 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("wall-clock Sleep(30ms) took %v — the default seam is not the real clock", elapsed)
	}
}
