package api_test

import (
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/api"
)

// ADV-R11 (GAP-048 cure): the per-spawn memory cap must be observable on
// the /api/v1/config surface — the startup resolved-config snapshot
// carries the armed value verbatim so an operator can confirm the daemon
// booted with the cap (0 = off).

// TestAPI_Config_SpawnMemLimitMB_OffByDefault pins the zero-value
// snapshot: a server whose resolved config never set the field reports
// 0 (off) — and the key is present, not missing.
func TestAPI_Config_SpawnMemLimitMB_OffByDefault(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{})

	status, body := a.do(t, "GET", "/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	v, ok := body["spawn_mem_limit_mb"]
	if !ok {
		t.Fatal("spawn_mem_limit_mb missing from /api/v1/config")
	}
	got, ok := v.(float64)
	if !ok {
		t.Fatalf("spawn_mem_limit_mb wrong type: %T", v)
	}
	if got != 0 {
		t.Errorf("spawn_mem_limit_mb = %v, want 0 (off by default)", got)
	}
}

// TestAPI_Config_SpawnMemLimitMB_CarriesArmedValue pins the armed value
// flowing verbatim through the snapshot.
func TestAPI_Config_SpawnMemLimitMB_CarriesArmedValue(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		SlotPatience:    "5m0s",
		SpawnMemLimitMB: 512,
	})

	status, body := a.do(t, "GET", "/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	got, _ := body["spawn_mem_limit_mb"].(float64)
	if got != 512 {
		t.Errorf("spawn_mem_limit_mb = %v, want 512", got)
	}
	// The sibling field must remain intact (no struct-field drift).
	if sp, _ := body["slot_patience"].(string); sp != "5m0s" {
		t.Errorf("slot_patience = %q, want \"5m0s\" (sibling intact)", sp)
	}
}
