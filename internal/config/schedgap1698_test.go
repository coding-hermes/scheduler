package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1698 acceptance, config-layer surface: the TOML key
// scheduler.session_poll_loop_min_ticks must load, survive the env layer
// (SCHEDULER_SESSION_POLL_LOOP_MIN_TICKS wins), and validate (negative
// rejected, 1-5 rejected with a pointer at the built-in floor, 0 = the
// explicit disable).
func TestSCHEDGAP1698_ConfigLayerValidation(t *testing.T) {
	t.Run("toml loads", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[daemon]
db_path = "/tmp/sg1698.db"
listen = "127.0.0.1:9099"

[scheduler]
min_interval = "30s"
max_interval = "24h"
num_levels = 10
weight_budget = 100
max_concurrent = 10
session_poll_loop_min_ticks = 8
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		cfg, err := LoadRootConfig(path)
		if err != nil {
			t.Fatalf("LoadRootConfig: %v", err)
		}
		if cfg.Scheduler.SessionPollLoopMinTicks != 8 {
			t.Errorf("SessionPollLoopMinTicks = %d, want 8", cfg.Scheduler.SessionPollLoopMinTicks)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})

	t.Run("env wins", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "schedulerd.toml")
		body := `
[daemon]
db_path = "/tmp/sg1698b.db"
listen = "127.0.0.1:9099"

[scheduler]
session_poll_loop_min_ticks = 8
`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		t.Setenv("SCHEDULER_SESSION_POLL_LOOP_MIN_TICKS", "10")
		// LoadConfig is the LAYERED loader (defaults < TOML < env);
		// LoadRootConfig is the raw decoder and deliberately skips env.
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Scheduler.SessionPollLoopMinTicks != 10 {
			t.Errorf("env layer lost: SessionPollLoopMinTicks = %d, want 10", cfg.Scheduler.SessionPollLoopMinTicks)
		}
	})

	t.Run("negative rejected", func(t *testing.T) {
		cfg := &RootConfig{
			Daemon: DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
			Scheduler: SchedulerConfig{
				MinInterval: "30s", MaxInterval: "24h", NumLevels: 10,
				WeightBudget: 100, MaxConcurrent: 10,
				SessionPollLoopMinTicks: -1,
			},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate accepted a negative session_poll_loop_min_ticks")
		}
		if !strings.Contains(err.Error(), "session_poll_loop_min_ticks") {
			t.Errorf("error %q must name the key", err)
		}
	})

	t.Run("one-to-five rejected (below the built-in floor)", func(t *testing.T) {
		for _, n := range []int{1, 2, 3, 4, 5} {
			cfg := &RootConfig{
				Daemon: DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
				Scheduler: SchedulerConfig{
					MinInterval: "30s", MaxInterval: "24h", NumLevels: 10,
					WeightBudget: 100, MaxConcurrent: 10,
					SessionPollLoopMinTicks: n,
				},
			}
			err := cfg.Validate()
			if err == nil {
				t.Errorf("Validate accepted session_poll_loop_min_ticks=%d (below the floor of 6)", n)
				continue
			}
			if !strings.Contains(err.Error(), ">= 6") {
				t.Errorf("error for %d = %q, want it to name the floor of 6", n, err)
			}
		}
	})

	t.Run("six-and-up valid", func(t *testing.T) {
		for _, n := range []int{6, 12, 100} {
			cfg := &RootConfig{
				Daemon: DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
				Scheduler: SchedulerConfig{
					MinInterval: "30s", MaxInterval: "24h", NumLevels: 10,
					WeightBudget: 100, MaxConcurrent: 10,
					SessionPollLoopMinTicks: n,
				},
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate rejected session_poll_loop_min_ticks=%d: %v", n, err)
			}
		}
	})

	t.Run("unset valid (disabled)", func(t *testing.T) {
		cfg := &RootConfig{
			Daemon: DaemonConfig{DBPath: "/tmp/x.db", Listen: "127.0.0.1:9099"},
			Scheduler: SchedulerConfig{
				MinInterval: "30s", MaxInterval: "24h", NumLevels: 10,
				WeightBudget: 100, MaxConcurrent: 10,
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected an unset poll-loop minimum: %v", err)
		}
	})
}
