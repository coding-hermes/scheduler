package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withEnv sets env vars for the duration of the test and restores the
// prior values on cleanup. Nil values unset the variable.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	original := map[string]string{}
	for k, v := range vars {
		original[k] = os.Getenv(k)
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	t.Cleanup(func() {
		for k, v := range original {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})
}

// allSchedulerEnvVars is the full set of SCHEDULER_* env vars touched by
// applyEnvOverrides. Tests unset these up-front so the host environment
// cannot leak into the results.
var allSchedulerEnvVars = []string{
	"SCHEDULER_DB_PATH",
	"SCHEDULER_LISTEN",
	"SCHEDULER_MIN_INTERVAL",
	"SCHEDULER_MAX_INTERVAL",
	"SCHEDULER_NUM_LEVELS",
	"SCHEDULER_BUDGET",
	"SCHEDULER_MAX_CONCURRENT",
	"SCHEDULER_TICK_TIMEOUT",
	"SCHEDULER_NAMESPACE_MODE",
	"SCHEDULER_GATEWAY_URL",
	"SCHEDULER_GATEWAY_KEY",
	"SCHEDULER_FOREMAN_HOME",
	"SCHEDULER_DUCK_BRAIN_NS",
	"SCHEDULER_DUCK_BRAIN_URL",
}

// clearSchedulerEnv unsets every SCHEDULER_* env var for the test's
// lifetime. Required because the host environment is unpredictable.
func clearSchedulerEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, k := range allSchedulerEnvVars {
		saved[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})
}

func TestLoadConfigDefaults(t *testing.T) {
	clearSchedulerEnv(t)

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Daemon.DBPath != defaultDBPath {
		t.Errorf("daemon.db_path default: got %q, want %q", cfg.Daemon.DBPath, defaultDBPath)
	}
	if cfg.Daemon.Listen != defaultListen {
		t.Errorf("daemon.listen default: got %q, want %q", cfg.Daemon.Listen, defaultListen)
	}
	if cfg.Scheduler.MinInterval != defaultMinInterval {
		t.Errorf("scheduler.min_interval default: got %q, want %q", cfg.Scheduler.MinInterval, defaultMinInterval)
	}
	if cfg.Scheduler.MaxInterval != defaultMaxInterval {
		t.Errorf("scheduler.max_interval default: got %q, want %q", cfg.Scheduler.MaxInterval, defaultMaxInterval)
	}
	if cfg.Scheduler.NumLevels != defaultNumLevels {
		t.Errorf("scheduler.num_levels default: got %d, want %d", cfg.Scheduler.NumLevels, defaultNumLevels)
	}
	if cfg.Scheduler.WeightBudget != defaultWeightBudget {
		t.Errorf("scheduler.weight_budget default: got %d, want %d", cfg.Scheduler.WeightBudget, defaultWeightBudget)
	}
	if cfg.Scheduler.MaxConcurrent != defaultMaxConcurrent {
		t.Errorf("scheduler.max_concurrent default: got %d, want %d", cfg.Scheduler.MaxConcurrent, defaultMaxConcurrent)
	}
	if cfg.Scheduler.TickTimeout != defaultTickTimeout {
		t.Errorf("scheduler.tick_timeout default: got %q, want %q", cfg.Scheduler.TickTimeout, defaultTickTimeout)
	}
	if cfg.Scheduler.NamespaceMode != false {
		t.Errorf("scheduler.namespace_mode default: got true, want false")
	}
	if cfg.Gateway.URL != defaultGatewayURL {
		t.Errorf("gateway.url default: got %q, want %q", cfg.Gateway.URL, defaultGatewayURL)
	}
	if cfg.Gateway.ForemanHome != defaultForemanHome {
		t.Errorf("gateway.foreman_home default: got %q, want %q", cfg.Gateway.ForemanHome, defaultForemanHome)
	}
	if cfg.DuckBrain.Namespace != defaultDuckBrainNS {
		t.Errorf("duckbrain.namespace default: got %q, want %q", cfg.DuckBrain.Namespace, defaultDuckBrainNS)
	}
	if cfg.DuckBrain.URL != defaultDuckBrainURL {
		t.Errorf("duckbrain.url default: got %q, want %q", cfg.DuckBrain.URL, defaultDuckBrainURL)
	}
}

func TestLoadConfigTOMLOverridesDefaults(t *testing.T) {
	clearSchedulerEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `[daemon]
db_path = "/data/scheduler.db"
listen = "0.0.0.0:8080"

[scheduler]
min_interval = "10m"
max_interval = "12h"
num_levels = 8
weight_budget = 200
max_concurrent = 4
tick_timeout = "1h"
namespace_mode = true

[gateway]
url = "http://gw:8642"
key = "secret"
foreman_home = "/opt/hermes"

[duckbrain]
namespace = "test-ns"
url = "http://db:3000"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Daemon.DBPath != "/data/scheduler.db" {
		t.Errorf("daemon.db_path: got %q", cfg.Daemon.DBPath)
	}
	if cfg.Daemon.Listen != "0.0.0.0:8080" {
		t.Errorf("daemon.listen: got %q", cfg.Daemon.Listen)
	}
	if cfg.Scheduler.MinInterval != "10m" {
		t.Errorf("scheduler.min_interval: got %q", cfg.Scheduler.MinInterval)
	}
	if cfg.Scheduler.NumLevels != 8 {
		t.Errorf("scheduler.num_levels: got %d", cfg.Scheduler.NumLevels)
	}
	if cfg.Scheduler.WeightBudget != 200 {
		t.Errorf("scheduler.weight_budget: got %d", cfg.Scheduler.WeightBudget)
	}
	if cfg.Scheduler.MaxConcurrent != 4 {
		t.Errorf("scheduler.max_concurrent: got %d", cfg.Scheduler.MaxConcurrent)
	}
	if !cfg.Scheduler.NamespaceMode {
		t.Error("scheduler.namespace_mode: got false")
	}
	if cfg.Gateway.URL != "http://gw:8642" {
		t.Errorf("gateway.url: got %q", cfg.Gateway.URL)
	}
	if cfg.Gateway.Key != "secret" {
		t.Errorf("gateway.key: got %q", cfg.Gateway.Key)
	}
	if cfg.Gateway.ForemanHome != "/opt/hermes" {
		t.Errorf("gateway.foreman_home: got %q", cfg.Gateway.ForemanHome)
	}
	if cfg.DuckBrain.Namespace != "test-ns" {
		t.Errorf("duckbrain.namespace: got %q", cfg.DuckBrain.Namespace)
	}
	if cfg.DuckBrain.URL != "http://db:3000" {
		t.Errorf("duckbrain.url: got %q", cfg.DuckBrain.URL)
	}
}

func TestLoadConfigTOMLWithFleet(t *testing.T) {
	clearSchedulerEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `[daemon]
listen = "127.0.0.1:9090"

[[namespaces]]
id = "ns-a"

[[projects]]
name = "proj-a"
namespace_id = "ns-a"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0].ID != "ns-a" {
		t.Errorf("namespaces: %+v", cfg.Namespaces)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Name != "proj-a" {
		t.Errorf("projects: %+v", cfg.Projects)
	}
	// AsFleet should expose the same slices.
	fleet := cfg.AsFleet()
	if len(fleet.Projects) != 1 || fleet.Projects[0].Name != "proj-a" {
		t.Errorf("AsFleet projects: %+v", fleet.Projects)
	}
	if len(fleet.Namespaces) != 1 || fleet.Namespaces[0].ID != "ns-a" {
		t.Errorf("AsFleet namespaces: %+v", fleet.Namespaces)
	}
}

func TestLoadConfigEnvOverridesDefaults(t *testing.T) {
	clearSchedulerEnv(t)
	withEnv(t, map[string]string{
		"SCHEDULER_DB_PATH":        "/env/db.sqlite",
		"SCHEDULER_LISTEN":         "0.0.0.0:7777",
		"SCHEDULER_MIN_INTERVAL":   "5m",
		"SCHEDULER_MAX_INTERVAL":   "6h",
		"SCHEDULER_NUM_LEVELS":     "7",
		"SCHEDULER_BUDGET":         "50",
		"SCHEDULER_MAX_CONCURRENT": "3",
		"SCHEDULER_TICK_TIMEOUT":   "45m",
		"SCHEDULER_NAMESPACE_MODE": "true",
		"SCHEDULER_GATEWAY_URL":    "http://env-gw:8642",
		"SCHEDULER_GATEWAY_KEY":    "env-key",
		"SCHEDULER_FOREMAN_HOME":   "/env/foreman",
		"SCHEDULER_DUCK_BRAIN_NS":  "env-ns",
		"SCHEDULER_DUCK_BRAIN_URL": "http://env-db:3000",
	})

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Daemon.DBPath != "/env/db.sqlite" {
		t.Errorf("daemon.db_path: got %q", cfg.Daemon.DBPath)
	}
	if cfg.Daemon.Listen != "0.0.0.0:7777" {
		t.Errorf("daemon.listen: got %q", cfg.Daemon.Listen)
	}
	if cfg.Scheduler.MinInterval != "5m" {
		t.Errorf("scheduler.min_interval: got %q", cfg.Scheduler.MinInterval)
	}
	if cfg.Scheduler.MaxInterval != "6h" {
		t.Errorf("scheduler.max_interval: got %q", cfg.Scheduler.MaxInterval)
	}
	if cfg.Scheduler.NumLevels != 7 {
		t.Errorf("scheduler.num_levels: got %d", cfg.Scheduler.NumLevels)
	}
	if cfg.Scheduler.WeightBudget != 50 {
		t.Errorf("scheduler.weight_budget: got %d", cfg.Scheduler.WeightBudget)
	}
	if cfg.Scheduler.MaxConcurrent != 3 {
		t.Errorf("scheduler.max_concurrent: got %d", cfg.Scheduler.MaxConcurrent)
	}
	if cfg.Scheduler.TickTimeout != "45m" {
		t.Errorf("scheduler.tick_timeout: got %q", cfg.Scheduler.TickTimeout)
	}
	if !cfg.Scheduler.NamespaceMode {
		t.Error("scheduler.namespace_mode: got false")
	}
	if cfg.Gateway.URL != "http://env-gw:8642" {
		t.Errorf("gateway.url: got %q", cfg.Gateway.URL)
	}
	if cfg.Gateway.Key != "env-key" {
		t.Errorf("gateway.key: got %q", cfg.Gateway.Key)
	}
	if cfg.Gateway.ForemanHome != "/env/foreman" {
		t.Errorf("gateway.foreman_home: got %q", cfg.Gateway.ForemanHome)
	}
	if cfg.DuckBrain.Namespace != "env-ns" {
		t.Errorf("duckbrain.namespace: got %q", cfg.DuckBrain.Namespace)
	}
	if cfg.DuckBrain.URL != "http://env-db:3000" {
		t.Errorf("duckbrain.url: got %q", cfg.DuckBrain.URL)
	}
}

func TestLoadConfigEnvOverridesTOML(t *testing.T) {
	clearSchedulerEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `[scheduler]
weight_budget = 200
max_concurrent = 16
namespace_mode = false
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	withEnv(t, map[string]string{
		"SCHEDULER_BUDGET":         "50", // overrides TOML's 200
		"SCHEDULER_MAX_CONCURRENT": "4",  // overrides TOML's 16
	})

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.WeightBudget != 50 {
		t.Errorf("env should override TOML: weight_budget got %d", cfg.Scheduler.WeightBudget)
	}
	if cfg.Scheduler.MaxConcurrent != 4 {
		t.Errorf("env should override TOML: max_concurrent got %d", cfg.Scheduler.MaxConcurrent)
	}
}

func TestLoadConfigNamespaceModeEnv(t *testing.T) {
	clearSchedulerEnv(t)

	// Only "true" should flip the flag on. Anything else is a no-op.
	cases := []struct {
		val string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"1", false},
		{"", false},
		{"yes", false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			clearSchedulerEnv(t)
			if c.val != "" {
				withEnv(t, map[string]string{"SCHEDULER_NAMESPACE_MODE": c.val})
			}
			cfg, err := LoadConfig("")
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Scheduler.NamespaceMode != c.want {
				t.Errorf("namespace_mode=%q: got %v, want %v", c.val, cfg.Scheduler.NamespaceMode, c.want)
			}
		})
	}
}

func TestInterpolateEnv(t *testing.T) {
	clearSchedulerEnv(t)
	withEnv(t, map[string]string{
		"FOO":            "bar",
		"API_SERVER_KEY": "sk-xyz",
	})

	cases := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"${FOO}", "bar"},
		{"key = \"${API_SERVER_KEY}\"", "key = \"sk-xyz\""},
		{"multiple ${FOO} and ${API_SERVER_KEY}", "multiple bar and sk-xyz"},
		{"${UNKNOWN_VAR}", ""},                 // unknown → empty
		{"literal $$ not a var", "literal $$ not a var"},
		{"lowercase ${foo} not matched", "lowercase ${foo} not matched"}, // lowercase is not a var
		{"${A1_B2}", ""}, // valid pattern, unset
	}
	for _, c := range cases {
		got := interpolateEnv(c.in)
		if got != c.want {
			t.Errorf("interpolateEnv(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadConfigTOMLInterpolation(t *testing.T) {
	clearSchedulerEnv(t)
	withEnv(t, map[string]string{"API_SERVER_KEY": "sk-from-env"})

	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `[gateway]
key = "${API_SERVER_KEY}"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Gateway.Key != "sk-from-env" {
		t.Errorf("gateway.key interpolation: got %q, want %q", cfg.Gateway.Key, "sk-from-env")
	}
}

func TestLoadConfigValidateErrors(t *testing.T) {
	clearSchedulerEnv(t)

	cases := []struct {
		name    string
		mutate  func(*RootConfig)
		wantSub string
	}{
		{
			name:    "min > max",
			mutate:  func(r *RootConfig) { r.Scheduler.MinInterval = "10h"; r.Scheduler.MaxInterval = "1h" },
			wantSub: "min_interval",
		},
		{
			name:    "bad duration",
			mutate:  func(r *RootConfig) { r.Scheduler.TickTimeout = "not-a-duration" },
			wantSub: "tick_timeout",
		},
		{
			name:    "num_levels < 1",
			mutate:  func(r *RootConfig) { r.Scheduler.NumLevels = 0 },
			wantSub: "num_levels",
		},
		{
			name:    "weight_budget < 1",
			mutate:  func(r *RootConfig) { r.Scheduler.WeightBudget = 0 },
			wantSub: "weight_budget",
		},
		{
			name:    "max_concurrent < 1",
			mutate:  func(r *RootConfig) { r.Scheduler.MaxConcurrent = 0 },
			wantSub: "max_concurrent",
		},
		{
			name:    "bad gateway url",
			mutate:  func(r *RootConfig) { r.Gateway.URL = "ftp://nope" },
			wantSub: "gateway.url",
		},
		{
			name:    "empty listen",
			mutate:  func(r *RootConfig) { r.Daemon.Listen = "" },
			wantSub: "listen",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaultRootConfig()
			c.mutate(cfg)
			// Validate runs directly; we don't need to go through LoadConfig.
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("expected error containing %q, got %v", c.wantSub, err)
			}
		})
	}
}

func TestLoadConfigValidateFleet(t *testing.T) {
	clearSchedulerEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	// Project missing name, namespace missing id.
	content := `[[projects]]
workdir = "/x"

[[namespaces]]
description = "no id"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for missing project name and namespace id")
	}
	msg := err.Error()
	if !strings.Contains(msg, "name is required") {
		t.Errorf("error should mention project name: %v", err)
	}
	if !strings.Contains(msg, "id is required") {
		t.Errorf("error should mention namespace id: %v", err)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	clearSchedulerEnv(t)
	_, err := LoadConfig("/nonexistent/path/schedulerd.toml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfigTOMLDecodeError(t *testing.T) {
	clearSchedulerEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("not = valid = toml ="), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected decode error")
	}
}

func TestApplyRootConfig(t *testing.T) {
	clearSchedulerEnv(t)
	cfg := defaultRootConfig()
	cfg.Daemon.DBPath = "/from/cfg.db"
	cfg.Daemon.Listen = "1.2.3.4:99"
	cfg.Scheduler.MinInterval = "7m"
	cfg.Scheduler.MaxInterval = "11h"
	cfg.Scheduler.TickTimeout = "33m"
	cfg.Scheduler.NumLevels = 6
	cfg.Scheduler.WeightBudget = 77
	cfg.Scheduler.MaxConcurrent = 5
	cfg.Scheduler.NamespaceMode = true
	cfg.DuckBrain.Namespace = "ns-cfg"
	cfg.DuckBrain.URL = "http://db-cfg:3000"
	cfg.Gateway.URL = "http://gw-cfg:8642"
	cfg.Gateway.Key = "key-cfg"
	cfg.Gateway.ForemanHome = "/from/cfg/foreman"

	var (
		dbPath, listen                         string
		minInt, maxInt, tickT                  time.Duration
		numLevels, budget, maxConc             int
		nsMode                                 bool
		dbNS, dbURL, gwURL, gwKey, foremanHome string
	)
	if err := ApplyRootConfig(cfg, &dbPath, &listen, &minInt, &maxInt, &tickT,
		&numLevels, &budget, &maxConc, &nsMode,
		&dbNS, &dbURL, &gwURL, &gwKey, &foremanHome); err != nil {
		t.Fatalf("ApplyRootConfig: %v", err)
	}
	if dbPath != "/from/cfg.db" {
		t.Errorf("dbPath: %q", dbPath)
	}
	if listen != "1.2.3.4:99" {
		t.Errorf("listen: %q", listen)
	}
	if minInt != 7*time.Minute {
		t.Errorf("minInterval: %v", minInt)
	}
	if maxInt != 11*time.Hour {
		t.Errorf("maxInterval: %v", maxInt)
	}
	if tickT != 33*time.Minute {
		t.Errorf("tickTimeout: %v", tickT)
	}
	if numLevels != 6 || budget != 77 || maxConc != 5 {
		t.Errorf("ints: %d/%d/%d", numLevels, budget, maxConc)
	}
	if !nsMode {
		t.Error("namespaceMode should be true")
	}
	if dbNS != "ns-cfg" || dbURL != "http://db-cfg:3000" {
		t.Errorf("duckbrain: %q / %q", dbNS, dbURL)
	}
	if gwURL != "http://gw-cfg:8642" || gwKey != "key-cfg" || foremanHome != "/from/cfg/foreman" {
		t.Errorf("gateway: %q / %q / %q", gwURL, gwKey, foremanHome)
	}
}

func TestApplyRootConfigBadDuration(t *testing.T) {
	clearSchedulerEnv(t)
	cfg := defaultRootConfig()
	cfg.Scheduler.MinInterval = "not-a-duration"
	err := ApplyRootConfig(cfg,
		new(string), new(string),
		new(time.Duration), new(time.Duration), new(time.Duration),
		new(int), new(int), new(int), new(bool),
		new(string), new(string), new(string), new(string), new(string))
	if err == nil {
		t.Fatal("expected error for bad min_interval")
	}
	if !strings.Contains(err.Error(), "min_interval") {
		t.Errorf("error should mention min_interval: %v", err)
	}
}
