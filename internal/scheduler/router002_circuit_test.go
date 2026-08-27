package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── TASK-ROUTER-002: spawn-path circuit recording — integration tests ─────
//
// These pin the PASS criteria at the Spawn() level:
//   - AC1: on a gateway 401/403 the scheduler records the failing pair via
//     router_circuit.py record-failure (exact argv), then retries the NEXT
//     chain hop; on success it records record-success for the pair that
//     actually ran.
//   - AC2: a rejected head pair never gets re-sent in the same tick — the
//     retry advances to chain hop 2 (one attempt per hop per tick).
//   - AC4: exactly one SendResponse per hop per tick (2 dispatches max on
//     a rejected primary + successful retry; no retry loops).
//   - Fail-open: a nil circuit client records nothing and the spawn path
//     behaves exactly as before the breaker.

// circuitSpawnerForGateway builds a chain spawner (newChainSpawnerForGateway)
// plus a circuit client whose invocations are captured to a file. Returns
// the spawner, the capture path, and the gateway stub.
func circuitSpawnerForGateway(t *testing.T, gw *chainGatewayServer) (*Spawner, string) {
	t.Helper()
	s := newChainSpawnerForGateway(t, gw)
	capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
	s.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))
	return s, capture
}

// TestSpawn_CircuitRecordsFailureAndSuccessOn401Retry is the AC1+AC2+AC4
// core: the gateway rejects the resolved head pair with 401, the scheduler
// records record-failure for that pair, retries ONCE with chain hop 2
// (never re-sending the rejected pair), and on retry success records
// record-success for the pair that actually ran.
func TestSpawn_CircuitRecordsFailureAndSuccessOn401Retry(t *testing.T) {
	gw := &chainGatewayServer{rejectModel: "proj-model"}
	s, capture := circuitSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "router002-retry",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "router002-retry-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway retry success")
	}
	tick.Wait()

	// AC2/AC4: exactly two dispatches — the rejected head hop once, then
	// chain hop 2 once. The rejected pair is never re-sent.
	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (rejected head + one retry hop)", len(dispatches))
	}
	if dispatches[0].model != "proj-model" || dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("first dispatch = (%q, %d), want (proj-model, 401)", dispatches[0].model, dispatches[0].status)
	}
	if dispatches[1].model != "proj-fallback-model" || dispatches[1].provider != "proj-fallback-provider" {
		t.Errorf("retry dispatch = (%q, %q), want chain hop 2 (proj-fallback-model, proj-fallback-provider)",
			dispatches[1].model, dispatches[1].provider)
	}
	if dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch status = %d, want 200", dispatches[1].status)
	}

	// AC1: exact argv + ordering — record-failure for the REJECTED pair
	// first, then record-success for the pair that ACTUALLY ran.
	calls := readCircuitCalls(t, capture)
	if len(calls) != 2 {
		t.Fatalf("circuit invocations = %d, want 2 (record-failure + record-success): %v", len(calls), calls)
	}
	wantFail := "record-failure proj-provider proj-model gateway 401/403 rejected pair"
	if calls[0] != wantFail {
		t.Errorf("first circuit call = %q, want %q", calls[0], wantFail)
	}
	wantSuccess := "record-success proj-fallback-provider proj-fallback-model"
	if calls[1] != wantSuccess {
		t.Errorf("second circuit call = %q, want %q", calls[1], wantSuccess)
	}
}

// TestSpawn_CircuitRecordsFailureOnExhaustedAuth: when the retry hop is
// rejected too (chain exhausted), BOTH pairs are recorded — the retry hop
// too — and there are exactly two dispatches (AC4: no retry flood).
func TestSpawn_CircuitRecordsFailureOnExhaustedAuth(t *testing.T) {
	gw := &chainGatewayServer{rejectAll: true}
	s, capture := circuitSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "router002-exhausted",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "router002-exhausted-2026-08-27-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error when chain exhausted — 401 must surface")
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on exhausted auth failure")
	}

	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Errorf("dispatch count = %d, want exactly 2 (head + one retry hop, then stop)", len(dispatches))
	}

	// Both pairs recorded — the head and the retry hop.
	calls := readCircuitCalls(t, capture)
	var headRecorded, retryRecorded bool
	for _, c := range calls {
		if strings.HasPrefix(c, "record-failure proj-provider proj-model ") {
			headRecorded = true
		}
		if strings.HasPrefix(c, "record-failure proj-fallback-provider proj-fallback-model ") {
			retryRecorded = true
		}
	}
	if !headRecorded {
		t.Errorf("missing record-failure for rejected head pair; calls: %v", calls)
	}
	if !retryRecorded {
		t.Errorf("missing record-failure for rejected retry hop; calls: %v", calls)
	}
	// The head must be recorded exactly ONCE (the GATEWAY FAIL block must
	// not double-record after the 401 path already did).
	headCount := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "record-failure proj-provider proj-model ") {
			headCount++
		}
	}
	if headCount != 1 {
		t.Errorf("head pair recorded %d times, want exactly 1 (no double-record); calls: %v", headCount, calls)
	}
}

// TestSpawn_CircuitRecordsSuccessOnGatewayOK: a clean gateway spawn records
// record-success for the pair that ran (AC1 success path) and no failures.
func TestSpawn_CircuitRecordsSuccessOnGatewayOK(t *testing.T) {
	gw := &chainGatewayServer{}
	s, capture := circuitSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:     "router002-ok",
		Workdir:  t.TempDir(),
		Model:    "proj-model",
		Provider: "proj-provider",
	}
	tick, err := s.Spawn(project, "router002-ok-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	calls := readCircuitCalls(t, capture)
	if len(calls) != 1 {
		t.Fatalf("circuit invocations = %d, want 1: %v", len(calls), calls)
	}
	if calls[0] != "record-success proj-provider proj-model" {
		t.Errorf("circuit call = %q, want %q", calls[0], "record-success proj-provider proj-model")
	}
}

// TestSpawn_CircuitDisabledIsByteIdentical: with NO circuit client the
// spawn path records nothing and behaves exactly as before the breaker
// (AC3 fail-open) — the gateway still retries the chain hop on 401.
func TestSpawn_CircuitDisabledIsByteIdentical(t *testing.T) {
	gw := &chainGatewayServer{rejectModel: "proj-model"}
	s := newChainSpawnerForGateway(t, gw) // no SetCircuitClient → nil

	project := PackedProject{
		Name:             "router002-nocircuit",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "router002-nocircuit-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want 2 (chain retry still works without the circuit client)", len(dispatches))
	}
	if dispatches[1].model != "proj-fallback-model" || dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch = (%q, %d), want (proj-fallback-model, 200)", dispatches[1].model, dispatches[1].status)
	}
}

// newRejectingGateway returns an httptest server whose /health is 200 and
// whose /v1/responses always returns the given status. Used to exercise the
// non-auth gateway failure path (HTTP error → record-failure).
func newRejectingGateway(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(status)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":{"type":"internal_error","message":"boom"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSpawn_CircuitRecordsFailureOnGatewayError: a non-auth gateway failure
// (HTTP 500) records record-failure for the attempted pair (AC1: HTTP error
// path), then falls through to the exec fallback.
func TestSpawn_CircuitRecordsFailureOnGatewayError(t *testing.T) {
	// A gateway stub that 500s every /v1/responses but answers /health.
	srv := newRejectingGateway(t, http.StatusInternalServerError)
	db := newTestDB(t)
	s := newChainSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv, "sk-daemon-shared", 5))
	s.SetNoExecFallback(true) // keep the failure loud; no exec fallback needed

	capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
	s.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))

	project := PackedProject{
		Name:     "router002-gwerr",
		Workdir:  t.TempDir(),
		Model:    "proj-model",
		Provider: "proj-provider",
	}
	_, err := s.Spawn(project, "router002-gwerr-2026-08-27-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error on gateway 500 with exec fallback disabled")
	}
	calls := readCircuitCalls(t, capture)
	if len(calls) != 1 {
		t.Fatalf("circuit invocations = %d, want 1: %v", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "record-failure proj-provider proj-model gateway failure:") {
		t.Errorf("circuit call = %q, want record-failure with gateway failure reason", calls[0])
	}
}

// TestSpawn_CircuitRecordsFailureAndSuccessOnExecPath: the exec path
// records record-success when the process starts; a start failure records
// record-failure (AC1 exec path).
func TestSpawn_CircuitRecordsFailureAndSuccessOnExecPath(t *testing.T) {
	// Success path: fake hermes that exits immediately.
	db := newTestDB(t)
	s := NewSpawner(db, 1)
	s.SetGatewayClient(nil) // force exec path
	s.SetNoExecFallback(false)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
	s.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))

	project := PackedProject{
		Name:     "router002-exec-ok",
		Workdir:  t.TempDir(),
		Model:    "proj-model",
		Provider: "proj-provider",
	}
	tick, err := s.Spawn(project, "router002-exec-ok-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	tick.Wait()
	calls := readCircuitCalls(t, capture)
	if len(calls) != 1 || calls[0] != "record-success proj-provider proj-model" {
		t.Errorf("exec success calls = %v, want [record-success proj-provider proj-model]", calls)
	}

	// Failure path: hermes that cannot start (nonexistent binary via a
	// PATH without hermes).
	db2 := newTestDB(t)
	s2 := NewSpawner(db2, 1)
	s2.SetGatewayClient(nil)
	s2.SetNoExecFallback(false)
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir) // no hermes binary → cmd.Start fails
	capture2 := filepath.Join(t.TempDir(), "circuit_calls2.txt")
	s2.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture2)}, 0))

	_, err = s2.Spawn(PackedProject{
		Name:     "router002-exec-fail",
		Workdir:  t.TempDir(),
		Model:    "proj-model",
		Provider: "proj-provider",
	}, "router002-exec-fail-2026-08-27-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error with no hermes binary in PATH")
	}
	calls2 := readCircuitCalls(t, capture2)
	if len(calls2) != 1 || !strings.HasPrefix(calls2[0], "record-failure proj-provider proj-model exec start error:") {
		t.Errorf("exec failure calls = %v, want [record-failure proj-provider proj-model exec start error: ...]", calls2)
	}
}

// TestSpawn_AC5_LiveCircuitScriptFullChain is the AC5 evidence test: it
// drives the REAL router_circuit.py (the router-ops script) through the
// scheduler's spawn path with HOME redirected to a temp dir, so the shared
// state file is untouched. A forced head-pair failure (gateway 401 on
// proj-model) must: (1) record the open circuit entry into circuit-state.json,
// (2) advance the retry to chain hop 2 (never re-sending the failed pair),
// and (3) record success for hop 2 so only the failed pair stays open.
// Skipped when the real script is absent (e.g. CI runners without
// /home/kara/.hermes).
func TestSpawn_AC5_LiveCircuitScriptFullChain(t *testing.T) {
	const (
		interp = "/home/kara/.hermes/venvs/board/bin/python3"
		script = "/home/kara/.hermes/scripts/router_circuit.py"
	)
	if _, err := os.Stat(interp); err != nil {
		t.Skipf("real circuit interpreter %s not present — skipping live-script test", interp)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("real circuit script %s not present — skipping live-script test", script)
	}

	// Redirect HOME so the real shared circuit-state.json is never touched.
	home := t.TempDir()
	t.Setenv("HOME", home)

	gw := &chainGatewayServer{rejectModel: "proj-model"}
	s := newChainSpawnerForGateway(t, gw)
	s.SetCircuitClient(NewCircuitClient([]string{interp, script}, 0))

	project := PackedProject{
		Name:             "router002-ac5",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "router002-ac5-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	tick.Wait()

	// The failed head pair was never re-sent — the retry advanced to
	// chain hop 2 exactly once.
	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want 2 (head + hop 2)", len(dispatches))
	}
	if dispatches[0].model != "proj-model" || dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("first dispatch = (%q, %d), want (proj-model, 401)", dispatches[0].model, dispatches[0].status)
	}
	if dispatches[1].model != "proj-fallback-model" || dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch = (%q, %d), want (proj-fallback-model, 200) — hop 2", dispatches[1].model, dispatches[1].status)
	}

	// The breaker state file (in the redirected HOME) shows the OPEN
	// entry for the failed head pair — and hop 2's success closed/never
	// recorded it.
	statePath := home + "/.hermes/model-router/circuit-state.json"
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read circuit-state.json: %v — the scheduler never recorded the failure", err)
	}
	var state struct {
		Pairs map[string]struct {
			Failures  int    `json:"failures"`
			OpenUntil string `json:"open_until"`
			Reason    string `json:"reason"`
		} `json:"pairs"`
	}
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatalf("parse circuit-state.json: %v", err)
	}
	headEntry, ok := state.Pairs["proj-provider/proj-model"]
	if !ok {
		t.Fatalf("circuit-state.json missing open entry for proj-provider/proj-model; got: %s", stateBytes)
	}
	if headEntry.Failures != 1 {
		t.Errorf("head failures = %d, want 1", headEntry.Failures)
	}
	if headEntry.OpenUntil == "" {
		t.Error("head open_until empty — circuit not open")
	}
	if _, ok := state.Pairs["proj-fallback-provider/proj-fallback-model"]; ok {
		t.Errorf("hop-2 pair must NOT be open (its success closed it); got: %s", stateBytes)
	}

	// The real script's own status view agrees (same redirected HOME).
	status := runRealCircuitStatus(t, home, interp, script)
	if !strings.Contains(status, "proj-provider/proj-model") {
		t.Errorf("router_circuit.py status missing open pair; got:\n%s", status)
	}
}

// runRealCircuitStatus runs `router_circuit.py status` with the given HOME
// and returns its stdout.
func runRealCircuitStatus(t *testing.T, home, interp, script string) string {
	t.Helper()
	cmd := exec.Command(interp, script, "status")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("router_circuit.py status: %v", err)
	}
	return string(out)
}

// TestSpawn_CircuitRecordsFailureOnTickTimeoutOrFailed pins the completion
// path (AC1: timeout/tick failure): an exec tick that runs to a timeout or
// failed outcome records record-failure for the (provider, model) pair it
// ran with — threaded through SpawnedTick.model/provider. Uses a fake
// `hermes` in PATH (sleep for timeout, exit 3 for failure) so the spawn
// resolves a real pair through the regular exec path.
func TestSpawn_CircuitRecordsFailureOnTickTimeoutOrFailed(t *testing.T) {
	tests := []struct {
		name    string
		hermes  string
		wantErr string // substring of the outcome error
	}{
		{
			name:   "timeout",
			hermes: "#!/bin/sh\nsleep 30\n",
		},
		{
			name:   "failed",
			hermes: "#!/bin/sh\nexit 3\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestDB(t)
			s := NewSpawner(db, 1)
			s.SetGatewayClient(nil)
			s.SetNoExecFallback(false)
			s.timeout = 200 * time.Millisecond

			dir := t.TempDir()
			scriptPath := filepath.Join(dir, "hermes")
			if err := os.WriteFile(scriptPath, []byte(tt.hermes), 0o755); err != nil {
				t.Fatalf("write fake hermes: %v", err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

			capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
			s.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))

			project := PackedProject{
				Name:     "router002-tick-" + tt.name,
				Workdir:  t.TempDir(),
				Model:    "proj-model",
				Provider: "proj-provider",
			}
			tick, err := s.Spawn(project, "router002-tick-"+tt.name+"-2026-08-27-00-00-00")
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			tick.Wait()

			// The exec spawn records record-success at cmd.Start(), and
			// the completion path records record-failure for the
			// timeout/failed outcome — BOTH hook points fire for a tick
			// that started but did not complete. The net breaker state
			// is open (the failure lands last).
			calls := readCircuitCalls(t, capture)
			if len(calls) != 2 {
				t.Fatalf("circuit invocations = %d, want 2 (start success + completion failure): %v", len(calls), calls)
			}
			if calls[0] != "record-success proj-provider proj-model" {
				t.Errorf("first circuit call = %q, want %q (spawn started)", calls[0], "record-success proj-provider proj-model")
			}
			wantPrefix := "record-failure proj-provider proj-model tick "
			if !strings.HasPrefix(calls[1], wantPrefix) {
				t.Errorf("second circuit call = %q, want prefix %q", calls[1], wantPrefix)
			}
		})
	}
}
