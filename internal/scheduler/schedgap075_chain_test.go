package scheduler

import (
	"testing"
)

// ── SCHED-GAP-075: ordered model chain tests ────────────────────────────

// TestParseModelChain_Basic verifies the JSON → []chainEntry parser.
func TestParseModelChain_Basic(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		want0   chainEntry
	}{
		{
			name:    "empty string",
			raw:     "",
			wantLen: 0,
		},
		{
			name:    "invalid JSON",
			raw:     "not-json",
			wantLen: 0,
		},
		{
			name:    "empty array",
			raw:     "[]",
			wantLen: 0,
		},
		{
			name:    "single entry",
			raw:     `["deepseek-v4-flash@deepseek-foreman"]`,
			wantLen: 1,
			want0:   chainEntry{model: "deepseek-v4-flash", provider: "deepseek-foreman"},
		},
		{
			name:    "two entries",
			raw:     `["deepseek-v4-flash@deepseek-foreman","glm-5.2@zai-glm"]`,
			wantLen: 2,
			want0:   chainEntry{model: "deepseek-v4-flash", provider: "deepseek-foreman"},
		},
		{
			name:    "model only (no @)",
			raw:     `["deepseek-v4-flash"]`,
			wantLen: 1,
			want0:   chainEntry{model: "deepseek-v4-flash", provider: ""},
		},
		{
			name:    "three entries",
			raw:     `["a@x","b@y","c@z"]`,
			wantLen: 3,
			want0:   chainEntry{model: "a", provider: "x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := parseModelChain(tt.raw)
			if len(entries) != tt.wantLen {
				t.Fatalf("parseModelChain(%q) returned %d entries, want %d", tt.raw, len(entries), tt.wantLen)
			}
			if tt.wantLen > 0 && entries[0] != tt.want0 {
				t.Errorf("entries[0] = %v, want %v", entries[0], tt.want0)
			}
		})
	}
}

// TestSpawnChain_ModelChain_NLength verifies that a project with model_chain
// produces a chain with the correct number of project-tier entries.
func TestSpawnChain_ModelChain_NLength(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain: `["m1@p1","m2@p2","m3@p3"]`,
	}
	chain := s.spawnChain(project)
	// 3 project entries + 2 global = 5
	if len(chain) != 5 {
		t.Fatalf("len(chain) = %d, want 5 (3 project + 2 global)", len(chain))
	}
	// Verify project entries
	want := []chainEntry{
		{model: "m1", provider: "p1"},
		{model: "m2", provider: "p2"},
		{model: "m3", provider: "p3"},
		{model: "global-model", provider: "global-provider"},
		{model: "global-fallback-model", provider: "global-fallback-provider"},
	}
	for i, e := range chain {
		if e != want[i] {
			t.Errorf("chain[%d] = %v, want %v", i, e, want[i])
		}
	}
}

// TestSpawnChain_ModelChain_Empty falls back to model/provider + fallback fields.
func TestSpawnChain_ModelChain_Empty(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		Model:            "my-model",
		Provider:         "my-provider",
		FallbackModel:    "fb-model",
		FallbackProvider: "fb-provider",
	}
	chain := s.spawnChain(project)
	if len(chain) != 4 {
		t.Fatalf("len(chain) = %d, want 4", len(chain))
	}
	if chain[0] != (chainEntry{model: "my-model", provider: "my-provider"}) {
		t.Errorf("chain[0] = %v", chain[0])
	}
	if chain[1] != (chainEntry{model: "fb-model", provider: "fb-provider"}) {
		t.Errorf("chain[1] = %v", chain[1])
	}
}

// TestSpawnChain_ModelChain_NoGlobal verifies NoGlobalFallback skips global tiers.
func TestSpawnChain_ModelChain_NoGlobal(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain:       `["m1@p1","m2@p2"]`,
		NoGlobalFallback: true,
	}
	chain := s.spawnChain(project)
	if len(chain) != 2 {
		t.Fatalf("len(chain) = %d, want 2 (no global tiers)", len(chain))
	}
	if chain[0].model != "m1" || chain[1].model != "m2" {
		t.Errorf("chain = %v", chain)
	}
}

// TestSpawnChain_ModelChain_FirstSuccessStops verifies that the first
// present entry's resolution is used and the rest are for retry only.
func TestSpawnChain_ModelChain_FirstSuccessStops(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{
		ModelChain:       `["m1@p1","m2@p2","m3@p3"]`,
		NoGlobalFallback: true,
	}
	chain := s.spawnChain(project)
	m, p := resolveChain(chain)
	if m != "m1" || p != "p1" {
		t.Errorf("resolveChain = (%q, %q), want (m1, p1)", m, p)
	}
	// nextChainResolution should return m2/p2
	rm, rp, ok := nextChainResolution(chain)
	if !ok || rm != "m2" || rp != "p2" {
		t.Errorf("nextChainResolution = (%q, %q, %v), want (m2, p2, true)", rm, rp, ok)
	}
}

// TestSpawnChain_ModelChain_Exhaustion verifies that when all entries are
// empty, nextChainResolution returns ok=false.
func TestSpawnChain_ModelChain_Exhaustion(t *testing.T) {
	chain := []chainEntry{
		{model: "", provider: ""},
		{model: "", provider: ""},
	}
	_, _, ok := nextChainResolution(chain)
	if ok {
		t.Error("nextChainResolution should return ok=false for exhausted chain")
	}
}

// TestNextChainResolution_NLength verifies nextChainResolution on chains
// of arbitrary length (not just the legacy 4).
func TestNextChainResolution_NLength(t *testing.T) {
	chain := []chainEntry{
		{model: "a", provider: "x"},
		{model: "b", provider: "y"},
		{model: "c", provider: "z"},
		{model: "", provider: ""},
	}
	// First hop: a/x → next should be b/y
	rm, rp, ok := nextChainResolution(chain)
	if !ok || rm != "b" || rp != "y" {
		t.Errorf("hop 1: (%q, %q, %v), want (b, y, true)", rm, rp, ok)
	}
	// Second hop: b/y → next should be c/z
	rm, rp, ok = nextChainResolution(chain[1:])
	if !ok || rm != "c" || rp != "z" {
		t.Errorf("hop 2: (%q, %q, %v), want (c, z, true)", rm, rp, ok)
	}
	// Third hop: c/z → next is empty
	rm, rp, ok = nextChainResolution(chain[2:])
	if ok {
		t.Errorf("hop 3: (%q, %q, %v), want empty", rm, rp, ok)
	}
}

// TestMergedChain_SplicesRouter verifies mergedChain inserts router entries
// between project and global tiers.
func TestMergedChain_SplicesRouter(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{}
	base := []chainEntry{
		{model: "proj-m", provider: "proj-p"},
		{model: "", provider: ""}, // fallback
		{model: "global-model", provider: "global-provider"},
		{model: "global-fallback-model", provider: "global-fallback-provider"},
	}
	routerChain := []chainEntry{
		{model: "router-m1", provider: "router-p1"},
		{model: "router-m2", provider: "router-p2"},
	}
	merged := s.mergedChain(project, base, routerChain)
	// proj(2) + router(2) + global(2) = 6
	if len(merged) != 6 {
		t.Fatalf("len(merged) = %d, want 6", len(merged))
	}
	want := []chainEntry{
		{model: "proj-m", provider: "proj-p"},
		{model: "", provider: ""},
		{model: "router-m1", provider: "router-p1"},
		{model: "router-m2", provider: "router-p2"},
		{model: "global-model", provider: "global-provider"},
		{model: "global-fallback-model", provider: "global-fallback-provider"},
	}
	for i, e := range merged {
		if e != want[i] {
			t.Errorf("merged[%d] = %v, want %v", i, e, want[i])
		}
	}
}

// TestMergedChain_NoGlobal verifies mergedChain works when no global tiers.
func TestMergedChain_NoGlobal(t *testing.T) {
	s := newChainSpawner(nil, 10)
	project := PackedProject{NoGlobalFallback: true}
	base := []chainEntry{
		{model: "proj-m", provider: "proj-p"},
	}
	routerChain := []chainEntry{
		{model: "router-m", provider: "router-p"},
	}
	merged := s.mergedChain(project, base, routerChain)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	if merged[0].model != "proj-m" || merged[1].model != "router-m" {
		t.Errorf("merged = %v", merged)
	}
}

// TestOpenChain_DecodesRouterChain verifies OpenChain returns chainEntry
// from RouterResult.Chain.
func TestOpenChain_DecodesRouterChain(t *testing.T) {
	r := &RouterResult{
		Gate: "OPEN",
		Chain: []RouterHop{
			{Model: "m1", Provider: "p1"},
			{Model: "m2", Provider: "p2"},
		},
	}
	entries := r.OpenChain()
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].model != "m1" || entries[1].provider != "p2" {
		t.Errorf("entries = %v", entries)
	}
}

// TestOpenChain_NilOrClosed verifies OpenChain returns nil for nil/closed results.
func TestOpenChain_NilOrClosed(t *testing.T) {
	if (*RouterResult)(nil).OpenChain() != nil {
		t.Error("nil result should return nil")
	}
	r := &RouterResult{Gate: "CLOSED", Chain: []RouterHop{{Model: "m1", Provider: "p1"}}}
	if r.OpenChain() != nil {
		t.Error("closed gate should return nil")
	}
	r2 := &RouterResult{Gate: "OPEN", Chain: nil}
	if r2.OpenChain() != nil {
		t.Error("empty chain should return nil")
	}
}
