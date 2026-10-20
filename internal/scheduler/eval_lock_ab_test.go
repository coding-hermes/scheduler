package scheduler

// PERF-001-R2 (b): the eval-lock A/B.
//
// The row: /api/v1/status answers p50 326ms / p99 ~350ms against PERF-001's
// recorded PASS bar (p99 < 100ms). The handler profile is CPU:wall =
// 47ms:326ms — the request spends ~86% of its life NOT computing — and the
// goroutine dump taken at the stall proved Loop.evaluate() was the write-lock
// holder. The question the row asks is the A/B: does the READ path actually
// wait on that lock (idle vs immediately after an evaluate pass), and by how
// much — or is the wait somewhere else entirely?
//
// The read path's only Loop.mu site is ZeroSelectStats() (the GAP-043 block
// the status handler renders). These tests measure exactly that acquire in
// two states, with the REAL evaluate() pass as the contended state:
//
//	TestPERF001R2ReadPathWaitsBehindWriteHold — controlled hold: a 120ms
//	write hold must produce a ~120ms read wait. This isolates the lock's cost
//	from the pass's composition, and fails loudly if the read path ever stops
//	taking Loop.mu (the "no lock wait detected" arm of the row).
//
//	TestPERF001R2EvalLockAB — the real thing: sample the read path while the
//	loop is idle (arm A) and while a production-shaped evaluate() pass holds
//	the write lock (arm B), report p50/p99/max for both, and require the
//	contended arm to track the pass's measured hold. Skipped under -short
//	because it seeds a production-shaped project list.

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
// the A/B and returns a restore func. evaluate() emits one grep-stable ADMIT
// line per eligible lane WHILE HOLDING the write lock, so at fixture scale the
// pass spends most of its hold writing log lines to stderr (measured: 5.5s and
// 9.2s at 8000 lanes with the logs on). That is real work the pass does under
// the lock, but it is not what this A/B measures — the question here is
// whether the READ path queues behind the lock — and it would otherwise make
// the test's runtime a function of log I/O. The report committed with this
// change carries both numbers.
func perf001r2QuietLogs() func() {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	return func() { log.SetOutput(prev) }
}

// perf001r2ABProjects sizes the fixture the A/B drives. The pass cost (and so
// the write-lock hold the read path queues behind) is linear in this number.
// It is deliberately modest: the hold only has to be long enough for a
// sampler to be caught behind it (tens of milliseconds), and a bigger fixture
// turns the test into a load test of the pass itself. The invariant below
// fails loudly if a future change to the pick path makes this fixture too
// small to hold the lock measurably.
const perf001r2ABProjects = 400

// perf001r2Samplers is how many goroutines hammer the read path. One is not
// enough: a single sampler that enters the RLock just before a pass starts is
// consumed for the WHOLE pass (that is the phenomenon under test), and then
// every later iteration it runs lands after the pass ended.
const perf001r2Samplers = 4

// seedPerf001r2Projects inserts n enabled, cooldown-fresh projects so a pass
// scores every one of them but selects none:
//
//   - weight 10 with the loop's budget of 1 keeps the greedy pack empty
//     (used+weight > budget), so the pass cannot enqueue a real spawn;
//   - last_tick_completed = now with cooldown_s = 86400 keeps every project
//     inside its cooldown, so GAP-011's overdue force-select cannot rescue
//     one either.
//
// The work the pass does is therefore the measurement-relevant part only:
// scan + score + sort every project under the write lock, then take the
// zero-select early return.
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

// TestPERF001R2ReadPathWaitsBehindWriteHold is the controlled-hold half of the
// A/B: hold Loop.mu for a known 120ms exactly the way evaluate() does, and
// require the status read path's acquire to pay for it. Deterministic, no
// fixture, and it is the arm that fails if a later change removes the read
// path's Loop.mu dependency (then the answer to the row becomes "no lock wait
// detected here", which must be a deliberate, visible change).
func TestPERF001R2ReadPathWaitsBehindWriteHold(t *testing.T) {
	db := newTestDB(t)
	defer func() { _ = db.Close() }()
	loop := NewLoop(db, time.Minute, time.Hour, 10, 1, 5)

	// Hold Loop.mu for the full `hold` window — the same write lock
	// evaluate() takes and holds across its Phase 1.
	const hold = 120 * time.Millisecond
	loop.mu.Lock()
	holdStart := time.Now()
	started := make(chan struct{})
	waited := make(chan time.Duration, 1)
	go func() {
		close(started)
		acquire := time.Now()
		// ZeroSelectStats is the ONLY Loop.mu site on /api/v1/status.
		loop.ZeroSelectStats()
		waited <- time.Since(acquire)
	}()
	<-started
	for time.Since(holdStart) < hold {
		time.Sleep(time.Millisecond)
	}
	loop.mu.Unlock()

	w := <-waited
	if w < hold/2 {
		t.Fatalf("status read path waited %s behind a %s write hold — the read path no longer convoys behind evaluate()'s lock (PERF-001-R2 attribution is now 'no lock wait')", w, hold)
	}

	attr := loop.LockAttribution()
	if attr.StatusReadLock.Samples < 1 {
		t.Fatalf("read-lock observer recorded no samples: %+v", attr.StatusReadLock)
	}
	if attr.StatusReadLock.Blocked < 1 {
		t.Fatalf("read-lock observer recorded no BLOCKED sample for a %s write hold: %+v", hold, attr.StatusReadLock)
	}
	if got := time.Duration(attr.StatusReadLock.MaxNs); got < hold/2 {
		t.Fatalf("read-lock observer max = %s, want >= %s (the measured wait must match the hold it waited for)", got, hold/2)
	}
	t.Logf("PERF-001-R2 controlled hold=%s read-wait=%s observed_max=%s blocked=%d",
		hold, w, time.Duration(attr.StatusReadLock.MaxNs), attr.StatusReadLock.Blocked)
}

// TestPERF001R2EvalLockAB measures the endpoint-side lock component in its two
// states — the pass idle, and a real evaluate() pass holding the write lock —
// using the same acquire the status handler makes. Arm A's samples are taken
// before the pass starts, arm B's while it holds the lock; the write hold is
// measured independently by the eval-hold observer, so the assertion is
// "arm B tracked the pass's own measured hold", not a hardcoded constant.
func TestPERF001R2EvalLockAB(t *testing.T) {
	if testing.Short() {
		t.Skip("PERF-001-R2 A/B seeds a production-shaped project list; run without -short")
	}
	db := newTestDB(t)
	defer func() { _ = db.Close() }()
	seedPerf001r2Projects(t, db, perf001r2ABProjects)
	loop := NewLoop(db, time.Minute, time.Hour, 10, 1, 5)
	defer perf001r2QuietLogs()()

	type sample struct {
		at   time.Time
		wait time.Duration
	}
	var (
		mu      sync.Mutex
		samples []sample
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < perf001r2Samplers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				at := time.Now()
				loop.ZeroSelectStats() // the status read path's only Loop.mu site
				w := time.Since(at)
				mu.Lock()
				samples = append(samples, sample{at: at, wait: w})
				mu.Unlock()
			}
		}()
	}

	time.Sleep(20 * time.Millisecond) // arm A: idle samples
	passStart := time.Now()
	loop.evaluate()
	passEnd := time.Now()
	close(stop)
	wg.Wait()

	// Classify by OVERLAP, not by start time: a sample that began just before
	// the pass took the lock and then waited for the pass is arm B (it is the
	// clearest evidence of the convoy there is), while a sample that finished
	// before the pass began is arm A.
	mu.Lock()
	armA := make([]time.Duration, 0, len(samples))
	armB := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		switch {
		case !s.at.Add(s.wait).After(passStart):
			armA = append(armA, s.wait)
		case s.at.Before(passEnd):
			armB = append(armB, s.wait)
		}
	}
	mu.Unlock()

	attr := loop.LockAttribution()
	hold := passEnd.Sub(passStart)
	observedHold := time.Duration(attr.EvalHold.MaxNs)

	// Report before asserting: a failed invariant must never hide the numbers.
	t.Logf("PERF-001-R2 eval-lock A/B: pass hold=%s (observed %s) | idle n=%d p50=%s p99=%s max=%s | contended n=%d p50=%s p99=%s max=%s | read-lock blocked=%d max=%s | stages=%s",
		hold, observedHold,
		len(armA), perf001r2Pct(armA, 0.50), perf001r2Pct(armA, 0.99), perf001r2Max(armA),
		len(armB), perf001r2Pct(armB, 0.50), perf001r2Pct(armB, 0.99), perf001r2Max(armB),
		attr.StatusReadLock.Blocked, time.Duration(attr.StatusReadLock.MaxNs),
		perf001r2StageSummary(attr.Stages))

	if len(armA) < 10 {
		t.Fatalf("arm A collected %d idle samples, want >= 10 (samplers did not run)", len(armA))
	}
	if len(armB) == 0 {
		t.Fatalf("arm B collected no sample overlapping the %s evaluate() pass", hold)
	}
	if hold < 20*time.Millisecond {
		t.Fatalf("the evaluate() pass held the write lock for only %s — the %d-project fixture no longer holds it long enough for the A/B; raise perf001r2ABProjects", hold, perf001r2ABProjects)
	}
	if attr.EvalHold.Samples != 1 {
		t.Fatalf("eval-hold observer recorded %d holds for one pass, want 1", attr.EvalHold.Samples)
	}
	if attr.EvalInFlight {
		t.Fatalf("eval_in_flight still true after evaluate() returned")
	}
	if len(attr.Stages) == 0 {
		t.Fatalf("stage attribution recorded nothing for a pass that ran: %+v", attr)
	}
	// The lock half of the attribution: a read sample overlapping the pass
	// must have waited for most of the pass's own hold.
	if blockedMax := perf001r2Max(armB); blockedMax < hold/2 {
		t.Fatalf("contended arm max wait = %s, want >= half the pass hold (%s): the read path did not convoy behind evaluate()", blockedMax, hold/2)
	}
	if idleMax := perf001r2Max(armA); idleMax >= hold/2 {
		t.Fatalf("idle arm max wait = %s is not distinguishable from the contended arm (%s hold) — the A/B has no signal", idleMax, hold)
	}
	if attr.StatusReadLock.Blocked < 1 {
		t.Fatalf("read-lock observer never recorded a blocked sample during the pass: %+v", attr.StatusReadLock)
	}
	if observedHold < hold/2 {
		t.Fatalf("eval-hold observer max = %s, want >= half the wall hold %s", observedHold, hold/2)
	}
}

// perf001r2StageSummary renders the stage attribution as
// "stage=total/max" pairs, longest total first: the row's "which sub-call
// holds the lock longest" in one line.
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
