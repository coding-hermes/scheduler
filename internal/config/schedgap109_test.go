package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── SCHED-GAP-109: wave namespace config plumbing ────────────────────────

// TestNamespaceFromDef_WaveDefaults pins the default-off contract: a
// namespace def without wave keys materializes waves disabled, timeout
// inherited (""), cap unlimited (0) — byte-identical to pre-v27 behavior.
func TestNamespaceFromDef_WaveDefaults(t *testing.T) {
	ns := namespaceFromDef(NamespaceDef{ID: "ns-defaults"})
	if ns.WaveEnabled {
		t.Errorf("wave_enabled default = true, want false")
	}
	if ns.WaveTickTimeout != "" {
		t.Errorf("wave_tick_timeout default = %q, want empty (inherit)", ns.WaveTickTimeout)
	}
	if ns.WaveWorkersCap != 0 {
		t.Errorf("wave_workers_cap default = %d, want 0 (unlimited)", ns.WaveWorkersCap)
	}
}

// TestNamespaceFromDef_WaveValuesPassThrough proves explicitly-set wave keys
// survive the def → Namespace mapping (including an explicit false and a
// negative cap normalizing to 0).
func TestNamespaceFromDef_WaveValuesPassThrough(t *testing.T) {
	on := true
	off := false
	ns := namespaceFromDef(NamespaceDef{
		ID: "ns-wave", WaveEnabled: &on, WaveTickTimeout: "3h", WaveWorkersCap: 3,
	})
	if !ns.WaveEnabled || ns.WaveTickTimeout != "3h" || ns.WaveWorkersCap != 3 {
		t.Errorf("wave pass-through: enabled=%v timeout=%q cap=%d", ns.WaveEnabled, ns.WaveTickTimeout, ns.WaveWorkersCap)
	}

	ns = namespaceFromDef(NamespaceDef{ID: "ns-off", WaveEnabled: &off})
	if ns.WaveEnabled {
		t.Errorf("explicit wave_enabled = false came through as true")
	}

	ns = namespaceFromDef(NamespaceDef{ID: "ns-negcap", WaveWorkersCap: -5})
	if ns.WaveWorkersCap != 0 {
		t.Errorf("negative cap = %d, want 0 (normalized to unlimited)", ns.WaveWorkersCap)
	}
}

// TestLoadFleetConfig_WaveNamespaceKeys proves the TOML keys parse into the
// def struct and that ApplyFleetConfig creates the namespace carrying them.
func TestLoadFleetConfig_WaveNamespaceKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	tomlContent := `[[namespaces]]
id = "coding-hermes"
weight = 50
wave_enabled = true
wave_tick_timeout = "3h"
wave_workers_cap = 3

[[namespaces]]
id = "plain-ns"

[[projects]]
name = "wave-proj"
repo_url = "https://github.com/example/wave-proj"
workdir = "/tmp/wave-proj"
namespace_id = "coding-hermes"
`
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if len(cfg.Namespaces) != 2 {
		t.Fatalf("namespaces = %d, want 2", len(cfg.Namespaces))
	}
	wave := cfg.Namespaces[0]
	if wave.WaveEnabled == nil || !*wave.WaveEnabled ||
		wave.WaveTickTimeout != "3h" || wave.WaveWorkersCap != 3 {
		t.Errorf("wave keys not parsed: %+v", wave)
	}
	plain := cfg.Namespaces[1]
	if plain.WaveEnabled != nil || plain.WaveTickTimeout != "" || plain.WaveWorkersCap != 0 {
		t.Errorf("plain namespace not default-off: %+v", plain)
	}

	// ApplyFleetConfig must create the namespace with the wave values.
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	if err := ApplyFleetConfig(t.Context(), db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	ns, err := database.GetNamespace(t.Context(), db, "coding-hermes")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if !ns.WaveEnabled || ns.WaveTickTimeout != "3h" || ns.WaveWorkersCap != 3 {
		t.Errorf("namespace row wave fields: enabled=%v timeout=%q cap=%d",
			ns.WaveEnabled, ns.WaveTickTimeout, ns.WaveWorkersCap)
	}
	plainNS, err := database.GetNamespace(t.Context(), db, "plain-ns")
	if err != nil {
		t.Fatalf("GetNamespace (plain): %v", err)
	}
	if plainNS.WaveEnabled || plainNS.WaveTickTimeout != "" || plainNS.WaveWorkersCap != 0 {
		t.Errorf("plain namespace row not default-off: %+v", plainNS)
	}
}
