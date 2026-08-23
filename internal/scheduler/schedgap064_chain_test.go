package scheduler

import (
	"database/sql"
	"os/exec"
	"testing"
	"time"
)

// ── SCHED-GAP-064: fleet.toml model/provider fallback chains ──────────────
//
// Resolution order (first non-empty wins, per field):
//   1. project primary   (project.Model / project.Provider)
//   2. project fallback  (project.FallbackModel / project.FallbackProvider)
//   3. global primary    (spawner defaults: s.model / s.provider)
//   4. global fallback   (spawner env fallback tier)
//
// no_global_fallback=true stops the chain after tier 2. Model and provider
// resolve INDEPENDENTLY — an entry contributes its non-empty fields, and a
// still-empty field falls through to the next entry.

// TestResolveChain_FirstNonEmptyPerField pins the chain semantics on the
// pure resolver: order wins, model and provider fill independently.
func TestResolveChain_FirstNonEmptyPerField(t *testing.T) {
	tests := []struct {
		name  string
		chain []chainEntry
		wantM string
		wantP string
	}{
		{name: "empty chain", chain: nil, wantM: "", wantP: ""},
		{name: "single entry both", chain: []chainEntry{{"m1", "p1"}}, wantM: "m1", wantP: "p1"},
		{name: "model from entry2 provider from entry1",
			chain: []chainEntry{{"", "p1"}, {"m2", ""}}, wantM: "m2", wantP: "p1"},
		{name: "skip empty entries",
			chain: []chainEntry{{"", ""}, {"m1", "p1"}, {"m2", "p2"}}, wantM: "m1", wantP: "p1"},
		{name: "model only falls through",
			chain: []chainEntry{{"m1", ""}, {"", "p2"}}, wantM: "m1", wantP: "p2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, p := resolveChain(tt.chain)
			if m != tt.wantM || p != tt.wantP {
				t.Errorf("resolveChain(%v) = (%q, %q), want (%q, %q)", tt.chain, m, p, tt.wantM, tt.wantP)
			}
		})
	}
}

// newChainSpawner builds a Spawner with fixed global primary + fallback tiers
// (as NewSpawner would read from env) so chain tests don't depend on the
// host environment. db may be nil for pure-resolution tests; Spawn-path
// tests use newChainSpawnerForGateway, which passes a real DB.
func newChainSpawner(db *sql.DB, maxConcurrent int) *Spawner {
	return &Spawner{
		db:                db,
		maxConcurrent:     maxConcurrent,
		active:            make(map[string]*exec.Cmd),
		timeout:           30 * time.Minute,
		heartbeatInterval: 5 * time.Minute,
		model:             "global-model",
		provider:          "global-provider",
		fallbackModel:     "global-fallback-model",
		fallbackProvider:  "global-fallback-provider",
	}
}

// TestResolveModelProvider_ChainOrder pins the full 4-tier resolution order.
func TestResolveModelProvider_ChainOrder(t *testing.T) {
	tests := []struct {
		name    string
		project PackedProject
		wantM   string
		wantP   string
	}{
		{
			name:    "empty project resolves to global primary",
			project: PackedProject{},
			wantM:   "global-model",
			wantP:   "global-provider",
		},
		{
			name:    "project primary wins over global",
			project: PackedProject{Model: "proj-model", Provider: "proj-provider"},
			wantM:   "proj-model",
			wantP:   "proj-provider",
		},
		{
			name:    "empty project provider fills from project fallback provider",
			project: PackedProject{Model: "proj-model", Provider: "", FallbackProvider: "good-fallback-provider"},
			wantM:   "proj-model",
			wantP:   "good-fallback-provider",
		},
		{
			name:    "empty project provider falls through to global provider",
			project: PackedProject{Model: "proj-model", Provider: ""},
			wantM:   "proj-model",
			wantP:   "global-provider",
		},
		{
			name:    "project fallback beats global",
			project: PackedProject{FallbackModel: "proj-fallback-model", FallbackProvider: "proj-fallback-provider"},
			wantM:   "proj-fallback-model",
			wantP:   "proj-fallback-provider",
		},
		{
			name:    "global fallback tier reached when global primary provider empty",
			project: PackedProject{Model: "proj-model"},
			wantM:   "proj-model",
			wantP:   "global-provider",
		},
		{
			name:    "no_global_fallback stops the chain after project tiers",
			project: PackedProject{Model: "proj-model", Provider: "proj-provider", NoGlobalFallback: true},
			wantM:   "proj-model",
			wantP:   "proj-provider",
		},
		{
			name:    "no_global_fallback with empty project tiers resolves to empty",
			project: PackedProject{NoGlobalFallback: true},
			wantM:   "",
			wantP:   "",
		},
		{
			name:    "no_global_fallback keeps project fallback tier",
			project: PackedProject{Provider: "", FallbackProvider: "good", NoGlobalFallback: true},
			wantM:   "",
			wantP:   "good",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newChainSpawner(nil, 1)
			m, p := s.resolveModelProvider(tt.project)
			if m != tt.wantM || p != tt.wantP {
				t.Errorf("resolveModelProvider(%+v) = (%q, %q), want (%q, %q)", tt.project, m, p, tt.wantM, tt.wantP)
			}
		})
	}
}

// TestResolveModelProvider_GlobalFallbackEnvTier covers tier 4 (the env
// fallback): when the global PRIMARY tier is itself empty, the env fallback
// tier fills the slot.
func TestResolveModelProvider_GlobalFallbackEnvTier(t *testing.T) {
	s := &Spawner{
		// global primary empty → tier 4 (env fallback) is reachable
		fallbackModel:    "env-fallback-model",
		fallbackProvider: "env-fallback-provider",
	}
	m, p := s.resolveModelProvider(PackedProject{})
	if m != "env-fallback-model" || p != "env-fallback-provider" {
		t.Errorf("resolveModelProvider(empty project, empty global primary) = (%q, %q), want (%q, %q)",
			m, p, "env-fallback-model", "env-fallback-provider")
	}
}

// TestNextChainResolution pins the retry step: after the primary entry fails
// with an auth rejection, the retry uses the FIRST PRESENT entry after it,
// resolving through the remainder of the chain.
func TestNextChainResolution(t *testing.T) {
	tests := []struct {
		name   string
		chain  []chainEntry
		wantM  string
		wantP  string
		wantOK bool
	}{
		{name: "no entries", chain: nil, wantOK: false},
		{name: "single entry — no retry", chain: []chainEntry{{"m1", "p1"}}, wantOK: false},
		{name: "retry to second entry",
			chain: []chainEntry{{"m1", "p1"}, {"m2", "p2"}}, wantM: "m2", wantP: "p2", wantOK: true},
		{name: "retry resolves through remaining chain",
			chain: []chainEntry{{"m1", "p1"}, {"", "p2"}, {"m3", ""}}, wantM: "m3", wantP: "p2", wantOK: true},
		{name: "skip empty leading entries",
			chain: []chainEntry{{"", ""}, {"m1", "p1"}, {"m2", "p2"}}, wantM: "m2", wantP: "p2", wantOK: true},
		{name: "no usable remainder — no retry",
			chain: []chainEntry{{"m1", "p1"}, {"", ""}}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, p, ok := nextChainResolution(tt.chain)
			if ok != tt.wantOK {
				t.Errorf("nextChainResolution(%v) ok = %v, want %v", tt.chain, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if m != tt.wantM || p != tt.wantP {
				t.Errorf("nextChainResolution(%v) = (%q, %q), want (%q, %q)", tt.chain, m, p, tt.wantM, tt.wantP)
			}
		})
	}
}
