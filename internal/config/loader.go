package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Sensible defaults matching the projects/namespaces schema; applied when
// the corresponding TOML field is omitted or set to its zero value.
const (
	defaultProjectWeight    = 10
	defaultProjectPriority  = 5
	defaultProjectCooldown  = 7200 // 2h baseline (Bane 08-07 3-speed policy) — was 900 (hot default); unpinned projects must not run hot
	defaultProjectDecayRate = 1.0
	// Bane 2026-08-27 + 2026-08-31: a fleet.toml project without
	// model/provider stays EMPTY — the spawn chain resolves it (namespace
	// model_chain → global env → schema default). The SCHEMA default is
	// now the PAYG foreman key (deepseek-v4-flash/deepseek-foreman,
	// migrations.go v1), so the last chain hop is the foreman key — NEVER
	// the legacy 'your-model-name' placeholder (a present placeholder
	// shadowed the namespace tier and the gateway fell to the MAIN key).
	defaultProjectModel    = "" // unset → chain-resolved
	defaultProjectProvider = "" // unset → chain-resolved

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
	// SCHED-GAP-1575-B: heavy-read API deadline env override. No parse gate
	// here — validation happens in Validate(); an unset variable leaves the
	// empty string (the daemon flag default of 5s applies).
	if v := os.Getenv("SCHEDULER_API_READ_TIMEOUT"); v != "" {
		cfg.API.ReadTimeout = v
	}
	// SCHED-GAP-117: per-turn gateway deadline env override ("0s" = the
	// explicit disable and must survive the layering, so no parse gate
	// here — validation happens in Validate()).
	if v := os.Getenv("SCHEDULER_GATEWAY_RESPONSE_TIMEOUT"); v != "" {
		cfg.Scheduler.GatewayResponseTimeout = v
	}
	// ADV-R08/G3: slot-wait patience env override — no parse gate here,
	// validation happens in Validate().
	if v := os.Getenv("SCHEDULER_SLOT_PATIENCE"); v != "" {
		cfg.Scheduler.SlotPatience = v
	}
	// ADV-R11: per-spawn memory cap env override — no parse gate here,
	// validation happens in Validate().
	if v := os.Getenv("SCHEDULER_SPAWN_MEM_LIMIT_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Scheduler.SpawnMemLimitMB = n
		}
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
	// SCHED-GAP-117: the per-turn gateway deadline is optional — empty means
	// "not set" (the daemon default applies). When set it must parse and be
	// non-negative ("0s" is the explicit disable).
	if v := r.Scheduler.GatewayResponseTimeout; v != "" {
		if d, err := parseDurationErr(v, "scheduler.gateway_response_timeout"); err != nil {
			errs = append(errs, err)
		} else if d < 0 {
			errs = append(errs, fmt.Errorf("scheduler.gateway_response_timeout (%s) must be >= 0 (\"0s\" disables)", v))
		}
	}
	// ADV-R08/G3: the slot-wait patience is optional — empty means "not
	// set" (the 5m daemon default applies). When set it must parse and be
	// STRICTLY positive: the slot drop always exists (it is the pool's
	// backstop against unbounded queueing), and the flag layer treats
	// <= 0 as "keep default", so accepting a TOML "0s" here would be a
	// silent no-op rather than a real setting.
	if v := r.Scheduler.SlotPatience; v != "" {
		if d, err := parseDurationErr(v, "scheduler.slot_patience"); err != nil {
			errs = append(errs, err)
		} else if d <= 0 {
			errs = append(errs, fmt.Errorf("scheduler.slot_patience (%s) must be > 0 — the slot drop always exists (use a positive duration; unset means the 5m default)", v))
		}
	}
	// ADV-R11: the per-spawn memory cap is optional — 0 (or unset) means
	// off (byte-identical spawns, no prlimit call). Negative values are
	// invalid: there is no meaning for a negative memory cap, and silently
	// normalizing one to "off" would hide a config typo from the operator.
	if r.Scheduler.SpawnMemLimitMB < 0 {
		errs = append(errs, fmt.Errorf("scheduler.spawn_mem_limit_mb (%d) must be >= 0 (0 = off; unset = off)", r.Scheduler.SpawnMemLimitMB))
	}
	// SCHED-GAP-1575-B: the heavy-read API deadline is optional — empty means
	// "not set" (the 5s daemon flag default applies). When set it must parse
	// and be STRICTLY positive: the deadline is the whole point of the row,
	// and the flag layer treats <= 0 as "keep default", so accepting a TOML
	// "0s"/negative here would be a silent no-op rather than a real setting.
	if v := r.API.ReadTimeout; v != "" {
		if d, err := parseDurationErr(v, "api.read_timeout"); err != nil {
			errs = append(errs, err)
		} else if d <= 0 {
			errs = append(errs, fmt.Errorf("api.read_timeout (%s) must be > 0 — the heavy read surfaces need a deadline (unset means the 5s default)", v))
		}
	}
	// SCHED-GAP-1602: a blank/whitespace api.operator_token is REJECTED at
	// load time — the operator almost certainly meant to configure a
	// credential (a quoted empty string survives as ""), and silently
	// treating it as unset would put the daemon in the fail-closed 503 mode
	// with no explanation. Fail at load instead.
	if v := r.API.OperatorToken; strings.TrimSpace(v) == "" && v != "" {
		errs = append(errs, fmt.Errorf("api.operator_token is blank/whitespace — set a real token or remove the key entirely (mutations stay fail-closed 503 either way)"))
	}
	// SCHED-GAP-1602: basic mode needs BOTH halves. Exactly one set = a typo
	// the operator should hear about at load, not a daemon that refuses
	// every login with 401.
	userSet := strings.TrimSpace(r.API.OperatorUser) != ""
	passSet := strings.TrimSpace(r.API.OperatorPassword) != ""
	if userSet != passSet {
		errs = append(errs, fmt.Errorf("api.operator_user and api.operator_password must be set together (basic mode) — set both or neither"))
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
			continue
		}
		// S12 §4.3 (SCHED-GAP-111): same wave_tick_timeout contract as
		// LoadFleetConfig — a root TOML carrying fleet declarations gets
		// the identical field-named rejection.
		if err := validateWaveTickTimeout(n.ID, n.WaveTickTimeout); err != nil {
			errs = append(errs, err)
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

// WaveTickTimeoutCeiling is the hard upper bound on a namespace's
// wave_tick_timeout (S12 §4.3 item 2). Anything above it is a config error —
// never silently clamped — because the scheduler's reaper staleness windows
// and the fleet's operational expectation ("a stuck project is visible
// within a few hours") are calibrated below it. The recommended value for
// wave-enabled namespaces is 3h (1.5x the 2h base: workers run concurrently,
// only merges are serial — 3x would hand a serial tick budget it does not
// need). --tick-timeout itself is NOT bounded by this ceiling.
const WaveTickTimeoutCeiling = 4 * time.Hour

// validateWaveTickTimeout enforces the S12 §4.3 contract on one namespace
// definition's wave_tick_timeout: empty (inherit) is always fine; a non-empty
// value must parse AND stay <= 4h. Errors are field-named after the TOML
// path (namespaces[id].wave_tick_timeout) so an operator can find the line.
func validateWaveTickTimeout(nsID, raw string) error {
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("namespaces[%s].wave_tick_timeout: parse duration %q: %w", nsID, raw, err)
	}
	if d > WaveTickTimeoutCeiling {
		return fmt.Errorf("namespaces[%s].wave_tick_timeout: %s exceeds the 4h ceiling (S12 §4.3)", nsID, d)
	}
	return nil
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
			continue
		}
		// S12 §4.3 (SCHED-GAP-111): wave_tick_timeout must parse and stay
		// <= 4h — field-named rejection, never a silent clamp.
		if err := validateWaveTickTimeout(n.ID, n.WaveTickTimeout); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// LoadRootConfig reads and validates a TOML file into a RootConfig.
// The same file may also contain fleet declarations alongside scheduler
// config sections (blackout windows, etc.). Unlike LoadFleetConfig,
// this function does not validate project/namespace fields — call
// ApplyFleetConfig separately with AsFleet() for that.
func LoadRootConfig(path string) (*RootConfig, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	var cfg RootConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyFleetConfig seeds the namespaces and projects defined in cfg into db.
// Namespaces are create-only (existing rows are skipped); project entries
// CREATE a row only when the DB has none (SCHED-GAP-219: fleet.toml is a
// DECLARATIVE SEED, not a maintained mirror). For an EXISTING project the
// loader pins nothing by default — the database is the cooldown authority —
// except for the GatewayKey-style conditional fields where the file key's
// PRESENCE is the operator's signal (see the per-field blocks below).
//
// Cooldown pin import (SCHED-GAP-219): a fleet.toml entry that carries an
// explicit positive cooldown_s on an EXISTING project records that value as
// the row's operator pin (cooldown_pin_s, provenance fleet-toml-import)
// WITHOUT overwriting the live cooldown_s — an API change made after the
// toml was written must survive the restart. The pin is enforced, not the
// file value: nothing may lower the live cooldown below the pin.
//
// Bump interaction (SCHED-GAP-107): while a bump is active the bump owns
// cooldown_s; no pin import happens for that row until the bump expires.
//
// Namespaces are applied first so that any project referencing one by id
// resolves cleanly.
func ApplyFleetConfig(ctx context.Context, db *sql.DB, cfg *FleetConfig) error {
	for _, nd := range cfg.Namespaces {
		// S12 §4.3 (SCHED-GAP-111): the 4h ceiling guards the DB write too —
		// FleetConfigs can be built in code (tests, tooling) or from
		// already-loaded files, so the create path re-checks the wave
		// timeout instead of trusting the caller's validation.
		if err := validateWaveTickTimeout(nd.ID, nd.WaveTickTimeout); err != nil {
			return err
		}
		if _, err := database.GetNamespace(ctx, db, nd.ID); err == nil {
			// Bane 2026-08-27: an existing namespace gets its default_prompt
			// updated when the fleet.toml entry carries one (prompt config is
			// data, and the fleet.toml entry is authoritative for it).
			if nd.DefaultPrompt != "" {
				if err := database.UpdateNamespace(ctx, db, nd.ID, database.NamespacePatch{
					DefaultPrompt: &nd.DefaultPrompt,
				}); err != nil {
					return fmt.Errorf("update namespace %q default_prompt: %w", nd.ID, err)
				}
				log.Printf("Config: updated namespace %q default_prompt (%d chars)", nd.ID, len(nd.DefaultPrompt))
			}
			// SCHED-GAP-124: admission_mode pins when the fleet.toml entry
			// explicitly sets a valid value; invalid values warn and skip
			// (never crash boot over a typo).
			if nd.AdmissionMode != "" {
				switch nd.AdmissionMode {
				case database.AdmissionModeCooldown, database.AdmissionModeTasks:
					m := nd.AdmissionMode
					if err := database.UpdateNamespace(ctx, db, nd.ID, database.NamespacePatch{
						AdmissionMode: &m,
					}); err != nil {
						return fmt.Errorf("update namespace %q admission_mode: %w", nd.ID, err)
					}
					log.Printf("Config: pinned namespace %q admission_mode=%s", nd.ID, m)
				default:
					log.Printf("Config: namespace %q has invalid admission_mode %q — skipped (want \"cooldown\" or \"tasks\")", nd.ID, nd.AdmissionMode)
				}
			}
			// SCHED-GAP-125: load_gate pins when explicitly set ("off" is
			// the only non-empty value today); anything else warns and
			// skips. Empty key = keep the current DB value (no pin).
			if nd.LoadGate != "" {
				if nd.LoadGate == "off" {
					v := nd.LoadGate
					if err := database.UpdateNamespace(ctx, db, nd.ID, database.NamespacePatch{
						LoadGate: &v,
					}); err != nil {
						return fmt.Errorf("update namespace %q load_gate: %w", nd.ID, err)
					}
					log.Printf("Config: pinned namespace %q load_gate=%s", nd.ID, v)
				} else {
					log.Printf("Config: namespace %q has invalid load_gate %q — skipped (want \"off\")", nd.ID, nd.LoadGate)
				}
			}
			// SCHED-GAP-149: max_concurrent pins from fleet.toml like the
			// keys above. 0 is the "no key" input: NamespaceDef.MaxConcurrent
			// is a plain int, so an absent key and an explicit
			// `max_concurrent = 0` are indistinguishable here — and 0 means
			// "unlimited" at create time anyway. A 0 entry therefore leaves
			// the live DB cap alone, so a cap assigned through the API
			// survives a restart with a keyless entry (the
			// GatewayKey-conditional pattern at line 586).
			//
			// A negative value normalizes to 0 exactly as the create-time
			// guard in namespaceFromDef does, so the same fleet.toml entry
			// cannot land two different caps depending on whether the
			// namespace already existed. Writing the raw negative would
			// instead fail the column's CHECK(max_concurrent >= 0) and turn
			// an operator typo into a boot error.
			if nd.MaxConcurrent != 0 {
				v := nd.MaxConcurrent
				if v < 0 {
					v = 0
				}
				if err := database.UpdateNamespace(ctx, db, nd.ID, database.NamespacePatch{
					MaxConcurrent: &v,
				}); err != nil {
					return fmt.Errorf("update namespace %q max_concurrent: %w", nd.ID, err)
				}
				log.Printf("Config: pinned namespace %q max_concurrent=%d", nd.ID, v)
			}
			if nd.DefaultPrompt == "" && nd.AdmissionMode == "" && nd.LoadGate == "" && nd.MaxConcurrent == 0 {
				log.Printf("Config: namespace %q already exists, skipped", nd.ID)
			}
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
			// SCHED-GAP-219: the DB is the cooldown authority — an EXISTING
			// project is no longer re-pinned from fleet.toml for cooldown,
			// model, provider, or enabled. The file is a declarative seed:
			// it creates rows, it does not rewrite them. This retires the
			// SCHED-GAP-025/121 two-config law (a value that lived only in
			// the DB was drift); the DB is now the ONLY authority, and API
			// changes are durable across restarts.
			//
			// The GatewayKey-style conditional pins below SURVIVE: for those
			// fields the PRESENCE of the key in fleet.toml is itself the
			// operator's signal (a keyless entry never rewrites the row —
			// SCHED-GAP-064/065/066/124/141), so they never normalize an
			// API-assigned value away.
			existing, _ := database.GetProject(ctx, db, pd.Name)
			p := projectFromDef(pd)
			// Cooldown pin import (SCHED-GAP-219): an explicit positive
			// cooldown_s in fleet.toml records the operator pin on the row.
			// The pin NEVER overwrites the live cooldown_s — an API change
			// made after the toml was written must survive the restart —
			// and it never silently LOWERS an existing pin. Skipped while a
			// bump is active (the bump owns cooldown_s; SCHED-GAP-107).
			if pd.CooldownS > 0 && existing != nil && !existing.BumpActive {
				// Skip the import when an equal pin is already present — a
				// restart must not churn rows on every boot.
				if existing.CooldownPinS == nil || *existing.CooldownPinS != pd.CooldownS {
					// Never silently LOWER an operator pin: the pin moves
					// only when the toml value is equal-or-higher; a lower
					// toml value keeps the DB pin (the DB is the authority,
					// the file is the seed). An explicit clear via the API
					// is the sanctioned path to a smaller pin.
					if existing.CooldownPinS == nil || pd.CooldownS >= *existing.CooldownPinS {
						if err := database.SetCooldownPin(ctx, db, pd.Name, pd.CooldownS, database.CooldownPinImportBy); err != nil {
							return fmt.Errorf("import cooldown pin for %q: %w", pd.Name, err)
						}
						log.Printf("Config: imported cooldown pin %q = %ds (fleet-toml-import)", pd.Name, pd.CooldownS)
					} else {
						log.Printf("Config: fleet.toml cooldown %ds for %q is BELOW the operator pin %ds — pin kept (the DB is the cooldown authority)", pd.CooldownS, pd.Name, *existing.CooldownPinS)
					}
				}
			}
			updates := database.ProjectUpdates{}
			// GatewayKey is pinned ONLY when fleet.toml explicitly sets one —
			// a per-foreman key assigned via API must never be cleared by a
			// restart with a keyless fleet.toml entry.
			if pd.GatewayKey != "" {
				updates.GatewayKey = &pd.GatewayKey
			}
			// SCHED-GAP-064: fallback tiers pin the same way as GatewayKey —
			// only when fleet.toml explicitly sets them, so an API-assigned
			// fallback survives a restart with a keyless entry. The
			// no_global_fallback FLAG pins unconditionally (as before): the
			// plain-bool TOML key cannot distinguish absent from false, and
			// the file remains the durable switch for the flag.
			if pd.FallbackModel != "" {
				updates.FallbackModel = &pd.FallbackModel
			}
			if pd.FallbackProvider != "" {
				updates.FallbackProvider = &pd.FallbackProvider
			}
			updates.NoGlobalFallback = &pd.NoGlobalFallback
			// SCHED-GAP-065: idle tiers pin the same way as the fallback
			// tiers — only when fleet.toml explicitly sets them.
			if pd.IdleModel != "" {
				updates.IdleModel = &pd.IdleModel
			}
			if pd.IdleProvider != "" {
				updates.IdleProvider = &pd.IdleProvider
			}
			// SCHED-GAP-066: budget caps pin when the key is PRESENT in
			// fleet.toml (pointer non-nil) — including an explicit 0, which
			// clears an API-assigned cap back to unlimited. A keyless entry
			// leaves an API-assigned budget untouched (GatewayKey-style
			// conditional pin).
			if pd.DailyBudgetUSD != nil {
				updates.DailyBudgetUSD = pd.DailyBudgetUSD
			}
			if pd.WeeklyBudgetUSD != nil {
				updates.WeeklyBudgetUSD = pd.WeeklyBudgetUSD
			}
			if pd.FinalBudgetUSD != nil {
				updates.FinalBudgetUSD = pd.FinalBudgetUSD
			}
			// Bane 2026-08-27: per-project prompt text + mode pin when
			// explicitly set in fleet.toml (GatewayKey-style conditional).
			if pd.Prompt != "" {
				updates.Prompt = &pd.Prompt
			}
			if pd.PromptMode != "" {
				updates.PromptMode = &pd.PromptMode
			}
			// SCHED-GAP-124: admission_mode pins ONLY when fleet.toml
			// explicitly sets it — a mode flipped via API survives a
			// restart with a keyless entry (GatewayKey-style conditional).
			if pd.AdmissionMode != "" {
				updates.AdmissionMode = &pd.AdmissionMode
			}
			// SCHED-GAP-141: board_ownership pins the same way and for
			// the same reason: a keyless entry leaves an API-assigned
			// value untouched, so a DB-only flip survives a restart too.
			if pd.BoardOwnership != "" {
				updates.BoardOwnership = &pd.BoardOwnership
			}
			// SCHED-GAP-150: a retired dagger-era driver must never be
			// re-armed through the loader. The enabled re-pin is retired
			// (SCHED-GAP-219: the DB owns enabled), but the guard still
			// applies to the conditional-pin pass — a relic row gets no
			// pins refreshed and a loud log line instead.
			if existing != nil {
				if drv := retiredDriverInCommand(existing.Command); drv != "" {
					log.Printf("Config: NOT pinning project %q — its command drives the retired driver %s; clear `command` (DB and fleet.toml) before re-enabling", pd.Name, drv)
					updates = database.ProjectUpdates{}
				}
			}
			// Adaptive cooldown: the FLAG pins only when the key is
			// explicitly present (pointer non-nil) — an API-armed row
			// survives a restart with a keyless entry (GatewayKey-style,
			// SCHED-GAP-219). Policy numbers ride along only when the flag
			// is explicitly on; projectFromDef already resolved them to
			// effective values.
			if pd.AdaptiveCooldown != nil {
				updates.AdaptiveCooldown = pd.AdaptiveCooldown
				if *pd.AdaptiveCooldown {
					updates.CooldownFloorS = &p.CooldownFloorS
					updates.CooldownCeilingS = &p.CooldownCeilingS
					updates.NoProgressThreshold = &p.NoProgressThreshold
				}
			}
			// Apply the conditional pins only when the update set is
			// non-empty — an empty UpdateProject would still churn
			// updated_at on every boot for every row.
			if !updates.IsEmpty() {
				if err := database.UpdateProject(ctx, db, pd.Name, updates); err != nil {
					return fmt.Errorf("pin project %q from fleet.toml: %w", pd.Name, err)
				}
				log.Printf("Config: pinned conditional keys for project %q from fleet.toml", pd.Name)
			}
			continue
		} else if !errors.Is(err, database.ErrProjectNotFound) {
			return fmt.Errorf("lookup project %q: %w", pd.Name, err)
		}
		// CREATE path: the seed applies only where the DB has no row.
		p := projectFromDef(pd)
		if err := database.CreateProject(ctx, db, p); err != nil {
			return fmt.Errorf("create project %q: %w", pd.Name, err)
		}
		// Stamp the operator pin at creation (SCHED-GAP-219): the seeded
		// cooldown IS an operator-declared value, so it records as the pin
		// from the first boot (provenance fleet-toml-import).
		if pd.CooldownS > 0 {
			if err := database.SetCooldownPin(ctx, db, pd.Name, pd.CooldownS, database.CooldownPinImportBy); err != nil {
				return fmt.Errorf("import cooldown pin for %q: %w", pd.Name, err)
			}
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
	// Adaptive cooldown is OPT-IN: absent key = false (default off).
	adaptive := false
	if pd.AdaptiveCooldown != nil {
		adaptive = *pd.AdaptiveCooldown
	}

	p := &database.Project{
		Name:             pd.Name,
		RepoURL:          pd.RepoURL,
		Workdir:          pd.Workdir,
		Weight:           weight,
		Priority:         priority,
		CooldownS:        cooldown,
		DecayRate:        decay,
		Model:            model,
		Provider:         provider,
		FallbackModel:    pd.FallbackModel,
		FallbackProvider: pd.FallbackProvider,
		NoGlobalFallback: pd.NoGlobalFallback,
		ModelChain:       serializeModelChain(pd.ModelChain),
		IdleModel:        pd.IdleModel,
		IdleProvider:     pd.IdleProvider,
		GatewayKey:       pd.GatewayKey,
		Command:          pd.Command,
		Prompt:           pd.Prompt,
		PromptMode:       pd.PromptMode,
		Deliver:          pd.Deliver,
		Enabled:          enabled,
		AdaptiveCooldown: adaptive,
		AdmissionMode:    pd.AdmissionMode,
		BoardOwnership:   pd.BoardOwnership,
	}
	// Normalize the adaptive policy for enabled projects so the stored row
	// carries EFFECTIVE values: floor = the fleet cooldown, ceiling and
	// threshold = the built-in defaults when the file omits them.
	if adaptive {
		if pd.CooldownFloorS > 0 {
			p.CooldownFloorS = pd.CooldownFloorS
		} else {
			p.CooldownFloorS = cooldown
		}
		if pd.CooldownCeilingS > 0 {
			p.CooldownCeilingS = pd.CooldownCeilingS
		} else {
			// Derived default (ADV-R10): 8 × the resolved floor above
			// (explicit, else the fleet cooldown) — never a second
			// hardcoded ceiling constant.
			p.CooldownCeilingS = database.DefaultAdaptiveCooldownCeiling(p.CooldownFloorS)
		}
		if pd.NoProgressThreshold > 0 {
			p.NoProgressThreshold = pd.NoProgressThreshold
		} else {
			p.NoProgressThreshold = database.DefaultAdaptiveCooldownThreshold
		}
	}
	// SCHED-GAP-066: budget caps are opt-in — absent or <= 0 means unlimited,
	// which is also the schema default (0.0), so only positive values map.
	if pd.DailyBudgetUSD != nil && *pd.DailyBudgetUSD > 0 {
		p.DailyBudgetUSD = *pd.DailyBudgetUSD
	}
	if pd.WeeklyBudgetUSD != nil && *pd.WeeklyBudgetUSD > 0 {
		p.WeeklyBudgetUSD = *pd.WeeklyBudgetUSD
	}
	if pd.FinalBudgetUSD != nil && *pd.FinalBudgetUSD > 0 {
		p.FinalBudgetUSD = *pd.FinalBudgetUSD
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
	// max_concurrent: 0 (unset) = unlimited — the global --max-concurrent
	// cap still applies. Only an explicitly positive value limits the
	// number of ticks running simultaneously in this namespace.
	maxConcurrent := nd.MaxConcurrent
	if maxConcurrent < 0 {
		maxConcurrent = 0
	}
	enabled := true
	if nd.Enabled != nil {
		enabled = *nd.Enabled
	}
	// S12 wave config is default-off: an absent key means waves disabled,
	// timeout inherited, cap unlimited. Negative caps normalize to 0
	// (unlimited) so a typo'd TOML value cannot trip the CHECK constraint
	// at CREATE time.
	waveEnabled := false
	if nd.WaveEnabled != nil {
		waveEnabled = *nd.WaveEnabled
	}
	waveWorkersCap := nd.WaveWorkersCap
	if waveWorkersCap < 0 {
		waveWorkersCap = 0
	}
	return &database.Namespace{
		ID:              nd.ID,
		Weight:          weight,
		Reserved:        reserved,
		HardCap:         hardCap,
		MaxConcurrent:   maxConcurrent,
		Enabled:         enabled,
		Description:     nd.Description,
		DefaultPrompt:   nd.DefaultPrompt,
		ModelChain:      serializeModelChain(nd.ModelChain),
		WaveEnabled:     waveEnabled,
		WaveTickTimeout: nd.WaveTickTimeout,
		WaveWorkersCap:  waveWorkersCap,
		AdmissionMode:   nsAdmissionMode(nd),
	}
}

// nsAdmissionMode resolves the effective admission mode for a fleet.toml
// namespace entry: the explicit key when valid, "cooldown" otherwise
// (empty or unknown values normalize to the cron default — SCHED-GAP-124).
func nsAdmissionMode(nd NamespaceDef) string {
	switch nd.AdmissionMode {
	case database.AdmissionModeTasks:
		return database.AdmissionModeTasks
	default:
		return database.AdmissionModeCooldown
	}
}

// serializeModelChain converts a []string model_chain config value to a JSON
// string for storage in the database. Empty input yields "" (not "null" or "[]").
func serializeModelChain(chain []string) string {
	if len(chain) == 0 {
		return ""
	}
	b, err := json.Marshal(chain)
	if err != nil {
		return ""
	}
	return string(b)
}
