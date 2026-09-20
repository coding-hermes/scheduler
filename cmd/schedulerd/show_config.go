package main

import (
	"fmt"
	"os"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
)

// printSchema emits a JSON Schema for schedulerd.toml describing every
// TOML key, its type, default, env-var override, and CLI flag mapping.
// FEAT-005 has landed: the daemon loads the root schedulerd.toml at boot
// (the four LoadRootConfig call sites in main.go apply it under the
// default-guard pattern), so this documents a LIVE layer, not a planned
// one. Resolution: TOML [scheduler] < SCHEDULER_* env vars < CLI flags.
func printSchema() {
	fmt.Printf(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/coding-hermes/scheduler/schemas/schedulerd.toml.schema.json",
  "title": "schedulerd.toml",
  "description": "Coding Hermes Scheduler daemon config — root TOML layer loaded (FEAT-005 landed). Resolution: TOML [scheduler] < SCHEDULER_* env vars < CLI flags. TOML values apply only where the corresponding flag sits at its default (default-guard pattern in main.go), so CLI and env keep precedence.",
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
        "gateway_response_timeout": { "type": "string", "default": "30m0s", "description": "Per-turn deadline for a gateway /v1/responses POST (SCHED-GAP-117). A POST that makes no progress for this long fails the tick as 'stalled' BEFORE --tick-timeout; '0s' disables (POST runs on the tick deadline alone). Effective POST deadline = min(this, tick_timeout).", "env": "SCHEDULER_GATEWAY_RESPONSE_TIMEOUT", "cli": "--gateway-response-timeout" },
        "slot_patience":  { "type": "string", "default": "5m0s", "description": "How long a tick waits for a free slot before being dropped; the drop emits a MEDIUM slot_pool event (ADV-R08/G3). Must be > 0 — the drop always exists; unset means the 5m default.", "env": "SCHEDULER_SLOT_PATIENCE", "cli": "--slot-patience" },
        "tasks_pacing": { "type": "string", "default": "1m0s", "description": "Minimum post-tick spacing before a tasks-mode project re-admits, plus up to 20 percent jitter (SCHED-GAP-136); '0s' disables. Composes with the failure backoff (S-GAP-001), never replaces it. Unset means the 1m fleet default.", "env": "SCHEDULER_TASKS_PACING", "cli": "--tasks-pacing" },
        "spawn_mem_limit_mb": { "type": "integer", "default": 0, "minimum": 0, "description": "Per-spawn RLIMIT_AS memory cap in MiB applied to spawned foreman processes (ADV-R11, GAP-048 cure); 0 = off (default — no limit call at all). NOT an admission gate: every selected project still spawns; the cap constrains the spawned process's resources at spawn time and is inherited by its workers. Best-effort — a failed cap WARNs and the spawn continues. Linux (prlimit); other platforms degrade to the documented no-op.", "env": "SCHEDULER_SPAWN_MEM_LIMIT_MB", "cli": "--spawn-mem-limit-mb" },
        "load_gate_threshold": { "type": "number", "default": 0.0, "minimum": 0.0, "description": "Defer new spawns while the 1-minute load average is at or above this threshold; 0 = disabled (SCHED-GAP-125). Work is deferred, not dropped — it runs once load drops. Namespaces opt out via load_gate='off'.", "env": "SCHEDULER_LOAD_GATE_THRESHOLD", "cli": "--load-gate-threshold" },
        "model_rates_file": { "type": "string", "default": "", "description": "JSON price-sticker file applied over the builtin model rates at startup (ADV-R09/G8): {as_of, models:{name:{in_per_m,out_per_m}}, providers:{...}} — refresh stickers without a rebuild. Empty = builtin rates only. The flag's default IS the env value, so an env-set path always surfaces in the resolved config.", "env": "SCHEDULER_MODEL_RATES_FILE", "cli": "--model-rates-file" },
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
        "namespace": { "type": "string", "default": "scheduler", "env": "SCHEDULER_DUCK_BRAIN_NS", "cli": "--duckbrain-ns" },
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
          "cooldown_s": { "type": "integer", "default": 7200, "description": "Seconds between ticks. Config/fleet.toml-seeded projects default to 7200 (2h baseline); projects created via POST /api/v1/projects instead default to 900 (SCHED-GAP-195 — intentional split by producer)." },
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
          "description": { "type": "string" },
          "wave_enabled": { "type": "boolean", "default": false, "description": "S12 concurrent wave scheduling; false/absent = waves off (byte-identical pre-v27 behavior)." },
          "wave_tick_timeout": { "type": "string", "default": "", "description": "Tick deadline override for wave-enabled namespaces. Recommended 3h (1.5x base); hard ceiling 4h enforced at config validation (S12 §4.3). Empty = inherit scheduler.tick_timeout. Env override at the spawn site: SCHEDULER_WAVE_TICK_TIMEOUT." },
          "wave_workers_cap": { "type": "integer", "default": 0, "description": "Max concurrent worker processes across the namespace's running ticks; 0 = unlimited (S12 §6)." }
        }
      }
    }
  }
}
`, config.DefaultModel, config.DefaultProvider)
}

// printConfig renders the CLI/env configuration layers (already resolved in
// main.go before this is called) as TOML. FEAT-005 has landed: the root
// schedulerd.toml layer IS applied at boot via the default-guard pattern
// (TOML [scheduler] < SCHEDULER_* env overrides < CLI flags), but the TOML
// layer resolves LATER in main.go — after this command's early exit — so the
// printed values reflect the CLI and env layers ONLY. A /tmp TOML probe with
// distinctive [scheduler] keys (SCHED-GAP-165, measured at 5225bbae) printed
// defaults for every one of them, proving the surface cannot show the TOML
// layer; the header says so instead of overclaiming. --schema documents the
// live layer including the default-guard TOML semantics.
func printConfig(
	configFile, dbPath, listen, logFile string,
	minInterval, maxInterval time.Duration,
	numLevels, weightBudget, maxConcurrent int,
	namespaceMode bool,
	tickTimeout, gatewayResponseTimeout, slotPatience, tasksPacing time.Duration,
	gatewayURL, gatewayKey, foremanHome string,
	noExecFallback bool,
	duckbrainNS, duckbrainURL string,
	autoDisableRate float64,
	autoDisableWindow, autoDisableMinTicks, failureWindow int,
	spawnMemLimitMB int64,
	loadGateThreshold float64,
	modelRatesFile string,
) {
	fmt.Printf(`# schedulerd resolved configuration (CLI flags + SCHEDULER_* env overrides; the root TOML [scheduler] layer resolves later in boot and is NOT reflected here)
# source: CLI flags + SCHEDULER_* env overrides (CLI > env). TOML [scheduler] (FEAT-005, applied via default-guard in main.go) resolves after this print and is not shown

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
gateway_response_timeout = %q
slot_patience = %q
tasks_pacing = %q
spawn_mem_limit_mb = %d
load_gate_threshold = %v
model_rates_file = %q
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
		tickTimeout, gatewayResponseTimeout, slotPatience, tasksPacing,
		spawnMemLimitMB,
		loadGateThreshold, modelRatesFile,
		namespaceMode, autoDisableRate, autoDisableWindow, autoDisableMinTicks, failureWindow,
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
		"SCHEDULER_GATEWAY_RESPONSE_TIMEOUT":  os.Getenv("SCHEDULER_GATEWAY_RESPONSE_TIMEOUT"),
		"SCHEDULER_SLOT_PATIENCE":             os.Getenv("SCHEDULER_SLOT_PATIENCE"),
		"SCHEDULER_SPAWN_MEM_LIMIT_MB":        os.Getenv("SCHEDULER_SPAWN_MEM_LIMIT_MB"),
		"SCHEDULER_WAVE_TICK_TIMEOUT":         os.Getenv("SCHEDULER_WAVE_TICK_TIMEOUT"),
		"SCHEDULER_NAMESPACE_MODE":            os.Getenv("SCHEDULER_NAMESPACE_MODE"),
		"SCHEDULER_AUTO_DISABLE_FAILURE_RATE": os.Getenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE"),
		"SCHEDULER_AUTO_DISABLE_WINDOW":       os.Getenv("SCHEDULER_AUTO_DISABLE_WINDOW"),
		"SCHEDULER_AUTO_DISABLE_MIN_TICKS":    os.Getenv("SCHEDULER_AUTO_DISABLE_MIN_TICKS"),
		"SCHEDULER_FAILURE_WINDOW":            os.Getenv("SCHEDULER_FAILURE_WINDOW"),
		"SCHEDULER_LOAD_GATE_THRESHOLD":       os.Getenv("SCHEDULER_LOAD_GATE_THRESHOLD"),
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
