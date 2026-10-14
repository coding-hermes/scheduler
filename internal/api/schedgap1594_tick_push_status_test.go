package api_test

import (
	"net/http"
	"testing"
)

// SCHED-GAP-1594 — /api/v1/status must expose whether the per-tick git push
// is disabled. The field is the machine-readable twin of the dashboard's
// live/pushed health card: an operator (or the daily digest builder) reads
// `tick_push_disabled` instead of grepping config. Default false — the
// SCHED-GAP-1694 push-at-exit stays on (backward compat).
func TestSCHEDGAP1594_StatusExposesTickPushDisabled(t *testing.T) {
	a := newAPITestServer(t)

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if v, ok := body["tick_push_disabled"]; !ok {
		t.Fatalf("tick_push_disabled missing from /api/v1/status: %v", body)
	} else if b, ok := v.(bool); !ok || b {
		t.Errorf("tick_push_disabled = %v, want false on a default server", v)
	}

	// Flip the flag on the loop the server was built with and re-read: the
	// field must track the live daemon state, not a boot-time snapshot.
	a.loop.SetTickPushDisabled(true)
	status, body = a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if v, ok := body["tick_push_disabled"]; !ok {
		t.Fatalf("tick_push_disabled missing after SetTickPushDisabled(true): %v", body)
	} else if b, ok := v.(bool); !ok || !b {
		t.Errorf("tick_push_disabled = %v, want true after SetTickPushDisabled(true)", v)
	}
}
