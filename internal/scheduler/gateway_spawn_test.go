package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression tests for GAP-001 (2026-08-04): a Hermes gateway update stopped
// accepting per-foreman "fk-*" gateway keys (401 auth errors, 8208+ failed
// ticks fleet-wide). The fleet was restored by clearing projects.gateway_key
// for every project, so Spawn() falls back to the daemon's shared
// --gateway-key. The client-level key fallback is covered by
// gateway_client_test.go; these tests pin the SPAWN-level wiring:
// Spawn() must forward project.GatewayKey to the gateway, an empty key must
// resolve to the daemon shared key, and an auth failure must surface as an
// error — never a silently "completed" tick.

// gatewaySpawnOKHandler captures the Authorization header and replies with a
// minimal valid /v1/responses payload.
func gatewaySpawnOKHandler(capturedAuth *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap001",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}
}

// TestSpawn_ForwardsPerForemanGatewayKey proves a project with GatewayKey set
// authenticates to the gateway with ITS OWN key (not the daemon shared key).
func TestSpawn_ForwardsPerForemanGatewayKey(t *testing.T) {
	db := newTestDB(t)

	var capturedAuth string
	srv := httptest.NewServer(gatewaySpawnOKHandler(&capturedAuth))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:       "gap001-per-foreman",
		Workdir:    t.TempDir(),
		GatewayKey: "fk-test-abc",
	}
	tick, err := spawner.Spawn(project, "gap001-per-foreman-2026-08-04-15-50-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}

	if capturedAuth != "Bearer fk-test-abc" {
		t.Errorf("Authorization = %q, want per-foreman 'Bearer fk-test-abc' — "+
			"Spawn() is not forwarding project.GatewayKey", capturedAuth)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_gap001" {
		t.Errorf("SessionID = %q, want real gateway response id 'resp_gap001' (S-GAP-003: no more hardcoded 'gateway')", tick.SessionID)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 1 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (1, 0) — spawn must use the gateway, not exec", httpCount, execCount)
	}
}

// TestSpawn_EmptyGatewayKeyFallsBackToDaemonKey is THE regression guard for
// the 2026-08-04 outage: a project with GatewayKey == "" (the state the fleet
// was restored to) must authenticate with the daemon's shared --gateway-key.
// Re-populating fk-* keys, or breaking the empty-key fallback in either
// Spawn() or GatewayClient.setAuth(), fails this test.
func TestSpawn_EmptyGatewayKeyFallsBackToDaemonKey(t *testing.T) {
	db := newTestDB(t)

	var capturedAuth string
	srv := httptest.NewServer(gatewaySpawnOKHandler(&capturedAuth))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:       "gap001-daemon-fallback",
		Workdir:    t.TempDir(),
		GatewayKey: "", // post-outage fleet state: no per-foreman key
	}
	tick, err := spawner.Spawn(project, "gap001-daemon-fallback-2026-08-04-15-50-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}

	if capturedAuth != "Bearer sk-daemon-shared" {
		t.Errorf("Authorization = %q, want daemon shared 'Bearer sk-daemon-shared' — "+
			"empty project.GatewayKey must fall back to --gateway-key", capturedAuth)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 1 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (1, 0)", httpCount, execCount)
	}
}

// TestSpawn_GatewayAuthFailureLoud proves a gateway 401 surfaces as a Spawn()
// error and does NOT mark the tick completed — during the outage, failed
// spawns must be visible, never silently swallowed. With noExecFallback set,
// the tick is dropped (no exec fallback) and its row keeps its prior status.
func TestSpawn_GatewayAuthFailureLoud(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "auth_error",
				"message": "Invalid gateway API key",
			},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	// Seed the rows a real tick would have so we can prove the failure path
	// never marks the tick completed. Helpers from tick_process_infra012_test.go.
	const (
		projectName = "gap001-auth-failure"
		tickID      = "gap001-auth-failure-2026-08-04-15-50-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	project := PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		GatewayKey: "fk-revoked-key", // the outage: gateway rejects this key
	}
	tick, err := spawner.Spawn(project, tickID)

	if err == nil {
		t.Fatal("Spawn returned nil error on gateway 401 — auth failure was silent")
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("error = %q, want it to surface the gateway error type 'auth_error'", err.Error())
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error = %q, want it to mention the gateway", err.Error())
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on auth failure — no tick should be produced")
	}

	if got := tickStatusOf(t, db, tickID); got != "running" {
		t.Errorf("tick status = %q after failed spawn, want 'running' — "+
			"a failed spawn must NOT mark the tick completed", got)
	}

	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0) — no HTTP success, no exec fallback", httpCount, execCount)
	}
}

// Regression tests for SCHED-GAP-060 (2026-08-21): /api/v1/projects reported
// the last tick's COMPLETION time under last_tick_started because the
// gateway branch of Spawn() wrote time.Now() AFTER SendResponse returned
// (i.e. after the session completed). The stamp must be the SPAWN time
// (reqStart, captured before SendResponse per SCHED-GAP-029), written at
// spawn — and the post-completion UPDATE must only reset the backoff
// counter (S-GAP-001), never rewrite last_tick_started.

// TestSpawn_GatewayLastTickStarted — a completed gateway spawn must stamp
// projects.last_tick_started with ≈ the SPAWN time, not the completion time.
// The fake gateway delays its response by 2s so the completion moment is
// measurably later than spawn: the old code (stamp after SendResponse)
// lands ~2s after start and fails the ±1s window.
func TestSpawn_GatewayLastTickStarted(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap060-completed"
	mustCreateProjectINFRA012(t, db, projectName)
	// Seed a nonzero failure count: a successful spawn must still reset it.
	if _, err := db.Exec(`UPDATE projects SET consecutive_failures = 3 WHERE name = ?`, projectName); err != nil {
		t.Fatalf("seed consecutive_failures: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap060",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"gap060-completed-2026-08-21-06-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()
	after := time.Now()

	var stamped string
	if err := db.QueryRow(`SELECT last_tick_started FROM projects WHERE name = ?`, projectName).Scan(&stamped); err != nil {
		t.Fatalf("query last_tick_started: %v", err)
	}
	got, err := time.Parse(time.RFC3339, stamped)
	if err != nil {
		t.Fatalf("last_tick_started %q is not RFC3339: %v", stamped, err)
	}
	// The stamp must be ≈ reqStart (before the 2s handler delay) — within
	// ±1s of start. The OLD code wrote the completion moment (~start+2s).
	if got.Before(start.Add(-time.Second)) || got.After(start.Add(time.Second)) {
		t.Errorf("last_tick_started = %s (parsed %v), want ≈ spawn time within ±1s of start=%v (completion would be ~2s later)",
			stamped, got, start)
	}
	if !got.Before(after) {
		t.Errorf("last_tick_started %v must be strictly before completion %v", got, after)
	}

	// The successful completion still resets the backoff counter (S-GAP-001).
	var failures int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, projectName).Scan(&failures); err != nil {
		t.Fatalf("query consecutive_failures: %v", err)
	}
	if failures != 0 {
		t.Errorf("consecutive_failures = %d after successful gateway spawn, want 0 (S-GAP-001 reset)", failures)
	}
}

// TestSpawn_GatewayRunningLastTickStarted pins the SCHED-GAP-060 PASS
// criterion: a project with a RUNNING gateway tick reports last_tick_started
// ≈ its spawn time while the session is still in flight — and the spawn path
// never touches last_tick_completed (that column belongs to
// lifecycle.Complete) nor resets consecutive_failures at spawn time.
func TestSpawn_GatewayRunningLastTickStarted(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap060-running"
	mustCreateProjectINFRA012(t, db, projectName)
	// Prior completion + failure count the spawn path must NOT touch at
	// spawn time (S-GAP-001: the counter resets only on success, after the
	// session completes).
	const priorCompleted = "2026-08-20T12:00:00Z"
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ?, consecutive_failures = 2 WHERE name = ?`,
		priorCompleted, projectName); err != nil {
		t.Fatalf("seed prior state: %v", err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release // hold the session open — the tick is still RUNNING
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap060",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	start := time.Now()
	type spawnResult struct {
		tick *SpawnedTick
		err  error
	}
	done := make(chan spawnResult, 1)
	go func() {
		tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
			"gap060-running-2026-08-21-06-32-00")
		done <- spawnResult{tick, err}
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway handler never entered")
	}

	// The session is in flight — last_tick_started must ALREADY be stamped
	// with the spawn time (SCHED-GAP-060 PASS criterion).
	var stamped string
	if err := db.QueryRow(`SELECT last_tick_started FROM projects WHERE name = ?`, projectName).Scan(&stamped); err != nil {
		t.Fatalf("query last_tick_started: %v", err)
	}
	got, err := time.Parse(time.RFC3339, stamped)
	if err != nil {
		t.Fatalf("last_tick_started %q is not RFC3339: %v", stamped, err)
	}
	if got.Before(start.Add(-5*time.Second)) || got.After(start.Add(5*time.Second)) {
		t.Errorf("running tick last_tick_started = %s (parsed %v), want ≈ spawn time within ±5s of start=%v",
			stamped, got, start)
	}

	// Spawn time must NOT reset the backoff counter (a failed spawn must
	// still increment it via noteSpawnFailure).
	var failures int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, projectName).Scan(&failures); err != nil {
		t.Fatalf("query consecutive_failures: %v", err)
	}
	if failures != 2 {
		t.Errorf("consecutive_failures = %d while running, want 2 (no reset at spawn time)", failures)
	}

	// Let the session complete.
	close(release)
	res := <-done
	if res.err != nil {
		t.Fatalf("Spawn: %v", res.err)
	}
	if res.tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	res.tick.Wait()

	// The spawn path must never write last_tick_completed — that column
	// belongs to lifecycle.Complete (slot_pool), which is not in this path.
	var completed string
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, projectName).Scan(&completed); err != nil {
		t.Fatalf("query last_tick_completed: %v", err)
	}
	if completed != priorCompleted {
		t.Errorf("last_tick_completed = %q after gateway spawn, want prior %q unchanged (spawn path must not touch it)",
			completed, priorCompleted)
	}

	// And the completed session DID reset the backoff counter (S-GAP-001).
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, projectName).Scan(&failures); err != nil {
		t.Fatalf("query consecutive_failures: %v", err)
	}
	if failures != 0 {
		t.Errorf("consecutive_failures = %d after completed gateway spawn, want 0 (S-GAP-001 reset on success)", failures)
	}
}
