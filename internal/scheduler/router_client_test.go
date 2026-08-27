package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── TASK-ROUTER-001: spawn-time model/provider resolution — unit tests ─────
//
// The task router (router_spawn.py) resolves a project to the cheapest
// HEALTHY (provider, model) pair at spawn time. RouterClient runs the
// router command with a bounded timeout and parses its JSON output.
// Fail-open contract: EVERY failure mode (disabled, missing script,
// non-zero exit, non-JSON output, router error object, timeout, null head
// / gate != OPEN) yields a fallback signal — never an error that could
// block a spawn. These tests pin the client contract with fake router
// scripts; they never invoke the real host router.

// writeFakeRouter writes an executable shell script that prints body to
// stdout and returns its path.
func writeFakeRouter(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake_router.sh")
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake router: %v", err)
	}
	return path
}

// writeRawRouter writes an executable shell script verbatim (no heredoc
// wrapping) — for fakes that must actually RUN commands (exit codes,
// sleeps) rather than print JSON.
func writeRawRouter(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake_router_raw.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile raw router: %v", err)
	}
	return path
}

const openRouterJSON = `{
  "project": "test-project",
  "profile": "P0_FORE",
  "resolved_at": "2026-08-27T00:00:00+00:00",
  "head": {
    "hop": 1,
    "provider": "router-provider",
    "model": "router-model",
    "usd_1m": 0.033,
    "data_class": "zdr"
  },
  "chain": [],
  "exclusions": [],
  "gate_reasons": [],
  "gate": "OPEN"
}`

// TestRouterClient_Resolve pins the client contract: a canned OPEN JSON
// parses into a RouterResult whose head resolves to the router's
// model/provider.
func TestRouterClient_Resolve(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		timeout    time.Duration
		wantOK     bool
		wantGate   string
		wantReason string // substring; empty = no reason assertion
	}{
		{
			name:     "open head parses",
			argv:     []string{writeFakeRouter(t, openRouterJSON)},
			wantOK:   true,
			wantGate: "OPEN",
		},
		{
			name:       "null head with NO-OPEN-HOP is a fallback",
			argv:       []string{writeFakeRouter(t, `{"project":"p","profile":"P0_FORE","gate":"NO-OPEN-HOP","head":null}`)},
			wantOK:     false,
			wantReason: "no open head",
		},
		{
			name:       "missing script is a fallback",
			argv:       []string{filepath.Join(t.TempDir(), "no-such-router.py")},
			wantOK:     false,
			wantReason: "exec error",
		},
		{
			name:       "non-zero exit is a fallback",
			argv:       []string{writeRawRouter(t, "#!/bin/sh\nexit 3\n")},
			wantOK:     false,
			wantReason: "exec error",
		},
		{
			name:       "non-JSON output is a fallback",
			argv:       []string{writeFakeRouter(t, "this is not json")},
			wantOK:     false,
			wantReason: "non-JSON",
		},
		{
			name:       "router error object is a fallback",
			argv:       []string{writeFakeRouter(t, `{"error": "project not in registry"}`)},
			wantOK:     false,
			wantReason: "project not in registry",
		},
		{
			name:       "nil client is disabled",
			argv:       nil,
			timeout:    0,
			wantOK:     false,
			wantReason: "not configured",
		},
		{
			name:       "empty argv is disabled",
			argv:       []string{},
			timeout:    0,
			wantOK:     false,
			wantReason: "not configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := NewRouterClient(tt.argv, tt.timeout)
			res, ok, reason := rc.Resolve(context.Background(), "test-project")
			if ok != tt.wantOK {
				t.Fatalf("Resolve ok = %v, want %v (reason=%q)", ok, tt.wantOK, reason)
			}
			if !ok {
				if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
					t.Errorf("reason = %q, want it to contain %q", reason, tt.wantReason)
				}
				return
			}
			if res.Gate != tt.wantGate {
				t.Errorf("gate = %q, want %q", res.Gate, tt.wantGate)
			}
			if tt.wantGate == "OPEN" {
				m, p, headOK := res.OpenHead()
				if !headOK {
					t.Fatal("OpenHead() = false, want true for OPEN head")
				}
				if m != "router-model" || p != "router-provider" {
					t.Errorf("OpenHead() = (%q, %q), want (\"router-model\", \"router-provider\")", m, p)
				}
			}
		})
	}
}

// TestRouterResult_OpenHead pins the usability rule: only gate OPEN with a
// non-nil head carrying BOTH fields yields a usable model/provider.
func TestRouterResult_OpenHead(t *testing.T) {
	tests := []struct {
		name   string
		res    RouterResult
		wantOK bool
		wantM  string
		wantP  string
	}{
		{
			name:   "OPEN with full head",
			res:    RouterResult{Gate: "OPEN", Head: &RouterHead{Provider: "p", Model: "m"}},
			wantOK: true,
			wantM:  "m",
			wantP:  "p",
		},
		{
			name:   "OPEN with nil head",
			res:    RouterResult{Gate: "OPEN", Head: nil},
			wantOK: false,
		},
		{
			name:   "NO-OPEN-HOP with nil head",
			res:    RouterResult{Gate: "NO-OPEN-HOP", Head: nil},
			wantOK: false,
		},
		{
			name:   "NO-CHAIN with nil head",
			res:    RouterResult{Gate: "NO-CHAIN", Head: nil},
			wantOK: false,
		},
		{
			name:   "OPEN head with empty model",
			res:    RouterResult{Gate: "OPEN", Head: &RouterHead{Provider: "p", Model: ""}},
			wantOK: false,
		},
		{
			name:   "OPEN head with empty provider",
			res:    RouterResult{Gate: "OPEN", Head: &RouterHead{Provider: "", Model: "m"}},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, p, ok := tt.res.OpenHead()
			if ok != tt.wantOK {
				t.Fatalf("OpenHead() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (m != tt.wantM || p != tt.wantP) {
				t.Errorf("OpenHead() = (%q, %q), want (%q, %q)", m, p, tt.wantM, tt.wantP)
			}
		})
	}
}

// TestRouterClient_Resolve_Timeout proves the bounded-timeout contract: a
// router that outlives the client's timeout is killed and reported as a
// fallback — the spawn can never be stalled by a wedged router. The test
// uses a tiny injectable timeout so it stays fast.
func TestRouterClient_Resolve_Timeout(t *testing.T) {
	slow := writeRawRouter(t, "#!/bin/sh\nsleep 2\n")
	rc := NewRouterClient([]string{slow}, 100*time.Millisecond)

	start := time.Now()
	res, ok, reason := rc.Resolve(context.Background(), "test-project")
	elapsed := time.Since(start)

	if ok {
		t.Fatalf("Resolve ok = true, want false on timeout (res=%+v)", res)
	}
	if !strings.Contains(reason, "timed out") {
		t.Errorf("reason = %q, want it to contain %q", reason, "timed out")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Resolve took %v — router timeout did not bound the invocation", elapsed)
	}
}
