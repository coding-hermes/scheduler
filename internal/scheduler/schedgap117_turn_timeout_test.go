package scheduler

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-117 regression tests: a hung gateway /v1/responses POST used to
// consume the WHOLE --tick-timeout slot undetected (live evidence: every
// failure had completed_at = spawned_at + exactly 7200s, and agent.log showed
// zero tool calls — the LLM turn went idle). The per-turn deadline
// (--gateway-response-timeout, default 30m) must trip FIRST, abort the POST,
// and fail the tick as a STALLED failure (status=failed, error mentioning
// "stalled"), with the slot released well before the tick deadline.

// stallGatewayHandler accepts the POST and then never answers (until release
// closes). requestSeen fires once the POST lands.
func stallGatewayHandler(requestSeen, release chan struct{}) http.HandlerFunc {
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestSeen) })
		<-release
	}
}

// tickErrorOf reads the ticks.error column for the given tick ("" when NULL).
func tickErrorOf(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var errText sql.NullString
	if err := db.QueryRow(`SELECT error FROM ticks WHERE id = ?`, tickID).Scan(&errText); err != nil {
		t.Fatalf("query tick error %s: %v", tickID, err)
	}
	return errText.String
}

// waitForTickTerminal polls the tick row until it leaves running/queued or
// the deadline passes. A missing row (the slot-pool goroutine has not
// enqueued yet) is not terminal — keep polling. Returns (status, ok).
func waitForTickTerminal(t *testing.T, db *sql.DB, tickID string, within time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var status string
		switch err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status); {
		case err == sql.ErrNoRows:
			// not enqueued yet
		case err != nil:
			t.Fatalf("query tick status %s: %v", tickID, err)
		case status != "running" && status != "queued":
			return status, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

// TestSpawn_GatewayStalledTurnFailsBeforeTickTimeout is the PRIMARY
// SCHED-GAP-117 regression: a gateway POST that blocks forever must be
// aborted by the per-turn deadline (250ms here) while the tick deadline
// (30s) is still far away. The tick must be recorded status=failed with an
// error mentioning "stalled"/"no progress", Spawn must return a tick whose
// Wait() yields TickFailed (NOT TickTimeout), and the whole elapsed time
// stays far below the tick timeout.
func TestSpawn_GatewayStalledTurnFailsBeforeTickTimeout(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap117-stalled-turn"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(stallGatewayHandler(requestSeen, release))
	defer func() {
		releaseOnce.Do(func() { close(release) }) // never let srv.Close() hang
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second // in-package: arm the tick deadline directly
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	start := time.Now()
	tick, err := spawner.Spawn(PackedProject{
		Name:    projectName,
		Workdir: t.TempDir(),
	}, "sgap117-stalled-2026-09-14-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Spawn (gateway path) returns only after the POST resolves — here after
	// the per-turn deadline aborts it. The elapsed time is the live proof
	// that the stall was detected before the tick deadline.
	elapsed := time.Since(start)
	if elapsed >= 5*time.Second {
		t.Fatalf("Spawn blocked %v — per-turn deadline did not abort the stalled POST (tick deadline 30s)", elapsed)
	}

	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (stalled turn must be a STALLED failure, not TickTimeout)", outcome.Status, TickFailed)
	}
	if !strings.Contains(outcome.Error, "stalled") || !strings.Contains(outcome.Error, "no progress") {
		t.Errorf("Wait() error = %q, want text mentioning \"stalled\" and \"no progress\"", outcome.Error)
	}
	if outcome.Duration >= 5*time.Second {
		t.Errorf("outcome.Duration = %v — the stalled turn consumed nearly the whole tick-timeout slot", outcome.Duration)
	}

	// Slot bookkeeping: the spawn returned, so the slot must be free.
	if n := len(spawner.RunningSet()); n != 0 {
		t.Errorf("RunningSet() has %d entries after stalled spawn, want 0", n)
	}

	// Full-stack persistence proof (slot_pool → lifecycle.Complete): run the
	// same spawn through the loop's slot pool and assert the DB row lands
	// status=failed with the stall text in ticks.error.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.SetNoDeliver(true)
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	loop.SetNoExecFallback(true)
	loop.SetTickTimeout(30 * time.Second)
	loop.SetGatewayResponseTimeout(250 * time.Millisecond)

	fsStart := time.Now()
	tickID := loop.slotPool.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, time.Now(), true, db)
	status, ok := waitForTickTerminal(t, db, tickID, 5*time.Second)
	if !ok {
		t.Fatalf("tick %s never reached a terminal state within 5s — per-turn deadline did not fail the stalled POST", tickID)
	}
	if status != "failed" {
		t.Errorf("ticks.status = %q, want \"failed\" (stalled classification, schema-legal — no new enum value)", status)
	}
	errText := tickErrorOf(t, db, tickID)
	if !strings.Contains(errText, "stalled") || !strings.Contains(errText, "no progress") {
		t.Errorf("ticks.error = %q, want text mentioning \"stalled\" and \"no progress\"", errText)
	}
	// AC #4 live-verification shape: the terminal state must land well
	// before --tick-timeout (30s) — the measured pre-fix fingerprint was
	// completed_at = spawned_at + exactly 7200s.
	if fsElapsed := time.Since(fsStart); fsElapsed >= 5*time.Second {
		t.Errorf("slot-pool tick took %v to fail — stall not detected before the tick deadline", fsElapsed)
	}
	// Slot release is async: the DB row reaches its terminal state before
	// SlotPool.Running() drains. Poll up to 2s — the slot is freed by the
	// same goroutine that closed the tick row, so a 2s window covers the
	// observed 5/5 un-raced + 5/5 raced release lag. FND-001 (load-only
	// flake surface; 15eec5c CI-004 race-detector run): synchronous read
	// races the release on extreme host load (~loadavg 30).
	const releaseWait = 2 * time.Second
	releaseDeadline := time.Now().Add(releaseWait)
	var n int
	for {
		n = loop.slotPool.Running()
		if n == 0 {
			break
		}
		if time.Now().After(releaseDeadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n != 0 {
		t.Errorf("slot pool Running() = %d after stalled tick + %v bounded wait, want 0 (async release did not land)", n, releaseWait)
	}
}

// TestSpawn_GatewaySlowTurnUnderDeadlineStillCompletes: the per-turn
// deadline must NOT abort a legitimately slow turn — a gateway that answers
// after 300ms with a completed payload (turn deadline 2s) must complete the
// tick. Guards against an over-eager deadline.
func TestSpawn_GatewaySlowTurnUnderDeadlineStillCompletes(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap117-slow-turn-ok"
	mustCreateProjectINFRA012(t, db, projectName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_sgap117_slow","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tick done"}]}],"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`))
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(2 * time.Second)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap117-slow-2026-09-14-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s (a 300ms turn under a 2s per-turn deadline must complete)", outcome.Status, TickCompleted)
	}
}

// TestSpawn_GatewayResponseTimeoutDisabledRestoresLegacyBehavior: with the
// per-turn deadline disabled (0), a stalled POST runs until the TICK ctx
// expires (tick timeout 300ms here) and the spawn drops with the legacy
// gateway-failure classification (exec fallback disabled) — NOT the new
// "stalled" text. Pins the "0 disables" contract and that TickTimeout
// semantics are unchanged.
func TestSpawn_GatewayResponseTimeoutDisabledRestoresLegacyBehavior(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap117-legacy-off"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(stallGatewayHandler(requestSeen, release))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 300 * time.Millisecond
	spawner.SetGatewayResponseTimeout(0) // explicit disable

	start := time.Now()
	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap117-legacy-2026-09-14-00-00-00")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Spawn with a stalled POST and exec fallback disabled must return an error (legacy drop)")
	}
	if !strings.Contains(err.Error(), "gateway unreachable and exec fallback disabled") {
		t.Errorf("error = %q, want the legacy gateway-drop text", err.Error())
	}
	if strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %q — the stalled classification must NOT fire when the per-turn deadline is disabled", err.Error())
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("Spawn returned after %v — the tick ctx (300ms) must bound the POST when the per-turn deadline is off", elapsed)
	}
}

// TestSpawn_GatewayResponseTimeoutClampedToTickTimeout: a per-turn deadline
// >= the tick deadline collapses to the tick deadline (min() semantics) — a
// POST may never outlive --tick-timeout because of this knob. With the turn
// deadline 30s and the tick deadline 300ms, the stalled POST fails at ~300ms
// with the LEGACY classification (tick ctx expired), never the stalled one.
func TestSpawn_GatewayResponseTimeoutClampedToTickTimeout(t *testing.T) {
	db := newTestDB(t)
	const projectName = "sgap117-clamp"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(stallGatewayHandler(requestSeen, release))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	}()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 300 * time.Millisecond
	spawner.SetGatewayResponseTimeout(30 * time.Second) // >= tick deadline → min() collapses

	start := time.Now()
	_, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()},
		"sgap117-clamp-2026-09-14-00-00-00")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Spawn must fail — the POST cannot outlive the tick deadline")
	}
	if strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %q — with the turn deadline collapsed onto the tick deadline the classification must be the legacy tick-ctx expiry, not \"stalled\"", err.Error())
	}
	if elapsed >= 2*time.Second {
		t.Errorf("Spawn blocked %v — the 300ms tick ctx did not bound the POST", elapsed)
	}
}

// TestGatewayResponseTimeoutFromEnv pins the env resolution contract:
// unset → 30m default; "0s" → 0 (explicit disable); garbage → default;
// valid → the parsed value.
func TestGatewayResponseTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset → default", "", DefaultGatewayResponseTimeout},
		{"zero disables", "0s", 0},
		{"valid override", "5m", 5 * time.Minute},
		{"garbage → default", "not-a-duration", DefaultGatewayResponseTimeout},
		{"negative → default", "-1h", DefaultGatewayResponseTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envGatewayResponseTimeout, tc.env)
			if got := gatewayResponseTimeoutFromEnv(); got != tc.want {
				t.Errorf("gatewayResponseTimeoutFromEnv() = %v, want %v (env %q)", got, tc.want, tc.env)
			}
		})
	}
}

// TestSpawn_GatewayResponseTimeoutDefaultArmed: a Spawner built without an
// explicit SetGatewayResponseTimeout (env unset) arms the 30m default —
// the fleet-wide protection is on unless explicitly disabled.
func TestSpawn_GatewayResponseTimeoutDefaultArmed(t *testing.T) {
	t.Setenv(envGatewayResponseTimeout, "")
	s := NewSpawner(nil, 1)
	if got := s.GatewayResponseTimeout(); got != DefaultGatewayResponseTimeout {
		t.Errorf("default GatewayResponseTimeout = %v, want %v", got, DefaultGatewayResponseTimeout)
	}
}
