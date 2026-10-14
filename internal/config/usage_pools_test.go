package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRootConfigUsagePools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedulerd.toml")
	configText := `[usage_pools]
enabled = true
observe_only = true
local_pool_id = "local:control"

[[usage_pools.pools]]
id = "local:control"
kind = "local"
active_limit = 12

[[usage_pools.pools]]
id = "host:build-01"
kind = "host"
active_limit = 8

[[usage_pools.memberships]]
lane = "foreman-a"
pool_ids = ["project:alpha", "shared:build"]
`
	if err := os.WriteFile(path, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRootConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UsagePools.Enabled || !cfg.UsagePools.ObserveOnly || cfg.UsagePools.LocalPoolID != "local:control" {
		t.Fatalf("usage-pool settings = %+v", cfg.UsagePools)
	}
	if len(cfg.UsagePools.Pools) != 2 || cfg.UsagePools.Pools[1].ID != "host:build-01" || cfg.UsagePools.Pools[1].ActiveLimit != 8 {
		t.Fatalf("pool definitions = %+v", cfg.UsagePools.Pools)
	}
	if len(cfg.UsagePools.Memberships) != 1 || cfg.UsagePools.Memberships[0].Lane != "foreman-a" || len(cfg.UsagePools.Memberships[0].PoolIDs) != 2 {
		t.Fatalf("memberships = %+v", cfg.UsagePools.Memberships)
	}
}

func TestUsagePoolsNoLiveConfigMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedulerd.toml")
	original := []byte("[usage_pools]\nenabled = false\nobserve_only = true\nlocal_pool_id = \"local:control\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRootConfig(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("config loader mutated its input: got %q, want byte-identical %q", got, original)
	}
}
