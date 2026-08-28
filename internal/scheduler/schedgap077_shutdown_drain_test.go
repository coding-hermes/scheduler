package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-077 (2026-08-27): graceful shutdown orphaned
// in-flight GATEWAY ticks. systemctl restart of schedulerd drained exec ticks,
// but a gateway tick (deepseek-dashboard 17:33:55) was still in flight — the
// daemon logged 'LOOP: all in-flight ticks completed' and exited while the
// gateway request was still blocking, leaving the tick row status='running'
// with a stale heartbeat until a manual UPDATE.
//
// Root cause: Loop.Stop() waited on l.running (sync.WaitGroup) which has NO
// Add()/Done() callers anywhere in the repo — the drain returned instantly.
// The real in-flight source is the SlotPool: a slot is held for the WHOLE tick
// (Acquire→Spawn→st.Wait()→Complete→Release), so slotPool.Running()/Wait(ctx)
// is the truth. These tests pin BOTH paths:
//
//  1. drain-waits-for-gateway-tick: Stop() must NOT return while a gateway
//     request is in flight, and the row must land 'completed' after the tick
//     finishes (the 17:33:55 scenario — without the manual UPDATE workaround).
//  2. drain-timeout-marks-failed: when the drain grace expires with a tick
//     still blocking, Stop() marks every 'running' row 'failed' (schema-legal
//     status; CHECK in internal/database/migrations.go allows
//     queued/running/completed/failed/timeout — no 'orphaned' without a
//     migration), emits a HIGH event, and releases all slots so the process
//     can exit. No row survives shutdown as 'running'.

// blockingGatewayHandler is a stub /v1/responses gateway whose handler blocks
// until release is closed (or forever), then replies with a minimal valid
// completed payload. requestSeen is closed (once) when the POST lands —
// signalling that the spawn goroutine has passed Enqueue+StartRunning and the
// tick row is 'running'.
func blockingGatewayHandler(requestSeen, release chan struct{}) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestSeen) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_sgap077",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}
}

// TestStop_DrainsInFlightGatewayTick is the primary SCHED-GAP-077 regression:
// with a gateway request in flight (slot held), Loop.Stop() must block until
// the tick completes — it must NOT log 'all in-flight ticks completed' and
// return while the request is still blocking. After the gateway responds, the
// tick row must be 'completed' with the slot released.
func TestStop_DrainsInFlightGatewayTick(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap077-drain-wait"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(blockingGatewayHandler(requestSeen, release))
	// Never let srv.Close() hang on a still-blocked handler, even on the
	// test's failure paths.
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second) // lazy-inits the slot pool

	// Enqueue the tick exactly as evaluate() does: slot held for the WHOLE
	// tick (Acquire→Spawn→st.Wait()→Complete→Release).
	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	<-requestSeen // request is in flight; row is 'running'

	// Start the eval loop now — the packer's RunningSet dedup must skip the
	// already-running project (no second spawn).
	go loop.Run()

	stopped := make(chan struct{})
	go func() {
		loop.Stop()
		close(stopped)
	}()

	// THE regression assertion: Stop() must still be blocked while the
	// gateway request is in flight. Against the old l.running.Wait() drain
	// (no Add()/Done() callers) Stop() returned instantly — this select
	// failed the test immediately, reproducing the deepseek-dashboard
	// 17:33:55 orphan.
	select {
	case <-stopped:
		t.Fatal("Stop() returned while gateway request in flight — drain is not waiting on the slot pool")
	case <-time.After(500 * time.Millisecond):
	}
	if got := tickStatusOf(t, db, tickID); got != "running" {
		t.Fatalf("tick status while request in flight = %q, want running", got)
	}

	// Let the gateway respond — the tick completes, the slot releases, and
	// Stop() must now return.
	releaseOnce.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return after the in-flight tick completed")
	}

	if got := tickStatusOf(t, db, tickID); got != "completed" {
		t.Errorf("tick status after drain = %q, want completed (the manual UPDATE workaround must stay dead)", got)
	}
	if n := loop.slotPool.Running(); n != 0 {
		t.Errorf("slot pool Running() = %d after drain, want 0", n)
	}
}

// TestStop_DrainTimeoutMarksRunningTicksFailed is the abort path: when a
// gateway request blocks past the drain grace period, Stop() must mark every
// still-'running' row 'failed' (schema-legal status — no new 'orphaned' value,
// no migration), log per tick, emit a HIGH event, and release all slots so the
// process can exit. No row may survive shutdown as 'running' with a stale
// heartbeat (the 17:33:55 scenario is impossible without the manual UPDATE).
func TestStop_DrainTimeoutMarksRunningTicksFailed(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap077-drain-timeout"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(blockingGatewayHandler(requestSeen, release))
	defer func() {
		releaseOnce.Do(func() { close(release) }) // unblock the handler so srv.Close() cannot hang
		srv.Close()
	}()

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	// Short grace for the test — the daemon default (15s) would make this
	// test take 15 seconds. The timeout path is identical.
	loop.stopGrace = 500 * time.Millisecond

	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	<-requestSeen // request is in flight; row is 'running'

	go loop.Run()

	// Stop() must block for the grace period, then mark the stuck row failed
	// and return.
	start := time.Now()
	loop.Stop()
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("Stop() returned after %v — drain grace (%v) was not honored", elapsed, loop.stopGrace)
	}

	if got := tickStatusOf(t, db, tickID); got != "failed" {
		t.Errorf("tick status after drain timeout = %q, want failed", got)
	}
	var running int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&running); err != nil {
		t.Fatalf("count running ticks: %v", err)
	}
	if running != 0 {
		t.Errorf("%d tick row(s) survived shutdown as 'running' — stale-heartbeat orphan scenario not closed", running)
	}

	// HIGH event must be emitted with the drain-timeout message.
	var severity, component, message string
	row := db.QueryRow(
		`SELECT severity, component, message FROM events
		 WHERE component = 'loop' AND message LIKE '%shutdown drain timed out%'
		 ORDER BY created_at DESC LIMIT 1`)
	if err := row.Scan(&severity, &component, &message); err != nil {
		t.Fatalf("no drain-timeout HIGH event found in events table: %v", err)
	}
	if severity != string(SeverityHigh) {
		t.Errorf("event severity = %q, want %q", severity, SeverityHigh)
	}
	if component != "loop" {
		t.Errorf("event component = %q, want loop", component)
	}

	// Slots must be released so the process can exit (ReleaseAll; Release is
	// idempotent per slot_pool.go, so a racing release cannot double-free).
	if n := loop.slotPool.Running(); n != 0 {
		t.Errorf("slot pool Running() = %d after drain timeout, want 0", n)
	}
}
