// Package config provides declarative fleet definition (TOML) support for
// seeding namespaces and projects at scheduler startup.
//
// A fleet.toml is loaded once at boot via the --config flag and upserted
// into the existing SQLite database. Namespaces are create-only (existing
// rows are skipped), but EXISTING projects are re-pinned from fleet.toml at
// every startup — cooldown, model, provider, and enabled are overwritten with
// the fleet.toml values (see ApplyFleetConfig). fleet.toml is therefore the
// durable pin source across restarts: API-side tweaks to a pinned project
// survive only until the next restart.
//
// FEAT-005 extends this with a three-layer configuration model covering
// daemon, scheduler, gateway, and duckbrain settings. Resolution priority
// (lowest → highest): TOML config file < SCHEDULER_* env vars < CLI flags.
package config

import (
	"strconv"
	"strings"
	"time"
)

// Default model and provider names used as fallbacks when no value is
// specified in TOML, environment variables, or CLI flags.
const (
	DefaultModel    = "deepseek-v4-flash"
	DefaultProvider = "deepseek-foreman"
)

// FleetConfig is the top-level TOML structure decoded from a fleet.toml.
// The [[projects]] and [[namespaces]] array-of-tables slices allow the
// same project/namespace to be defined declaratively and in order.
//
// Retained for backward compatibility with existing callers and tests.
// New code should prefer RootConfig, which embeds the same Projects and
// Namespaces slices plus the daemon/scheduler/gateway/duckbrain sections.
type FleetConfig struct {
	Projects   []ProjectDef   `toml:"projects"`
	Namespaces []NamespaceDef `toml:"namespaces"`
}

// DaemonConfig covers process-level daemon settings: where the SQLite
// database lives and where the HTTP server listens.
type DaemonConfig struct {
	DBPath string `toml:"db_path"`
	Listen string `toml:"listen"`
}

// SchedulerConfig covers the scheduling core: interval ladder, weight
// budget, concurrency cap, tick timeout, namespace-mode toggle, and
// blackout windows for peak-pricing slowdown.
// MinInterval/MaxInterval/TickTimeout are stored as duration strings
// (e.g. "20m", "24h", "2h") and parsed with time.ParseDuration by callers.
type SchedulerConfig struct {
	MinInterval     string           `toml:"min_interval"`
	MaxInterval     string           `toml:"max_interval"`
	NumLevels       int              `toml:"num_levels"`
	WeightBudget    int              `toml:"weight_budget"`
	MaxConcurrent   int              `toml:"max_concurrent"`
	TickTimeout     string           `toml:"tick_timeout"`
	NamespaceMode   bool             `toml:"namespace_mode"`
	BlackoutWindows []BlackoutWindow `toml:"blackout_windows"`

	// GatewayResponseTimeout (SCHED-GAP-117) is the per-turn deadline for a
	// single gateway /v1/responses POST, stored as a duration string
	// (e.g. "30m"; "0s" = disabled — the POST runs on the tick deadline
	// alone). Effective POST deadline = min(this, tick_timeout). A POST
	// that makes no progress for this long fails the tick as "stalled"
	// BEFORE --tick-timeout, so a hung turn no longer consumes the whole
	// 2h slot. Empty = the daemon flag default (30m).
	GatewayResponseTimeout string `toml:"gateway_response_timeout"`

	// SlotPatience (ADV-R08/G3) is how long a spawn waits for a free slot
	// before the project is dropped (the drop emits a MEDIUM slot_pool
	// event), stored as a duration string (e.g. "5m"). Empty = the daemon
	// flag default (5m). Must parse to > 0 — the drop always exists, so a
	// non-positive value is invalid (a TOML "0s" would otherwise be a
	// silent no-op: the flag's <= 0 means "keep default").
	SlotPatience string `toml:"slot_patience"`

	// LoadGateThreshold (SCHED-GAP-125) defers new spawns while the 1-minute
	// load average is at or above this value (e.g. 12.0 on a 16-core box).
	// 0 = the gate is off (flag default). Applies only when the operator
	// never passed --load-gate-threshold (same precedence chain as budget:
	// TOML < env SCHEDULER_LOAD_GATE_THRESHOLD < flag).
	LoadGateThreshold float64 `toml:"load_gate_threshold"`

	// SpawnMemLimitMB (ADV-R11, GAP-048 cure) is the per-spawn RLIMIT_AS
	// memory cap in MiB applied to spawned foreman processes. 0 = off (the
	// default — no limit call at all, byte-identical spawns). NOT an
	// admission gate: every selected project still spawns; the cap
	// constrains the spawned process's resources at spawn time and is
	// inherited by the workers it forks. Best-effort — a failed cap WARNs
	// and the spawn continues unlimited. Linux (prlimit/RLIMIT_AS);
	// non-Linux builds degrade to the documented no-op stub. Precedence:
	// TOML < env SCHEDULER_SPAWN_MEM_LIMIT_MB < flag --spawn-mem-limit-mb.
	SpawnMemLimitMB int64 `toml:"spawn_mem_limit_mb"`

	// AutoDisableFailureRate (0.0–1.0) is the per-project failure-rate
	// threshold over the last AutoDisableWindow ticks at or above which the
	// scheduler will disable the project automatically. Default 0 = feature
	// off (SCHED-GAP-018). Operators opt in.
	AutoDisableFailureRate float64 `toml:"auto_disable_failure_rate"`
	// AutoDisableWindow is the number of recent ticks (per project) over
	// which the failure rate is computed for auto-disable.
	AutoDisableWindow int `toml:"auto_disable_window"`
	// AutoDisableMinTicks is the minimum number of ticks a project must have
	// within the window before it can be auto-disabled (sample-size guard).
	AutoDisableMinTicks int `toml:"auto_disable_min_ticks"`
	// FailureWindow is the number of recent ticks (per project) used by the
	// /api/v1/status per-project failure-rate breakdown. Independent of the
	// auto-disable window so the dashboard can stay readable even when
	// auto-disable is off.
	FailureWindow int `toml:"failure_window"`
}

// BlackoutWindow defines a peak-pricing window during which the scheduler
// applies a cooldown multiplier to reduce API costs. All times are UTC.
// If Multiplier <= 0, the project is not spawned during this window.
//
// Example: DeepSeek peak hours 01:00-04:00 and 06:00-10:00 UTC at 2x price,
// weekdays only (DeepSeek official card 2026-08-23: off-peak all day weekends,
// Beijing time — weekends are Sat/Sun Beijing = UTC+8).
type BlackoutWindow struct {
	Start        string  `toml:"start"`         // "HH:MM" in UTC (e.g. "01:00")
	End          string  `toml:"end"`           // "HH:MM" in UTC (e.g. "04:00")
	Multiplier   float64 `toml:"multiplier"`    // e.g. 2.0 = double cooldown, 0 = skip entirely
	WeekdaysOnly bool    `toml:"weekdays_only"` // apply only Mon-Fri (Beijing time); weekends all off-peak
}

// ActiveMultiplier returns the slowdown multiplier for the given time.
// Returns 1.0 (no slowdown) if now is not inside any blackout window.
// Returns 0 if Multiplier <= 0 (skip/project blackout).
func ActiveMultiplier(windows []BlackoutWindow, now time.Time) (float64, bool) {
	for _, w := range windows {
		// Weekday check in BEIJING time (UTC+8) — DeepSeek's weekend rule is
		// defined in Beijing time; Sat/Sun Beijing = off-peak all day.
		if w.WeekdaysOnly {
			bj := now.UTC().Add(8 * time.Hour)
			if bj.Weekday() == time.Saturday || bj.Weekday() == time.Sunday {
				continue
			}
		}
		startH, startM := parseHM(w.Start)
		endH, endM := parseHM(w.End)
		start := time.Date(now.Year(), now.Month(), now.Day(), startH, startM, 0, 0, time.UTC)
		end := time.Date(now.Year(), now.Month(), now.Day(), endH, endM, 0, 0, time.UTC)
		if end.Before(start) || end.Equal(start) {
			end = end.Add(24 * time.Hour) // overnight window
		}
		if (now.After(start) || now.Equal(start)) && now.Before(end) {
			if w.Multiplier <= 0 {
				return 0, true // in blackout — skip entirely
			}
			return w.Multiplier, true
		}
	}
	return 1.0, false
}

func parseHM(s string) (int, int) {
	parts := strings.SplitN(s, ":", 2)
	h, _ := strconv.Atoi(parts[0])
	m := 0
	if len(parts) > 1 {
		m, _ = strconv.Atoi(parts[1])
	}
	return h, m
}

// GatewayConfig covers the Hermes gateway HTTP API used to spawn foreman
// ticks (FEAT-003). An empty URL falls back to exec.Command. Key supports
// ${VAR} env-var interpolation at TOML load time.
type GatewayConfig struct {
	URL         string `toml:"url"`
	Key         string `toml:"key"`
	ForemanHome string `toml:"foreman_home"`
}

// DuckBrainConfig covers the DuckBrain memory sync subsystem.
type DuckBrainConfig struct {
	Namespace string `toml:"namespace"`
	URL       string `toml:"url"`
}

// RootConfig is the top-level structure decoded from a schedulerd.toml
// (the FEAT-005 unified config file). It wraps the daemon/scheduler/
// gateway/duckbrain sections plus the existing fleet definitions, which
// can live in the same file or a fleet-only file loaded via the legacy
// LoadFleetConfig entrypoint.
type RootConfig struct {
	Daemon     DaemonConfig    `toml:"daemon"`
	Scheduler  SchedulerConfig `toml:"scheduler"`
	Gateway    GatewayConfig   `toml:"gateway"`
	DuckBrain  DuckBrainConfig `toml:"duckbrain"`
	Projects   []ProjectDef    `toml:"projects"`
	Namespaces []NamespaceDef  `toml:"namespaces"`
}

// AsFleet returns a FleetConfig view of this RootConfig's Projects and
// Namespaces slices. The slices are shared (not copied) — callers should
// not mutate the result if the source RootConfig is still in use.
func (r *RootConfig) AsFleet() *FleetConfig {
	return &FleetConfig{
		Projects:   r.Projects,
		Namespaces: r.Namespaces,
	}
}

// ProjectDef mirrors the subset of database.Project fields that are
// meaningful to set declaratively (see internal/database/models.go).
// Fields left at their zero value get defaults matching the db schema,
// applied in loader.go when materializing the row.
type ProjectDef struct {
	Name             string   `toml:"name"`
	RepoURL          string   `toml:"repo_url"`
	Workdir          string   `toml:"workdir"`
	Weight           int      `toml:"weight"`             // default 10 if <= 0
	Priority         int      `toml:"priority"`           // default 5 if <= 0
	CooldownS        int      `toml:"cooldown_s"`         // default 7200 if <= 0 (2h baseline, 3-speed policy)
	DecayRate        float64  `toml:"decay_rate"`         // default 1.0 if <= 0
	Model            string   `toml:"model"`              // default DefaultModel
	Provider         string   `toml:"provider"`           // default DefaultProvider
	FallbackModel    string   `toml:"fallback_model"`     // SCHED-GAP-064: fallback model tier for the spawn chain; empty = no project fallback
	FallbackProvider string   `toml:"fallback_provider"`  // SCHED-GAP-064: fallback provider tier for the spawn chain
	NoGlobalFallback bool     `toml:"no_global_fallback"` // true → skip the spawner-level (env) fallback tier entirely
	ModelChain       []string `toml:"model_chain"`        // SCHED-GAP-075: ordered list of "model@provider" hops; empty = use model/provider + fallback fields
	IdleModel        string   `toml:"idle_model"`         // SCHED-GAP-065: idle-tick model tier — used when the project board has ZERO pending tasks; empty = no project idle tier (falls through to the regular chain)
	IdleProvider     string   `toml:"idle_provider"`      // SCHED-GAP-065: idle-tick provider tier
	DailyBudgetUSD   *float64 `toml:"daily_budget_usd"`   // SCHED-GAP-066: per-UTC-day spend cap; nil or <= 0 = unlimited
	WeeklyBudgetUSD  *float64 `toml:"weekly_budget_usd"`  // SCHED-GAP-066: per-UTC-week spend cap (Monday 00:00 UTC reset); nil or <= 0 = unlimited
	FinalBudgetUSD   *float64 `toml:"final_budget_usd"`   // SCHED-GAP-066: one-time lifetime spend cap, never resets; nil or <= 0 = unlimited
	GatewayKey       string   `toml:"gateway_key"`        // per-foreman Hermes gateway key; empty = shared --gateway-key
	Command          string   `toml:"command"`
	Prompt           string   `toml:"prompt"`       // Bane 2026-08-27: extra foreman prompt; appended to namespace default_prompt unless prompt_mode="replace"
	PromptMode       string   `toml:"prompt_mode"`  // Bane 2026-08-27: "append" (default) | "replace"
	NamespaceID      string   `toml:"namespace_id"` // optional FK → namespaces.id
	Deliver          string   `toml:"deliver"`
	Enabled          *bool    `toml:"enabled"` // default true if nil
	// Adaptive cooldown (auto slow-down / speed-up) — OPT-IN per project.
	// adaptive_cooldown = true arms the no-progress streak escalator:
	// no_progress_threshold consecutive ticks with 0 commits AND no new
	// tasks.jsonl rows multiply cooldown_s by 2 up to cooldown_ceiling_s
	// (no explicit ceiling → the derived default 8 × floor, ADV-R10);
	// ANY progress resets cooldown_s to cooldown_floor_s (default: the
	// cooldown_s in force at enable time). Zero-valued numeric keys fall
	// back to the built-in defaults. Pinned like enabled/cooldown_s: a
	// project listed here without adaptive_cooldown is re-pinned to false
	// at every startup.
	AdaptiveCooldown    *bool `toml:"adaptive_cooldown"`
	CooldownFloorS      int   `toml:"cooldown_floor_s"`
	CooldownCeilingS    int   `toml:"cooldown_ceiling_s"`
	NoProgressThreshold int   `toml:"no_progress_threshold"`
	// SCHED-GAP-124: per-project admission-mode override; "" = inherit
	// namespace default. Valid values: "cooldown" | "tasks". Pins only
	// when explicitly set (GatewayKey-style conditional pin).
	AdmissionMode string `toml:"admission_mode"`
}

// NamespaceDef mirrors the subset of database.Namespace fields that are
// meaningful to set declaratively. ID is the only required field.
type NamespaceDef struct {
	ID            string   `toml:"id"`
	Weight        int      `toml:"weight"`         // default 10 if <= 0
	Reserved      int      `toml:"reserved"`       // default 1 if <= 0
	HardCap       int      `toml:"hard_cap"`       // default 100 if <= 0
	MaxConcurrent int      `toml:"max_concurrent"` // 0 = unlimited (global --max-concurrent still applies); positive = max ticks running at once in this namespace
	Enabled       *bool    `toml:"enabled"`        // default true if nil
	Description   string   `toml:"description"`
	DefaultPrompt string   `toml:"default_prompt"` // Bane 2026-08-27: foreman prompt default for all projects in this namespace; empty = built-in
	ModelChain    []string `toml:"model_chain"`    // Bane 2026-08-27: namespace-level model chain ("model@provider" hops); tier between project chain and router
	// S12 concurrent wave scheduling (SCHED-GAP-109), all default-off:
	// wave_enabled = false leaves namespace behavior byte-identical.
	WaveEnabled     *bool  `toml:"wave_enabled"`      // optional; default false (waves off)
	WaveTickTimeout string `toml:"wave_tick_timeout"` // duration string; "" = inherit scheduler tick timeout (S12 §4)
	WaveWorkersCap  int    `toml:"wave_workers_cap"`  // max concurrent worker processes across the namespace's running ticks; 0 = unlimited (S12 §6)
	// SCHED-GAP-124: namespace admission mode — "cooldown" (default, cron)
	// or "tasks" (work-driven: non-perpetual pending board work admits
	// immediately). Pins via the namespace upsert at load time.
	AdmissionMode string `toml:"admission_mode"`
	// SCHED-GAP-125: namespace load-gate opt-out — "off" exempts this
	// namespace's projects from the global --load-gate-threshold deferral
	// (always-on infra lanes). Empty = gate applies when enabled globally.
	// Pins via the namespace upsert at load time.
	LoadGate string `toml:"load_gate"`
}
