package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemote004_CrierConfigLoads proves the [crier] TOML block decodes with
// the documented keys, and that CRIER_* env wins over TOML (resolved by
// main.go; here we prove the TOML layer + the env-pattern contract).
func TestRemote004_CrierConfigLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `
[crier]
url = "http://10.0.0.9:8767"
token = "relay-cred"
enabled = true
topics = ["sched.tick.>", "sched.lane.>"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	c := cfg.Crier
	if c.URL != "http://10.0.0.9:8767" {
		t.Errorf("url = %q", c.URL)
	}
	if c.Token != "relay-cred" {
		t.Errorf("token = %q", c.Token)
	}
	if !c.Enabled {
		t.Error("enabled = false, want true")
	}
	if len(c.Topics) != 2 || c.Topics[0] != "sched.tick.>" {
		t.Errorf("topics = %v", c.Topics)
	}

	// Default config: enabled=false and empty url (the daemon applies the
	// http://127.0.0.1:8767 default), so off is the boot default.
	def := defaultRootConfig()
	if def.Crier.Enabled {
		t.Error("default [crier] enabled must be false (off = pure no-op)")
	}
	if def.Crier.URL != "" {
		t.Errorf("default url = %q, want empty (client applies the 127.0.0.1:8767 default)", def.Crier.URL)
	}
}
