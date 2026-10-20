package scheduler

// PERF-001-R3 — /api/v1/status must not queue behind an evaluation pass.
//
// THE MEASURED DEFECT. A live 504 taken under load (47 → HTTP 504 after
// 21.819845 s) left this goroutine dump:
//
//	goroutine 1983047 [sync.RWMutex.RLock]:
//	  sync.(*RWMutex).RLock(...)                       src/sync/rwmutex.go:74
//	  internal/scheduler.(*Loop).ZeroSelectStats(...)  internal/scheduler/loop.go:1746
//	  internal/api.(*Server).status(...)               internal/api/server.go:656
//
// while the pass holding the write lock was inside the per-board
// board-freshness battery (ReadBoardFreshness → classifyCompleteRow →
// gitCommitInfo → syscall.forkExec; 41.9% of all daemon CPU per PERF-002-R2).
// The armed 5s read budget bounds "how much more work the handler does"
// (it is a context deadline checked BETWEEN steps), never "how long the
// caller waits" — so the request was answered 13.9-21.8s late, and the WARN
// line the daemon emitted even mis-attributed the stall to whichever step
// happened to be open when the budget tripped.
//
// THE FIX (two halves, each pinned below):
//
//  1. The operator read path takes no lock. ZeroSelectStats() reads atomic
//     write-through mirrors of the GAP-043 trio — the pattern LastEvalTime()
//     established in SCHED-GAP-1621 — so no reader can queue behind a pass.
//  2. evaluate() no longer holds the write lock across the expensive work.
//     The pass keeps its serialization in evalMu (a lock no reader ever
//     takes) and takes mu only for the O(1) state updates at its head and
//     tail, so an operator read can never wait on a pass at all.
//
// Each test below is written to FAIL on the pre-fix code (red-proof): (1)
// against a held write lock, (2) by probing TryLock from inside the pack,
// (3) by sampling the operator getters while a pass is parked mid-pack.

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// perf001r3Fixture builds the smallest fleet that actually runs the
// board-freshness read: one enabled project whose workdir carries a
// one-row pending JSONL board. CountPending only consults the freshness
// seam for a board file with a non-zero raw pending count, so without this
// fixture a probe seam would never fire and the tests below would pass
// vacuously.
func perf001r3Fixture(t *testing.T) (*Loop, string) {
	t.Helper()
	db := newTestDB(t)
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(boardDir, "tasks.jsonl"),
		[]byte(`{"id":"PERF-001-R3-FIXTURE","status":"pending"}`+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write fixture board: %v", err)
	}
	insertTestProject(t, db, "perf-r3", 0, true, "")
	if _, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = 'perf-r3'`, dir); err != nil {
		t.Fatalf("point project at fixture workdir: %v", err)
	}
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)
	// Simulated spawns: Phase 2 must not launch real tick sessions. The pack
	// (Phase 1b) — the part these tests measure — is byte-identical.
	l.SetSimulation(1.0)
	return l, dir
}

// perf001r3InstallProbe installs a pending counter on BOTH packers whose
// freshness seam runs probe once per board read. A fresh counter (ttl 0 =
// cache nothing) guarantees the seam is reached on every pass instead of
// being served from the pending-count cache.
func perf001r3InstallProbe(t *testing.T, l *Loop, probe func()) {
	t.Helper()
	counter := NewPendingTaskCounter(0)
	counter.freshnessRead = func(workdir, boardPath string) FreshnessReport {
		probe()
		return FreshnessReport{} // empty report → fail-open: raw count stands
	}
	l.packer.SetPendingCounter(counter)
	l.multiPoolPacker.SetPendingCounter(counter)
}

// TestPERF001R3_EvaluateDoesNotHoldLoopLockAcrossPack is the red-proof for
// the evaluate() restructure. The probe runs INSIDE the board-freshness read
// — i.e. at the exact instant the live goroutine dump caught the write lock
// held (gitCommitInfo under evaluate()). TryLock reports whether mu is
// locked by ANYONE (Go's RWMutex is not reentrant), so a pass that still
// holds the write lock across the pack fails this probe by construction,
// with no timing assertion and no sleep to tune.
func TestPERF001R3_EvaluateDoesNotHoldLoopLockAcrossPack(t *testing.T) {
	l, _ := perf001r3Fixture(t)

	var probes, heldProbes int64
	perf001r3InstallProbe(t, l, func() {
		atomic.AddInt64(&probes, 1)
		if !l.mu.TryLock() {
			atomic.AddInt64(&heldProbes, 1)
			return
		}
		l.mu.Unlock()
	})

	l.evaluate()

	if got := atomic.LoadInt64(&probes); got == 0 {
		t.Fatal("the board-freshness seam never fired: the pack did not reach the per-board read, " +
			"so this test proved nothing (fixture broken, not code fixed)")
	}
	if got := atomic.LoadInt64(&heldProbes); got != 0 {
		t.Fatalf("evaluate() held the loop WRITE lock across %d of %d board-freshness reads — "+
			"/api/v1/status queues behind this (PERF-001-R3: the measured handler was parked in "+
			"sync.RWMutex.RLock inside ZeroSelectStats for the whole battery)", got, probes)
	}
}

// TestPERF001R3_OperatorsAnswerWhilePassIsMidPack pins the operator-visible
// contract the row's acceptance criteria state, at unit scale and
// deterministically: while an evaluation pass is parked INSIDE the pack (the
// seam blocks on a channel, so the pass is provably mid-battery), 20
// consecutive reads of every getter the /api/v1/status and /health handlers
// call must each answer immediately. Pre-fix, ZeroSelectStats() blocked on
// mu.RLock() until the pack finished, which is the 13.9-21.8s 504.
//
// The window is a handshake, not a sleep: the pass cannot proceed until the
// reads are done, so there is no race for the test to lose.
func TestPERF001R3_OperatorsAnswerWhilePassIsMidPack(t *testing.T) {
	l, _ := perf001r3Fixture(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	perf001r3InstallProbe(t, l, func() {
		once.Do(func() { close(entered) })
		<-release // park the pass inside the board-freshness battery
	})

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		l.evaluate()
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the evaluation pass never reached the board-freshness read")
	}

	// The pass is mid-pack now. The decision instant is already published
	// (Phase 1a), so a non-zero last_evaluation proves the pass is genuinely
	// in flight while the reads below are served.
	if l.LastEvalTime().IsZero() {
		close(release)
		<-passDone
		t.Fatal("LastEvalTime() is zero while the pass is mid-pack — the pass never started")
	}

	// AC(a) shape: 20 consecutive reads, no sample anywhere near the budget.
	for i := 0; i < 20; i++ {
		start := time.Now()
		// Exactly the loop getters the two operator surfaces call
		// (server.go:status → ZeroSelectStats/LastEvalTime/IsPaused/
		// GatewayErrorCount/GatewayResponseTimeout/GatewayTransientRetries/
		// AdmissionCounters/BoardUnchangedSkips/TickPushDisabled;
		// server.go:health → LastEvalTime/SpawnMethodCounts/GatewayErrorCount).
		_, _, _ = l.ZeroSelectStats()
		_ = l.LastEvalTime()
		_ = l.IsPaused()
		_ = l.GatewayResponseTimeout()
		_ = l.GatewayTransientRetries()
		_ = l.GatewayErrorCount()
		_ = l.AdmissionCounters()
		_, _ = l.SpawnMethodCounts()
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			close(release)
			<-passDone
			t.Fatalf("operator read #%d took %v while a pass was mid-pack — the read path is "+
				"queueing behind the pass again (PERF-001-R3 regression)", i, elapsed)
		}
	}

	close(release)
	select {
	case <-passDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the evaluation pass did not finish after the seam was released")
	}
}

// TestPERF001R3_ZeroSelectStatsDoesNotBlockOnLoopLock is the direct analogue
// of the SCHED-GAP-1621 LastEvalTime red-proof, for the getter the live
// goroutine dump actually named (loop.go:1746). A goroutine holds the write
// lock the way a mid-pack pass used to; the getter must answer anyway.
func TestPERF001R3_ZeroSelectStatsDoesNotBlockOnLoopLock(t *testing.T) {
	l := NewLoop(nil, time.Minute, time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)

	evAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l.mu.Lock()
	l.zeroSelectCount = 4
	l.zeroSelectEligible = 11
	l.lastZeroSelectEvent = evAt
	l.publishZeroSelect()
	l.mu.Unlock()

	release := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		l.mu.Lock()
		defer l.mu.Unlock()
		<-release // hold the write lock the way a mid-pack evaluate() does
	}()

	// Wait until the holder owns the lock (same probe as last_eval_lock_test):
	// the loop exits the first time TryLock FAILS, i.e. the convoy state is
	// established.
	deadline := time.Now().Add(5 * time.Second)
	for l.mu.TryLock() {
		l.mu.Unlock()
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("holder goroutine never acquired the lock")
		}
		time.Sleep(time.Millisecond)
	}

	type stats struct {
		c, e int
		last string
	}
	got := make(chan stats, 1)
	go func() {
		c, e, last := l.ZeroSelectStats()
		got <- stats{c, e, last}
	}()

	select {
	case v := <-got:
		if v.c != 4 || v.e != 11 || v.last != evAt.Format(time.RFC3339) {
			t.Fatalf("ZeroSelectStats() = (%d, %d, %q), want (4, 11, %q)",
				v.c, v.e, v.last, evAt.Format(time.RFC3339))
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("ZeroSelectStats() blocked while Loop.mu was write-held — /api/v1/status " +
			"convoys behind evaluate() again (PERF-001-R3 regression)")
	}
	close(release)
	<-holderDone
}

// TestPERF001R3_ZeroSelectStatsMirrorMatchesFields proves the lock-free
// getter cannot drift from the state it mirrors: every mutation path of the
// GAP-043 trio (the counting path, both early resets, the explicit reset)
// publishes, so the getter's three values equal what a locked read of the
// fields would return.
func TestPERF001R3_ZeroSelectStatsMirrorMatchesFields(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "perf-r3-eligible", 900, true, "")
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)

	assertMirrorMatchesFields := func(t *testing.T, stage string) {
		t.Helper()
		l.mu.RLock()
		wantCount, wantEligible := l.zeroSelectCount, l.zeroSelectEligible
		wantEvent := l.lastZeroSelectEvent
		l.mu.RUnlock()

		gotCount, gotEligible, gotLast := l.ZeroSelectStats()
		if gotCount != wantCount || gotEligible != wantEligible {
			t.Fatalf("%s: ZeroSelectStats() = (%d, %d), locked fields = (%d, %d)",
				stage, gotCount, gotEligible, wantCount, wantEligible)
		}
		wantLast := ""
		if !wantEvent.IsZero() {
			wantLast = wantEvent.UTC().Format(time.RFC3339)
		}
		if gotLast != wantLast {
			t.Fatalf("%s: ZeroSelectStats() lastEvent = %q, locked field renders %q",
				stage, gotLast, wantLast)
		}
	}

	assertMirrorMatchesFields(t, "fresh loop")

	// Counting path: an eligible project present, two consecutive zero
	// selects (the GAP-043 threshold) → count 2, eligible 1, event stamped.
	l.noteZeroSelect(time.Now(), map[string]bool{})
	l.noteZeroSelect(time.Now(), map[string]bool{})
	assertMirrorMatchesFields(t, "after two zero selects")

	gotCount, gotEligible, gotLast := l.ZeroSelectStats()
	if gotCount != 2 || gotEligible != 1 {
		t.Fatalf("after two zero selects ZeroSelectStats() = (%d, %d), want (2, 1)", gotCount, gotEligible)
	}
	if gotLast == "" {
		t.Fatal("after a zero-select event the mirror reports no last event")
	}

	// Saturation reset (the first early-return path): a full slot pool resets
	// the counter without emitting — the mirror must follow.
	l.noteZeroSelect(time.Now(), map[string]bool{"a": true, "b": true, "c": true, "d": true})
	assertMirrorMatchesFields(t, "after saturation reset")
	if c, _, _ := l.ZeroSelectStats(); c != 0 {
		t.Fatalf("after a saturation reset ZeroSelectStats() count = %d, want 0", c)
	}

	// Explicit reset when a selection did occur.
	l.resetZeroSelect()
	assertMirrorMatchesFields(t, "after resetZeroSelect")
}
