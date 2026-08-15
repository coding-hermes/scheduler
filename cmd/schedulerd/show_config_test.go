package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
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
	// DOGFOOD-012: the description must not claim a loaded three-layer model —
	// schedulerd.toml is NOT loaded by the daemon yet (FEAT-005 wiring only).
	desc, ok := schema["description"].(string)
	if !ok {
		t.Fatal("schema missing description")
	}
	if strings.Contains(desc, "three-layer model (TOML < env vars < CLI flags)") {
		t.Errorf("schema description still claims a loaded three-layer model: %q", desc)
	}
	if !strings.Contains(desc, "NOT yet loaded by the daemon") || !strings.Contains(desc, "FEAT-005") {
		t.Errorf("schema description must state schedulerd.toml is NOT yet loaded (FEAT-005 contract): %q", desc)
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
			"http://127.0.0.1:8642",
			"secret",
			"/tmp/foreman",
			true,
			"coding-hermes",
			"http://localhost:3000",
			0.5,
			100, 50, 100,
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
		"namespace = \"coding-hermes\"",
		"url = \"http://localhost:3000\"",
		"# fleet config file: /tmp/fleet.toml",
		"# active env var overrides:",
		"#   SCHEDULER_DB_PATH=testdb",
		"#   SCHEDULER_AUTO_DISABLE_FAILURE_RATE=0.5",
	}
	for _, substr := range checks {
		if !strings.Contains(out, substr) {
			t.Errorf("printConfig() output missing %q\nGot:\n%s", substr, out)
		}
	}

	// Header honesty (DOGFOOD-012): must not claim "CLI flags only" and must
	// state the effective-value source plus the FEAT-005 root TOML deferral.
	if strings.Contains(out, "CLI flags only") {
		t.Errorf("printConfig() header still claims 'CLI flags only'\nGot:\n%s", out)
	}
	if !strings.Contains(out, "effective values") ||
		!strings.Contains(out, "root TOML loading comes in FEAT-005") {
		t.Errorf("printConfig() header missing effective-values / FEAT-005 wording\nGot:\n%s", out)
	}
}

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	os.Stdout = old
	return buf.String()
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
