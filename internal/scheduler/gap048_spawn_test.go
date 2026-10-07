package scheduler

import (
	"strings"
	"testing"
	"time"
)

// Regression tests for GAP-048 (2026-08-14): when the Hermes gateway is
// unreachable at daemon startup and --no-exec-fallback is set (the default),
// the spawner has no HTTP client (gateway is nil). Without the nil-gateway
// guard, Spawn() falls straight through to the exec.Command path, bypassing
// the noExecFallback check that only lives inside the gateway-fail branch —
// the daemon exec-spawns the whole fleet forever despite the flag saying to
// stay idle. These tests pin the guard at the spawner level.

// TestSpawn_NilGatewayNoExecFallback_DropsTick proves that a spawner with no
// gateway client and noExecFallback=true does NOT exec-spawn.
//
// SCHED-GAP-1664 UPDATE: the outcome changed from DROP (nil tick + error) to
// DEFER (a non-nil SpawnedTick whose Wait() yields TickDeferred). The core
// GAP-048 invariant — the daemon must not silently degrade to exec when the
// flag is set — is unchanged: no HTTP and no exec spawn occurs, and the
// tick carries the reason text. The lane's consecutive_failures must stay
// untouched (a transport outage is not a lane fault).
func TestSpawn_NilGatewayNoExecFallback_DropsTick(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap048-nil-nofb")

	spawner := NewSpawner(db, 4)
	// No SetGatewayClient — gateway stays nil (startup fallback state).
	spawner.SetNoExecFallback(true)

	project := PackedProject{Name: "gap048-nil-nofb", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap048-nil-nofb-2026-08-14-15-00-00")

	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — since SCHED-GAP-1664 the nil-gateway path DEFERS, not drops", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick — the nil-gateway path must now return a deferred tick")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Errorf("Wait() status = %s, want %s — the nil-gateway path is a DEFERRAL since SCHED-GAP-1664", outcome.Status, TickDeferred)
	}
	if !strings.Contains(outcome.Error, "exec fallback disabled") {
		t.Errorf("outcome.Error = %q, want it to mention 'exec fallback disabled'", outcome.Error)
	}

	// No spawn should have occurred — neither HTTP nor exec.
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0) — no spawn should occur when idle", httpCount, execCount)
	}
	// The lane must not be charged for the transport outage.
	if n := schedGap203BConsecutiveFailures(t, db, "gap048-nil-nofb"); n != 0 {
		t.Errorf("consecutive_failures = %d, want 0 — a deferral is not the lane's failure (SCHED-GAP-1664)", n)
	}
}

// TestSpawn_NilGatewayExecFallbackAllowed_StillExecSpawns proves that when
// noExecFallback is FALSE and the gateway is nil, the spawner falls back to
// exec.Command as before — the flag gates the behavior, not the nil state.
func TestSpawn_NilGatewayExecFallbackAllowed_StillExecSpawns(t *testing.T) {
	if testing.Short() {
		t.Skip("exec spawn test requires hermes binary — skip in short mode")
	}
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap048-nil-fb")

	spawner := NewSpawner(db, 4)
	spawner.timeout = 5 * time.Second
	// No SetGatewayClient — gateway stays nil.
	spawner.SetNoExecFallback(false)

	// Use a custom command that exits immediately so we don't need hermes.
	project := PackedProject{
		Name:    "gap048-nil-fb",
		Workdir: t.TempDir(),
		Command: "bash -c 'echo hello'",
	}
	tick, err := spawner.Spawn(project, "gap048-nil-fb-2026-08-14-15-00-00")
	if err != nil {
		t.Fatalf("Spawn with nil gateway + noExecFallback=false: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick — exec fallback should have produced a tick")
	}
	// Wait for the process to finish so we don't leak.
	tick.Wait()

	// The spawn must have gone through the exec path (not HTTP).
	httpCount, execCount := spawner.SpawnMethodCounts()
	if execCount != 1 {
		t.Errorf("exec spawn count = %d, want 1 — nil gateway + noExecFallback=false should exec-spawn", execCount)
	}
	if httpCount != 0 {
		t.Errorf("http spawn count = %d, want 0 — no gateway client means no HTTP spawn", httpCount)
	}
}

// TestSpawn_NilGatewayNoExecFallback_EmitsHighEvent proves the HIGH event is
// emitted when the event logger is wired, so a startup-fallback daemon is
// visible in the events table/dashboard instead of silently degrading.
// SCHED-GAP-1664: the event now says the tick was DEFERRED (not dropped).
func TestSpawn_NilGatewayNoExecFallback_EmitsHighEvent(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "gap048-event")

	spawner := NewSpawner(db, 4)
	spawner.SetNoExecFallback(true)
	spawner.SetEventLogger(NewEventLogger(db))

	project := PackedProject{Name: "gap048-event", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "gap048-event-2026-08-14-15-00-00")
	if err != nil {
		t.Fatalf("Spawn returned an error (%v) — the nil-gateway path defers since SCHED-GAP-1664", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned a nil tick — the nil-gateway path must return a deferred tick")
	}
	outcome := tick.Wait()
	if outcome.Status != TickDeferred {
		t.Fatalf("Wait() status = %s, want %s", outcome.Status, TickDeferred)
	}

	// Verify a HIGH event was written to the events table.
	var severity, component string
	row := db.QueryRow(
		`SELECT severity, component FROM events WHERE component = 'spawn' AND message LIKE '%exec fallback disabled%' ORDER BY created_at DESC LIMIT 1`)
	if err := row.Scan(&severity, &component); err != nil {
		t.Fatalf("no HIGH event found in events table: %v", err)
	}
	if severity != string(SeverityHigh) {
		t.Errorf("event severity = %q, want %q", severity, SeverityHigh)
	}
}
