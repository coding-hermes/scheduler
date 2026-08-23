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
	Name             string  `toml:"name"`
	RepoURL          string  `toml:"repo_url"`
	Workdir          string  `toml:"workdir"`
	Weight           int     `toml:"weight"`             // default 10 if <= 0
	Priority         int     `toml:"priority"`           // default 5 if <= 0
	CooldownS        int     `toml:"cooldown_s"`         // default 7200 if <= 0 (2h baseline, 3-speed policy)
	DecayRate        float64 `toml:"decay_rate"`         // default 1.0 if <= 0
	Model            string  `toml:"model"`              // default DefaultModel
	Provider         string  `toml:"provider"`           // default DefaultProvider
	FallbackModel    string  `toml:"fallback_model"`     // SCHED-GAP-064: fallback model tier for the spawn chain; empty = no project fallback
	FallbackProvider string  `toml:"fallback_provider"`  // SCHED-GAP-064: fallback provider tier for the spawn chain
	NoGlobalFallback bool    `toml:"no_global_fallback"` // true → skip the spawner-level (env) fallback tier entirely
	GatewayKey       string  `toml:"gateway_key"`        // per-foreman Hermes gateway key; empty = shared --gateway-key
	Command          string  `toml:"command"`
	NamespaceID      string  `toml:"namespace_id"` // optional FK → namespaces.id
	Deliver          string  `toml:"deliver"`
	Enabled          *bool   `toml:"enabled"` // default true if nil
}

// NamespaceDef mirrors the subset of database.Namespace fields that are
// meaningful to set declaratively. ID is the only required field.
type NamespaceDef struct {
	ID          string `toml:"id"`
	Weight      int    `toml:"weight"`   // default 10 if <= 0
	Reserved    int    `toml:"reserved"` // default 1 if <= 0
	HardCap     int    `toml:"hard_cap"` // default 100 if <= 0
	Enabled     *bool  `toml:"enabled"`  // default true if nil
	Description string `toml:"description"`
}
