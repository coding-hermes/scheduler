package scheduler

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// ── TASK-ROUTER-001: spawn-level router integration tests ─────────────────
//
// These pin the integration point in Spawn(): the router resolves BEFORE
// the SPAWN log line and its open head replaces the chain-resolved
// model/provider for BOTH the log line and the gateway dispatch. Every
// failure mode falls back to the chain values with a warning — and when no
// router is configured, the spawn is byte-identical to pre-router
// behavior (proven by the untouched SCHED-GAP-064/065 suites).

// routerRouterJSON is the canned OPEN payload the fake router prints for
// the spawn-level tests. The model/provider deliberately differ from the
// chain tiers so a test can tell which source won.
const routerRouterJSON = `{
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

// TestSpawn_RouterHeadOverridesChain: with a fake router configured, the
// SPAWN log line AND the gateway dispatch both show the router's head —
// the router wins over the project chain.
func TestSpawn_RouterHeadOverridesChain(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetRouterClient(NewRouterClient([]string{writeFakeRouter(t, routerRouterJSON)}, 0))

	project := PackedProject{
		Name:     "router-ok",
		Workdir:  t.TempDir(),
		Model:    "work-model",
		Provider: "work-provider",
	}
	tick, err := s.Spawn(project, "router-ok-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	out := buf.String()
	spawnLine := `SPAWN: router-ok tick=router-ok-2026-08-27-00-00-00 chain=work model="router-model" provider="router-provider"`
	if !strings.Contains(out, spawnLine) {
		t.Errorf("SPAWN log line missing router head %q; got:\n%s", spawnLine, out)
	}
	if !strings.Contains(out, "ROUTER:") {
		t.Errorf("missing ROUTER provenance line; got:\n%s", out)
	}

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	if dispatches[0].model != "router-model" || dispatches[0].provider != "router-provider" {
		t.Errorf("gateway dispatch = (%q, %q), want router head (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "router-model", "router-provider")
	}
}

// TestSpawn_RouterDownFallsBackToChain: every router failure mode keeps the
// chain-resolved values on the SPAWN line and the gateway dispatch, with a
// ROUTER warning line explaining the fallback. A nil client (the default —
// no env, no SetRouterClient) must produce output byte-identical to
// pre-router behavior: the existing SCHED-GAP-064/065 suites pin that for
// the chain cases, so this test pins the WARNING for the failure modes and
// asserts the chain values survive.
func TestSpawn_RouterDownFallsBackToChain(t *testing.T) {
	tests := []struct {
		name       string
		router     *RouterClient
		wantReason string // substring of the ROUTER line
	}{
		{
			name:   "nil client (router not configured)",
			router: nil,
		},
		{
			name:       "missing script",
			router:     NewRouterClient([]string{"/no/such/router.py"}, 0),
			wantReason: "unavailable",
		},
		{
			name:       "malformed JSON",
			router:     NewRouterClient([]string{writeFakeRouter(t, "not json at all")}, 0),
			wantReason: "unavailable",
		},
		{
			name:       "null head — NO-OPEN-HOP",
			router:     NewRouterClient([]string{writeFakeRouter(t, `{"project":"p","profile":"P0_FORE","gate":"NO-OPEN-HOP","head":null}`)}, 0),
			wantReason: "no open head",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(prev) })

			gw := &chainGatewayServer{}
			s := newChainSpawnerForGateway(t, gw)
			if tt.router != nil {
				s.SetRouterClient(tt.router)
			}

			project := PackedProject{
				Name:     "router-down",
				Workdir:  t.TempDir(),
				Model:    "work-model",
				Provider: "work-provider",
			}
			tick, err := s.Spawn(project, "router-down-2026-08-27-00-00-00")
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if tick == nil {
				t.Fatal("Spawn returned nil tick on gateway success")
			}
			tick.Wait()

			out := buf.String()
			spawnLine := `SPAWN: router-down tick=router-down-2026-08-27-00-00-00 chain=work model="work-model" provider="work-provider"`
			if !strings.Contains(out, spawnLine) {
				t.Errorf("SPAWN line must keep chain values %q; got:\n%s", spawnLine, out)
			}
			routerLine := "ROUTER:"
			if !strings.Contains(out, routerLine) {
				t.Errorf("missing ROUTER warning line; got:\n%s", out)
			}
			if tt.wantReason != "" {
				if !strings.Contains(out, tt.wantReason) {
					t.Errorf("ROUTER line should mention %q; got:\n%s", tt.wantReason, out)
				}
			}

			dispatches := gw.snapshot()
			if len(dispatches) != 1 {
				t.Fatalf("dispatch count = %d, want 1", len(dispatches))
			}
			if dispatches[0].model != "work-model" || dispatches[0].provider != "work-provider" {
				t.Errorf("gateway dispatch = (%q, %q), want chain fallback (%q, %q)",
					dispatches[0].model, dispatches[0].provider, "work-model", "work-provider")
			}
		})
	}
}

// TestSpawn_RouterAppliesToIdleTicks: the router also overrides the IDLE
// chain — the router's head is the cheapest healthy option regardless of
// tick kind (cost-optimization is the point); the idle chain remains the
// fallback when the router is down (covered above for the work kind, and
// by the untouched idle tests when no router is configured).
func TestSpawn_RouterAppliesToIdleTicks(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetPendingCounter(NewPendingTaskCounter(0))
	s.SetRouterClient(NewRouterClient([]string{writeFakeRouter(t, routerRouterJSON)}, 0))

	workdir := t.TempDir()
	writeGap065Board(t, workdir, `{"id":"DONE-A","status":"complete"}`)
	project := PackedProject{
		Name:         "router-idle",
		Workdir:      workdir,
		Model:        "work-model",
		Provider:     "work-provider",
		IdleModel:    "idle-model",
		IdleProvider: "idle-provider",
	}
	tick, err := s.Spawn(project, "router-idle-2026-08-27-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	out := buf.String()
	spawnLine := `SPAWN: router-idle tick=router-idle-2026-08-27-00-00-00 chain=idle model="router-model" provider="router-provider"`
	if !strings.Contains(out, spawnLine) {
		t.Errorf("SPAWN line missing router head on idle tick %q; got:\n%s", spawnLine, out)
	}

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	if dispatches[0].model != "router-model" || dispatches[0].provider != "router-provider" {
		t.Errorf("idle gateway dispatch = (%q, %q), want router head (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "router-model", "router-provider")
	}
}
