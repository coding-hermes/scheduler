package scheduler

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-135 regression tests (2026-09-17): the 20:07:35Z gateway-restart
// burst left 4 ticks (9router, warpfs, off-by-one, gitreins-poc) admitted at
// the boundary in status='running', pid=0, files_changed=0 — every attempt a
// transport-refused dial (GATEWAY RETRY attempt=1..3/3) and the row NEVER
// marked failed. Consequences: 4 of 5 packer slots stuck on phantom ticks,
// no EVAL/spawn line for 4m52s, and checkEvalStall's `running > 0` early
// return (loop.go) blinding the GAP-042 watchdog against the very wedge it
// exists to catch. These tests pin both halves:
//
//  1. a spawn whose gateway dials are ALL refused (retry-exhaust) must reach a
//     TERMINAL status and give the slot back — through the real SlotPool,
//     with the production daemon shape (2h tick deadline + 30m per-turn
//     knob, i.e. the supervised stream path);
//  2. a fleet whose "running" rows are stale pid=0 phantoms must not
//     silence checkEvalStall.
//
// SCHED-GAP-203 UPDATE (this row's sibling): the terminal status this shape
// now lands is "deferred", not "failed". The refused-dial burst above IS the
// evidence 203 measured (5 of its 9 ticks), and the whole point of that row is
// that such a blip is a gateway-side deferral rather than a lane failure — so
// the expectation below moved with the behaviour. What this test exists to
// protect is unchanged and still asserted: the row is TERMINAL (never left
// "running"), the slot is released, and the retry ladder demonstrably ran. It is
// NOT a licence to relax: "queued" or "running" still fails the test.

// schedGap135ClosedGatewayURL stands up a real HTTP server, captures its
// URL, then closes the listener so every dial to it is an instant
// "connect: connection refused" — the exact live 20:07:35Z transport class.
func schedGap135ClosedGatewayURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// schedGap135WaitAllAdmitted closes the launch-window race between the
// fire-and-forget Spawn loop in the caller and pool.Wait. SlotPool.Spawn
// (slot_pool.go:327) returns the tick id before its goroutine has done
// anything; the goroutine only LATER acquires a slot, enqueues the tick row
// and dials the gateway. pool.Wait (slot_pool.go:643) is a poll loop that
// returns nil the instant pool.Running() == 0 — which is legitimately true
// inside that launch window — so without this wait the test could race ahead
// to tickStatusOf and find no row at all (CI: "sql: no rows in result set",
// load-sensitive; the orphaned "SPAWN: enqueue ...: sql: database is closed"
// lines logged AFTER the failure are the same goroutines still logging during
// t.Cleanup — a symptom, not the cause).
//
// Ordering proof (why waiting for row existence is sufficient, not merely
// helpful): a goroutine INSERTs its tick row only AFTER its slot Acquire,
// and the slot is Released (deferred at goroutine entry) only AFTER
// lifecycle.Complete has written the terminal status. So once ALL n rows
// exist, Running() >= 1 at every later instant until every tick is terminal
// — pool.Wait can no longer exit early, and the assertions in the caller see
// a settled pool.
//
// Bounded and deliberately non-failing on timeout: if the rows never appear,
// the caller falls through to pool.Wait + the original assertions, so a
// genuine regression (rows never created) still fails the test with its
// original semantics instead of a different error.
func schedGap135WaitAllAdmitted(t *testing.T, db *sql.DB, ticks []string) {
	t.Helper()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ticks)), ",")
	args := make([]any, len(ticks))
	for i, id := range ticks {
		args[i] = id
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var have int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM ticks WHERE id IN (`+ph+`)`, args...,
		).Scan(&have); err != nil {
			t.Fatalf("count admitted tick rows: %v", err)
		}
		if have == len(ticks) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("admission wait timed out after 10s — falling through to pool.Wait + original assertions")
}

// TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot — the live wedge:
// 4 projects spawn against a gateway whose listener is closed; each exhausts
// the 3 GATEWAY RETRY attempts on transport-refused dials. Every tick row
// must end in a TERMINAL status (never left 'running') — "deferred" for this
// transport blip since SCHED-GAP-203, see the file header — and the 5-slot
// pool must drain back to 0 running.
func TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot(t *testing.T) {
	db := newTestDB(t)
	buf := schedGap080CaptureLog(t)

	// Production daemon shape: --tick-timeout 7200s with the default 30m
	// per-turn knob → supervised (stream) POST path, as on the live host.
	spawner := NewSpawner(db, 8, 2*time.Hour)
	spawner.SetGatewayClient(NewGatewayClient(schedGap135ClosedGatewayURL(t), "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	pool := NewSlotPool(5, spawner, NewLifecycleTracker(db))

	const n = 4
	names := make([]string, n)
	ticks := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = "gap135-phantom-" + string(rune('a'+i))
		mustCreateProjectINFRA012(t, db, names[i])
		ticks[i] = pool.Spawn(PackedProject{Name: names[i], Workdir: t.TempDir()}, time.Now(), true, db)
	}

	// Close the launch-window race BEFORE waiting on the pool: Spawn is
	// fire-and-forget and pool.Wait returns the moment Running() == 0, which
	// is already true here (no goroutine has acquired a slot yet). Poll for
	// all n tick rows to exist first — see the ordering proof on the helper.
	// Non-failing on timeout: a genuine regression still fails below, with
	// the original error semantics.
	schedGap135WaitAllAdmitted(t, db, ticks)

	// The retry ladder sleeps ~3.5s per tick (500ms+1s+2s) and the dials
	// fail instantly; healthy drain is <10s. A wedged spawn goroutine
	// (the bug) holds its slot forever — the bounded wait catches it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := pool.Wait(ctx); err != nil {
		for i := range ticks {
			t.Logf("tick %s (%s) status=%s", ticks[i], names[i], tickStatusOf(t, db, ticks[i]))
		}
		t.Fatalf("slot pool never drained after retry-exhaust: %v — phantom ticks still hold slots", err)
	}

	for i := range ticks {
		got := tickStatusOf(t, db, ticks[i])
		// The load-bearing property is TERMINALITY. It is asserted first so a
		// future vocabulary change cannot make this test pass on a row that
		// was left queued/running (the 2026-09-17 phantom wedge).
		if got == "running" || got == "queued" {
			t.Errorf("tick %s (%s) status=%q — a retry-exhausted transport failure must never be left running", ticks[i], names[i], got)
			continue
		}
		if got != "deferred" {
			t.Errorf("tick %s (%s) status=%q, want %q — a retry-exhausted transport blip is a gateway deferral (SCHED-GAP-203), not a lane failure", ticks[i], names[i], got, "deferred")
		}
	}
	if got := NewLifecycleTracker(db).RunningCount(); got != 0 {
		t.Errorf("running tick rows = %d, want 0 — phantom rows must not survive the spawn path", got)
	}
	if got := pool.Running(); got != 0 {
		t.Errorf("pool.Running() = %d, want 0 — every retry-exhausted spawn must release its slot", got)
	}
	// Prove these rows went through the retry-exhaust path (not a
	// single-attempt failure): the third and final retry logged per tick.
	logs := buf.String()
	for i := range ticks {
		want := "GATEWAY RETRY: " + names[i] + " tick=" + ticks[i] + " attempt=3/3"
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing final retry line %q:\n%s", want, logs)
		}
	}
}

// schedGap135InsertPhantom writes the live 20:07:35Z row shape: a
// status='running' tick with pid=0 and a heartbeat older than
// gatewayZombieMaxAge — the spawn wrote the placeholder and vanished.
func schedGap135InsertPhantom(t *testing.T, db *sql.DB, tickID, project string, heartbeatAge time.Duration) {
	t.Helper()
	stale := time.Now().UTC().Add(-heartbeatAge).Format(time.RFC3339)
	now := stale
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid, session_id, heartbeat_at)
		 VALUES (?, ?, 'running', ?, ?, 0, ?, ?)`,
		tickID, project, now, now, tickID, stale); err != nil {
		t.Fatalf("insert phantom tick %s: %v", tickID, err)
	}
}

// TestSCHEDGAP135_EvalStallNotBlindedByPhantomRunning — the second half of
// the wedge: 4 stale pid=0 'running' rows suppressed checkEvalStall's
// forced re-evaluation via `running > 0` while no eval had run for 10
// minutes (threshold 5m). Stale phantom rows must not count as "work in
// flight": the watchdog must still fire.
func TestSCHEDGAP135_EvalStallNotBlindedByPhantomRunning(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	for i := 0; i < 4; i++ {
		name := "gap135-stall-" + string(rune('a'+i))
		mustCreateProjectINFRA012(t, db, name)
		schedGap135InsertPhantom(t, db, name+"-2026-09-17-01-07-35", name, 20*time.Minute)
	}

	// running=4 as observed live — but every row behind it is a stale
	// pid=0 phantom. The detector must fire, not silently return.
	l.checkEvalStall(4)

	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

// TestSCHEDGAP135_EvalStallLiveRunningStillSuppresses — the healthy fleet
// guard: genuinely in-flight work (recent heartbeat, live pid) must keep
// suppressing the watchdog exactly as before. A busy fleet with fresh rows
// is not a stall.
func TestSCHEDGAP135_EvalStallLiveRunningStillSuppresses(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	// Fresh heartbeat (30s old) — a live gateway tick, not a phantom.
	name := "gap135-live"
	mustCreateProjectINFRA012(t, db, name)
	schedGap135InsertPhantom(t, db, name+"-2026-09-17-02-00-00", name, 30*time.Second)

	l.checkEvalStall(1)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

var _ = sql.ErrNoRows // keep database/sql imported for fixture helpers
