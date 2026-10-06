package scheduler

// SCHED-GAP-1660 — RED/GREEN battery for the Bane 2026-09-28 ruling: a board
// write must have NO effect on a cooldown, except the one allowed flip (a
// tasks-admission lane parked on an empty board flips back to admission when
// a non-perpetual row lands), plus the admit_reason coverage requirement.
//
// Test 1 — cooldown lane, 72h pin, 7 simulated days of board writes: zero
//          board_wake admit_reason rows.
// Test 2 — tasks lane, perpetual-only board: parks; one non-perpetual row
//          flips it (wake fires, tick stamped flip:board_empty); a
//          perpetual-only write does not.
// Test 3 — a restart (fresh Loop, cleared park registry) does not admit a
//          lane whose cooldown has not expired.
// Test 4 — admit_reason non-empty on every tick row from every path
//          (packer, sim spawn).

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// gap1660BoardDir builds a workdir whose board holds exactly the given row
// lines and returns (workdir, boardPath).
func gap1660BoardDir(t *testing.T, lines ...string) (string, string) {
	t.Helper()
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(boardPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return wd, boardPath
}

// gap1660TickReasons queries admit_reason for all tick rows of a project
// (COALESCE: schema default is ” — exactly the field requirement (d)
// audits).
func gap1660TickReasons(t *testing.T, db *sql.DB, project string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT COALESCE(admit_reason, '') FROM ticks WHERE project_name = ? ORDER BY id`, project)
	if err != nil {
		t.Fatalf("query admit_reason: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// gap1660WriteBoard replaces the board content and guarantees an mtime bump
// (same retry shape as rewriteBoard in board_wake_test.go — coarse
// filesystem timestamps can miss a same-second rewrite).
func gap1660WriteBoard(t *testing.T, path string, lines ...string) {
	t.Helper()
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	prev := fi.ModTime()
	for attempt := 0; attempt < 40; attempt++ {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(prev) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("board rewrite could not produce an mtime change")
}

// gap1660WakeNow fires one wake synchronously through the watcher's wake()
// path — the production shape: the SCHED-GAP-1660 wrapped hook runs (stamp
// decision), the wake event is emitted, and the forced evaluation runs.
func gap1660WakeNow(t *testing.T, w *BoardWakeWatcher, project, workdir, boardPath string) {
	t.Helper()
	w.wake(boardWakeTarget{project: project, workdir: workdir, boardPath: boardPath})
}

// gap1660SetLastCompleted pins last_tick_completed age ago on the wall clock
// (test-file clock reads are allowed; the stdlib guard covers non-test code).
func gap1660SetLastCompleted(t *testing.T, db *sql.DB, name string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`, stamp, name); err != nil {
		t.Fatal(err)
	}
}

// gap1660TickTerminal reports whether the nth (1-based, id-ordered) tick row
// of a project has SETTLED. The slot pool reserves a lane for the life of its
// tick, so a test that needs the lane admissible again must wait for the tick
// to leave queued/running — otherwise the next admission is dedupe-suppressed
// rather than recorded, and the assertion reads an empty table.
func gap1660TickTerminal(t *testing.T, db *sql.DB, project string, n int) bool {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM ticks WHERE project_name = ? ORDER BY id LIMIT 1 OFFSET ?`,
		project, n-1).Scan(&status); err != nil {
		return false
	}
	return status != "queued" && status != "running"
}

// insertGap1660Project seeds one enabled project row with the given
// admission mode (database.AdmissionModeCooldown / database.AdmissionModeTasks).
func insertGap1660Project(t *testing.T, db *sql.DB, name, workdir string, cooldownS int, mode string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at, admission_mode)
		VALUES (?, 'https://example.com/x', ?, 10, 5, ?, 1.0, 'true', 'p', 1,
		 datetime('now'), datetime('now'), ?)`,
		name, workdir, cooldownS, mode); err != nil {
		t.Fatal(err)
	}
}

// gap1660LoopAndWatcher builds a Loop + BoardWakeWatcher on a sim clock, the
// way the daemon wires them: NewLoop installs the SCHED-GAP-1660 wrapped
// nudge hook in boardWakeNudgeSource, and NewBoardWakeWatcher consumes it.
func gap1660LoopAndWatcher(t *testing.T, db *sql.DB) (*Loop, *BoardWakeWatcher, *clock.SimClock) {
	t.Helper()
	sim := clock.NewSimClockAt(1000, time.Now())
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.noDeliver = true
	l.SetClock(sim)
	w := NewBoardWakeWatcher(db, l.ForceEvaluate)
	w.SetClock(sim)
	return l, w, sim
}

// gap1660WallLoopAndWatcher is the wall-clock variant: tests that drive real
// (or sim-spawner) ticks to completion read the production clock — a sim
// clock nobody advances would park the spawn-side wait timers and force the
// 15s shutdown drain instead of a fast settle.
func gap1660WallLoopAndWatcher(t *testing.T, db *sql.DB) (*Loop, *BoardWakeWatcher) {
	t.Helper()
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.noDeliver = true
	// Real-spawn tests need ticks that SETTLE: the httptest gateway (the
	// gap143 fixture shape) completes each /v1/responses POST in-process —
	// an exec spawn would hold the loop in the 15s shutdown drain.
	gw := newResumeGateway(t)
	gw.wire(l)
	w := NewBoardWakeWatcher(db, l.ForceEvaluate)
	return l, w
}

// gap1660PinnedLane inserts a cooldown-mode lane parked INSIDE a 72h pin
// (last tick 1h ago) and returns the loop+watcher fixture.
func gap1660PinnedLane(t *testing.T, db *sql.DB, name, workdir string, cooldownS int) (*Loop, *BoardWakeWatcher, *clock.SimClock) {
	t.Helper()
	insertGap1660Project(t, db, name, workdir, cooldownS, database.AdmissionModeCooldown)
	gap1660SetLastCompleted(t, db, name, time.Hour)
	return gap1660LoopAndWatcher(t, db)
}

func waitFor1660(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within the wait budget")
}

// TestSCHEDGAP1660_CooldownLaneIgnoresBoardWrites is requirement (c) and RED
// test 1: a cooldown-mode lane at a 72h pin, driven through SEVEN SIMULATED
// DAYS of board writes + wakes, must record ZERO ticks (and therefore zero
// board_wake admit_reason rows) while the pin holds.
func TestSCHEDGAP1660_CooldownLaneIgnoresBoardWrites(t *testing.T) {
	db := newTestDB(t)
	wd, board := gap1660BoardDir(t, `{"id":"R1","status":"pending","title":"work"}`)
	const proj = "gap1660-cooldown"

	l, w, sim := gap1660PinnedLane(t, db, proj, wd, 720*3600)
	defer l.Stop()
	defer w.Stop()

	// Seven simulated days of writes, one wake each.
	for day := 0; day < 7; day++ {
		sim.Advance(24 * time.Hour)
		gap1660WriteBoard(t, board,
			`{"id":"R1","status":"pending","title":"work"}`,
			fmt.Sprintf(`{"id":"R%d","status":"pending","title":"day %d"}`, day, day))
		gap1660WakeNow(t, w, proj, wd, board)
	}

	if rows := gap1660TickReasons(t, db, proj); len(rows) != 0 {
		t.Fatalf("cooldown lane admitted %d tick(s) from board writes over 7 days, admit_reasons=%v — want 0 (the pin is the sole authority)", len(rows), rows)
	}
}

// TestSCHEDGAP1660_TasksLaneParkAndFlip is RED test 2: a tasks-mode lane
// with ONLY perpetual rows parks (temporary cooldown). One non-perpetual row
// added + wake flips it (tick stamped flip:board_empty). A later
// perpetual-only write does NOT flip it again.
func TestSCHEDGAP1660_TasksLaneParkAndFlip(t *testing.T) {
	db := newTestDB(t)
	perpetual := `{"id":"PERP-1","status":"pending","title":"never done","perpetual":true}`
	wd, board := gap1660BoardDir(t, perpetual)
	const proj = "gap1660-tasks"

	insertGap1660Project(t, db, proj, wd, 72*3600, database.AdmissionModeTasks)
	gap1660SetLastCompleted(t, db, proj, time.Hour)
	l, w := gap1660WallLoopAndWatcher(t, db)
	defer l.Stop()
	defer w.Stop()

	// Baseline: the lane is parked (board holds only a perpetual row). The
	// classification pass records the park at the point it already knows it.
	l.evaluate()
	if !isParkedEmpty(proj) {
		t.Fatal("a tasks lane whose board holds only perpetual rows was not recorded parked-empty")
	}

	// The flip: add ONE non-perpetual row and fire the wake.
	gap1660WriteBoard(t, board, perpetual, `{"id":"REAL-1","status":"pending","title":"real work"}`)
	gap1660WakeNow(t, w, proj, wd, board)
	var flipID string
	waitFor1660(t, 10*time.Second, func() bool {
		return db.QueryRow(`SELECT id FROM ticks WHERE project_name = ? AND admit_reason = ?`, proj, AdmissionReasonFlipBoardEmpty).Scan(&flipID) == nil
	})

	// A perpetual-only write afterwards must NOT flip again: the park
	// returns and the wake is structurally unable to admit.
	gap1660SetLastCompleted(t, db, proj, 10*time.Minute)
	gap1660WriteBoard(t, board, perpetual)
	gap1660WakeNow(t, w, proj, wd, board)
	time.Sleep(500 * time.Millisecond) // settle any async spawn the second wake might have caused
	var extra int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ? AND admit_reason = ?`, proj, AdmissionReasonFlipBoardEmpty).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if extra != 1 {
		t.Fatalf("perpetual-only board write flipped the parked lane again: %d flip ticks, want exactly 1", extra)
	}
}

// TestSCHEDGAP1660_WakeOnLiveTasksLaneIsNotAFlip is the SCOPING guard
// (deliverable (a)): a tasks-admission lane whose board ALREADY holds
// non-perpetual work never parked, so a board write that wakes it must not
// be recorded as the sanctioned park-flip. The board-wake stamp is
// process-wide — one fired wake stamps it and whichever lane spawns next
// consumes it — so a flip label keyed off the stamp alone marks every
// wake-driven admission on the fleet as a park-flip and makes the flip
// unmeasurable. RED before the park mark is consumed by the stamp site.
func TestSCHEDGAP1660_WakeOnLiveTasksLaneIsNotAFlip(t *testing.T) {
	db := newTestDB(t)
	const live1 = `{"id":"LIVE-1","status":"pending","title":"already queued"}`
	wd, board := gap1660BoardDir(t, live1)
	const proj = "gap1660-live-tasks"

	insertGap1660Project(t, db, proj, wd, 720*3600, database.AdmissionModeTasks)
	gap1660SetLastCompleted(t, db, proj, 24*time.Hour)
	l, w := gap1660WallLoopAndWatcher(t, db)
	defer l.Stop()
	defer w.Stop()

	// Baseline: the board already holds non-perpetual work, so the pass
	// ADMITS the lane — the park mark is only ever set by a pass that
	// DEFERS the lane on an empty board.
	l.evaluate()
	waitFor1660(t, 10*time.Second, func() bool {
		rows := gap1660TickReasons(t, db, proj)
		return len(rows) == 1 && rows[0] != ""
	})
	waitFor1660(t, 10*time.Second, func() bool { return gap1660TickTerminal(t, db, proj, 1) })
	if isParkedEmpty(proj) {
		t.Fatal("precondition: a tasks lane whose board holds non-perpetual work was recorded parked-empty")
	}
	// A tick id is <project>-<second> (see the pool's tickID), so an
	// admission inside the SAME wall second as the previous tick collides on
	// the primary key and the enqueue is dropped — not the path under test.
	// Cross the second boundary deterministically (a fixed wall-clock sleep
	// is non-deterministic under CI load — INT-CI-180).
	firstSec := time.Now().Second()
	for time.Now().Second() == firstSec {
		time.Sleep(50 * time.Millisecond)
	}

	// The board write (adding a second non-perpetual row) plus the wake it
	// fires: this admission IS board-wake sourced, but the lane never
	// parked — the label must therefore stay in the ordinary nudge family.
	gap1660WriteBoard(t, board, live1, `{"id":"LIVE-2","status":"pending","title":"second"}`)
	gap1660WakeNow(t, w, proj, wd, board)
	// Tick insertion precedes the async admit_reason stamp; wait for the
	// second row's reason to be committed, not just for its id to appear.
	waitFor1660(t, 10*time.Second, func() bool {
		reasons := gap1660TickReasons(t, db, proj)
		return len(reasons) > 1 && reasons[1] != ""
	})

	reasons := gap1660TickReasons(t, db, proj)
	seenWake := false
	for i, r := range reasons {
		if r == AdmissionReasonFlipBoardEmpty {
			t.Fatalf("tick #%d on a never-parked tasks lane is stamped %q — flip:board_empty belongs ONLY to a lane that parked on an empty board (deliverable (a)); all reasons=%v",
				i, r, reasons)
		}
		if r == "resume:"+NudgeSourceBoardWake {
			seenWake = true
		}
	}
	// Non-vacuity: the wake really was the entry point for the second tick,
	// so the assertion above ran against a live board-wake admission.
	if !seenWake {
		t.Fatalf("no tick carries %q — the wake path was not exercised, so the no-flip assertion is vacuous; all reasons=%v",
			"resume:"+NudgeSourceBoardWake, reasons)
	}
}

// TestSCHEDGAP1660_CooldownLaneImmuneOnMixedFleet is requirement (c) in the
// FLEET shape: a pinned cooldown lane whose board even HAD pending work, next
// to a tasks lane whose writes keep the board-wake stamp flowing. The
// SCHED-GAP-1660 hook cannot protect this case (wakeAdmitsAnyLane is true
// whenever a tasks lane is enabled, which is the real fleet), so the immunity
// has to be structural: the packer's wall-clock gate refuses the cooldown
// lane however the pass was triggered. The tasks lane must tick (non-vacuity:
// the stamp really was live).
func TestSCHEDGAP1660_CooldownLaneImmuneOnMixedFleet(t *testing.T) {
	db := newTestDB(t)
	coolRow := `{"id":"C1","status":"pending","title":"cool lane work"}`
	taskRow := `{"id":"T1","status":"pending","title":"tasks lane work"}`
	coolWD, coolBoard := gap1660BoardDir(t, coolRow)
	taskWD, taskBoard := gap1660BoardDir(t, taskRow)
	const cool, task = "gap1660-mixed-cool", "gap1660-mixed-task"

	insertGap1660Project(t, db, cool, coolWD, 720*3600, database.AdmissionModeCooldown)
	gap1660SetLastCompleted(t, db, cool, time.Hour) // inside the 30-day pin
	insertGap1660Project(t, db, task, taskWD, 720*3600, database.AdmissionModeTasks)
	gap1660SetLastCompleted(t, db, task, 24*time.Hour)

	l, w := gap1660WallLoopAndWatcher(t, db)
	defer l.Stop()
	defer w.Stop()

	for i := 0; i < 4; i++ {
		gap1660WriteBoard(t, taskBoard, taskRow,
			fmt.Sprintf(`{"id":"T%d","status":"pending","title":"more %d"}`, i, i))
		gap1660WakeNow(t, w, task, taskWD, taskBoard)
		gap1660WriteBoard(t, coolBoard, coolRow,
			fmt.Sprintf(`{"id":"C%d","status":"pending","title":"wake %d"}`, i, i))
		gap1660WakeNow(t, w, cool, coolWD, coolBoard)
	}

	waitFor1660(t, 10*time.Second, func() bool { return len(gap1660TickReasons(t, db, task)) > 0 })
	if rows := gap1660TickReasons(t, db, cool); len(rows) != 0 {
		t.Fatalf("a cooldown lane pinned at 30 days took %d tick(s) while a tasks lane kept the board-wake stamp live: %v — the pin is the sole authority for a cooldown lane",
			len(rows), rows)
	}
}

// TestSCHEDGAP1660_RestartDoesNotAdmitInsideCooldown is RED test 3: after a
// restart (the boot scan resumeOrphansAtStartup runs against a live gateway,
// then the park registry is cleared exactly as a fresh NewLoop does), a lane
// whose cooldown has not expired is not admitted — a board write included.
func TestSCHEDGAP1660_RestartDoesNotAdmitInsideCooldown(t *testing.T) {
	db := newTestDB(t)
	wd, board := gap1660BoardDir(t, `{"id":"S1","status":"pending","title":"work"}`)
	const proj = "gap1660-restart"

	l, w, _ := gap1660PinnedLane(t, db, proj, wd, 72*3600)
	defer l.Stop()
	defer w.Stop()

	// The restart: the boot orphan scan (SCHED-GAP-101 / the criterion's
	// named entry point) then the fresh-process park registry, then a board
	// write wakes the lane.
	l.resumeOrphansAtStartup()
	clearParkedEmpty()
	if rows := gap1660TickReasons(t, db, proj); len(rows) != 0 {
		t.Fatalf("resumeOrphansAtStartup admitted a lane inside its cooldown: %v", rows)
	}
	gap1660WriteBoard(t, board,
		`{"id":"S1","status":"pending","title":"work"}`,
		`{"id":"S2","status":"pending","title":"more"}`)
	gap1660WakeNow(t, w, proj, wd, board)
	time.Sleep(500 * time.Millisecond)

	if rows := gap1660TickReasons(t, db, proj); len(rows) != 0 {
		t.Fatalf("a restart admitted a lane inside its cooldown from a board write: %v", rows)
	}
}

// TestSCHEDGAP1660_AdmitReasonAlwaysStamped is RED test 4: every tick row
// from every path carries a non-empty admit_reason.
func TestSCHEDGAP1660_AdmitReasonAlwaysStamped(t *testing.T) {
	db := newTestDB(t)

	// (1) Packer path: a lane admitted by evaluate() carries "ok".
	// SCHED-GAP-1655 note: the board carries a pending row — the test's
	// subject is the admit_reason STAMP (requirement d), not the board;
	// under the 1655 policy a cooldown builder on a proven-drained board
	// is deferred (reason no_work) and would never produce the tick this
	// test reads.
	wd, _ := gap1660BoardDir(t, `{"id":"S0","status":"pending","title":"work"}`)
	const proj = "gap1660-stamp-packer"
	insertGap1660Project(t, db, proj, wd, 21600, database.AdmissionModeCooldown)
	gap1660SetLastCompleted(t, db, proj, 24*time.Hour) // pin long elapsed
	l, _ := gap1660WallLoopAndWatcher(t, db)
	l.evaluate()
	waitFor1660(t, 10*time.Second, func() bool {
		return len(gap1660TickReasons(t, db, proj)) > 0
	})
	l.Stop()
	for i, r := range gap1660TickReasons(t, db, proj) {
		if r == "" {
			t.Fatalf("packer tick #%d has an empty admit_reason — (d) requires a stamp on every row", i)
		}
	}

	// (2) Sim-spawn path: the row the SimSpawner inserts itself (the path
	// that never had a stamp before SCHED-GAP-1660). The board carries a
	// pending row — same SCHED-GAP-1655 note as arm (1): the subject is
	// the stamp, not the board.
	wd2, _ := gap1660BoardDir(t, `{"id":"S0","status":"pending","title":"work"}`)
	const simProj = "gap1660-stamp-sim"
	insertGap1660Project(t, db, simProj, wd2, 21600, database.AdmissionModeCooldown)
	l.SetSimulation(1.0)
	gap1660SetLastCompleted(t, db, simProj, 24*time.Hour)
	l.evaluate()
	waitFor1660(t, 10*time.Second, func() bool {
		rows := gap1660TickReasons(t, db, simProj)
		if len(rows) == 0 {
			return false
		}
		for _, r := range rows {
			if r == "" {
				return false // row visible, stamp not yet settled
			}
		}
		return true
	})
	for i, r := range gap1660TickReasons(t, db, simProj) {
		if r == "" {
			t.Fatalf("sim tick #%d has an empty admit_reason — (d) requires a stamp on every row", i)
		}
	}
}

// TestSCHEDGAP1660_WakeOnCooldownOnlyFleetDropsStamp pins the hook's
// decision directly: on a fleet whose only lane is cooldown-mode, a fired
// wake leaves NO board_wake stamp behind — a later MANUAL spawn must record
// resume:manual, proving the wake never touched the stamp slot.
func TestSCHEDGAP1660_WakeOnCooldownOnlyFleetDropsStamp(t *testing.T) {
	db := newTestDB(t)
	wd, board := gap1660BoardDir(t, `{"id":"H1","status":"pending","title":"work"}`)
	const proj = "gap1660-hook"

	l, w, _ := gap1660PinnedLane(t, db, proj, wd, 72*3600)
	defer l.Stop()
	defer w.Stop()

	gap1660WakeNow(t, w, proj, wd, board)

	// The board-wake hook must not have stamped: a manual spawn afterwards
	// records its OWN source, not a leftover board_wake.
	l.SetNudgeSource(NudgeSourceManual)
	if got := l.clearNudgeSource(); got != NudgeSourceManual {
		t.Fatalf("after a wake on a cooldown-only fleet the stamp slot held %q, want empty (manual set next)", got)
	}
}
