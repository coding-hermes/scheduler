package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPrintSchema(t *testing.T) {
	out := captureStdout(printSchema)
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(out), &schema); err != nil {
		t.Fatalf("printSchema() did not emit valid JSON: %v\n%s", err, out)
	}
	if schema["$schema"] == nil {
		t.Error("schema missing $schema")
	}
	if schema["title"] == nil {
		t.Error("schema missing title")
	}
	// SCHED-GAP-165 (closing row): FEAT-005 has LANDED — main.go applies the
	// root schedulerd.toml at boot under the default-guard pattern, so the
	// description must DECLARE the loaded resolution chain
	// (TOML [scheduler] < env vars < CLI flags) instead of denying it. The
	// pre-FEAT-005 "NOT yet loaded by the daemon" wording is now a
	// surface-parity defect.
	desc, ok := schema["description"].(string)
	if !ok {
		t.Fatal("schema missing description")
	}
	if strings.Contains(desc, "NOT yet loaded by the daemon") {
		t.Errorf("schema description still denies the root TOML layer is loaded: %q", desc)
	}
	if !strings.Contains(desc, "FEAT-005") || !strings.Contains(desc, "TOML [scheduler]") {
		t.Errorf("schema description must declare the root TOML layer loaded (FEAT-005 landed): %q", desc)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties object")
	}
	for _, key := range []string{"daemon", "scheduler", "gateway", "duckbrain", "projects", "namespaces"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema missing property section: %s", key)
		}
	}

	// Defaults must match the flag definitions in main.go (SCHED-GAP-003).
	schedProps := props["scheduler"].(map[string]interface{})["properties"].(map[string]interface{})
	minInterval := schedProps["min_interval"].(map[string]interface{})
	if minInterval["default"] != "30s" {
		t.Errorf("min_interval default = %v, want \"30s\" (main.go flag default)", minInterval["default"])
	}
	maxConcurrent := schedProps["max_concurrent"].(map[string]interface{})
	if maxConcurrent["default"] != float64(10) {
		t.Errorf("max_concurrent default = %v, want 10 (main.go flag default)", maxConcurrent["default"])
	}
	// SCHED-GAP-117: the per-turn gateway deadline must exist in the schema
	// and match the main.go flag default (30m prints as "30m0s").
	gwrt := schedProps["gateway_response_timeout"].(map[string]interface{})
	if gwrt["default"] != "30m0s" {
		t.Errorf("gateway_response_timeout default = %v, want \"30m0s\" (main.go flag default)", gwrt["default"])
	}
	if gwrt["env"] != "SCHEDULER_GATEWAY_RESPONSE_TIMEOUT" {
		t.Errorf("gateway_response_timeout env = %v, want SCHEDULER_GATEWAY_RESPONSE_TIMEOUT", gwrt["env"])
	}

	// ADV-R08/G3: the slot-wait patience must exist in the schema and
	// match the main.go flag default (5m prints as "5m0s").
	sp, ok := schedProps["slot_patience"].(map[string]interface{})
	if !ok {
		t.Error("scheduler section missing slot_patience property")
	} else {
		if sp["default"] != "5m0s" {
			t.Errorf("slot_patience default = %v, want \"5m0s\" (main.go flag default)", sp["default"])
		}
		if sp["env"] != "SCHEDULER_SLOT_PATIENCE" {
			t.Errorf("slot_patience env = %v, want SCHEDULER_SLOT_PATIENCE", sp["env"])
		}
		if sp["cli"] != "--slot-patience" {
			t.Errorf("slot_patience cli = %v, want --slot-patience", sp["cli"])
		}
	}

	// ADV-R11: the per-spawn memory cap must exist in the schema, match
	// the main.go flag default (0 = off), and carry its env/cli mapping.
	sml, ok := schedProps["spawn_mem_limit_mb"].(map[string]interface{})
	if !ok {
		t.Error("scheduler section missing spawn_mem_limit_mb property")
	} else {
		if sml["default"] != float64(0) {
			t.Errorf("spawn_mem_limit_mb default = %v, want 0 (off; main.go flag default)", sml["default"])
		}
		if sml["env"] != "SCHEDULER_SPAWN_MEM_LIMIT_MB" {
			t.Errorf("spawn_mem_limit_mb env = %v, want SCHEDULER_SPAWN_MEM_LIMIT_MB", sml["env"])
		}
		if sml["cli"] != "--spawn-mem-limit-mb" {
			t.Errorf("spawn_mem_limit_mb cli = %v, want --spawn-mem-limit-mb", sml["cli"])
		}
	}

	// SCHED-GAP-168: the load gate threshold and model rates file must
	// exist in the schema with accurate types/defaults and their env/cli
	// mappings — these are the keys an operator needs exactly when spawns
	// are being load-deferred, so a schema drop-out re-breaks the
	// introspection surface this row closes.
	lgt, ok := schedProps["load_gate_threshold"].(map[string]interface{})
	if !ok {
		t.Error("scheduler section missing load_gate_threshold property")
	} else {
		if lgt["type"] != "number" {
			t.Errorf("load_gate_threshold type = %v, want number", lgt["type"])
		}
		if lgt["default"] != float64(0) {
			t.Errorf("load_gate_threshold default = %v, want 0 (off; main.go flag default)", lgt["default"])
		}
		if lgt["env"] != "SCHEDULER_LOAD_GATE_THRESHOLD" {
			t.Errorf("load_gate_threshold env = %v, want SCHEDULER_LOAD_GATE_THRESHOLD", lgt["env"])
		}
		if lgt["cli"] != "--load-gate-threshold" {
			t.Errorf("load_gate_threshold cli = %v, want --load-gate-threshold", lgt["cli"])
		}
	}
	mrf, ok := schedProps["model_rates_file"].(map[string]interface{})
	if !ok {
		t.Error("scheduler section missing model_rates_file property")
	} else {
		if mrf["type"] != "string" {
			t.Errorf("model_rates_file type = %v, want string", mrf["type"])
		}
		if mrf["default"] != "" {
			t.Errorf("model_rates_file default = %v, want \"\" (builtin rates only)", mrf["default"])
		}
		if mrf["env"] != "SCHEDULER_MODEL_RATES_FILE" {
			t.Errorf("model_rates_file env = %v, want SCHEDULER_MODEL_RATES_FILE", mrf["env"])
		}
		if mrf["cli"] != "--model-rates-file" {
			t.Errorf("model_rates_file cli = %v, want --model-rates-file", mrf["cli"])
		}
	}

	gwProps := props["gateway"].(map[string]interface{})["properties"].(map[string]interface{})
	noExec, ok := gwProps["no_exec_fallback"].(map[string]interface{})
	if !ok {
		t.Error("gateway section missing no_exec_fallback property")
	} else if noExec["default"] != true {
		t.Errorf("no_exec_fallback default = %v, want true (main.go flag default)", noExec["default"])
	}

	// Project cooldown default must match what projectFromDef() applies in
	// internal/config/loader.go (SCHED-GAP-033): 7200 (2h baseline, 3-speed
	// policy), NOT the legacy hot default of 900.
	projProps := props["projects"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	cooldown, ok := projProps["cooldown_s"].(map[string]interface{})
	if !ok {
		t.Error("projects.items section missing cooldown_s property")
	} else if cooldown["default"] != float64(7200) {
		t.Errorf("cooldown_s default = %v, want 7200 (loader defaultProjectCooldown)", cooldown["default"])
	}
}

func TestPrintConfig(t *testing.T) {
	os.Setenv("SCHEDULER_DB_PATH", "testdb")
	defer os.Unsetenv("SCHEDULER_DB_PATH")
	// DOGFOOD-012: an env override must surface as an EFFECTIVE value in the
	// output (main.go resolves SCHEDULER_* overrides before calling printConfig).
	os.Setenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE", "0.5")
	defer os.Unsetenv("SCHEDULER_AUTO_DISABLE_FAILURE_RATE")
	// ADV-R08/G3: the slot-patience env var must be listed among the
	// active overrides.
	os.Setenv("SCHEDULER_SLOT_PATIENCE", "90s")
	defer os.Unsetenv("SCHEDULER_SLOT_PATIENCE")
	// ADV-R11: the spawn mem limit env var must be listed among the
	// active overrides too.
	os.Setenv("SCHEDULER_SPAWN_MEM_LIMIT_MB", "512")
	defer os.Unsetenv("SCHEDULER_SPAWN_MEM_LIMIT_MB")

	out := captureStdout(func() {
		printConfig(
			"/tmp/fleet.toml",
			"/tmp/test.db",
			"127.0.0.1:9090",
			"/tmp/scheduler.log",
			20*60*1000000000,
			24*60*60*1000000000,
			10, 100, 10,
			false,
			2*60*60*1000000000,
			30*60*1000000000,
			5*60*1000000000,
			60*1000000000,
			"http://127.0.0.1:8642",
			"secret",
			"/tmp/foreman",
			true,
			"scheduler",
			"http://localhost:3000",
			0.5,
			100, 50, 100,
			512,
			12.0,
			"/tmp/rates.json",
		)
	})

	checks := []string{
		"db_path = \"/tmp/test.db\"",
		"listen = \"127.0.0.1:9090\"",
		"log_file = \"/tmp/scheduler.log\"",
		"[scheduler]",
		"min_interval = \"20m0s\"",
		"max_interval = \"24h0m0s\"",
		"num_levels = 10",
		"weight_budget = 100",
		"max_concurrent = 10",
		"tick_timeout = \"2h0m0s\"",
		// SCHED-GAP-117: the per-turn gateway deadline must surface in
		// --show-config output.
		"gateway_response_timeout = \"30m0s\"",
		// ADV-R08/G3: the slot-wait patience must surface in
		// --show-config output.
		"slot_patience = \"5m0s\"",
		// SCHED-GAP-136: tasks-mode post-tick pacing surfaces too.
		"tasks_pacing = \"1m0s\"",
		// ADV-R11: the per-spawn memory cap must surface in --show-config
		// output (the value passed at the call site).
		"spawn_mem_limit_mb = 512",
		// SCHED-GAP-168: the load gate and model rates file must surface in
		// --show-config output (the RESOLVED values passed at the call site —
		// the same variables that feed /api/v1/config in main.go).
		"load_gate_threshold = 12",
		"model_rates_file = \"/tmp/rates.json\"",
		"namespace_mode = false",
		// SCHEDULER_AUTO_DISABLE_FAILURE_RATE=0.5 resolved into the printed
		// effective value (was previously invisible to --show-config).
		"auto_disable_failure_rate = 0.5",
		"auto_disable_window = 100",
		"auto_disable_min_ticks = 50",
		"failure_window = 100",
		"[gateway]",
		"url = \"http://127.0.0.1:8642\"",
		"key = \"secret\"",
		"foreman_home = \"/tmp/foreman\"",
		"no_exec_fallback = true",
		"[duckbrain]",
		// SCHED-GAP-167: the namespace default tracks the flag default
		// (main.go --duckbrain-ns), NOT the retired "coding-hermes" value.
		"namespace = \"scheduler\"",
		"url = \"http://localhost:3000\"",
		"# fleet config file: /tmp/fleet.toml",
		"# active env var overrides:",
		"#   SCHEDULER_DB_PATH=testdb",
		"#   SCHEDULER_AUTO_DISABLE_FAILURE_RATE=0.5",
		"#   SCHEDULER_SLOT_PATIENCE=90s",
		"#   SCHEDULER_SPAWN_MEM_LIMIT_MB=512",
	}
	for _, substr := range checks {
		if !strings.Contains(out, substr) {
			t.Errorf("printConfig() output missing %q\nGot:\n%s", substr, out)
		}
	}

	// Header honesty (DOGFOOD-012, SCHED-GAP-165): the header must not claim
	// "CLI flags only" (env overrides exist) and must not claim "effective
	// values" — the FEAT-005 root TOML layer resolves LATER in boot, after
	// this command's early exit, so TOML-applied values are NOT shown here.
	// The header must disclose that gap explicitly. Measured at 5225bbae: a
	// /tmp TOML probe (weight_budget/max_concurrent/auto_disable_failure_rate/
	// slot_patience set) printed defaults for all four, proving the surface
	// cannot show the TOML layer — the wording must admit it, not overclaim.
	if strings.Contains(out, "CLI flags only") {
		t.Errorf("printConfig() header still claims 'CLI flags only'\nGot:\n%s", out)
	}
	if strings.Contains(out, "root TOML loading comes in FEAT-005") {
		t.Errorf("printConfig() header still defers the root TOML layer to FEAT-005\nGot:\n%s", out)
	}
	if strings.Contains(out, "effective values") {
		t.Errorf("printConfig() header must not claim 'effective values' — the TOML layer is not reflected in this output\nGot:\n%s", out)
	}
	if !strings.Contains(out, "FEAT-005") ||
		!strings.Contains(out, "TOML [scheduler]") ||
		!strings.Contains(out, "NOT reflected") {
		t.Errorf("printConfig() header must disclose FEAT-005 TOML [scheduler] resolves later in boot and is NOT reflected in this output\nGot:\n%s", out)
	}
}

// SCHED-GAP-168 — the load gate threshold and model rates file are published
// by /api/v1/config (main.go feeds the resolved *loadGateThreshold and
// *modelRatesFile into the config payload), so the two introspection surfaces
// operators use instead — --show-config and --schema — must carry BOTH keys
// too. The two checks below are written as independent failures so a
// drop-out on exactly one surface names that surface; the pair mirrors the
// defect shape this row closes (daemon published it, both CLI surfaces
// omitted it).
func TestLoadGateAndModelRatesOnBothIntrospectionSurfaces(t *testing.T) {
	t.Run("printSchema --schema JSON", func(t *testing.T) {
		var schema struct {
			Properties struct {
				Scheduler struct {
					Properties struct {
						LoadGateThreshold *struct {
							Type    string `json:"type"`
							Default any    `json:"default"`
							Env     string `json:"env"`
							CLI     string `json:"cli"`
						} `json:"load_gate_threshold"`
						ModelRatesFile *struct {
							Type    string `json:"type"`
							Default any    `json:"default"`
							Env     string `json:"env"`
							CLI     string `json:"cli"`
						} `json:"model_rates_file"`
					} `json:"properties"`
				} `json:"scheduler"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(captureStdout(printSchema)), &schema); err != nil {
			t.Fatalf("printSchema() did not emit valid JSON: %v", err)
		}
		if schema.Properties.Scheduler.Properties.LoadGateThreshold == nil {
			t.Fatal("--schema is missing properties.scheduler.properties.load_gate_threshold — the load-gate drop-out this row closed has regressed")
		}
		if schema.Properties.Scheduler.Properties.ModelRatesFile == nil {
			t.Fatal("--schema is missing properties.scheduler.properties.model_rates_file — the model-rates drop-out this row closed has regressed")
		}
		if schema.Properties.Scheduler.Properties.LoadGateThreshold.Env != "SCHEDULER_LOAD_GATE_THRESHOLD" {
			t.Errorf("load_gate_threshold env = %q, want SCHEDULER_LOAD_GATE_THRESHOLD", schema.Properties.Scheduler.Properties.LoadGateThreshold.Env)
		}
		if schema.Properties.Scheduler.Properties.ModelRatesFile.Env != "SCHEDULER_MODEL_RATES_FILE" {
			t.Errorf("model_rates_file env = %q, want SCHEDULER_MODEL_RATES_FILE", schema.Properties.Scheduler.Properties.ModelRatesFile.Env)
		}
	})

	t.Run("printConfig --show-config TOML", func(t *testing.T) {
		out := captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-168.db",
				"127.0.0.1:9090",
				"",
				30*time.Second,
				24*time.Hour,
				10, 100, 10,
				false,
				2*time.Hour, 30*time.Minute, 5*time.Minute, time.Minute,
				"http://127.0.0.1:8642", "secret", "/tmp/foreman",
				true,
				"scheduler", "http://localhost:3000",
				0,
				100, 50, 100,
				0,
				12.0,
				"/tmp/rates.json",
			)
		})
		if got := tomlSectionValue(t, out, "scheduler", "load_gate_threshold"); got != "12" {
			t.Errorf("[scheduler] load_gate_threshold in --show-config = %q, want \"12\" (the argument)", got)
		}
		if got := tomlSectionValue(t, out, "scheduler", "model_rates_file"); got != "/tmp/rates.json" {
			t.Errorf("[scheduler] model_rates_file in --show-config = %q, want \"/tmp/rates.json\" (the argument)", got)
		}
	})

	t.Run("printConfig TOML is argument-driven for both keys", func(t *testing.T) {
		// Sentinels: prove the printed values are the ARGUMENTS and not
		// literals baked into printConfig's format string.
		out := captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-168.db",
				"127.0.0.1:9090",
				"",
				30*time.Second,
				24*time.Hour,
				10, 100, 10,
				false,
				2*time.Hour, 30*time.Minute, 5*time.Minute, time.Minute,
				"http://127.0.0.1:8642", "secret", "/tmp/foreman",
				true,
				"scheduler", "http://localhost:3000",
				0,
				100, 50, 100,
				0,
				7.5,
				"/tmp/sentinel-rates.json",
			)
		})
		if got := tomlSectionValue(t, out, "scheduler", "load_gate_threshold"); got != "7.5" {
			t.Errorf("[scheduler] load_gate_threshold = %q, want the argument 7.5 — printConfig must not hardcode it", got)
		}
		if got := tomlSectionValue(t, out, "scheduler", "model_rates_file"); got != "/tmp/sentinel-rates.json" {
			t.Errorf("[scheduler] model_rates_file = %q, want the argument — printConfig must not hardcode it", got)
		}
	})
}

// SCHED-GAP-167 — the --duckbrain-ns default is restated on several surfaces,
// and five of them had drifted to the retired "coding-hermes" value while the
// flag declaration and the live daemon both report "scheduler" (Bane
// 2026-08-27: DuckBrain sync consolidated under the scheduler namespace). The
// flag declaration in main.go is the single source of truth; every other
// surface is read independently and must agree with it. A surface that can no
// longer be LOCATED fails too — a deleted row or a relocated example is drift,
// not a pass.
func TestDuckBrainNSDefaultMatchesFlag(t *testing.T) {
	// The live value, pinned so a deliberate rename of the flag default has to
	// land together with every surface instead of sliding past this test.
	const want = "scheduler"

	flagDefault := duckbrainNSFlagDefaultFromSource(t, "main.go")

	// One emitter for both printConfig subtests so they cannot drift apart.
	emitConfig := func(t *testing.T, ns string) string {
		t.Helper()
		return captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-167.db",
				"127.0.0.1:9090",
				"",
				30*time.Second,
				24*time.Hour,
				10, 100, 10,
				false,
				2*time.Hour, 30*time.Minute, 5*time.Minute, time.Minute,
				"http://127.0.0.1:8642", "secret", "/tmp/foreman",
				true,
				ns, "http://localhost:3000",
				0,
				100, 50, 100,
				0,
				0,
				"",
			)
		})
	}

	t.Run("main.go flag declaration", func(t *testing.T) {
		if flagDefault != want {
			t.Errorf("main.go flag.String(\"duckbrain-ns\", ...) default = %q, want %q", flagDefault, want)
		}
	})

	t.Run("printSchema --schema JSON", func(t *testing.T) {
		var schema struct {
			Properties struct {
				DuckBrain struct {
					Properties struct {
						Namespace struct {
							Default string `json:"default"`
						} `json:"namespace"`
					} `json:"properties"`
				} `json:"duckbrain"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(captureStdout(printSchema)), &schema); err != nil {
			t.Fatalf("printSchema() did not emit valid JSON: %v", err)
		}
		got := schema.Properties.DuckBrain.Properties.Namespace.Default
		if got == "" {
			t.Fatal("printSchema() no longer declares properties.duckbrain.properties.namespace.default — re-anchor this test")
		}
		if got != flagDefault {
			t.Errorf("--schema default for duckbrain.namespace = %q, want %q (main.go --duckbrain-ns)", got, flagDefault)
		}
	})

	t.Run("printConfig --show-config TOML", func(t *testing.T) {
		got := tomlSectionValue(t, emitConfig(t, flagDefault), "duckbrain", "namespace")
		if got != flagDefault {
			t.Errorf("[duckbrain] namespace in --show-config = %q, want %q (main.go --duckbrain-ns)", got, flagDefault)
		}
	})

	t.Run("printConfig TOML is argument-driven", func(t *testing.T) {
		// Sentinel: proves the printed namespace is the ARGUMENT and not a
		// literal baked into printConfig's format string — the exact shape of
		// the drift this row closes (the TOML example claimed coding-hermes).
		const sentinel = "sched-gap-167-sentinel"
		got := tomlSectionValue(t, emitConfig(t, sentinel), "duckbrain", "namespace")
		if got != sentinel {
			t.Errorf("[duckbrain] namespace = %q, want the argument %q — printConfig must not hardcode it", got, sentinel)
		}
	})

	t.Run("README.md flags table", func(t *testing.T) {
		checkMarkdownFlagDefault(t, filepath.Join("..", "..", "README.md"), "duckbrain-ns", flagDefault)
	})

	t.Run("docs/reference/flags.md flags table", func(t *testing.T) {
		checkMarkdownFlagDefault(t, filepath.Join("..", "..", "docs", "reference", "flags.md"), "duckbrain-ns", flagDefault)
	})

	t.Run("docs/api.md /api/v1/config example", func(t *testing.T) {
		got := docsAPIExampleNamespace(t, filepath.Join("..", "..", "docs", "api.md"))
		if got != flagDefault {
			t.Errorf("docs/api.md /api/v1/config example duckbrain.namespace = %q, want %q (main.go --duckbrain-ns)", got, flagDefault)
		}
	})
}

// duckbrainNSFlagDefaultFromSource extracts the --duckbrain-ns default from the
// flag declaration in main.go — per docs/reference/flags.md the flag
// declarations there are the canonical source of defaults. Reading the source
// keeps this test from hardcoding the value it polices; a reformatted
// declaration fails loudly instead of silently comparing nothing.
func duckbrainNSFlagDefaultFromSource(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`flag\.String\(\s*"duckbrain-ns"\s*,\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer declares flag.String(\"duckbrain-ns\", \"<default>\", ...) as a single literal; re-anchor TestDuckBrainNSDefaultMatchesFlag", path)
	}
	return string(m[1])
}

// checkMarkdownFlagDefault asserts the default column of a
// `| \`--flag\` | \`value\` |` row. Both spellings are accepted: README.md uses
// `-duckbrain-ns`, docs/reference/flags.md uses `--duckbrain-ns`.
func checkMarkdownFlagDefault(t *testing.T, path, flag, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	re := regexp.MustCompile("(?m)^\\|\\s*`--?" + regexp.QuoteMeta(flag) + "`\\s*\\|\\s*`([^`]+)`")
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no flags-table row for `%s`; the documented surface vanished — re-anchor this test", path, flag)
	}
	if got := string(m[1]); got != want {
		t.Errorf("%s: `%s` default = %q, want %q (main.go --duckbrain-ns)", path, flag, got, want)
	}
}

// tomlSectionValue returns the string value of key under [section] in the TOML
// emitted by printConfig.
func tomlSectionValue(t *testing.T, out, section, key string) string {
	t.Helper()
	in := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			in = line == "["+section+"]"
			continue
		}
		if !in {
			continue
		}
		if v, ok := strings.CutPrefix(line, key+" = "); ok {
			return strings.Trim(strings.TrimSpace(v), "\"")
		}
	}
	t.Fatalf("no %s.%s key in --show-config output:\n%s", section, key, out)
	return ""
}

// docsAPIExampleNamespace parses the fenced ```json block of docs/api.md that
// carries duckbrain.namespace (the /api/v1/config example) instead of grepping
// the line, so re-indenting or moving the example cannot silently disarm the
// check. Illustrative blocks with elisions ("...") do not parse and are skipped.
func docsAPIExampleNamespace(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var found []string
	for _, f := range regexp.MustCompile("(?s)```json\\n(.*?)```").FindAllStringSubmatch(string(data), -1) {
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(f[1]), &doc); err != nil {
			continue
		}
		duck, ok := doc["duckbrain"].(map[string]interface{})
		if !ok {
			continue
		}
		if ns, ok := duck["namespace"].(string); ok {
			found = append(found, ns)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want exactly one parseable ```json example carrying duckbrain.namespace, found %d %v", path, len(found), found)
	}
	return found[0]
}

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	// Drain CONCURRENTLY. Reading only after f() returns deadlocks as soon as
	// the captured output exceeds the pipe capacity (F_GETPIPE_SZ = 8192 on
	// this host): the writer blocks mid-Write, f() never returns, and the
	// reader never starts. --schema crossed that line at 8301 bytes
	// (SCHED-GAP-168); the old shape had ~580 bytes of headroom left.
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	w.Close()
	out := <-done
	r.Close()
	os.Stdout = old
	return out
}

func captureLogOutput(f func()) string {
	oldOut := os.Stdout
	oldErr := os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr
	f()
	wOut.Close()
	wErr.Close()
	var buf bytes.Buffer
	io.Copy(&buf, rOut)
	io.Copy(&buf, rErr)
	os.Stdout = oldOut
	os.Stderr = oldErr
	return buf.String()
}
