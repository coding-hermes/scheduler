package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
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
// minimal valid /v1/responses payload. (SCHED-GAP-205: the payload carries a
// minimal assistant output item — a completed-but-zero-assistant response
// with non-zero tokens is now a gated failure, so the stub speaks the real
// wire shape of a successful completion.)
func gatewaySpawnOKHandler(capturedAuth *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap001",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "ok"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  800,
				"output_tokens": 20,
				"total_tokens":  820,
			},
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
// projects.last_tick_started with exactly the SPAWN instant, never the
// completion instant (SCHED-GAP-060). The wall-clock delay + ±1s window this
// test used before (2s handler sleep, CI-005 class) is replaced with the
// clock seam: the spawner runs on a DORMANT sim clock, so the spawn instant
// and the completion instant are two values the test chooses — completion is
// deterministically 2 virtual minutes after spawn (the artificial sleep is
// gone) and the equality assertion is exact. The dormant clock cannot stall
// the gateway path: every time read in Spawn/Wait goes through the seam, and
// no seam wait (Sleep/After/timer) is armed on the completed-gateway route.
func TestSpawn_GatewayLastTickStarted(t *testing.T) {
	db := newTestDB(t)
	const projectName = "gap060-completed"
	mustCreateProjectINFRA012(t, db, projectName)
	// Seed a nonzero failure count: a successful spawn must still reset it.
	if _, err := db.Exec(`UPDATE projects SET consecutive_failures = 3 WHERE name = ?`, projectName); err != nil {
		t.Fatalf("seed consecutive_failures: %v", err)
	}

	// spawnInstant and completionInstant are the two virtual instants the
	// seam hands out; completion is deliberately far in the virtual future —
	// under the old bug (stamp after SendResponse) the DB would hold
	// completionInstant and the exact-equality assertion below fails.
	spawnInstant := time.Date(2026, 9, 20, 6, 0, 0, 0, time.UTC)
	completionInstant := spawnInstant.Add(2 * time.Minute)
	simClock := clock.NewManualSimClock(spawnInstant)

	// The handler moves the seam to the completion instant as it responds:
	// by the time SendResponse returns, the spawn instant (already handed to
	// reqStart BEFORE the POST) and the completion instant are two distinct,
	// deterministically chosen instants — the property this test needs, with
	// no wall-clock delay and no failure path (a POST that timed out would
	// arm the SCHED-GAP-064 retry wait, which is a seam wait this dormant
	// clock never fires).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		simClock.AdvanceTo(completionInstant)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap060",
			"status": "completed",
			// SCHED-GAP-205: minimal assistant item — completed with
			// non-zero tokens and no assistant output is a gated failure.
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "ok"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  800,
				"output_tokens": 20,
				"total_tokens":  820,
			},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetClock(simClock)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"gap060-completed-2026-08-21-06-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	outcome := tick.Wait()

	// Witness the separation: the tick STARTED at the spawn instant and
	// FINISHED at the completion instant, so a stamp carrying the completion
	// instant is distinguishable from one carrying the spawn instant — that
	// is the discriminating power the old ±1s window approximated.
	if !outcome.Started.Equal(spawnInstant) {
		t.Errorf("tick Started = %v, want the spawn instant %v", outcome.Started, spawnInstant)
	}
	if !outcome.Finished.Equal(completionInstant) {
		t.Errorf("tick Finished = %v, want the completion instant %v (2 virtual minutes after spawn)",
			outcome.Finished, completionInstant)
	}
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}

	// THE PROPERTY (SCHED-GAP-060): the stamp is the SPAWN instant, exactly.
	var stamped string
	if err := db.QueryRow(`SELECT last_tick_started FROM projects WHERE name = ?`, projectName).Scan(&stamped); err != nil {
		t.Fatalf("query last_tick_started: %v", err)
	}
	if stamped != spawnInstant.Format(time.RFC3339) {
		t.Errorf("last_tick_started = %s, want exactly the spawn instant %s — the stamp must be the SPAWN instant, never the completion instant %s (SCHED-GAP-060)",
			stamped, spawnInstant.Format(time.RFC3339), completionInstant.Format(time.RFC3339))
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
// == its spawn instant while the session is still in flight — and the spawn
// path never touches last_tick_completed (that column belongs to
// lifecycle.Complete) nor resets consecutive_failures at spawn time.
// The pre-seam shape of this test (a 10s entry deadline plus a ±5s wall-clock
// window against the real clock) was a fourth instance of the CI-005 ambient
// class; the dormant sim clock makes both observations exact.
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

	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered) // request reached the handler; the session is now in flight
		<-release      // hold the session open — the tick is still RUNNING
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap060",
			"status": "completed",
			// SCHED-GAP-205: minimal assistant item — completed with
			// non-zero tokens and no assistant output is a gated failure.
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "ok"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  800,
				"output_tokens": 20,
				"total_tokens":  820,
			},
		})
	}))
	defer srv.Close()

	// The seam hands out spawnInstant to every clock read while the session
	// is held open; only the test's AdvanceTo moves it to completionInstant.
	spawnInstant := time.Date(2026, 9, 20, 6, 32, 0, 0, time.UTC)
	completionInstant := spawnInstant.Add(3 * time.Minute)
	simClock := clock.NewManualSimClock(spawnInstant)

	spawner := NewSpawner(db, 4)
	spawner.SetClock(simClock)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

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

	<-entered // handler is parked before its response — the session is in flight

	// The session is in flight — last_tick_started must ALREADY be stamped
	// with exactly the spawn instant (SCHED-GAP-060 PASS criterion). The
	// pre-seam ±5s wall window is now an exact equality at a chosen instant.
	var stamped string
	if err := db.QueryRow(`SELECT last_tick_started FROM projects WHERE name = ?`, projectName).Scan(&stamped); err != nil {
		t.Fatalf("query last_tick_started: %v", err)
	}
	if stamped != spawnInstant.Format(time.RFC3339) {
		t.Errorf("running tick last_tick_started = %s, want exactly the spawn instant %s while the session is in flight (SCHED-GAP-060)",
			stamped, spawnInstant.Format(time.RFC3339))
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

	// Let the session complete at the completion instant.
	simClock.AdvanceTo(completionInstant)
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
