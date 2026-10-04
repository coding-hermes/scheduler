package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/api"
)

// SCHED-GAP-1707 acceptance, API-snapshot surface: /api/v1/config must
// carry the session-silence grace the daemon resolved (the four-surface
// knob convention). SetResolvedConfig stores it; the GET endpoint serves it
// unchanged (the gateway-key mask is the only mutation on that path).
func TestSCHEDGAP1707_SessionSilenceGraceOnConfigSnapshot(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		SessionSilenceGrace: "45m0s",
	})
	code, cfg := a.do(t, "GET", "/api/v1/config", nil)
	if code != 200 {
		t.Fatalf("GET /api/v1/config = %d, want 200", code)
	}
	if got := cfg["session_silence_grace"]; got != "45m0s" {
		t.Errorf("/api/v1/config session_silence_grace = %v, want \"45m0s\"", got)
	}

	// The zero value reads as the documented disabled default ("0s" string
	// comes from main.go's Duration.String(); a Server never Set'ed reads
	// the struct zero "" — assert the key EXISTS in the payload).
	raw, ok := cfg["session_silence_grace"]
	if !ok {
		t.Fatal("/api/v1/config payload missing the session_silence_grace key")
	}
	if _, isStr := raw.(string); !isStr {
		t.Errorf("session_silence_grace = %T, want string", raw)
	}
}

// The JSON key name is pinned by the snapshot struct's tag; assert it
// survives marshaling (a renamed tag would silently break operators'
// dashboards).
func TestSCHEDGAP1707_ConfigSnapshotJSONKey(t *testing.T) {
	b, err := json.Marshal(api.ResolvedConfig{SessionSilenceGrace: "1h0m0s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"session_silence_grace":"1h0m0s"`) {
		t.Errorf("snapshot JSON missing \"session_silence_grace\":\"1h0m0s\": %s", b)
	}
}
