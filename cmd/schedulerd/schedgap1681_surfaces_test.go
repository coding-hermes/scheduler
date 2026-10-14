package main

// SCHED-GAP-1681 knob surfaces: --gateway-transient-retries must appear on
// all four documented surfaces — the --schema JSON, the --show-config TOML
// print, the two flag-table doc files, and main.go's resolution chain — the
// same contract schedgap1707_surfaces_test.go enforces for its knob.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSCHEDGAP1681_TransientRetriesOnAllSurfaces(t *testing.T) {
	t.Run("printSchema --schema JSON", func(t *testing.T) {
		var schema struct {
			Properties struct {
				Scheduler struct {
					Properties struct {
						TransientRetries *struct {
							Type    string `json:"type"`
							Default any    `json:"default"`
							Env     string `json:"env"`
							CLI     string `json:"cli"`
						} `json:"gateway_transient_retries"`
					} `json:"properties"`
				} `json:"scheduler"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(captureStdout(printSchema)), &schema); err != nil {
			t.Fatalf("printSchema() did not emit valid JSON: %v", err)
		}
		g := schema.Properties.Scheduler.Properties.TransientRetries
		if g == nil {
			t.Fatal("--schema is missing properties.scheduler.properties.gateway_transient_retries (SCHED-GAP-1681)")
		}
		if g.Default != float64(3) {
			t.Errorf("gateway_transient_retries default = %v, want 3 (the pre-1681 hardcoded value)", g.Default)
		}
		if g.Env != "SCHEDULER_GATEWAY_TRANSIENT_RETRIES" {
			t.Errorf("gateway_transient_retries env = %q, want SCHEDULER_GATEWAY_TRANSIENT_RETRIES", g.Env)
		}
		if g.CLI != "--gateway-transient-retries" {
			t.Errorf("gateway_transient_retries cli = %q, want --gateway-transient-retries", g.CLI)
		}
	})

	t.Run("printConfig --show-config TOML", func(t *testing.T) {
		out := captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-1681.db",
				"127.0.0.1:9090",
				"",
				30*time.Second,
				24*time.Hour,
				10, 100, 10,
				false,
				2*time.Hour, 30*time.Minute, 5*time.Minute, time.Minute,
				7, // SCHED-GAP-1681 sentinel: the printed value is this argument
				"http://127.0.0.1:8642", "secret", "/tmp/foreman",
				true,
				"scheduler", "http://localhost:3000",
				0,
				100, 50, 100,
				0,
				0,
				"",
				0,
				0,
			)
		})
		if got := tomlSectionValue(t, out, "scheduler", "gateway_transient_retries"); got != "7" {
			t.Errorf("[scheduler] gateway_transient_retries in --show-config = %q, want \"7\" (the argument)", got)
		}
	})

	t.Run("docs tables carry the flag", func(t *testing.T) {
		for _, rel := range []string{filepath.Join("..", "..", "README.md"), filepath.Join("..", "..", "docs", "reference", "flags.md")} {
			body, err := os.ReadFile(rel)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if !strings.Contains(string(body), "gateway-transient-retries") {
				t.Errorf("%s is missing a --gateway-transient-retries row (SCHED-GAP-1681 flag parity)", rel)
			}
		}
	})

	t.Run("main.go registers the flag", func(t *testing.T) {
		names := sourceFlagNames(t, "main.go")
		if !names["gateway-transient-retries"] {
			t.Fatal("main.go does not register --gateway-transient-retries — the knob dropped out of the daemon surface")
		}
	})
}
