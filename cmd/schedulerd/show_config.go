package main

import (
	"fmt"
	"os"
	"time"
)

// printSchema emits a JSON Schema for schedulerd.toml describing every
// TOML key, its type, default, env-var override, and CLI flag mapping.
func printSchema() {
	fmt.Print(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/coding-hermes/scheduler/schemas/schedulerd.toml.schema.json",
  "title": "schedulerd.toml",
  "description": "Coding Hermes Scheduler daemon config — three-layer model (TOML < env vars < CLI flags)",
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
        "min_interval":   { "type": "string", "default": "20m", "env": "SCHEDULER_MIN_INTERVAL", "cli": "--min-interval" },
        "max_interval":   { "type": "string", "default": "24h", "env": "SCHEDULER_MAX_INTERVAL", "cli": "--max-interval" },
        "num_levels":     { "type": "integer", "default": 10, "minimum": 1, "env": "SCHEDULER_NUM_LEVELS", "cli": "--num-levels" },
        "weight_budget":  { "type": "integer", "default": 100, "minimum": 1, "env": "SCHEDULER_BUDGET", "cli": "--budget" },
        "max_concurrent": { "type": "integer", "default": 8, "minimum": 1, "env": "SCHEDULER_MAX_CONCURRENT", "cli": "--max-concurrent" },
        "tick_timeout":   { "type": "string", "default": "2h", "env": "SCHEDULER_TICK_TIMEOUT", "cli": "--tick-timeout" },
        "namespace_mode": { "type": "boolean", "default": false, "env": "SCHEDULER_NAMESPACE_MODE", "cli": "--namespace-mode" }
      }
    },
    "gateway": {
      "type": "object",
      "properties": {
        "url":          { "type": "string", "default": "http://127.0.0.1:8642", "env": "SCHEDULER_GATEWAY_URL", "cli": "--gateway-url" },
        "key":          { "type": "string", "env": "SCHEDULER_GATEWAY_KEY", "cli": "--gateway-key" },
        "foreman_home": { "type": "string", "default": "~/.hermes/foreman", "env": "SCHEDULER_FOREMAN_HOME", "cli": "--foreman-home" }
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
          "cooldown_s": { "type": "integer", "default": 900 },
          "decay_rate": { "type": "number", "default": 1.0 },
          "model": { "type": "string", "default": "deepseek-v4-pro" },
          "provider": { "type": "string", "default": "deepseek-foreman" },
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
`)
}

// printConfig renders the current (CLI-level) configuration as TOML.
// For now, Layer 1 (TOML) and Layer 2 (env vars) are not shown — this
// will be extended when the three-layer LoadConfig is wired in main.go.
func printConfig(
	configFile, dbPath, listen string,
	minInterval, maxInterval time.Duration,
	numLevels, weightBudget, maxConcurrent int,
	namespaceMode bool,
	tickTimeout time.Duration,
	gatewayURL, gatewayKey, foremanHome,
	duckbrainNS, duckbrainURL string,
) {
	fmt.Printf(`# schedulerd resolved configuration (CLI flags only)
# source: command-line flags
# (TOML + env var layers coming in FEAT-005 full wiring)

[daemon]
db_path = %q
listen = %q

[scheduler]
min_interval = %q
max_interval = %q
num_levels = %d
weight_budget = %d
max_concurrent = %d
tick_timeout = %q
namespace_mode = %v

[gateway]
url = %q
key = %q
foreman_home = %q

[duckbrain]
namespace = %q
url = %q
`,
		dbPath, listen,
		minInterval, maxInterval,
		numLevels, weightBudget, maxConcurrent,
		tickTimeout, namespaceMode,
		gatewayURL, gatewayKey, foremanHome,
		duckbrainNS, duckbrainURL,
	)
	if configFile != "" {
		fmt.Printf("# fleet config file: %s\n", configFile)
	}

	// Print env var overrides
	envVars := map[string]string{
		"SCHEDULER_DB_PATH":        os.Getenv("SCHEDULER_DB_PATH"),
		"SCHEDULER_LISTEN":         os.Getenv("SCHEDULER_LISTEN"),
		"SCHEDULER_MIN_INTERVAL":   os.Getenv("SCHEDULER_MIN_INTERVAL"),
		"SCHEDULER_MAX_INTERVAL":   os.Getenv("SCHEDULER_MAX_INTERVAL"),
		"SCHEDULER_NUM_LEVELS":     os.Getenv("SCHEDULER_NUM_LEVELS"),
		"SCHEDULER_BUDGET":         os.Getenv("SCHEDULER_BUDGET"),
		"SCHEDULER_MAX_CONCURRENT": os.Getenv("SCHEDULER_MAX_CONCURRENT"),
		"SCHEDULER_TICK_TIMEOUT":   os.Getenv("SCHEDULER_TICK_TIMEOUT"),
		"SCHEDULER_NAMESPACE_MODE": os.Getenv("SCHEDULER_NAMESPACE_MODE"),
		"SCHEDULER_GATEWAY_URL":    os.Getenv("SCHEDULER_GATEWAY_URL"),
		"SCHEDULER_GATEWAY_KEY":    os.Getenv("SCHEDULER_GATEWAY_KEY"),
		"SCHEDULER_FOREMAN_HOME":   os.Getenv("SCHEDULER_FOREMAN_HOME"),
		"SCHEDULER_DUCK_BRAIN_NS":  os.Getenv("SCHEDULER_DUCK_BRAIN_NS"),
		"SCHEDULER_DUCK_BRAIN_URL": os.Getenv("SCHEDULER_DUCK_BRAIN_URL"),
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
