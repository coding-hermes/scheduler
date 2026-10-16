package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarness001Config(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		bad       bool
	}{
		{"", "router", false}, {"router", "router", false}, {"fixed", "fixed", false},
		{"fixed ", "", true}, {"FIXED", "", true}, {"invalid", "", true},
	} {
		t.Run("parse-"+tc.raw, func(t *testing.T) {
			got, err := ParseForemanRoutingMode(tc.raw)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("parse=%q/%v", got, err)
			}
			cfg := defaultRootConfig()
			cfg.Scheduler.ForemanRoutingMode = tc.raw
			err = cfg.Validate()
			if (err != nil) != tc.bad {
				t.Fatalf("Validate=%v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "foreman_routing_mode") {
				t.Fatalf("unnamed error: %v", err)
			}
		})
	}
	t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[scheduler]\nforeman_routing_mode = 'fixed'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"", "router", "fixed"} {
		t.Run("load-"+env, func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", env)
			want := "fixed"
			if env != "" {
				want = env
			}
			for _, load := range []func(string) (*RootConfig, error){LoadConfig, LoadRootConfig} {
				cfg, err := load(path)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Scheduler.ForemanRoutingMode != want {
					t.Fatalf("mode=%q want %q", cfg.Scheduler.ForemanRoutingMode, want)
				}
			}
		})
	}
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.ForemanRoutingMode != "router" {
		t.Fatalf("default=%q", cfg.Scheduler.ForemanRoutingMode)
	}
}
