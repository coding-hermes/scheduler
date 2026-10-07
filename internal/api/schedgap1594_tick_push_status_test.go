package api_test

// SCHED-GAP-1594 — /api/v1/status carries the tick_push block so an
// operator (or agent) can read the live-vs-pushed state from the API
// instead of grepping scheduler log lines: mode=push means the spawner
// pushes each tick's commits at tick exit (SCHED-GAP-1694 default);
// mode=local-only means --disable-tick-PUSH armed web-primary and commits
// stay local until the fleet-strand-push cron or an operator pushes.

import (
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func TestSCHEDGAP1594_StatusExposesTickPush(t *testing.T) {
	a := newAPITestServer(t)

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	raw, ok := body["tick_push"]
	if !ok {
		t.Fatalf("tick_push missing from /api/v1/status: %v", body)
	}
	block, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("tick_push = %T, want an object", raw)
	}
	for _, key := range []string{"mode", "disabled"} {
		if _, ok := block[key]; !ok {
			t.Errorf("tick_push.%s missing: %v", key, block)
		}
	}
	// The test server never arms the switch: the default state MUST read
	// push, never a fabricated local-only.
	if mode, _ := block["mode"].(string); mode != "push" {
		t.Errorf("tick_push.mode = %v, want \"push\" (SCHED-GAP-1694 default untouched)", block)
	}
	if disabled, _ := block["disabled"].(bool); disabled {
		t.Errorf("tick_push.disabled = true on an unarmed server — the surface must report the LIVE state, not assume it: %v", block)
	}
}

// TestSCHEDGAP1594_StatusTickPushReflectsToggle flips the package state and
// proves the block follows it — the surface reads the spawner's decision
// (via TickPushDisabledDefault), never a hardcoded value. Restores the
// state before returning so other tests in the package are untouched.
func TestSCHEDGAP1594_StatusTickPushReflectsToggle(t *testing.T) {
	a := newAPITestServer(t)

	scheduler.SetTickPushDisabledDefaultForTest(true)
	t.Cleanup(func() { scheduler.SetTickPushDisabledDefaultForTest(false) })

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	block, ok := body["tick_push"].(map[string]interface{})
	if !ok {
		t.Fatalf("tick_push = %T, want an object: %v", body["tick_push"], body)
	}
	if mode, _ := block["mode"].(string); mode != "local-only" {
		t.Errorf("tick_push.mode = %v, want \"local-only\" while the switch is armed", block)
	}
	if disabled, _ := block["disabled"].(bool); !disabled {
		t.Errorf("tick_push.disabled = false while the switch is armed: %v", block)
	}
}
