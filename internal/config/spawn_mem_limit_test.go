package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
)

// ADV-R11 — per-spawn memory rlimit config surface (GAP-048 cure).
//
// The cap must ride the same TOML < env chain as every other scheduler
// knob: [scheduler] spawn_mem_limit_mb parses into the root config, the
// SCHEDULER_SPAWN_MEM_LIMIT_MB env var overrides it, and Validate()
// rejects negatives (a negative memory cap is a typo, not a setting).

// TestSpawnMemLimitMB_TOMLParses pins the TOML layer: the key lands in
// Scheduler.SpawnMemLimitMB and validates clean.
func TestSpawnMemLimitMB_TOMLParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte(`
[scheduler]
spawn_mem_limit_mb = 512
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.SpawnMemLimitMB != 512 {
		t.Errorf("SpawnMemLimitMB = %d, want 512", cfg.Scheduler.SpawnMemLimitMB)
	}
}

// TestSpawnMemLimitMB_DefaultOff pins the default: unset means 0 (off) —
// the fleet runs byte-identical until an operator arms the cap.
func TestSpawnMemLimitMB_DefaultOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte("[scheduler]\nmin_interval = \"30s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.SpawnMemLimitMB != 0 {
		t.Errorf("SpawnMemLimitMB = %d when unset, want 0 (off by default)", cfg.Scheduler.SpawnMemLimitMB)
	}
}

// TestSpawnMemLimitMB_NegativeRejected pins validation: a negative cap is
// a config error, never a silently normalized value.
func TestSpawnMemLimitMB_NegativeRejected(t *testing.T) {
	cfg := &config.RootConfig{}
	cfg.Daemon.Listen = "127.0.0.1:9090"
	cfg.Daemon.DBPath = "x.db"
	cfg.Scheduler.SpawnMemLimitMB = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted spawn_mem_limit_mb = -1, want rejection")
	}
	if !strings.Contains(err.Error(), "spawn_mem_limit_mb") {
		t.Errorf("error = %v, want it to name spawn_mem_limit_mb", err)
	}
}

// TestSpawnMemLimitMB_EnvOverridesTOML pins the env layer: with TOML 333
// and env 768, the env value wins.
func TestSpawnMemLimitMB_EnvOverridesTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte("[scheduler]\nspawn_mem_limit_mb = 333\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHEDULER_SPAWN_MEM_LIMIT_MB", "768")
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.SpawnMemLimitMB != 768 {
		t.Errorf("SpawnMemLimitMB = %d with TOML 333 + env 768, want 768 (env wins)", cfg.Scheduler.SpawnMemLimitMB)
	}
}
