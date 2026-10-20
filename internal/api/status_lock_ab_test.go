package api

// PERF-001-R2 (b) — the endpoint-level A/B, in the form PERF-001-R3 left it.
//
// The row's driver is /api/v1/status sampled while the scheduler is idle vs
// while an evaluate() pass is running:
//
//	p50 326 ms / p99 ~350 ms against PERF-001's bar (p99 < 100 ms)
//	CPU:wall = 47 ms : 326 ms — the request was WAITING for ~86 % of its life
//
// PERF-001-R2 attributed that wait; PERF-001-R3 removed it (the pass no longer
// holds Loop.mu across the pack, and the status read path has no mu site left).
// This test measures the endpoint in both states — arm A idle, arm B sampled
// while a real pass window is open — and requires arm B to stay far below the
// window's own duration. A read that lands during a pass must not track it.
//
// It also pins the endpoint surface the row asked for: the payload carries
// `lock_attribution` (with the read path's lock discipline stated as a fact)
// and `block_profile` (the armed profiler).

import (
	"database/sql"
	"encoding/json"
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

// perf001r2QuietLogs silences the process log for the duration of the A/B:
// a pass emits one ADMIT line per eligible lane, which at fixture scale is
// thousands of stderr writes that dominate the window and swamp the output.
func perf001r2QuietLogs() func() {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	return func() { log.SetOutput(prev) }
}

// perf001r2APIProjects sizes the fixture that lengthens the pass window.
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

// perf001r2StatusCall runs the real handler once and returns its latency, the
// status code and the decoded body.
func perf001r2StatusCall(t *testing.T, srv *Server) (time.Duration, int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	started := time.Now()
	srv.status(rec, req)
	elapsed := time.Since(started)
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status body is not a JSON object: %v", err)
		}
	}
	return elapsed, rec.Code, body
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

func TestPERF001R2StatusPassAB(t *testing.T) {
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
	// spawn, while still doing all of its decision work.
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 1, 5)
	loop.SetNoExecFallback(true)
	defer loop.Stop()
	defer perf001r2QuietLogs()()
	srv := NewServer(db, loop)
	// The A/B measures whether a request tracks the pass WINDOW, so the
	// request budget is deliberately generous: under the shipped 5s read
	// deadline a long pass can also blow the request's step budget on the
	// shared single-writer connection, which is a different finding with its
	// own test (SCHED-GAP-1575-B).
	srv.SetReadTimeout(60 * time.Second)

	// ── arm A: idle ────────────────────────────────────────────────────────
	var armA []time.Duration
	var surfaceBody map[string]any
	for i := 0; i < 20; i++ {
		if loop.EvalInFlight() {
			t.Fatalf("arm A sample %d started with a pass window open — the idle arm is contaminated", i)
		}
		d, code, body := perf001r2StatusCall(t, srv)
		if code != http.StatusOK {
			t.Fatalf("idle /api/v1/status = %d, want 200", code)
		}
		armA = append(armA, d)
		surfaceBody = body
	}

	// ── arm B: sampled while a real pass window is open ────────────────────
	before := loop.LockAttribution()
	loop.ForceEvaluate()
	var armB []time.Duration
	sawWindow := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if loop.EvalInFlight() {
			sawWindow = true
			d, code, _ := perf001r2StatusCall(t, srv)
			if code != http.StatusOK {
				t.Fatalf("in-window /api/v1/status = %d, want 200", code)
			}
			armB = append(armB, d)
			continue
		}
		if sawWindow {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	for i := 0; i < 20000 && loop.EvalInFlight(); i++ {
		time.Sleep(50 * time.Microsecond)
	}
	after := loop.LockAttribution()

	window := time.Duration(after.EvalPass.MaxNs)
	p50A, p99A, maxA := perf001r2Stats(armA)
	p50B, p99B, maxB := perf001r2Stats(armB)

	// Report before asserting: a failed invariant must never hide the numbers.
	t.Logf("PERF-001-R2 /api/v1/status pass A/B: window=%s | idle n=%d p50=%s p99=%s max=%s | in-window n=%d p50=%s p99=%s max=%s | stages=%s",
		window, len(armA), p50A, p99A, maxA, len(armB), p50B, p99B, maxB,
		perf001r2StageSummary(after.Stages))

	// The endpoint surface the row asked for.
	if surfaceBody == nil {
		t.Fatal("no status body captured")
	}
	attrRaw, ok := surfaceBody["lock_attribution"]
	if !ok {
		t.Fatal("/api/v1/status carries no lock_attribution block")
	}
	attr, ok := attrRaw.(map[string]any)
	if !ok {
		t.Fatalf("lock_attribution is %T, want an object", attrRaw)
	}
	if site, _ := attr["read_path_lock_site"].(string); site == "" {
		t.Fatal("lock_attribution.read_path_lock_site is empty — a consumer cannot tell 'lock-free' from 'not measured'")
	} else if !strings.Contains(site, "lock-free") {
		t.Fatalf("read_path_lock_site = %q, want it to state the lock-free discipline", site)
	}
	if _, ok := attr["eval_pass"]; !ok {
		t.Fatal("lock_attribution carries no eval_pass block")
	}
	if _, ok := attr["eval_in_flight"]; !ok {
		t.Fatal("lock_attribution carries no eval_in_flight flag")
	}
	bpRaw, ok := surfaceBody["block_profile"].(map[string]any)
	if !ok {
		t.Fatal("/api/v1/status carries no block_profile block")
	}
	if _, ok := bpRaw["armed"]; !ok {
		t.Fatal("block_profile carries no armed flag")
	}
	if env, _ := bpRaw["env"].(string); env != scheduler.BlockProfileRateEnv {
		t.Fatalf("block_profile.env = %q, want %q", env, scheduler.BlockProfileRateEnv)
	}

	// The measured contract: a request inside the window must not track it.
	if !sawWindow {
		t.Fatal("ForceEvaluate never opened a pass window (eval_in_flight stayed false) — the A/B has no contended arm")
	}
	if after.EvalPass.Samples-before.EvalPass.Samples != 1 {
		t.Fatalf("pass observer recorded %d window(s), want exactly 1", after.EvalPass.Samples-before.EvalPass.Samples)
	}
	if window < 20*time.Millisecond {
		t.Fatalf("the pass window was only %s — the %d-project fixture is too small for the A/B; raise perf001r2APIProjects", window, perf001r2APIProjects)
	}
	if len(after.Stages) == 0 {
		t.Fatalf("stage attribution recorded nothing for a pass that ran: %+v", after)
	}
	if len(armB) == 0 {
		t.Fatal("arm B collected no sample inside the pass window")
	}
	if maxB >= window/2 {
		t.Fatalf("in-window /api/v1/status max = %s, want far below half the pass window (%s): the endpoint tracks the pass again", maxB, window/2)
	}
	if maxA >= window/2 {
		t.Fatalf("idle /api/v1/status max = %s is not below half the pass window (%s) — the endpoint is slow for another reason", maxA, window/2)
	}
}

// perf001r2StageSummary renders the stage attribution as "stage=total/max"
// pairs, longest total first: the row's "which sub-call spends the window" in
// one line.
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
