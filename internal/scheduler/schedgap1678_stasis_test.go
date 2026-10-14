package scheduler_test

// SCHED-GAP-1678 tests: the board-stasis spawn gate.
//
// The measured problem (2026-09-29): a tick fired on a board that has NOT
// changed since the lane's previous tick no-ops 69% of the time (164/235)
// versus 25% (66/260) when the board moved, and the no-ops are the EXPENSIVE
// ticks. The gate fingerprints the lane's board file (mtime + size) at SPAWN
// time and excludes a cooldown-mode BUILDER lane from selection while that
// fingerprint is unchanged — before any LLM call and before any tick row
// exists.
//
// These tests drive the gate through the REAL selection path (the multi-pool
// packer, namespace mode — the fleet's live path), plus the gate's own
// primitives, so the row's regression criterion is covered end to end:
// board unchanged -> zero spawns; board changed -> exactly one spawn.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	scheduler "github.com/coding-hermes/scheduler/internal/scheduler"
)

// packWithStasis runs one namespace-mode pack with the given gate installed,
// mirroring packNamespaces but wiring SetBoardStasisGate.
func packWithStasis(t *testing.T, projects []database.Project, namespaces []database.Namespace,
	lastCompleted map[string]time.Time, gate *scheduler.BoardStasisGate) scheduler.PackResult {
	t.Helper()
	mp := scheduler.NewMultiPoolPacker(400, 8, nil)
	mp.SetBoardStasisGate(gate)
	return mp.Pack(projects, namespaces, defaultUrgencyCalc(), lastCompleted, nil, time.Now().UTC())
}

// stasisLane builds a cooldown-mode builder lane with a COMPLETED previous
// tick — the only shape the gate governs.
func stasisLane(name, wd string) database.Project {
	p := modeProject(name, "qa", wd, "")
	p.LastTickStatus = database.LastStatusCompleted
	return p
}

// T-STASIS-1: the row's regression criterion. A cooldown-mode builder lane
// whose board has not moved since its previous tick is NOT selected (zero
// spawns), the skip is counted once, and a board write unblocks it (exactly
// one spawn).
func TestBoardStasis_UnchangedBoardBlocksThenBoardMoveUnblocks(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)
	p := stasisLane("stasis-a", wd)
	ns := cooldownNs("qa")
	now := time.Now().UTC()
	// 5h since the previous tick against a 4h pin: pin ELAPSED, so without
	// the gate this lane is selected.
	last := map[string]time.Time{"stasis-a": now.Add(-5 * time.Hour)}

	gate := scheduler.NewBoardStasisGate()
	gate.SetEnabled(true)
	gate.RecordSpawn("stasis-a", wd, "tick-0001")

	res := packWithStasis(t, []database.Project{p}, []database.Namespace{ns}, last, gate)
	if modeSelected(t, res, "stasis-a") {
		t.Fatalf("T-STASIS-1 FAIL: lane selected on an UNCHANGED board — the gate must exclude it (zero spawns)")
	}
	// Exactly ONE skip per lane per pass: the selection site notes it, the
	// borrowed-budget mirror must stay quiet.
	if got := gate.Skips(); got != 1 {
		t.Fatalf("T-STASIS-1 FAIL: skip counter = %d, want exactly 1 (double-counted?)", got)
	}

	// The board MOVES (new row): the next pass must spawn exactly one tick.
	writeModeBoard(t, wd,
		`{"id":"REAL-1","status":"pending"}`,
		`{"id":"REAL-2","status":"pending"}`)
	res = packWithStasis(t, []database.Project{p}, []database.Namespace{ns}, last, gate)
	if !modeSelected(t, res, "stasis-a") {
		t.Fatalf("T-STASIS-1 FAIL: lane NOT selected after its board moved — the gate must release it")
	}
	if got := gate.Skips(); got != 1 {
		t.Fatalf("T-STASIS-1 FAIL: skip counter moved on a board CHANGED pass: got %d, want 1", got)
	}
}

// T-STASIS-2: with the gate DISARMED the lane is selected on an unchanged
// board — the pre-gate behavior every existing entry point keeps until the
// daemon arms it.
func TestBoardStasis_DisabledGateKeepsPreGateBehavior(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)
	p := stasisLane("stasis-b", wd)
	now := time.Now().UTC()

	gate := scheduler.NewBoardStasisGate() // disabled at construction
	gate.RecordSpawn("stasis-b", wd, "tick-0001")

	res := packWithStasis(t, []database.Project{p}, []database.Namespace{cooldownNs("qa")},
		map[string]time.Time{"stasis-b": now.Add(-5 * time.Hour)}, gate)
	if !modeSelected(t, res, "stasis-b") {
		t.Fatalf("T-STASIS-2 FAIL: disabled gate changed selection — must be byte-identical to pre-gate")
	}
	if gate.Skips() != 0 {
		t.Fatalf("T-STASIS-2 FAIL: disabled gate counted a skip: %d", gate.Skips())
	}
}

// T-STASIS-3: escape hatches. A lane with NO baseline (first tick) and a lane
// whose previous tick did NOT complete are both spawned even on an unchanged
// board — stasis proves nothing when the previous tick may have died before
// reading the board.
func TestBoardStasis_EscapeHatchesSpawn(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)
	now := time.Now().UTC()
	gate := scheduler.NewBoardStasisGate()
	gate.SetEnabled(true)

	// (a) no baseline recorded for this lane -> spawn.
	fresh := stasisLane("stasis-fresh", wd)
	res := packWithStasis(t, []database.Project{fresh}, []database.Namespace{cooldownNs("qa")},
		map[string]time.Time{"stasis-fresh": now.Add(-5 * time.Hour)}, gate)
	if !modeSelected(t, res, "stasis-fresh") {
		t.Fatalf("T-STASIS-3 FAIL: a lane with no baseline was gated — the first tick must always spawn")
	}

	// (b) baseline exists but the previous tick FAILED -> spawn (retry, never
	// stranded behind stasis).
	failed := stasisLane("stasis-failed", wd)
	failed.LastTickStatus = database.LastStatusFailed
	gate.RecordSpawn("stasis-failed", wd, "tick-0002")
	res = packWithStasis(t, []database.Project{failed}, []database.Namespace{cooldownNs("qa")},
		map[string]time.Time{"stasis-failed": now.Add(-5 * time.Hour)}, gate)
	if !modeSelected(t, res, "stasis-failed") {
		t.Fatalf("T-STASIS-3 FAIL: a lane whose previous tick FAILED was gated — it must be retried")
	}
}

// T-STASIS-4: lane scope. A tasks-mode lane keeps the SCHED-GAP-124 waiver
// and a reporter-class lane keeps its timer cadence — neither is gated, even
// on an unchanged board with a recorded baseline.
func TestBoardStasis_LaneScopeExemptsTasksAndReporter(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)
	now := time.Now().UTC()
	gate := scheduler.NewBoardStasisGate()
	gate.SetEnabled(true)

	// (a) tasks-mode lane: the waiver exists precisely to re-fire on standing
	// board work, so an unchanged board must not gate it.
	tasks := modeProject("stasis-tasks", "coding-hermes", wd, database.AdmissionModeTasks)
	tasks.LastTickStatus = database.LastStatusCompleted
	gate.RecordSpawn("stasis-tasks", wd, "tick-0003")
	res := packWithStasis(t, []database.Project{tasks}, []database.Namespace{tasksNs("coding-hermes")},
		map[string]time.Time{"stasis-tasks": now.Add(-time.Hour)}, gate)
	if !modeSelected(t, res, "stasis-tasks") {
		t.Fatalf("T-STASIS-4 FAIL: tasks-mode lane gated — the SCHED-GAP-124 waiver must stand")
	}

	// (b) reporter-class lane (the -sync satellite family): its product is a
	// periodic report/key, so the timer cadence is BY DESIGN.
	reporter := stasisLane("stasis-sync", wd)
	gate.RecordSpawn("stasis-sync", wd, "tick-0004")
	res = packWithStasis(t, []database.Project{reporter}, []database.Namespace{cooldownNs("qa")},
		map[string]time.Time{"stasis-sync": now.Add(-5 * time.Hour)}, gate)
	if !modeSelected(t, res, "stasis-sync") {
		t.Fatalf("T-STASIS-4 FAIL: reporter-class lane gated — reporter lanes keep their timer cadence")
	}
}

// T-STASIS-5: the gate primitives — the decision is a fingerprint compare,
// and every unreadable/absent state fails OPEN (spawn).
func TestBoardStasisGate_Primitives(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-1","status":"pending"}`)

	gate := scheduler.NewBoardStasisGate()
	gate.SetEnabled(true)

	// No baseline -> not blocked (first tick).
	if blocked, _ := gate.AdmissionBlocked("lane-x", wd); blocked {
		t.Fatalf("T-STASIS-5 FAIL: no baseline must not block")
	}

	gate.RecordSpawn("lane-x", wd, "tick-0005")
	if blocked, prev := gate.AdmissionBlocked("lane-x", wd); !blocked || prev != "tick-0005" {
		t.Fatalf("T-STASIS-5 FAIL: unchanged board must block and name the previous tick (got blocked=%v prev=%q)", blocked, prev)
	}

	// The board moves -> not blocked.
	writeModeBoard(t, wd, `{"id":"REAL-2","status":"pending"}`)
	if blocked, _ := gate.AdmissionBlocked("lane-x", wd); blocked {
		t.Fatalf("T-STASIS-5 FAIL: a changed board must not block")
	}

	// ClearBaseline -> not blocked (the next pass re-decides).
	gate.RecordSpawn("lane-x", wd, "tick-0006")
	gate.ClearBaseline("lane-x")
	if blocked, _ := gate.AdmissionBlocked("lane-x", wd); blocked {
		t.Fatalf("T-STASIS-5 FAIL: a cleared baseline must not block")
	}

	// A lane whose workdir has NO board file fails open.
	noBoard := t.TempDir()
	gate.RecordSpawn("lane-noboard", noBoard, "tick-0007")
	if blocked, _ := gate.AdmissionBlocked("lane-noboard", noBoard); blocked {
		t.Fatalf("T-STASIS-5 FAIL: a missing board must fail OPEN (no evidence of stasis)")
	}
	if blocked, _ := gate.AdmissionBlocked("lane-noboard", ""); blocked {
		t.Fatalf("T-STASIS-5 FAIL: an empty workdir must fail OPEN")
	}
}

// T-STASIS-6: the fingerprint is mtime+size over the board file the lane
// actually reads (findBoardFile's .coding-hermes/board walk) — content that
// changes only in SIZE is detected even when the mtime granularity is coarse.
func TestBoardStasisGate_FingerprintTracksBoardFile(t *testing.T) {
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	board := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(board, []byte(`{"id":"A"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	gate := scheduler.NewBoardStasisGate()
	gate.SetEnabled(true)
	gate.RecordSpawn("lane-y", wd, "tick-0008")
	if blocked, _ := gate.AdmissionBlocked("lane-y", wd); !blocked {
		t.Fatalf("T-STASIS-6 FAIL: unchanged board file must block")
	}
	// Same second, different SIZE: append.
	f, err := os.OpenFile(board, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"id":"B"}` + "\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	_ = f.Close()
	if blocked, _ := gate.AdmissionBlocked("lane-y", wd); blocked {
		t.Fatalf("T-STASIS-6 FAIL: an appended board must not block")
	}
}
