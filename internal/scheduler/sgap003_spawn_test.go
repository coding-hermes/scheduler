package scheduler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Regression tests for S-GAP-003 (2026-08-05): gateway-spawned tick rows sat
// 'running' with session_id NULL for the whole (synchronous) gateway flight —
// the exact zombie signature the board PM kept clearing by hand. Every
// gateway tick row must now carry a non-NULL session id from spawn onward
// (placeholder = tick id, replaced by the real resp.ID at completion) plus a
// heartbeat that proves the daemon writing it is still alive.

// gatedGatewayHandler blocks each request until released, so tests can
// inspect the tick row mid-flight. arrived is closed when the first request
// lands; the handler replies with the given response id once release closes.
func gatedGatewayHandler(arrived, release chan struct{}, respID string) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     respID,
			"status": "completed",
			"output": []map[string]any{},
			// SCHED-GAP-102: successful dispatches carry billed tokens —
			// 0/0 + empty output is gated as provider instant-death.
			"usage": map[string]int{
				"input_tokens":  500,
				"output_tokens": 10,
				"total_tokens":  510,
			},
		})
	}
}

// TestSpawn_GatewayTickRowHasSessionIDAndHeartbeat pins AC1: while the
// gateway request is in flight the tick row must already show
// session_id = tickID (placeholder) and a non-empty heartbeat_at — never
// NULL. After completion the real gateway session id replaces the
// placeholder (via lifecycle.Complete, exactly as slot_pool drives it).
func TestSpawn_GatewayTickRowHasSessionIDAndHeartbeat(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap003-gated"
		tickID      = "sgap003-gated-2026-08-05-10-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	arrived := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(gatedGatewayHandler(arrived, release, "resp_sgap003_gated"))
	defer srv.Close()

	// Every failure path must release the blocked handler or srv.Close()
	// (deferred above) hangs forever waiting for it.
	var relOnce sync.Once
	releaseFn := func() { relOnce.Do(func() { close(release) }) }
	defer releaseFn()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	type spawnResult struct {
		tick *SpawnedTick
		err  error
	}
	resCh := make(chan spawnResult, 1)
	go func() {
		tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
		resCh <- spawnResult{tick, err}
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway handler never received the spawn request")
	}

	// Mid-flight row inspection: pre-fix rows were session_id NULL here.
	var (
		sessionID sql.NullString
		heartbeat sql.NullString
		status    string
	)
	if err := db.QueryRow(`SELECT session_id, heartbeat_at, status FROM ticks WHERE id = ?`, tickID).
		Scan(&sessionID, &heartbeat, &status); err != nil {
		t.Fatalf("query in-flight tick row: %v", err)
	}
	if status != "running" {
		t.Errorf("in-flight tick status = %q, want running", status)
	}
	if !sessionID.Valid || sessionID.String != tickID {
		t.Errorf("in-flight session_id = %+v, want placeholder %q (non-NULL from spawn onward)", sessionID, tickID)
	}
	if !heartbeat.Valid || heartbeat.String == "" {
		t.Errorf("in-flight heartbeat_at = %+v, want non-empty first heartbeat", heartbeat)
	} else if _, err := time.Parse(time.RFC3339, heartbeat.String); err != nil {
		t.Errorf("heartbeat_at %q is not RFC3339: %v", heartbeat.String, err)
	}

	releaseFn()
	res := <-resCh
	if res.err != nil {
		t.Fatalf("Spawn: %v", res.err)
	}
	if res.tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}
	outcome := res.tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	if outcome.SessionID != "resp_sgap003_gated" {
		t.Errorf("outcome SessionID = %q, want real gateway id 'resp_sgap003_gated'", outcome.SessionID)
	}

	// The slot pool completes the tick via lifecycle — the real session id
	// must overwrite the placeholder in the row.
	if err := NewLifecycleTracker(db).Complete(outcome); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	sessionID = sql.NullString{}
	if err := db.QueryRow(`SELECT session_id FROM ticks WHERE id = ?`, tickID).Scan(&sessionID); err != nil {
		t.Fatalf("query completed tick row: %v", err)
	}
	if !sessionID.Valid || sessionID.String != "resp_sgap003_gated" {
		t.Errorf("completed session_id = %+v, want 'resp_sgap003_gated'", sessionID)
	}
}

// TestSpawn_GatewayHeartbeatRefreshes pins AC2: the heartbeat goroutine
// (interval shrunk to 200ms; prod default is 5m) keeps heartbeat_at fresh
// while the request is in flight, and stops when Spawn returns (no leak).
func TestSpawn_GatewayHeartbeatRefreshes(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap003-heartbeat"
		tickID      = "sgap003-heartbeat-2026-08-05-10-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	arrived := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(gatedGatewayHandler(arrived, release, "resp_sgap003_hb"))
	defer srv.Close()

	// Every failure path must release the blocked handler or srv.Close()
	// (deferred above) hangs forever waiting for it.
	var relOnce sync.Once
	releaseFn := func() { relOnce.Do(func() { close(release) }) }
	defer releaseFn()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 10*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.heartbeatInterval = 200 * time.Millisecond // prod 5m is too slow for a unit test

	type spawnResult struct {
		tick *SpawnedTick
		err  error
	}
	resCh := make(chan spawnResult, 1)
	go func() {
		tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
		resCh <- spawnResult{tick, err}
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway handler never received the spawn request")
	}

	heartbeatOf := func() string {
		var hb sql.NullString
		if err := db.QueryRow(`SELECT heartbeat_at FROM ticks WHERE id = ?`, tickID).Scan(&hb); err != nil {
			t.Fatalf("query heartbeat_at: %v", err)
		}
		if !hb.Valid || hb.String == "" {
			t.Fatalf("heartbeat_at NULL/empty mid-flight — placeholder write missing")
		}
		return hb.String
	}

	initial := heartbeatOf()
	// The goroutine must refresh heartbeat_at to a NEWER value while the
	// gateway request is still blocked. RFC3339 is second-precision, so the
	// string can take ~1s + one interval to change even at 200ms cadence.
	deadline := time.Now().Add(5 * time.Second)
	var refreshed string
	for {
		refreshed = heartbeatOf()
		if refreshed != initial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat_at still %q after 5s — heartbeat goroutine not running", initial)
		}
		time.Sleep(50 * time.Millisecond)
	}
	it, err := time.Parse(time.RFC3339, initial)
	if err != nil {
		t.Fatalf("parse initial heartbeat %q: %v", initial, err)
	}
	rt, err := time.Parse(time.RFC3339, refreshed)
	if err != nil {
		t.Fatalf("parse refreshed heartbeat %q: %v", refreshed, err)
	}
	if !rt.After(it) {
		t.Errorf("refreshed heartbeat %v not newer than initial %v", rt, it)
	}

	releaseFn()
	res := <-resCh
	if res.err != nil {
		t.Fatalf("Spawn: %v", res.err)
	}
	if outcome := res.tick.Wait(); outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}

	// No goroutine leak: once Spawn returned (stop channel closed), the row
	// must stop changing. Settle one full interval first so any write that
	// was in flight at close time lands before the snapshot.
	time.Sleep(2 * spawner.heartbeatInterval)
	still := heartbeatOf()
	time.Sleep(3 * spawner.heartbeatInterval)
	if got := heartbeatOf(); got != still {
		t.Errorf("heartbeat_at kept refreshing after Spawn returned (%q -> %q) — heartbeat goroutine leaked", still, got)
	}
}

// TestSpawn_ExecPathSetsSessionIDAndHeartbeat pins AC1 for the exec fallback
// path: the running UPDATE must also set session_id (placeholder = tick id)
// and heartbeat_at, so no running row ever has NULL session_id. Uses a
// custom bash command (the same harness as TestSpawnTimeoutKillsProcessGroup)
// so no real `hermes` binary is needed.
func TestSpawn_ExecPathSetsSessionIDAndHeartbeat(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap003-exec"
		tickID      = "sgap003-exec-2026-08-05-10-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)

	lc := NewLifecycleTracker(db)
	if err := lc.Enqueue(projectName, tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	spawner := NewSpawner(db, 4)
	spawner.heartbeatInterval = 100 * time.Millisecond
	project := PackedProject{
		Name:    projectName,
		Workdir: t.TempDir(),
		Command: "bash -c 'sleep 0.3'",
	}
	tick, err := spawner.Spawn(project, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The row stays 'running' until lifecycle.Complete (not called here), so
	// these assertions hold even if the 0.3s process has already exited.
	var (
		sessionID sql.NullString
		heartbeat sql.NullString
		status    string
		pid       int
	)
	if err := db.QueryRow(`SELECT session_id, heartbeat_at, status, pid FROM ticks WHERE id = ?`, tickID).
		Scan(&sessionID, &heartbeat, &status, &pid); err != nil {
		t.Fatalf("query tick row: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running", status)
	}
	if pid <= 0 {
		t.Errorf("pid = %d, want > 0 for exec spawn", pid)
	}
	if !sessionID.Valid || sessionID.String != tickID {
		t.Errorf("session_id = %+v, want placeholder %q (no running row may have NULL session_id)", sessionID, tickID)
	}
	if !heartbeat.Valid || heartbeat.String == "" {
		t.Errorf("heartbeat_at = %+v, want non-empty first heartbeat", heartbeat)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
}
