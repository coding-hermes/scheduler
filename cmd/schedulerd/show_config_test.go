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
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties object")
	}
	for _, key := range []string{"daemon", "scheduler", "gateway", "duckbrain", "projects", "namespaces"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema missing property section: %s", key)
		}
	}
}

func TestPrintConfig(t *testing.T) {
	os.Setenv("SCHEDULER_DB_PATH", "testdb")
	defer os.Unsetenv("SCHEDULER_DB_PATH")

	out := captureStdout(func() {
		printConfig(
			"/tmp/fleet.toml",
			"/tmp/test.db",
			"127.0.0.1:9090",
			20*60*1000000000,
			24*60*60*1000000000,
			10, 100, 8,
			false,
			2*60*60*1000000000,
			"http://127.0.0.1:8642",
			"secret",
			"/tmp/foreman",
			"coding-hermes",
			"http://localhost:3000",
		)
	})

	checks := []string{
		"db_path = \"/tmp/test.db\"",
		"listen = \"127.0.0.1:9090\"",
		"[scheduler]",
		"min_interval = \"20m0s\"",
		"max_interval = \"24h0m0s\"",
		"num_levels = 10",
		"weight_budget = 100",
		"max_concurrent = 8",
		"tick_timeout = \"2h0m0s\"",
		"namespace_mode = false",
		"[gateway]",
		"url = \"http://127.0.0.1:8642\"",
		"key = \"secret\"",
		"foreman_home = \"/tmp/foreman\"",
		"[duckbrain]",
		"namespace = \"coding-hermes\"",
		"url = \"http://localhost:3000\"",
		"# fleet config file: /tmp/fleet.toml",
		"# active env var overrides:",
		"#   SCHEDULER_DB_PATH=testdb",
	}
	for _, substr := range checks {
		if !strings.Contains(out, substr) {
			t.Errorf("printConfig() output missing %q\nGot:\n%s", substr, out)
		}
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
