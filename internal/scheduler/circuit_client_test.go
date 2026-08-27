package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── TASK-ROUTER-002: circuit-breaker recording client — unit tests ────────
//
// CircuitClient runs router_circuit.py record-failure/record-success with a
// bounded timeout. Fail-open contract: EVERY failure mode (disabled,
// missing script, timeout, exec error) returns false and is logged as a
// WARN — the spawn path must never be blocked by the circuit script. These
// tests pin the client contract with fake circuit scripts; they never
// invoke the real host script.

// writeFakeCircuit writes an executable shell script that appends its argv
// (one line per invocation) to captureFile, and returns its path. The
// capture file lets tests assert exact argv AND invocation ordering.
func writeFakeCircuit(t *testing.T, captureFile string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake_circuit.sh")
	script := "#!/bin/sh\necho \"$@\" >> \"" + captureFile + "\"\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake circuit: %v", err)
	}
	return path
}

// readCircuitCalls returns the recorded invocations, one per line.
func readCircuitCalls(t *testing.T, captureFile string) []string {
	t.Helper()
	b, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read circuit capture: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestCircuitClient_RecordExactArgvAndOrdering pins the wire contract:
// record-failure is invoked as `record-failure <provider> <model> [reason]`
// and record-success as `record-success <provider> <model>`, in call order
// (AC1: exact argv + ordering).
func TestCircuitClient_RecordExactArgvAndOrdering(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
	cc := NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0)
	if !cc.Enabled() {
		t.Fatal("configured CircuitClient Enabled() = false")
	}

	if ok := cc.RecordFailure(context.Background(), "provider-a", "model-b", "gateway 401/403 rejected pair"); !ok {
		t.Fatal("RecordFailure returned false on a healthy script")
	}
	if ok := cc.RecordSuccess(context.Background(), "provider-a", "model-b"); !ok {
		t.Fatal("RecordSuccess returned false on a healthy script")
	}

	calls := readCircuitCalls(t, capture)
	if len(calls) != 2 {
		t.Fatalf("captured %d invocations, want 2: %v", len(calls), calls)
	}
	wantFail := "record-failure provider-a model-b gateway 401/403 rejected pair"
	if calls[0] != wantFail {
		t.Errorf("first invocation = %q, want %q", calls[0], wantFail)
	}
	wantSuccess := "record-success provider-a model-b"
	if calls[1] != wantSuccess {
		t.Errorf("second invocation = %q, want %q", calls[1], wantSuccess)
	}
}

// TestCircuitClient_DisabledIsFailOpen: a nil/empty-argv client is
// "not configured" — Record* return false and never panic.
func TestCircuitClient_DisabledIsFailOpen(t *testing.T) {
	cc := NewCircuitClient(nil, 0)
	if cc.Enabled() {
		t.Error("nil-argv CircuitClient Enabled() = true, want false")
	}
	if ok := cc.RecordFailure(context.Background(), "p", "m", "r"); ok {
		t.Error("RecordFailure on disabled client = true, want false")
	}
	if ok := cc.RecordSuccess(context.Background(), "p", "m"); ok {
		t.Error("RecordSuccess on disabled client = true, want false")
	}

	empty := NewCircuitClient([]string{}, 0)
	if empty.Enabled() {
		t.Error("empty-argv CircuitClient Enabled() = true, want false")
	}
}

// TestCircuitClient_MissingScriptIsFailOpen: a missing circuit script is a
// logged WARN and a false return — never an error that could fail a spawn.
func TestCircuitClient_MissingScriptIsFailOpen(t *testing.T) {
	cc := NewCircuitClient([]string{filepath.Join(t.TempDir(), "no-such-circuit.py")}, 0)
	if ok := cc.RecordFailure(context.Background(), "p", "m", "r"); ok {
		t.Error("RecordFailure with missing script = true, want false")
	}
	if ok := cc.RecordSuccess(context.Background(), "p", "m"); ok {
		t.Error("RecordSuccess with missing script = true, want false")
	}
}

// TestCircuitClient_TimeoutIsFailOpen proves the bounded-timeout contract: a
// circuit script that outlives the client's timeout is killed and reported
// as false — the spawn can never be stalled by a wedged circuit script.
func TestCircuitClient_TimeoutIsFailOpen(t *testing.T) {
	slow := writeRawRouter(t, "#!/bin/sh\nsleep 2\n")
	cc := NewCircuitClient([]string{slow}, 100*time.Millisecond)

	start := time.Now()
	ok := cc.RecordFailure(context.Background(), "p", "m", "r")
	elapsed := time.Since(start)

	if ok {
		t.Fatal("RecordFailure ok = true, want false on timeout")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("RecordFailure took %v — circuit timeout did not bound the invocation", elapsed)
	}
}
