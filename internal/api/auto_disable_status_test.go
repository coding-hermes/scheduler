package api_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
)

// TestAPI_Status_AutoDisableConfig (GAP-047) verifies /api/v1/status carries
// an "auto_disable" block with the resolved policy (enabled/threshold/window/
// min_ticks) and that projects_failure_rates entries surface the per-project
// auto_disable_armed flag computed from the same policy.
func TestAPI_Status_AutoDisableConfig(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		AutoDisableFailureRate: 0.5,
		AutoDisableWindow:      100,
		AutoDisableMinTicks:    5,
	})
	mustCreateAPITestProject(t, a.db, "alpha")

	// alpha: 8 failed + 2 completed = 80% failure over 10 ticks → armed
	// under threshold 0.5 / minTicks 5.
	now := time.Now()
	for i := 0; i < 8; i++ {
		insertAPITestTick(t, a.db, "alpha-fail-"+strconv.Itoa(i), "alpha", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertAPITestTick(t, a.db, "alpha-ok-"+strconv.Itoa(i), "alpha", "completed",
			now.Add(-time.Duration(2-i)*time.Minute))
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// --- auto_disable config block ---
	ad, ok := body["auto_disable"].(map[string]interface{})
	if !ok {
		t.Fatalf("auto_disable missing or wrong type: %T", body["auto_disable"])
	}
	if enabled, ok := ad["enabled"].(bool); !ok || !enabled {
		t.Errorf("auto_disable.enabled = %v, want true", ad["enabled"])
	}
	if th, ok := ad["threshold"].(float64); !ok || th != 0.5 {
		t.Errorf("auto_disable.threshold = %v, want 0.5", ad["threshold"])
	}
	if w, ok := ad["window"].(float64); !ok || int(w) != 100 {
		t.Errorf("auto_disable.window = %v, want 100", ad["window"])
	}
	if mt, ok := ad["min_ticks"].(float64); !ok || int(mt) != 5 {
		t.Errorf("auto_disable.min_ticks = %v, want 5", ad["min_ticks"])
	}

	// --- per-project armed state ---
	rates, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}
	alpha, ok := rates["alpha"].(map[string]interface{})
	if !ok {
		t.Fatalf("alpha missing from projects_failure_rates: %v", rates)
	}
	if armed, ok := alpha["auto_disable_armed"].(bool); !ok || !armed {
		t.Errorf("alpha auto_disable_armed = %v, want true (rate 0.8 >= 0.5, total 10 >= 5)",
			alpha["auto_disable_armed"])
	}
}

// TestAPI_Status_AutoDisableZeroValue (GAP-047) verifies the guard for a
// Server constructed without SetResolvedConfig (zero-value ResolvedConfig):
// auto_disable.enabled must be false, no panic, and no project may be armed
// even at a catastrophic failure rate.
func TestAPI_Status_AutoDisableZeroValue(t *testing.T) {
	a := newAPITestServer(t) // never calls SetResolvedConfig
	mustCreateAPITestProject(t, a.db, "alpha")

	now := time.Now()
	for i := 0; i < 10; i++ {
		insertAPITestTick(t, a.db, "alpha-fail-"+strconv.Itoa(i), "alpha", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	ad, ok := body["auto_disable"].(map[string]interface{})
	if !ok {
		t.Fatalf("auto_disable missing from status payload: %v", body)
	}
	if enabled, ok := ad["enabled"].(bool); !ok || enabled {
		t.Errorf("auto_disable.enabled = %v, want false (zero-value resolved config)", ad["enabled"])
	}
	// The block must still carry its keys (threshold 0, window default 100).
	if th, ok := ad["threshold"].(float64); !ok || th != 0 {
		t.Errorf("auto_disable.threshold = %v, want 0", ad["threshold"])
	}

	rates, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}
	alpha, ok := rates["alpha"].(map[string]interface{})
	if !ok {
		t.Fatalf("alpha missing from projects_failure_rates: %v", rates)
	}
	if armed, ok := alpha["auto_disable_armed"].(bool); !ok || armed {
		t.Errorf("alpha auto_disable_armed = %v, want false (threshold == 0)", alpha["auto_disable_armed"])
	}
}
