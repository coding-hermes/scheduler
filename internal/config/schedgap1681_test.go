package config

// SCHED-GAP-1681 config-layer surface: the TOML key
// scheduler.gateway_transient_retries must load with 0-vs-unset kept
// DISTINCT (0 = explicitly single attempt; unset/nil = the daemon flag
// default). That distinction is why the field is a *int — a plain int would
// make a TOML "0" indistinguishable from an absent key and silently disable
// the retry loop for every operator who merely re-stated the default.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSCHEDGAP1681_ConfigLayer(t *testing.T) {
	t.Run("toml loads N", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[daemon]
db_path = "/tmp/sg1681.db"
listen = "127.0.0.1:9099"

[scheduler]
gateway_transient_retries = 5
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		cfg, err := LoadRootConfig(path)
		if err != nil {
			t.Fatalf("LoadRootConfig: %v", err)
		}
		if cfg.Scheduler.GatewayTransientRetries == nil {
			t.Fatal("GatewayTransientRetries = nil, want 5 (the TOML value must load)")
		}
		if *cfg.Scheduler.GatewayTransientRetries != 5 {
			t.Errorf("GatewayTransientRetries = %d, want 5", *cfg.Scheduler.GatewayTransientRetries)
		}
	})

	t.Run("toml 0 stays distinct from unset", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[scheduler]
gateway_transient_retries = 0
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		cfg, err := LoadRootConfig(path)
		if err != nil {
			t.Fatalf("LoadRootConfig: %v", err)
		}
		if cfg.Scheduler.GatewayTransientRetries == nil {
			t.Fatal("GatewayTransientRetries = nil — a TOML 0 was collapsed into unset; the pointer contract is broken (0 must mean explicit single-attempt)")
		}
		if *cfg.Scheduler.GatewayTransientRetries != 0 {
			t.Errorf("GatewayTransientRetries = %d, want 0", *cfg.Scheduler.GatewayTransientRetries)
		}
	})

	t.Run("absent key reads nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[scheduler]
gateway_response_timeout = "30m"
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		cfg, err := LoadRootConfig(path)
		if err != nil {
			t.Fatalf("LoadRootConfig: %v", err)
		}
		if cfg.Scheduler.GatewayTransientRetries != nil {
			t.Errorf("GatewayTransientRetries = %v, want nil (absent key = the flag default stands)", *cfg.Scheduler.GatewayTransientRetries)
		}
	})
}
