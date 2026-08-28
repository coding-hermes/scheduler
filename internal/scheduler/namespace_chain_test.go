package scheduler

import (
	"testing"
)

// TestSpawnChain_NamespaceTierBetweenProjectAndGlobal verifies the
// namespace-level model chain (Bane 2026-08-27, 3-tier routing) sits between
// the project entries and the global tiers in the spawn chain.
func TestSpawnChain_NamespaceTierBetweenProjectAndGlobal(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain:     `["proj-m@p1"]`,
		NamespaceChain: `["ns-m@ns-p"]`,
	}
	chain := s.spawnChain(project)
	// 1 project + 1 namespace + 2 global = 4
	if len(chain) != 4 {
		t.Fatalf("len(chain) = %d, want 4 (project + namespace + 2 global)", len(chain))
	}
	want := []chainEntry{
		{model: "proj-m", provider: "p1"},
		{model: "ns-m", provider: "ns-p"},
		{model: "global-model", provider: "global-provider"},
		{model: "global-fallback-model", provider: "global-fallback-provider"},
	}
	for i, e := range chain {
		if e != want[i] {
			t.Errorf("chain[%d] = %v, want %v", i, e, want[i])
		}
	}
}

// TestSpawnChain_NamespaceTierOnly_NoProjectChain verifies the namespace
// chain still lands before global tiers when the project has no explicit
// model_chain (legacy model/provider fields only).
func TestSpawnChain_NamespaceTierOnly_NoProjectChain(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		Model:          "my-model",
		Provider:       "my-provider",
		NamespaceChain: `["ns-m@ns-p"]`,
	}
	chain := s.spawnChain(project)
	// 2 legacy entries (primary + empty fallback) + 1 namespace + 2 global = 5
	if len(chain) != 5 {
		t.Fatalf("len(chain) = %d, want 5", len(chain))
	}
	if chain[0] != (chainEntry{model: "my-model", provider: "my-provider"}) {
		t.Errorf("chain[0] = %v, want legacy project entry", chain[0])
	}
	if chain[1].present() {
		t.Errorf("chain[1] = %v, want empty legacy fallback entry", chain[1])
	}
	if chain[2] != (chainEntry{model: "ns-m", provider: "ns-p"}) {
		t.Errorf("chain[2] = %v, want namespace entry", chain[2])
	}
	if chain[3] != (chainEntry{model: "global-model", provider: "global-provider"}) {
		t.Errorf("chain[3] = %v, want global primary", chain[3])
	}
}

// TestSpawnChain_NamespaceTier_EmptyContributesNothing verifies an empty
// namespace chain does not add entries (fail-open, backward compatible).
func TestSpawnChain_NamespaceTier_EmptyContributesNothing(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain: `["m1@p1"]`,
	}
	chain := s.spawnChain(project)
	if len(chain) != 3 {
		t.Fatalf("len(chain) = %d, want 3 (1 project + 2 global, no namespace)", len(chain))
	}
	if chain[1] != (chainEntry{model: "global-model", provider: "global-provider"}) {
		t.Errorf("chain[1] = %v, want global primary directly after project", chain[1])
	}
}

// TestMergedChain_NamespaceTier_AheadOfRouter verifies the final merged
// precedence: project > namespace > router > global (Bane 2026-08-27).
func TestMergedChain_NamespaceTier_AheadOfRouter(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain:     `["proj-m@p1"]`,
		NamespaceChain: `["ns-m@ns-p"]`,
	}
	base := s.spawnChain(project)
	routerChain := []chainEntry{
		{model: "router-m", provider: "router-p"},
		{model: "router-fallback-m", provider: "router-fallback-p"},
	}
	merged := s.mergedChain(project, base, routerChain)
	want := []chainEntry{
		{model: "proj-m", provider: "p1"},
		{model: "ns-m", provider: "ns-p"},
		{model: "router-m", provider: "router-p"},
		{model: "router-fallback-m", provider: "router-fallback-p"},
		{model: "global-model", provider: "global-provider"},
		{model: "global-fallback-model", provider: "global-fallback-provider"},
	}
	if len(merged) != len(want) {
		t.Fatalf("len(merged) = %d, want %d", len(merged), len(want))
	}
	for i, e := range merged {
		if e != want[i] {
			t.Errorf("merged[%d] = %v, want %v", i, e, want[i])
		}
	}
}
