package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/coding-herms/scheduler/internal/database"
)

// Sensible defaults matching the projects/namespaces schema; applied when
// the corresponding TOML field is omitted or set to its zero value.
const (
	defaultProjectWeight    = 10
	defaultProjectPriority  = 5
	defaultProjectCooldown  = 900
	defaultProjectDecayRate = 1.0
	defaultProjectModel    = "your-model-name"    // agent fills in
	defaultProjectProvider = "your-provider-name" // agent fills in

	defaultNamespaceWeight   = 10
	defaultNamespaceReserved = 1
	defaultNamespaceHardCap  = 100
)

// Hardcoded defaults for the FEAT-005 three-layer config. These mirror
// the flag defaults that previously lived in cmd/schedulerd/main.go and
// act as layer 0 (below TOML, env vars, and CLI flags).
const (
	defaultDBPath        = "~/.hermes/coding-hermes/scheduler.db"
	defaultListen        = "127.0.0.1:9090"
	defaultMinInterval   = "20m"
	defaultMaxInterval   = "24h"
	defaultNumLevels     = 10
	defaultWeightBudget  = 100
	defaultMaxConcurrent = 8
	defaultTickTimeout   = "2h"
	defaultGatewayURL    = "http://127.0.0.1:8642"
	defaultForemanHome   = "~/.hermes/foreman"
	defaultDuckBrainNS   = "coding-hermes"
	defaultDuckBrainURL  = "http://localhost:3000"
)

// envVarPattern matches ${VAR} placeholders in TOML string values. Used by
// interpolateEnv to substitute environment variables at load time.
var envVarPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// defaultRootConfig returns a RootConfig populated with the hardcoded
// layer-0 defaults. Callers mutate this struct in place as each higher
// layer (TOML, env vars, CLI flags) is applied.
func defaultRootConfig() *RootConfig {
	return &RootConfig{
		Daemon: DaemonConfig{
			DBPath: defaultDBPath,
			Listen: defaultListen,
		},
		Scheduler: SchedulerConfig{
			MinInterval:   defaultMinInterval,
			MaxInterval:   defaultMaxInterval,
			NumLevels:     defaultNumLevels,
			WeightBudget:  defaultWeightBudget,
			MaxConcurrent: defaultMaxConcurrent,
			TickTimeout:   defaultTickTimeout,
			NamespaceMode: false,
		},
		Gateway: GatewayConfig{
			URL:         defaultGatewayURL,
			Key:         "",
			ForemanHome: defaultForemanHome,
		},
		DuckBrain: DuckBrainConfig{
			Namespace: defaultDuckBrainNS,
			URL:       defaultDuckBrainURL,
		},
	}
}

// LoadConfig implements the three-layer configuration merge for FEAT-005.
//
// Resolution order (lowest → highest):
//  1. Hardcoded defaults (defaultRootConfig)
//  2. TOML config file at tomlPath (if non-empty and the file exists)
//  3. SCHEDULER_* environment variables (applyEnvOverrides)
//
// CLI flags are applied by the caller (cmd/schedulerd/main.go) after this
// function returns, since flag parsing happens in main. This keeps the
// config package free of flag-package dependencies.
//
// If tomlPath is empty, only defaults + env vars are applied. If the path
// is non-empty but the file does not exist, an error is returned.
func LoadConfig(tomlPath string) (*RootConfig, error) {
	cfg := defaultRootConfig()

	if tomlPath != "" {
		if _, err := os.Stat(tomlPath); err != nil {
			return nil, fmt.Errorf("stat config %s: %w", tomlPath, err)
		}
		raw, err := os.ReadFile(tomlPath)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", tomlPath, err)
		}
		// Apply ${VAR} env-var interpolation before TOML decode so string
		// values like gateway.key = "${API_SERVER_KEY}" resolve.
		interpolated := interpolateEnv(string(raw))
		if _, err := toml.Decode(interpolated, cfg); err != nil {
			return nil, fmt.Errorf("decode config %s: %w", tomlPath, err)
		}
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}

// ApplyRootConfig copies resolved values from cfg into the CLI flag pointer
// variables that main.go owns. Only flags that have a TOML equivalent are
// covered; pure-CLI flags (simulate, sim-count, test-verify, etc.) are left
// untouched. The duration pointers (min/max interval, tick timeout) receive
// parsed time.Duration values — a parse error is returned rather than
// fatal-exiting so the caller can report it cleanly.
//
// ApplyRootConfig unconditionally writes every mapped pointer; the caller is
// responsible for deciding whether the resolved value originated from TOML,
// env vars, or a CLI default (it cannot tell, and that is by design — once
// resolution has happened the source is moot for the running process). For
// --show-config, which needs source annotations, use the dedicated
// showConfigPath in cmd/schedulerd instead.
func ApplyRootConfig(cfg *RootConfig,
	dbPath, listen *string,
	minInterval, maxInterval, tickTimeout *time.Duration,
	numLevels, weightBudget, maxConcurrent *int,
	namespaceMode *bool,
	duckbrainNS, duckbrainURL, gatewayURL, gatewayKey, foremanHome *string,
) error {
	*dbPath = cfg.Daemon.DBPath
	*listen = cfg.Daemon.Listen

	minD, err := parseDurationErr(cfg.Scheduler.MinInterval, "scheduler.min_interval")
	if err != nil {
		return err
	}
	maxD, err := parseDurationErr(cfg.Scheduler.MaxInterval, "scheduler.max_interval")
	if err != nil {
		return err
	}
	tickD, err := parseDurationErr(cfg.Scheduler.TickTimeout, "scheduler.tick_timeout")
	if err != nil {
		return err
	}
	*minInterval = minD
	*maxInterval = maxD
	*tickTimeout = tickD

	*numLevels = cfg.Scheduler.NumLevels
	*weightBudget = cfg.Scheduler.WeightBudget
	*maxConcurrent = cfg.Scheduler.MaxConcurrent
	*namespaceMode = cfg.Scheduler.NamespaceMode

	*duckbrainNS = cfg.DuckBrain.Namespace
	*duckbrainURL = cfg.DuckBrain.URL
	*gatewayURL = cfg.Gateway.URL
	*gatewayKey = cfg.Gateway.Key
	*foremanHome = cfg.Gateway.ForemanHome
	return nil
}

// interpolateEnv replaces every ${VAR} placeholder in input with
// os.Getenv("VAR"). Unknown variables expand to the empty string.
func interpolateEnv(input string) string {
	return envVarPattern.ReplaceAllStringFunc(input, func(m string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(m, "${"), "}")
		return os.Getenv(name)
	})
}

// applyEnvOverrides applies SCHEDULER_* environment variables on top of
// the supplied RootConfig. Only non-empty env values override — an unset
// variable leaves the field untouched. This is layer 2, above TOML.
func applyEnvOverrides(cfg *RootConfig) {
	// Daemon.
	if v := os.Getenv("SCHEDULER_DB_PATH"); v != "" {
		cfg.Daemon.DBPath = v
	}
	if v := os.Getenv("SCHEDULER_LISTEN"); v != "" {
		cfg.Daemon.Listen = v
	}

	// Scheduler.
	if v := os.Getenv("SCHEDULER_MIN_INTERVAL"); v != "" {
		cfg.Scheduler.MinInterval = v
	}
	if v := os.Getenv("SCHEDULER_MAX_INTERVAL"); v != "" {
		cfg.Scheduler.MaxInterval = v
	}
	if v := os.Getenv("SCHEDULER_NUM_LEVELS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Scheduler.NumLevels = n
		}
	}
	if v := os.Getenv("SCHEDULER_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Scheduler.WeightBudget = n
		}
	}
	if v := os.Getenv("SCHEDULER_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Scheduler.MaxConcurrent = n
		}
	}
	if v := os.Getenv("SCHEDULER_TICK_TIMEOUT"); v != "" {
		cfg.Scheduler.TickTimeout = v
	}
	// Namespace mode is a bool: only "true" flips it on. This mirrors the
	// pre-FEAT-005 behavior in main.go (any value != "true" is a no-op).
	if v := os.Getenv("SCHEDULER_NAMESPACE_MODE"); v == "true" {
		cfg.Scheduler.NamespaceMode = true
	}

	// Gateway.
	if v := os.Getenv("SCHEDULER_GATEWAY_URL"); v != "" {
		cfg.Gateway.URL = v
	}
	if v := os.Getenv("SCHEDULER_GATEWAY_KEY"); v != "" {
		cfg.Gateway.Key = v
	}
	if v := os.Getenv("SCHEDULER_FOREMAN_HOME"); v != "" {
		cfg.Gateway.ForemanHome = v
	}

	// DuckBrain.
	if v := os.Getenv("SCHEDULER_DUCK_BRAIN_NS"); v != "" {
		cfg.DuckBrain.Namespace = v
	}
	if v := os.Getenv("SCHEDULER_DUCK_BRAIN_URL"); v != "" {
		cfg.DuckBrain.URL = v
	}
}

// Validate sanity-checks the resolved RootConfig. It verifies that
// duration strings parse, that the interval ladder and budget/concurrency
// bounds are sensible, and that the gateway URL (if set) looks like a URL.
// Returns an error aggregating all violations.
func (r *RootConfig) Validate() error {
	var errs []error

	minD, err := parseDurationErr(r.Scheduler.MinInterval, "scheduler.min_interval")
	if err != nil {
		errs = append(errs, err)
	}
	maxD, err := parseDurationErr(r.Scheduler.MaxInterval, "scheduler.max_interval")
	if err != nil {
		errs = append(errs, err)
	}
	if _, err := parseDurationErr(r.Scheduler.TickTimeout, "scheduler.tick_timeout"); err != nil {
		errs = append(errs, err)
	}
	if minD > 0 && maxD > 0 && minD > maxD {
		errs = append(errs, fmt.Errorf("scheduler.min_interval (%s) must be <= scheduler.max_interval (%s)",
			r.Scheduler.MinInterval, r.Scheduler.MaxInterval))
	}
	if r.Scheduler.NumLevels < 1 {
		errs = append(errs, fmt.Errorf("scheduler.num_levels must be >= 1, got %d", r.Scheduler.NumLevels))
	}
	if r.Scheduler.WeightBudget < 1 {
		errs = append(errs, fmt.Errorf("scheduler.weight_budget must be >= 1, got %d", r.Scheduler.WeightBudget))
	}
	if r.Scheduler.MaxConcurrent < 1 {
		errs = append(errs, fmt.Errorf("scheduler.max_concurrent must be >= 1, got %d", r.Scheduler.MaxConcurrent))
	}
	if r.Daemon.Listen == "" {
		errs = append(errs, errors.New("daemon.listen is required"))
	}
	if r.Daemon.DBPath == "" {
		errs = append(errs, errors.New("daemon.db_path is required"))
	}
	if r.Gateway.URL != "" && !strings.HasPrefix(r.Gateway.URL, "http://") && !strings.HasPrefix(r.Gateway.URL, "https://") {
		errs = append(errs, fmt.Errorf("gateway.url must start with http:// or https://, got %q", r.Gateway.URL))
	}
	if r.DuckBrain.URL != "" && !strings.HasPrefix(r.DuckBrain.URL, "http://") && !strings.HasPrefix(r.DuckBrain.URL, "https://") {
		errs = append(errs, fmt.Errorf("duckbrain.url must start with http:// or https://, got %q", r.DuckBrain.URL))
	}

	// Validate fleet declarations (same rules as LoadFleetConfig).
	for i, p := range r.Projects {
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("projects[%d]: name is required", i))
		}
	}
	for i, n := range r.Namespaces {
		if n.ID == "" {
			errs = append(errs, fmt.Errorf("namespaces[%d]: id is required", i))
		}
	}

	return errors.Join(errs...)
}

// parseDurationErr wraps time.ParseDuration with a field-name for error
// messages. An empty string returns a zero duration without error so
// callers can guard optional fields explicitly.
func parseDurationErr(s, field string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: parse duration %q: %w", field, s, err)
	}
	return d, nil
}

// LoadFleetConfig reads and decodes the TOML file at path into a FleetConfig.
// It validates that every project has a Name and every namespace has an ID,
// returning an error aggregating all violations so operators can fix the file
// in one pass.
func LoadFleetConfig(path string) (*FleetConfig, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	var cfg FleetConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	var errs []error
	for i, p := range cfg.Projects {
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("projects[%d]: name is required", i))
		}
	}
	for i, n := range cfg.Namespaces {
		if n.ID == "" {
			errs = append(errs, fmt.Errorf("namespaces[%d]: id is required", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyFleetConfig seeds the namespaces and projects defined in cfg into db.
// Rows that already exist are left untouched — this is a create-only upsert,
// never an overwrite, so operator-made tweaks survive restarts.
//
// Namespaces are applied first so that any project referencing one by id
// resolves cleanly.
func ApplyFleetConfig(ctx context.Context, db *sql.DB, cfg *FleetConfig) error {
	for _, nd := range cfg.Namespaces {
		if _, err := database.GetNamespace(ctx, db, nd.ID); err == nil {
			log.Printf("Config: namespace %q already exists, skipped", nd.ID)
			continue
		} else if !errors.Is(err, database.ErrNamespaceNotFound) {
			return fmt.Errorf("lookup namespace %q: %w", nd.ID, err)
		}
		ns := namespaceFromDef(nd)
		if err := database.CreateNamespace(ctx, db, ns); err != nil {
			return fmt.Errorf("create namespace %q: %w", nd.ID, err)
		}
		log.Printf("Config: imported namespace %q", nd.ID)
	}

	for _, pd := range cfg.Projects {
		if _, err := database.GetProject(ctx, db, pd.Name); err == nil {
			log.Printf("Config: project %q already exists, skipped", pd.Name)
			continue
		} else if !errors.Is(err, database.ErrProjectNotFound) {
			return fmt.Errorf("lookup project %q: %w", pd.Name, err)
		}
		p := projectFromDef(pd)
		if err := database.CreateProject(ctx, db, p); err != nil {
			return fmt.Errorf("create project %q: %w", pd.Name, err)
		}
		log.Printf("Config: imported project %q", pd.Name)
	}
	return nil
}

// projectFromDef materializes a *database.Project from a ProjectDef,
// substituting schema-matching defaults for zero-valued fields.
func projectFromDef(pd ProjectDef) *database.Project {
	weight := pd.Weight
	if weight <= 0 {
		weight = defaultProjectWeight
	}
	priority := pd.Priority
	if priority <= 0 {
		priority = defaultProjectPriority
	}
	cooldown := pd.CooldownS
	if cooldown <= 0 {
		cooldown = defaultProjectCooldown
	}
	decay := pd.DecayRate
	if decay <= 0 {
		decay = defaultProjectDecayRate
	}
	model := pd.Model
	if model == "" {
		model = defaultProjectModel
	}
	provider := pd.Provider
	if provider == "" {
		provider = defaultProjectProvider
	}
	enabled := true
	if pd.Enabled != nil {
		enabled = *pd.Enabled
	}

	p := &database.Project{
		Name:      pd.Name,
		RepoURL:   pd.RepoURL,
		Workdir:   pd.Workdir,
		Weight:    weight,
		Priority:  priority,
		CooldownS: cooldown,
		DecayRate: decay,
		Model:     model,
		Provider:  provider,
		Command:   pd.Command,
		Deliver:   pd.Deliver,
		Enabled:   enabled,
	}
	if pd.NamespaceID != "" {
		nsID := pd.NamespaceID
		p.NamespaceID = &nsID
	}
	return p
}

// namespaceFromDef materializes a *database.Namespace from a NamespaceDef,
// substituting schema-matching defaults for zero-valued fields.
func namespaceFromDef(nd NamespaceDef) *database.Namespace {
	weight := nd.Weight
	if weight <= 0 {
		weight = defaultNamespaceWeight
	}
	reserved := nd.Reserved
	if reserved <= 0 {
		reserved = defaultNamespaceReserved
	}
	hardCap := nd.HardCap
	if hardCap <= 0 {
		hardCap = defaultNamespaceHardCap
	}
	enabled := true
	if nd.Enabled != nil {
		enabled = *nd.Enabled
	}
	return &database.Namespace{
		ID:          nd.ID,
		Weight:      weight,
		Reserved:    reserved,
		HardCap:     hardCap,
		Enabled:     enabled,
		Description: nd.Description,
	}
}
