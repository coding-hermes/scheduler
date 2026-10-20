package api

// PERF-001-R2 (b) — the endpoint-level eval-lock A/B.
//
// The row's exact driver is /api/v1/status sampled while the scheduler's
// evaluate() pass is idle vs immediately after a pass:
//
//	p50 326ms / p99 ~350ms against PERF-001's PASS bar (p99 < 100ms)
//	CPU:wall = 47ms:326ms — the request is WAITING for ~86% of its life
//
// This test measures that difference on the real handler and attributes its
// lock half:
//
//	arm A (idle)      — /api/v1/status latency with no pass in flight
//	arm B (contended) — /api/v1/status latency sampled while a real
//	                    evaluate() pass holds Loop.mu (the pass is triggered
//	                    through the production entry point, ForceEvaluate, and
//	                    detected through the exposed eval_in_flight flag)
//
// Assertions are on the mechanism, not on a wall-clock constant: arm B's worst
// request must track the pass's own measured write-lock hold, the read-lock
// observer must have recorded a blocked sample, and arm A must be
// distinguishable from arm B. The fixture (thousands of enabled projects, a
// budget below every weight) makes the pass hold the lock long enough to
// overlap a request deterministically, without enqueueing a single spawn.

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// perf001r2QuietLogs silences the process log for the duration of the A/B and
// returns a restore func: the evaluate() pass emits one ADMIT line per eligible
// lane while holding the write lock, which at fixture scale is thousands of
// stderr writes that dominate the hold and swamp the test output. See the
// PERF-001-R2 report for the measured with/without numbers.
func perf001r2QuietLogs() func() {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	return func() { log.SetOutput(prev) }
}

// perf001r2APIProjects sizes the fixture that lengthens the pass. The pass's
// write-lock hold is linear in this number (one row scan + one urgency score +
// the shared sort per project). Kept modest so the pass holds the lock long
// enough for a request to be caught behind it without turning the test into a
// load test of the pass itself.
const perf001r2APIProjects = 400

// perf001r2Seed inserts n enabled, cooldown-fresh projects so the pass scores
// every one of them and selects none: weight 10 against budget 1 keeps the
// greedy pack empty, and a fresh last_tick_completed inside a 24h cooldown
// keeps GAP-011's overdue force-select from rescuing one. No spawn can be
// enqueued by the measured pass.
func perf001r2Seed(t *testing.T, db *sql.DB, n int) {
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

// perf001r2StatusCall runs the real handler once and returns its latency.
func perf001r2StatusCall(t *testing.T, srv *Server) (time.Duration, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	started := time.Now()
	srv.status(rec, req)
	return time.Since(started), rec.Code
}

func perf001r2Stats(ds []time.Duration) (p50, p99, max time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	pick := func(p float64) time.Duration {
		idx := int(float64(len(cp)-1) * p)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return pick(0.50), pick(0.99), cp[len(cp)-1]
}

func TestPERF001R2StatusLockAB(t *testing.T) {
	if testing.Short() {
		t.Skip("PERF-001-R2 A/B seeds a production-shaped project list; run without -short")
	}
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	perf001r2Seed(t, db, perf001r2APIProjects)

	// budget 1 < every project weight ⇒ the pass selects nothing and cannot
	// spawn; the write lock is still held across the whole pick.
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 1, 5)
	loop.SetNoExecFallback(true)
	defer loop.Stop()
	defer perf001r2QuietLogs()()
	srv := NewServer(db, loop)
	// The A/B isolates the LOCK wait, so the request budget is deliberately
	// generous: under the shipped 5s read deadline a long pass can also blow
	// the request's step budget on the shared single-writer connection
	// (DATABASE-BUSY/pool wait), which is a different finding with its own
	// test (SCHED-GAP-1575-B). The lock wait itself is not context-aware —
	// an RLock does not observe a deadline — so the deadline would otherwise
	// truncate the measurement instead of failing it.
	srv.SetReadTimeout(60 * time.Second)

	// ── arm A: idle ────────────────────────────────────────────────────────
	var armA []time.Duration
	for i := 0; i < 20; i++ {
		if loop.EvalInFlight() {
			t.Fatalf("arm A sample %d started with an evaluate() pass in flight — the idle arm is contaminated", i)
		}
		d, code := perf001r2StatusCall(t, srv)
		if code != http.StatusOK {
			t.Fatalf("idle /api/v1/status = %d, want 200", code)
		}
		armA = append(armA, d)
	}

	// ── arm B: a real evaluate() pass in flight ────────────────────────────
	before := loop.LockAttribution()
	loop.ForceEvaluate()
	var armB []time.Duration
	deadline := time.Now().Add(20 * time.Second)
	sawInFlight := false
	for time.Now().Before(deadline) {
		if loop.EvalInFlight() {
			sawInFlight = true
			d, code := perf001r2StatusCall(t, srv)
			if code != http.StatusOK {
				t.Fatalf("contended /api/v1/status = %d, want 200", code)
			}
			armB = append(armB, d)
			continue
		}
		if sawInFlight {
			break
		}
		time.Sleep(50 * time.Microsecond) // wait for the forced pass to take the lock
	}
	if !sawInFlight {
		t.Fatal("ForceEvaluate never reported eval_in_flight=true — the A/B has no contended arm")
	}
	// Let the pass finish before reading the observers.
	for i := 0; i < 20000 && loop.EvalInFlight(); i++ {
		time.Sleep(50 * time.Microsecond)
	}
	after := loop.LockAttribution()

	hold := time.Duration(after.EvalHold.MaxNs)
	p50A, p99A, maxA := perf001r2Stats(armA)
	p50B, p99B, maxB := perf001r2Stats(armB)

	// The lock half of the attribution: the read path paid part of the pass's
	// hold, and the observer recorded it.
	lockBlockedDelta := after.StatusReadLock.Blocked - before.StatusReadLock.Blocked
	lockMaxDelta := time.Duration(after.StatusReadLock.MaxNs) // max is monotonic; before was 0 for a fresh loop

	// Report before asserting: a failed invariant must never hide the numbers.
	t.Logf("PERF-001-R2 /api/v1/status eval-lock A/B: pass hold=%s | idle n=%d p50=%s p99=%s max=%s | contended n=%d p50=%s p99=%s max=%s | read-lock blocked=%d max=%s | stages=%s",
		hold, len(armA), p50A, p99A, maxA, len(armB), p50B, p99B, maxB, lockBlockedDelta, lockMaxDelta,
		perf001r2StageSummary(after.Stages))

	if after.EvalHold.Samples-before.EvalHold.Samples != 1 {
		t.Fatalf("eval-hold observer recorded %d pass(es), want exactly 1", after.EvalHold.Samples-before.EvalHold.Samples)
	}
	if hold < 20*time.Millisecond {
		t.Fatalf("the evaluate() pass held the write lock for only %s — the %d-project fixture is too small for the A/B; raise perf001r2APIProjects", hold, perf001r2APIProjects)
	}
	if len(after.Stages) == 0 {
		t.Fatalf("stage attribution recorded nothing for a pass that ran: %+v", after)
	}
	if lockBlockedDelta < 1 {
		t.Fatalf("read-lock observer recorded %d blocked sample(s) across the pass, want >= 1: %+v", lockBlockedDelta, after.StatusReadLock)
	}
	if lockMaxDelta < hold/2 {
		t.Fatalf("read-lock observer max = %s, want >= half the pass hold (%s)", lockMaxDelta, hold/2)
	}
	if maxB < hold/2 {
		t.Fatalf("contended /api/v1/status max = %s, want >= half the pass hold (%s): the endpoint did not wait for evaluate()", maxB, hold/2)
	}
	if maxA >= hold/2 {
		t.Fatalf("idle /api/v1/status max = %s is not below half the pass hold (%s) — the two arms are indistinguishable", maxA, hold/2)
	}
}

// perf001r2StageSummary renders the stage attribution as "stage=total/max"
// pairs, longest total first: the row's "which sub-call holds the lock
// longest" in one line.
func perf001r2StageSummary(stages []scheduler.EvalStageStats) string {
	cp := append([]scheduler.EvalStageStats(nil), stages...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].TotalNs > cp[j].TotalNs })
	parts := make([]string, 0, len(cp))
	for _, s := range cp {
		parts = append(parts, fmt.Sprintf("%s=%s/%s", s.Stage,
			time.Duration(s.TotalNs).Round(time.Millisecond),
			time.Duration(s.MaxNs).Round(time.Millisecond)))
	}
	return strings.Join(parts, ",")
}
