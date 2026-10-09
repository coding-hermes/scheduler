package api_test

// SCHED-GAP-1681 API surfaces: the armed transient-retry count must be
// introspectable on /api/v1/status (the loop mirror) and /api/v1/config
// (the resolved snapshot), next to its SCHED-GAP-117 sibling.

import (
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/api"
)

func TestAPI_Status_GatewayTransientRetries(t *testing.T) {
	a := newAPITestServer(t)

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	got, ok := body["gateway_transient_retries"].(float64)
	if !ok {
		t.Fatalf("gateway_transient_retries missing or wrong type: %T", body["gateway_transient_retries"])
	}
	if got != 3 {
		t.Errorf("gateway_transient_retries = %v, want 3 (the SCHED-GAP-1681 default)", got)
	}
	// The GAP-117 sibling must remain intact (no key displacement).
	if _, ok := body["gateway_response_timeout"].(string); !ok {
		t.Errorf("gateway_response_timeout missing from /api/v1/status after the 1681 addition")
	}
}

func TestAPI_Config_GatewayTransientRetries(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		TickTimeout:             "2h0m0s",
		GatewayResponseTimeout:  "30m0s",
		GatewayTransientRetries: 0, // explicit single-attempt
	})

	status, body := a.do(t, "GET", "/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// 0 = single attempt must survive the JSON round trip as an explicit
	// zero, not vanish (an absent key would read as the default 3).
	if got, _ := body["gateway_transient_retries"].(float64); got != 0 {
		t.Errorf("gateway_transient_retries = %v, want 0 (explicit single-attempt round-trips)", got)
	}
}
