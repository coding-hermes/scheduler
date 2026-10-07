package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── SCHED-GAP-1727: deferred:true board rows are not actionable work ──
//
// BT-076 gave boardctl the deferred:true contract: a deferred row is
// searchable/returnable board state but is EXCLUDED from open-work
// counting exactly like a NEVER-DONE/perpetual fixture. This file proves
// the Go side at every consumer layer:
//
//	T1  deferred-only board → boardOpenRows = 0; a tasks-admission
//	    foreman parks on its cooldown fallback (no admission).
//	T2  deferred + perpetual only → open = 0, parked.
//	T3  deferred + one plain pending row → open = 1, admission proceeds.
//	T4  wake path: deferred-only board change arms NOTHING; adding one
//	    normal row arms + fires the wake.
//	T5  boardOpenUniqueIDs agrees with boardOpenRows.
//	    Freshness reader: a deferred row is neither work-to-spawn nor
//	    idle-proof, and stays VISIBLE in Rows/Counts (counting-only
//	    exclusion — result/report surfaces never hide deferred rows).
//
// (Internal package: boardOpenRows / boardOpenUniqueIDs / countPendingBoard
// are unexported; the packer fixture helpers mirror admission_mode_test.go's
// scheduler_test shapes locally.)

const (
	g1727DeferredRow = `{"id":"DEF-1","status":"pending","deferred":true}`
	g1727Perpetual   = `{"id":"NEVER-DONE","status":"pending","perpetual":true}`
	g1727Plain       = `{"id":"REAL-1","status":"pending"}`
)

// g1727Board writes a tasks.jsonl board into a fresh workdir.
func g1727Board(t *testing.T, rows ...string) string {
	t.Helper()
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, r := range rows {
		content += r + "\n"
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return wd
}

// g1727Pack runs one multi-pool pack over a single tasks-mode project the
// same way admission_mode_test.go does (namespace weight 100, 4h cooldown
// pin, last completion 1h ago → inside the pin).
func g1727Pack(t *testing.T, name, workdir string) PackResult {
	t.Helper()
	nsID := "coding-hermes"
	p := database.Project{
		Name:        name,
		RepoURL:     "local://" + name,
		Workdir:     workdir,
		Weight:      10,
		Priority:    5,
		CooldownS:   14400,
		Enabled:     true,
		CreatedAt:   time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		NamespaceID: &nsID,
	}
	ns := database.Namespace{ID: nsID, Weight: 100, Reserved: 1, HardCap: 100, Enabled: true, MaxConcurrent: 4, AdmissionMode: database.AdmissionModeTasks}
	now := time.Now().UTC()
	mp := NewMultiPoolPacker(400, 8, nil)
	return mp.Pack([]database.Project{p}, []database.Namespace{ns}, NewUrgencyCalculator(time.Minute, time.Hour, 10),
		map[string]time.Time{name: now.Add(-time.Hour)}, nil, now)
}

func g1727Selected(res PackResult, name string) bool {
	for _, p := range res.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// T1a: the counting half.
func TestSCHEDGAP1727_DeferredOnlyBoardOpenRowsZero(t *testing.T) {
	n, ok := boardOpenRows(g1727Board(t, g1727DeferredRow))
	if !ok {
		t.Fatal("board exists but boardOpenRows reported absent")
	}
	if n != 0 {
		t.Fatalf("boardOpenRows = %d, want 0 (deferred row is not actionable work)", n)
	}
}

// T1b: the admission half — a tasks-mode foreman on a deferred-only
// board must NOT be admitted inside its cooldown window (parks on the
// cooldown fallback, identical to the SCHED-GAP-106 perpetual case).
func TestSCHEDGAP1727_DeferredOnlyBoardDoesNotAdmit(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow)
	res := g1727Pack(t, "gap1727-a", wd)
	if g1727Selected(res, "gap1727-a") {
		t.Fatal("deferred-only board admitted a tasks-mode lane — deferred rows must not count as open work")
	}
}

// T2: deferred + perpetual both excluded → open = 0, parked.
func TestSCHEDGAP1727_DeferredPlusPerpetualOnlyExcluded(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow, g1727Perpetual)
	n, ok := boardOpenRows(wd)
	if !ok || n != 0 {
		t.Fatalf("boardOpenRows = (%d, %v), want (0, true)", n, ok)
	}
	res := g1727Pack(t, "gap1727-b", wd)
	if g1727Selected(res, "gap1727-b") {
		t.Fatal("deferred+perpetual-only board admitted — must park on cooldown")
	}
}

// T3: deferred row PLUS one plain pending row → open = 1 and admission
// proceeds (the exclusion must not eat real work).
func TestSCHEDGAP1727_DeferredPlusPlainRowAdmits(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow, g1727Plain)
	n, ok := boardOpenRows(wd)
	if !ok || n != 1 {
		t.Fatalf("boardOpenRows = (%d, %v), want (1, true)", n, ok)
	}
	res := g1727Pack(t, "gap1727-c", wd)
	if !g1727Selected(res, "gap1727-c") {
		t.Fatal("deferred + plain pending row not admitted — the plain row is actionable work")
	}
}

// T4: the wake path. A deferred-only board change arms NOTHING; the
// same board gaining one normal actionable row arms + fires the wake.
// Driven on constructed instants via pollOnce (no sleeps).
func TestSCHEDGAP1727_DeferredOnlyChangeDoesNotWake(t *testing.T) {
	fx := newWakeFix(t)
	// Baseline with a deferred-only board.
	rewriteBoard(t, fx.boardPath, g1727DeferredRow)
	t0 := time.Now()
	fx.w.pollOnce(t0)

	// Deferred-only change (a second deferred row appended): mtime moves,
	// but no wake may arm.
	t1 := t0.Add(10 * time.Minute) // past any debounce from the baseline
	rewriteBoard(t, fx.boardPath, g1727DeferredRow, `{"id":"DEF-2","status":"pending","deferred":true}`)
	fx.w.pollOnce(t1.Add(1 * time.Minute))
	if got := fx.evals(); got != 0 {
		t.Fatalf("deferred-only board change woke the watcher: %d evals, want 0", got)
	}
	// Advance well past a debounce in case something armed anyway.
	fx.w.pollOnce(t1.Add(30 * time.Minute))
	if got := fx.evals(); got != 0 {
		t.Fatalf("deferred-only change fired a wake after debounce: %d evals, want 0", got)
	}

	// Now one normal actionable row lands → wake arms and fires exactly
	// once, one debounce later.
	rewriteBoard(t, fx.boardPath, g1727DeferredRow, g1727Plain)
	t2 := t1.Add(31 * time.Minute)
	fx.w.pollOnce(t2) // detects change, arms
	if got := fx.evals(); got != 0 {
		t.Fatalf("wake fired before debounce elapsed: %d evals, want 0", got)
	}
	t3 := t2.Add(fx.w.debounce)
	fx.w.pollOnce(t3)
	if got := fx.evals(); got != 1 {
		t.Fatalf("actionable row on deferred board: evals = %d, want exactly 1 (wake fired)", got)
	}
}

// T5: boardOpenUniqueIDs agrees with boardOpenRows — deferred excluded.
func TestSCHEDGAP1727_UniqueIDsExcludeDeferred(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow, `{"id":"DEF-3","status":"pending","deferred":true}`, g1727Plain)
	ids, ok := boardOpenUniqueIDs(wd)
	if !ok {
		t.Fatal("board exists but boardOpenUniqueIDs reported absent")
	}
	if ids != 1 {
		t.Fatalf("boardOpenUniqueIDs = %d, want 1 (two deferred rows excluded, one plain row counted)", ids)
	}
	if n, _ := boardOpenRows(wd); n != 1 {
		t.Fatalf("boardOpenRows = %d, want 1 — the two counters must agree", n)
	}
}

// Freshness reader: a deferred row is neither work-to-spawn nor
// idle-proof, and stays VISIBLE in Rows/Counts (counting-only exclusion
// — result/report surfaces never hide deferred rows).
func TestSCHEDGAP1727_FreshnessReaderDeferredVisibleNotWork(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow, g1727Plain)
	board := filepath.Join(wd, ".coding-hermes", "board", "tasks.jsonl")
	rep := ReadBoardFreshness(wd, board, FreshnessOptions{Now: time.Now()})
	// WorkToSpawn is TRUE here — the plain row is open work (repo
	// unreadable keeps it RowOpen, the never-hide-work law). The
	// deferred-only case (WorkToSpawn=false) is pinned in the next test.
	if rep.Counts[RowDeferred] != 1 {
		t.Fatalf("Counts[deferred] = %d, want 1 — deferred row must stay VISIBLE", rep.Counts[RowDeferred])
	}
	if rep.Counts[RowOpen] != 1 {
		t.Fatalf("Counts[open] = %d, want 1 (the plain row)", rep.Counts[RowOpen])
	}
	found := false
	for _, v := range rep.Rows {
		if v.ID == "DEF-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("deferred row DEF-1 missing from rep.Rows — reporting surfaces must still return it")
	}
}

// Freshness reader, deferred-only board: no work-to-spawn AND no idle
// claim (a deferred row is not idle evidence — the row exists, parked).
func TestSCHEDGAP1727_FreshnessReaderDeferredOnlyNeitherWorkNorIdle(t *testing.T) {
	wd := g1727Board(t, g1727DeferredRow)
	board := filepath.Join(wd, ".coding-hermes", "board", "tasks.jsonl")
	rep := ReadBoardFreshness(wd, board, FreshnessOptions{Now: time.Now()})
	if rep.WorkToSpawn {
		t.Fatal("deferred-only board raised WorkToSpawn — deferred rows are not actionable work")
	}
	if rep.VerifiablyIdle {
		t.Fatal("deferred-only board claimed VerifiablyIdle — a deferred row is not idle evidence")
	}
}

// countPendingBoard (the pendingBoost path) excludes deferred rows too —
// all layers agree.
func TestSCHEDGAP1727_CountPendingBoardExcludesDeferred(t *testing.T) {
	boardPath := filepath.Join(g1727Board(t, g1727DeferredRow, g1727Perpetual), ".coding-hermes", "board", "tasks.jsonl")
	fi, err := os.Stat(boardPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(boardPath, fi); got != 0 {
		t.Fatalf("countPendingBoard = %d, want 0 (deferred and perpetual excluded)", got)
	}
}

// RED-half sanity (proof of the defect this closes): a malformed row
// still counts open (never hide work) while a deferred row does not —
// the vocabulary branch is reachable, the exclusion is data-driven.
func TestSCHEDGAP1727_DeferredExclusionIsDataDriven(t *testing.T) {
	// deferred:false (or absent) must count — only true excludes.
	wd := g1727Board(t, `{"id":"DEF-4","status":"pending","deferred":false}`, g1727Plain)
	if n, ok := boardOpenRows(wd); !ok || n != 2 {
		t.Fatalf("boardOpenRows = (%d, %v), want (2, true) — deferred:false rows are ordinary open rows", n, ok)
	}
}
