package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SCHED-GAP-1707 acceptance, config-layer surface: the TOML key
// scheduler.session_silence_grace must load, survive the env layer
// (SCHEDULER_SESSION_SILENCE_GRACE wins), and validate (unparseable
// rejected, negative rejected, "0s" = explicit disable tolerated).
func TestSCHEDGAP1707_ConfigLayerValidation(t *testing.T) {
	t.Run("toml loads", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[daemon]
db_path = "/tmp/sg1707.db"
listen = "127.0.0.1:9099"

[scheduler]
min_interval = "30s"
max_interval = "24h"
num_levels = 10
weight_budget = 100
max_concurrent = 10
session_silence_grace = "45m"
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		cfg, err := LoadRootConfig(path)
		if err != nil {
			t.Fatalf("LoadRootConfig: %v", err)
		}
		if cfg.Scheduler.SessionSilenceGrace != "45m" {
			t.Errorf("SessionSilenceGrace = %q, want \"45m\"", cfg.Scheduler.SessionSilenceGrace)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
		d, perr := time.ParseDuration(cfg.Scheduler.SessionSilenceGrace)
		if perr != nil || d != 45*time.Minute {
			t.Errorf("loaded grace parses to %v/%v, want 45m", d, perr)
		}
	})

	t.Run("env wins", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[daemon]
db_path = "/tmp/sg1707b.db"
listen = "127.0.0.1:9099"

[scheduler]
session_silence_grace = "45m"
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		t.Setenv("SCHEDULER_SESSION_SILENCE_GRACE", "1h")
		// LoadConfig is the LAYERED loader (defaults < TOML < env);
		// LoadRootConfig is the raw decoder and deliberately skips env.
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Scheduler.SessionSilenceGrace != "1h" {
			t.Errorf("env layer lost: SessionSilenceGrace = %q, want \"1h\"", cfg.Scheduler.SessionSilenceGrace)
		}
	})

	t.Run("unparseable rejected", func(t *testing.T) {
		cfg := &RootConfig{
			Daemon:    DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
			Scheduler: SchedulerConfig{SessionSilenceGrace: "not-a-duration"},
		}
		if err := cfg.Validate(); err == nil {
			t.Error("Validate accepted an unparseable session_silence_grace")
		}
	})

	t.Run("negative rejected", func(t *testing.T) {
		cfg := &RootConfig{
			Daemon:    DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
			Scheduler: SchedulerConfig{SessionSilenceGrace: "-5m"},
		}
		if err := cfg.Validate(); err == nil {
			t.Error("Validate accepted a negative session_silence_grace")
		}
	})

	t.Run("unset and 0s valid (disabled)", func(t *testing.T) {
		cfg := &RootConfig{
			Daemon: DaemonConfig{
				DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099",
			},
			Scheduler: SchedulerConfig{
				MinInterval: "30s", MaxInterval: "24h", NumLevels: 10,
				WeightBudget: 100, MaxConcurrent: 10,
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected an unset grace: %v", err)
		}
		cfg.Scheduler.SessionSilenceGrace = "0s"
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected the explicit \"0s\" disable: %v", err)
		}
	})
}
