package api_test

import (
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/api"
)

// SCHED-GAP-117: the per-turn gateway POST deadline must be observable on
// both config surfaces — /api/v1/status (the live armed value from the
// loop's spawner) and /api/v1/config (the startup resolved-config
// snapshot).

// TestAPI_Status_GatewayResponseTimeout pins the /api/v1/status surface:
// the loop's default-armed spawner reports the 30m default.
func TestAPI_Status_GatewayResponseTimeout(t *testing.T) {
	a := newAPITestServer(t)

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	got, ok := body["gateway_response_timeout"].(string)
	if !ok {
		t.Fatalf("gateway_response_timeout missing or wrong type: %T", body["gateway_response_timeout"])
	}
	if got != "30m0s" {
		t.Errorf("gateway_response_timeout = %q, want \"30m0s\" (the SCHED-GAP-117 default)", got)
	}
}

// TestAPI_Config_GatewayResponseTimeout pins the /api/v1/config surface:
// the resolved-config snapshot carries the startup value verbatim.
func TestAPI_Config_GatewayResponseTimeout(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		TickTimeout:            "2h0m0s",
		GatewayResponseTimeout: "30m0s",
	})

	status, body := a.do(t, "GET", "/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got, _ := body["gateway_response_timeout"].(string); got != "30m0s" {
		t.Errorf("gateway_response_timeout = %q, want \"30m0s\"", got)
	}
	// The sibling field must remain intact (no struct-field drift).
	if got, _ := body["tick_timeout"].(string); got != "2h0m0s" {
		t.Errorf("tick_timeout = %q, want \"2h0m0s\"", got)
	}
}
