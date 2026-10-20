package scheduler

// PERF-001-R2 (b): the eval A/B, in the form PERF-001-R3 left it.
//
// The row's A/B was written to attribute a lock convoy: sample the status read
// path while evaluate() is idle vs while it holds Loop.mu. PERF-001-R3 then
// removed the coupling itself — passes serialize on evalMu, Loop.mu is held
// only for O(1) state updates, and ZeroSelectStats / LastEvalTime / IsPaused
// read atomics — so the A/B's answer is the row's own sanctioned alternative,
// "no lock wait detected", and these tests are what keeps it that way:
//
//	TestPERF001R3StatusReadPathIsLockFree — the guarantee, deterministically.
//	With Loop.mu write-held, the exact loop reads /api/v1/status makes must
//	still return promptly, and a TryRLock proves the premise (the write lock
//	really is held, so an RLock reader really would have blocked). Re-couple
//	the read path to mu and this fails.
//
//	TestPERF001R2EvalPassAB — the row's two-state measurement. Arm A samples
//	the read path while no pass is running; arm B samples it while a real
//	evaluate() pass is inside its decision window (driven through the
//	production ForceEvaluate entry point). Neither arm may approach the
//	window's own duration; the window and its stage split are reported from
//	the pass observer. Skipped under -short (it seeds a production-shaped
//	project list).

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// perf001r2QuietLogs silences the package's stdout logging for the duration of
// the A/B and returns a restore func. A pass emits one grep-stable ADMIT line
// per eligible lane, so at fixture scale the pass spends most of its window
// writing log lines to stderr — real work, but not what this A/B measures, and
// it would otherwise make the test's runtime a function of log I/O.
func perf001r2QuietLogs() func() {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	return func() { log.SetOutput(prev) }
}

// perf001r2ABProjects sizes the fixture the A/B drives: 400 enabled,
// cooldown-fresh lanes make the decision window long enough (tens of ms at
// least) for the contended arm to be sampled inside it. The invariant below
// fails loudly if a future change shrinks the window below measurability.
const perf001r2ABProjects = 400

// seedPerf001r2Projects inserts n enabled, cooldown-fresh projects so a pass
// scores every one of them but selects none:
//
//   - weight 10 with the loop's budget of 1 keeps the greedy pack empty
//     (used+weight > budget), so the pass cannot enqueue a real spawn;
//   - last_tick_completed = now with cooldown_s = 86400 keeps every project
//     inside its cooldown, so GAP-011's overdue force-select cannot rescue
//     one either.
//
// The work the pass does is therefore the measurement-relevant part only.
func seedPerf001r2Projects(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("seed tx: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, enabled, last_tick_completed, created_at, updated_at)
		VALUES (?, 'url', ?, 10, 5, 86400, 1, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("ab-lane-%06d", i), fmt.Sprintf("/nonexistent/ab-lane-%06d", i), now, now, now); err != nil {
			t.Fatalf("seed project %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("seed stmt close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// perf001r2Pct returns the p-th percentile (0..1) of ds, or 0 for an empty
// slice. The slice is copied: the caller's ordering is evidence.
func perf001r2Pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func perf001r2Max(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

// perf001r2ReadPath is the exact composite of loop reads /api/v1/status makes
// (ZeroSelectStats for the GAP-043 block, LastEvalTime for last_evaluation,
// IsPaused for paused). It is the read path PERF-001-R2 measured convoys and
// PERF-001-R3 made lock-free.
func perf001r2ReadPath(loop *Loop) {
	loop.ZeroSelectStats()
	_ = loop.LastEvalTime()
	_ = loop.IsPaused()
}

// TestPERF001R3StatusReadPathIsLockFree is the deterministic half: hold the
// write lock, prove the premise with TryRLock, and require the read path's
// three reads to return promptly regardless.
func TestPERF001R3StatusReadPathIsLockFree(t *testing.T) {
	db := newTestDB(t)
	defer func() { _ = db.Close() }()
	loop := NewLoop(db, time.Minute, time.Hour, 10, 1, 5)

	loop.mu.Lock()
	// Premise, deterministically: with the write lock held, an RLock reader
	// would block — so if the read path takes mu, this test fails.
	if loop.mu.TryRLock() {
		loop.mu.RUnlock()
		loop.mu.Unlock()
		t.Fatal("TryRLock succeeded while the write lock was held — the premise of this test is broken")
	}
	elapsed := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		perf001r2ReadPath(loop)
		elapsed <- time.Since(started)
	}()
	time.Sleep(50 * time.Millisecond) // let the goroutine run while the lock is held
	loop.mu.Unlock()

	got := <-elapsed
	if got > 50*time.Millisecond {
		t.Fatalf("status read path took %s while Loop.mu was write-held (an RLock would have blocked) — the read path is coupled to the pass again (PERF-001-R3 regression)", got)
	}
	attr := loop.LockAttribution()
	if attr.ReadPathLockSite == "" {
		t.Fatal("lock_attribution.read_path_lock_site is empty — a consumer cannot tell 'lock-free' from 'not measured'")
	}
	t.Logf("PERF-001-R3 read-path lock-freedom: read path=%s while the write lock was held; site=%q", got, attr.ReadPathLockSite)
}

// TestPERF001R2EvalPassAB measures the endpoint-side read path in its two
// states — no pass running (arm A) vs a real evaluate() pass inside its
// decision window (arm B) — and requires both to stay far below the window's
// own duration. The window and its stage split are reported from the pass
// observer, so "no convoy" and "what the pass costs" come from one run.
func TestPERF001R2EvalPassAB(t *testing.T) {
	if testing.Short() {
		t.Skip("PERF-001-R2 A/B seeds a production-shaped project list; run without -short")
	}
	db := newTestDB(t)
	defer func() { _ = db.Close() }()
	seedPerf001r2Projects(t, db, perf001r2ABProjects)
	loop := NewLoop(db, time.Minute, time.Hour, 10, 1, 5)
	defer perf001r2QuietLogs()()

	type sample struct {
		wait     time.Duration
		inFlight bool
	}
	var (
		mu      sync.Mutex
		samples []sample
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			inFlight := loop.EvalInFlight()
			started := time.Now()
			perf001r2ReadPath(loop)
			w := time.Since(started)
			mu.Lock()
			samples = append(samples, sample{wait: w, inFlight: inFlight})
			mu.Unlock()
		}
	}()

	time.Sleep(20 * time.Millisecond) // arm A: idle samples
	beforeSamples := loop.LockAttribution().EvalPass.Samples
	loop.ForceEvaluate()
	// Wait for the forced pass to open its window, then for it to close.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !loop.EvalInFlight() {
		time.Sleep(50 * time.Microsecond)
	}
	sawWindow := loop.EvalInFlight()
	for time.Now().Before(deadline) && loop.EvalInFlight() {
		time.Sleep(50 * time.Microsecond)
	}
	for i := 0; i < 20000 && loop.LockAttribution().EvalPass.Samples == beforeSamples; i++ {
		time.Sleep(50 * time.Microsecond)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	armA := make([]time.Duration, 0, len(samples))
	armB := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		if s.inFlight {
			armB = append(armB, s.wait)
		} else {
			armA = append(armA, s.wait)
		}
	}
	mu.Unlock()

	after := loop.LockAttribution()
	window := time.Duration(after.EvalPass.MaxNs)

	// Report before asserting: a failed invariant must never hide the numbers.
	t.Logf("PERF-001-R2 eval-pass A/B: pass window=%s | idle n=%d p50=%s p99=%s max=%s | in-window n=%d p50=%s p99=%s max=%s | stages=%s",
		window, len(armA), perf001r2Pct(armA, 0.50), perf001r2Pct(armA, 0.99), perf001r2Max(armA),
		len(armB), perf001r2Pct(armB, 0.50), perf001r2Pct(armB, 0.99), perf001r2Max(armB),
		perf001r2StageSummary(after.Stages))

	if !sawWindow {
		t.Fatal("ForceEvaluate never opened an evaluate() window (eval_in_flight stayed false) — the A/B has no contended arm")
	}
	if len(armA) < 10 {
		t.Fatalf("arm A collected %d idle samples, want >= 10 (sampler did not run)", len(armA))
	}
	if len(armB) == 0 {
		t.Fatal("arm B collected no sample inside the pass window")
	}
	if after.EvalPass.Samples-beforeSamples != 1 {
		t.Fatalf("pass observer recorded %d window(s), want exactly 1", after.EvalPass.Samples-beforeSamples)
	}
	if after.EvalInFlight {
		t.Fatal("eval_in_flight still true after the pass closed its window")
	}
	if window < 20*time.Millisecond {
		t.Fatalf("the pass window was only %s — the %d-lane fixture is too small for the A/B; raise perf001r2ABProjects", window, perf001r2ABProjects)
	}
	if len(after.Stages) == 0 {
		t.Fatalf("stage attribution recorded nothing for a pass that ran: %+v", after)
	}
	// THE assertion: PERF-001-R3's guarantee, measured. A read that lands
	// inside the pass window must not track the window — if the read path is
	// ever re-coupled to the pass, arm B jumps to the window's order of
	// magnitude and this fails.
	if got := perf001r2Max(armB); got >= window/2 {
		t.Fatalf("read path max inside the pass window = %s, want far below half the window (%s): the read path convoys behind evaluate() again", got, window/2)
	}
	if got := perf001r2Max(armA); got >= window/2 {
		t.Fatalf("idle arm max = %s is not below half the window (%s) — the read path is slow for another reason", got, window/2)
	}
}

// perf001r2StageSummary renders the stage attribution as
// "stage=total/max" pairs, longest total first: the row's "which sub-call
// spends the window" in one line.
func perf001r2StageSummary(stages []EvalStageStats) string {
	cp := append([]EvalStageStats(nil), stages...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].TotalNs > cp[j].TotalNs })
	parts := make([]string, 0, len(cp))
	for _, s := range cp {
		parts = append(parts, fmt.Sprintf("%s=%s/%s", s.Stage,
			time.Duration(s.TotalNs).Round(time.Millisecond),
			time.Duration(s.MaxNs).Round(time.Millisecond)))
	}
	return strings.Join(parts, ",")
}
