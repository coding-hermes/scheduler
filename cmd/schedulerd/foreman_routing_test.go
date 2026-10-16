package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
)

func TestHarness001Surfaces(t *testing.T) {
	var schema struct {
		Properties struct {
			Scheduler struct {
				Properties map[string]struct {
					Default any
					Env     string
					Enum    []string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(captureStdout(printSchema)), &schema); err != nil {
		t.Fatal(err)
	}
	knob, ok := schema.Properties.Scheduler.Properties["foreman_routing_mode"]
	if !ok || knob.Default != "router" || knob.Env != "SCHEDULER_FOREMAN_ROUTING_MODE" || strings.Join(knob.Enum, ",") != "router,fixed" {
		t.Fatalf("schema knob=%+v present=%t", knob, ok)
	}
	for _, mode := range []string{"router", "fixed"} {
		out := captureStdout(func() {
			printConfig("", "scratch.db", "127.0.0.1:0", "", time.Second, time.Hour, 10, 100, 10, false, time.Hour, time.Minute, time.Minute, time.Minute, 3, "http://127.0.0.1:8642", "", "", true, "scheduler", "", 0, 100, 50, 100, 0, 0, "", 0, 0, mode)
		})
		if got := tomlSectionValue(t, out, "scheduler", "foreman_routing_mode"); got != mode {
			t.Errorf("printed=%q want %q", got, mode)
		}
	}
	raw, err := json.Marshal(api.ResolvedConfig{ForemanRoutingMode: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"foreman_routing_mode":"fixed"`) {
		t.Fatalf("snapshot=%s", raw)
	}
}

func TestHarness001ProductionRoutingWiring(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	const construction = "loop := scheduler.NewLoop(db, *minInterval, *maxInterval, *numLevels, *weightBudget, *maxConcurrent, *namespaceMode)"
	if !strings.Contains(src, construction+"\n\tif err := loop.SetForemanRoutingMode(foremanRoutingMode); err != nil {") {
		t.Fatal("main must apply and validate the resolved routing mode immediately after Loop construction")
	}
	if strings.Contains(src, `os.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE"`) {
		t.Fatal("main must not bridge routing configuration through process-wide env")
	}
}

func TestHarness001BootMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.toml")
	if err := os.WriteFile(path, []byte("[scheduler]\nforeman_routing_mode = 'fixed'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, env, want string
		bad             bool
	}{
		{"", "", "router", false}, {"", "fixed", "fixed", false}, {path, "", "fixed", false},
		{path, "router", "router", false}, {path, "typo", "", true},
	} {
		t.Run(tc.path+tc.env, func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", tc.env)
			got, err := resolveForemanRoutingMode(tc.path)
			if got != tc.want || (err != nil) != tc.bad {
				t.Fatalf("resolved=%q/%v", got, err)
			}
		})
	}
}
