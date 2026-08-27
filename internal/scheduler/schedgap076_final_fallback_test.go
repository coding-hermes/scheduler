package scheduler

// ── SCHED-GAP-076: final-fallback rule (Bane 2026-08-27) ───────────────────
//
// When the whole chain is exhausted (or empty), the spawn makes ONE final
// attempt with the FOREMAN FALLBACK — the config-set ultimate
// (resolveModelProvider: project primary → project fallback → global env
// tiers). Only when that ALSO rejects does the tick fail with the terminal
// GAP-035 classification. The fallback is skipped when it duplicates a pair
// already attempted (single-tier projects keep the legacy fail-fast).

import (
	"net/http"
	"strings"
	"testing"
)

// TestSpawn_ForemanFallbackAfterChainExhaustion: router head 401s, the
// retry hop 401s, and the config-set foreman fallback (project primary —
// the DeepSeek V4 PAYG ultimate) succeeds as dispatch 3. The tick completes
// on the fallback pair.
func TestSpawn_ForemanFallbackAfterChainExhaustion(t *testing.T) {
	gw := &chainGatewayServer{
		allowOnlyModel:    "ff-model",
		allowOnlyProvider: "ff-provider",
	}
	s := newChainSpawnerForGateway(t, gw)
	s.SetRouterClient(NewRouterClient([]string{writeFakeRouter(t, routerRouterJSON)}, 0))

	project := PackedProject{
		Name:             "gap076-final-fallback",
		Workdir:          t.TempDir(),
		Model:            "ff-model",
		Provider:         "ff-provider",
		FallbackModel:    "ff-fallback-model",
		FallbackProvider: "ff-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap076-final-fallback-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on foreman-fallback success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 3 {
		t.Fatalf("dispatch count = %d, want exactly 3 (router head + retry hop + foreman fallback)", len(dispatches))
	}
	if dispatches[0].model != "router-model" || dispatches[0].provider != "router-provider" || dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("dispatch 1 = (%q, %q, %d), want (router-model, router-provider, 401)",
			dispatches[0].model, dispatches[0].provider, dispatches[0].status)
	}
	if dispatches[1].model != "ff-fallback-model" || dispatches[1].provider != "ff-fallback-provider" || dispatches[1].status != http.StatusUnauthorized {
		t.Errorf("dispatch 2 = (%q, %q, %d), want (ff-fallback-model, ff-fallback-provider, 401)",
			dispatches[1].model, dispatches[1].provider, dispatches[1].status)
	}
	if dispatches[2].model != "ff-model" || dispatches[2].provider != "ff-provider" || dispatches[2].status != http.StatusOK {
		t.Errorf("dispatch 3 = (%q, %q, %d), want the foreman fallback (ff-model, ff-provider, 200)",
			dispatches[2].model, dispatches[2].provider, dispatches[2].status)
	}
}

// TestSpawn_ForemanFallbackExhaustedReturnsError: the foreman fallback ALSO
// rejects — the tick fails with the terminal GAP-035 classification, and the
// fallback attempt is pinned by the 3rd dispatch (the change: it was 2
// before the final-fallback rule).
func TestSpawn_ForemanFallbackExhaustedReturnsError(t *testing.T) {
	gw := &chainGatewayServer{rejectAll: true}
	s := newChainSpawnerForGateway(t, gw)
	s.SetRouterClient(NewRouterClient([]string{writeFakeRouter(t, routerRouterJSON)}, 0))

	project := PackedProject{
		Name:             "gap076-final-fallback-exhausted",
		Workdir:          t.TempDir(),
		Model:            "ff-model",
		Provider:         "ff-provider",
		FallbackModel:    "ff-fallback-model",
		FallbackProvider: "ff-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap076-final-fallback-exhausted-2026-08-27-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error when the foreman fallback also rejects — 401 must surface")
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("error = %q, want gateway auth_error detail surfaced", err.Error())
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on exhausted auth failure")
	}
	dispatches := gw.snapshot()
	if len(dispatches) != 3 {
		t.Errorf("dispatch count = %d, want exactly 3 (head + retry + foreman fallback, then stop)", len(dispatches))
	}
}
