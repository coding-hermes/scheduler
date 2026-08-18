package main

import (
	"fmt"
	"os"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
)

// printSchema emits a JSON Schema for schedulerd.toml describing every
// TOML key, its type, default, env-var override, and CLI flag mapping.
// The schema is the contract for the planned FEAT-005 root TOML wiring:
// the daemon does NOT load schedulerd.toml yet (active layers: env vars <
// CLI flags), so this documents the future layer, not a loaded one.
func printSchema() {
	fmt.Printf(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/coding-hermes/scheduler/schemas/schedulerd.toml.schema.json",
  "title": "schedulerd.toml",
  "description": "Coding Hermes Scheduler daemon config — schema for the FEAT-005 root TOML wiring (NOT yet loaded by the daemon; active layers: env vars < CLI flags)",
  "type": "object",
  "properties": {
    "daemon": {
      "type": "object",
      "properties": {
        "db_path": { "type": "string", "default": "~/.hermes/coding-hermes/scheduler.db", "env": "SCHEDULER_DB_PATH", "cli": "--db" },
        "listen":  { "type": "string", "default": "127.0.0.1:9090", "env": "SCHEDULER_LISTEN", "cli": "--listen" }
      }
    },
    "scheduler": {
      "type": "object",
      "properties": {
        "min_interval":   { "type": "string", "default": "30s", "env": "SCHEDULER_MIN_INTERVAL", "cli": "--min-interval" },
        "max_interval":   { "type": "string", "default": "24h", "env": "SCHEDULER_MAX_INTERVAL", "cli": "--max-interval" },
        "num_levels":     { "type": "integer", "default": 10, "minimum": 1, "env": "SCHEDULER_NUM_LEVELS", "cli": "--num-levels" },
        "weight_budget":  { "type": "integer", "default": 100, "minimum": 1, "env": "SCHEDULER_BUDGET", "cli": "--budget" },
        "max_concurrent": { "type": "integer", "default": 10, "minimum": 1, "env": "SCHEDULER_MAX_CONCURRENT", "cli": "--max-concurrent" },
        "tick_timeout":   { "type": "string", "default": "2h", "env": "SCHEDULER_TICK_TIMEOUT", "cli": "--tick-timeout" },
        "namespace_mode": { "type": "boolean", "default": false, "env": "SCHEDULER_NAMESPACE_MODE", "cli": "--namespace-mode" },
        "auto_disable_failure_rate": { "type": "number", "default": 0.0, "minimum": 0.0, "maximum": 1.0, "description": "Per-project failure-rate threshold (0 = off). SCHED-GAP-018.", "env": "SCHEDULER_AUTO_DISABLE_FAILURE_RATE", "cli": "--auto-disable-failure-rate" },
        "auto_disable_window":       { "type": "integer", "default": 100, "minimum": 1, "description": "Ticks per project over which auto-disable failure rate is computed.", "env": "SCHEDULER_AUTO_DISABLE_WINDOW", "cli": "--auto-disable-window" },
        "auto_disable_min_ticks":    { "type": "integer", "default": 50, "minimum": 1, "description": "Minimum ticks in window before auto-disable can fire.", "env": "SCHEDULER_AUTO_DISABLE_MIN_TICKS", "cli": "--auto-disable-min-ticks" },
        "failure_window":            { "type": "integer", "default": 100, "minimum": 1, "description": "Ticks per project for /api/v1/status per-project failure-rate breakdown.", "env": "SCHEDULER_FAILURE_WINDOW", "cli": "--failure-window" }
      }
    },
    "gateway": {
      "type": "object",
      "properties": {
        "url":          { "type": "string", "default": "http://127.0.0.1:8642", "env": "SCHEDULER_GATEWAY_URL", "cli": "--gateway-url" },
        "key":          { "type": "string", "env": "SCHEDULER_GATEWAY_KEY", "cli": "--gateway-key" },
        "foreman_home": { "type": "string", "default": "~/.hermes/foreman", "env": "SCHEDULER_FOREMAN_HOME", "cli": "--foreman-home" },
        "no_exec_fallback": { "type": "boolean", "default": true, "cli": "--no-exec-fallback" }
      }
    },
    "duckbrain": {
      "type": "object",
      "properties": {
        "namespace": { "type": "string", "default": "coding-hermes", "env": "SCHEDULER_DUCK_BRAIN_NS", "cli": "--duckbrain-ns" },
        "url":       { "type": "string", "default": "http://localhost:3000", "env": "SCHEDULER_DUCK_BRAIN_URL", "cli": "--duckbrain-url" }
      }
    },
    "projects": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": { "type": "string" },
          "repo_url": { "type": "string" },
          "workdir": { "type": "string" },
          "weight": { "type": "integer", "default": 10 },
          "priority": { "type": "integer", "default": 5 },
          "cooldown_s": { "type": "integer", "default": 7200 },
          "decay_rate": { "type": "number", "default": 1.0 },
          "model": { "type": "string", "default": %q },
          "provider": { "type": "string", "default": %q },
          "command": { "type": "string" },
          "namespace_id": { "type": "string" },
          "deliver": { "type": "string" },
          "enabled": { "type": "boolean", "default": true }
        }
      }
    },
    "namespaces": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "weight": { "type": "integer", "default": 10 },
          "reserved": { "type": "integer", "default": 1 },
          "hard_cap": { "type": "integer", "default": 100 },
          "enabled": { "type": "boolean", "default": true },
          "description": { "type": "string" }
        }
      }
    }
  }
}
`, config.DefaultModel, config.DefaultProvider)
}

// printConfig renders the effective configuration (CLI flags + SCHEDULER_*
// env overrides, already resolved in main.go before this is called) as TOML.
// Root schedulerd.toml loading is deliberately NOT wired into daemon boot —
// it arrives with FEAT-005; the --schema output documents that planned layer.
func printConfig(
	configFile, dbPath, listen, logFile string,
	minInterval, maxInterval time.Duration,
	numLevels, weightBudget, maxConcurrent int,
	namespaceMode bool,
	tickTimeout time.Duration,
	gatewayURL, gatewayKey, foremanHome string,
	noExecFallback bool,
	duckbrainNS, duckbrainURL string,
	autoDisableRate float64,
	autoDisableWindow, autoDisableMinTicks, failureWindow int,
) {
	fmt.Printf(`# schedulerd resolved configuration (effective values: CLI flags + SCHEDULER_* env overrides applied)
# source: CLI flags + SCHEDULER_* env var overrides; root TOML loading comes in FEAT-005

[daemon]
db_path = %q
listen = %q
log_file = %q

[scheduler]
min_interval = %q
max_interval = %q
num_levels = %d
weight_budget = %d
max_concurrent = %d
tick_timeout = %q
namespace_mode = %v
auto_disable_failure_rate = %v
auto_disable_window = %d
auto_disable_min_ticks = %d
failure_window = %d

[gateway]
url = %q
key = %q
foreman_home = %q
no_exec_fallback = %v

[duckbrain]
namespace = %q
url = %q
`,
		dbPath, listen, logFile,
		minInterval, maxInterval,
		numLevels, weightBudget, maxConcurrent,
		tickTimeout, namespaceMode, autoDisableRate, autoDisableWindow, autoDisableMinTicks, failureWindow,
		gatewayURL, gatewayKey, foremanHome, noExecFallback,
		duckbrainNS, duckbrainURL,
	)
	if configFile != "" {
		fmt.Printf("# fleet config file: %s\n", configFile)
	}

	// Print env var overrides
	envVars := map[string]string{
		"SCHEDULER_DB_PATH":                   os.Getenv("SCHEDULER_DB_PATH"),
		"SCHEDULER_LISTEN":                    os.Getenv("SCHEDULER_LISTEN"),
		"SCHEDULER_MIN_INTERVAL":              os.Getenv("SCHEDULER_MIN_INTERVAL"),
		"SCHEDULER_MAX_INTERVAL":              os.Getenv("SCHEDULER_MAX_INTERVAL"),
		"SCHEDULER_NUM_LEVELS":                os.Getenv("SCHEDULER_NUM_LEVELS"),
		"SCHEDULER_BUDGET":                    os.Getenv("SCHEDULER_BUDGET"),
		"SCHEDULER_MAX_CONCURRENT":            os.Getenv("SCHEDULER_MAX_CONCURRENT"),
		"SCHEDULER_TICK_TIMEOUT":              os.Getenv("SCHEDULER_TICK_TIMEOUT"),
		"SCHEDULER_NAMESPACE_MODE":            os.Getenv("SCHEDULER_NAMESPACE_MODE"),
		"SCHEDULER_AUTO_DISABLE_FAILURE_RATE": os.Getenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE"),
		"SCHEDULER_AUTO_DISABLE_WINDOW":       os.Getenv("SCHEDULER_AUTO_DISABLE_WINDOW"),
		"SCHEDULER_AUTO_DISABLE_MIN_TICKS":    os.Getenv("SCHEDULER_AUTO_DISABLE_MIN_TICKS"),
		"SCHEDULER_FAILURE_WINDOW":            os.Getenv("SCHEDULER_FAILURE_WINDOW"),
		"SCHEDULER_GATEWAY_URL":               os.Getenv("SCHEDULER_GATEWAY_URL"),
		"SCHEDULER_GATEWAY_KEY":               os.Getenv("SCHEDULER_GATEWAY_KEY"),
		"SCHEDULER_FOREMAN_HOME":              os.Getenv("SCHEDULER_FOREMAN_HOME"),
		"SCHEDULER_DUCK_BRAIN_NS":             os.Getenv("SCHEDULER_DUCK_BRAIN_NS"),
		"SCHEDULER_DUCK_BRAIN_URL":            os.Getenv("SCHEDULER_DUCK_BRAIN_URL"),
	}
	activeEnvs := false
	for name, val := range envVars {
		if val != "" {
			if !activeEnvs {
				fmt.Println("# active env var overrides:")
				activeEnvs = true
			}
			fmt.Printf("#   %s=%s\n", name, val)
		}
	}
}
