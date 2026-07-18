// Package config provides declarative fleet definition (TOML) support for
// seeding namespaces and projects at scheduler startup.
//
// A fleet.toml is loaded once at boot via the --config flag and upserted
// into the existing SQLite database. Upsert here means create-only: rows
// that already exist are left untouched so operator-made tweaks survive
// restarts.
//
// FEAT-005 extends this with a three-layer configuration model covering
// daemon, scheduler, gateway, and duckbrain settings. Resolution priority
// (lowest → highest): TOML config file < SCHEDULER_* env vars < CLI flags.
package config

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
// budget, concurrency cap, tick timeout, and the namespace-mode toggle.
// MinInterval/MaxInterval/TickTimeout are stored as duration strings
// (e.g. "20m", "24h", "2h") and parsed with time.ParseDuration by callers.
type SchedulerConfig struct {
	MinInterval   string `toml:"min_interval"`
	MaxInterval   string `toml:"max_interval"`
	NumLevels     int    `toml:"num_levels"`
	WeightBudget  int    `toml:"weight_budget"`
	MaxConcurrent int    `toml:"max_concurrent"`
	TickTimeout   string `toml:"tick_timeout"`
	NamespaceMode bool   `toml:"namespace_mode"`
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
	Name        string  `toml:"name"`
	RepoURL     string  `toml:"repo_url"`
	Workdir     string  `toml:"workdir"`
	Weight      int     `toml:"weight"`     // default 10 if <= 0
	Priority    int     `toml:"priority"`   // default 5 if <= 0
	CooldownS   int     `toml:"cooldown_s"` // default 900 if <= 0
	DecayRate   float64 `toml:"decay_rate"` // default 1.0 if <= 0
	Model       string  `toml:"model"`      // default "deepseek-v4-pro"
	Provider    string  `toml:"provider"`   // default "deepseek-foreman"
	Command     string  `toml:"command"`
	NamespaceID string  `toml:"namespace_id"` // optional FK → namespaces.id
	Deliver     string  `toml:"deliver"`
	Enabled     *bool   `toml:"enabled"` // default true if nil
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
