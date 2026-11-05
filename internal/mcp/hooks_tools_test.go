package mcp

// hooks_tools_test.go — the S13 hooks tools, exercised against a real hooks
// file. CTL-003's parity test only proves a tool EXISTS for every route; these
// prove the tools actually read and write the same definitions the tick path
// will run (a registered-but-broken tool would pass parity and still be useless).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

func hooksToolServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "noop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	doc := `[scheduler]
hook_order = "level_first"

[[hook]]
id = "repo-fetch"
phase = "pre"
level = "global"
target = "*"
priority = 10
command = "/usr/bin/git"
args = ["-C", "{repo}", "fetch", "--prune", "origin"]
timeout_s = 60
on_failure = "open"

[[hook]]
id = "worktree-ensure"
phase = "pre"
level = "lane"
target = "code-factory-qa"
priority = 20
command = "` + script + `"
args = ["--lane", "{lane}", "--project", "{project}", "--worktree", "{workdir}"]
timeout_s = 120
on_failure = "closed"

[lane."code-factory-qa"]
repo = "/home/kara/code-factory"
base = "main"
policy = "persistent"
push = false
floor_gib = 8.0
gates = []
skip_hooks = []
`
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write hooks.toml: %v", err)
	}
	set, err := lanehook.Open(path)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	s := NewServer(nil, nil)
	s.SetHooks(set)
	return s, path
}

func TestHooksTools_ListAndPlan(t *testing.T) {
	s, path := hooksToolServer(t)

	out, err := s.toolHooksList(context.Background(), nil)
	if err != nil {
		t.Fatalf("hooks_list: %v", err)
	}
	var list struct {
		Path  string          `json:"path"`
		Order string          `json:"order"`
		Steps []lanehook.Step `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("hooks_list payload is not JSON: %v (%s)", err, out)
	}
	if list.Path != path {
		t.Errorf("path = %q, want %q", list.Path, path)
	}
	if len(list.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (%s)", len(list.Steps), out)
	}

	out, err = s.toolHooksPlan(context.Background(), map[string]interface{}{
		"lane": "code-factory-qa", "repo": "/home/kara/code-factory", "workdir": "/w",
	})
	if err != nil {
		t.Fatalf("hooks_plan: %v", err)
	}
	var plan struct {
		Pre  []lanehook.Step `json:"pre"`
		Post []lanehook.Step `json:"post"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("hooks_plan payload is not JSON: %v (%s)", err, out)
	}
	if len(plan.Pre) != 2 {
		t.Fatalf("pre = %d, want 2 (global repo-fetch + lane worktree-ensure): %s", len(plan.Pre), out)
	}
	if len(plan.Post) != 0 {
		t.Errorf("post = %d, want 0", len(plan.Post))
	}

	// A missing lane is a named argument error, not an empty plan.
	if _, err := s.toolHooksPlan(context.Background(), map[string]interface{}{}); err == nil {
		t.Error("hooks_plan accepted a call with no lane")
	}
}

func TestHooksTools_UpsertDeleteReload(t *testing.T) {
	s, path := hooksToolServer(t)

	// Upsert writes through to the file and reloads the armed set.
	_, err := s.toolHooksUpsert(context.Background(), map[string]interface{}{
		"id": "quiesce-check",
		"step": map[string]interface{}{
			"phase": "post", "level": "lane", "target": "code-factory-qa",
			"priority": 95, "command": "/bin/sh", "args": []interface{}{"-c", "exit 0"},
			"timeout_s": 30, "on_failure": "closed",
		},
	})
	if err != nil {
		t.Fatalf("hooks_upsert: %v", err)
	}
	if len(s.hooks.Steps()) != 3 {
		t.Fatalf("armed steps = %d, want 3 after upsert", len(s.hooks.Steps()))
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "quiesce-check") {
		t.Error("upsert did not write through to the hooks file")
	}

	// An unknown placeholder is refused BEFORE the file is touched.
	before, _ := os.ReadFile(path)
	if _, err := s.toolHooksUpsert(context.Background(), map[string]interface{}{
		"id": "broken",
		"step": map[string]interface{}{
			"phase": "pre", "level": "lane", "target": "code-factory-qa",
			"command": "/bin/sh", "args": []interface{}{"{workdirr}"},
		},
	}); err == nil {
		t.Fatal("hooks_upsert accepted an unknown placeholder")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a rejected upsert still modified the hooks file")
	}

	// Delete removes it; a second delete is a named not-found error.
	if _, err := s.toolHooksDelete(context.Background(), map[string]interface{}{"id": "quiesce-check"}); err != nil {
		t.Fatalf("hooks_delete: %v", err)
	}
	if len(s.hooks.Steps()) != 2 {
		t.Errorf("armed steps = %d, want 2 after delete", len(s.hooks.Steps()))
	}
	if _, err := s.toolHooksDelete(context.Background(), map[string]interface{}{"id": "quiesce-check"}); err == nil {
		t.Error("deleting an unknown id returned no error")
	}

	// Reload reports the state of the file it just rewrote.
	if _, err := s.toolHooksReload(context.Background(), nil); err != nil {
		t.Fatalf("hooks_reload: %v", err)
	}
}

func TestHooksTools_UnconfiguredIsExplicit(t *testing.T) {
	s := NewServer(nil, nil)
	for name, call := range map[string]func() (string, error){
		"hooks_list": func() (string, error) { return s.toolHooksList(context.Background(), nil) },
		"hooks_plan": func() (string, error) {
			return s.toolHooksPlan(context.Background(), map[string]interface{}{"lane": "x"})
		},
		"hooks_reload": func() (string, error) { return s.toolHooksReload(context.Background(), nil) },
		"hooks_delete": func() (string, error) {
			return s.toolHooksDelete(context.Background(), map[string]interface{}{"id": "x"})
		},
	} {
		if _, err := call(); err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Errorf("%s with no hooks armed = %v, want an explicit not-configured error", name, err)
		}
	}
}
