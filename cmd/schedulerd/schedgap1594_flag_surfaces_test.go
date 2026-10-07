package main

// SCHED-GAP-1594 — the --disable-tick-PUSH surfaces must agree with main.go:
// the flag default (false = SCHED-GAP-1694 pushes ON) is the single source
// of truth; the README and docs/reference/flags.md rows and the --show-config
// TOML key must state it, and the wiring must reach the spawner.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestSCHEDGAP1594_FlagDefaultMatchesDocs re-anchors the show-config pattern:
// the flag declaration in main.go is canonical; every doc copy must carry
// the same default.
func TestSCHEDGAP1594_FlagDefaultMatchesDocs(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	m := regexp.MustCompile(`flag\.Bool\(\s*"disable-tick-push"\s*,\s*(\w+)`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("main.go no longer declares flag.Bool(\"disable-tick-push\", <default>, ...) as a single literal; re-anchor TestSCHEDGAP1594_FlagDefaultMatchesDocs")
	}
	if string(m[1]) != "false" {
		t.Errorf("main.go --disable-tick-push default = %s, want false (SCHED-GAP-1694 pushes stay ON by default)", m[1])
	}
}

// TestSCHEDGAP1594_ShowConfigCarriesKey proves --show-config prints the
// resolved switch under [scheduler] disable_tick_push, argument-driven.
func TestSCHEDGAP1594_ShowConfigCarriesKey(t *testing.T) {
	out := captureStdout(func() {
		printConfig(
			"",
			"/tmp/sched-gap-1594.db",
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
			0,
			"",
			0,
			true, // disableTickPush — the sentinel argument
		)
	})
	if got := tomlSectionValue(t, out, "scheduler", "disable_tick_push"); got != "true" {
		t.Errorf("[scheduler] disable_tick_push in --show-config = %q, want \"true\" (the argument)", got)
	}
}

// TestSCHEDGAP1594_AgentDocMentionsFlag: docs/reference/flags.md is the
// agent-facing copy — the flag row must exist there and in the README (the
// parity guards verify set equality; this pins the DOCUMENTED default so a
// row with the wrong default cannot slip past a name-only check).
func TestSCHEDGAP1594_AgentDocMentionsFlag(t *testing.T) {
	for _, rel := range []string{filepath.Join("..", "..", "README.md"), filepath.Join("..", "..", "docs", "reference", "flags.md")} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		re := regexp.MustCompile("(?m)^\\|\\s*`--?disable-tick-push`\\s*\\|\\s*`([^`]+)`")
		m := re.FindSubmatch(raw)
		if m == nil {
			t.Errorf("%s has no flags-table row for `disable-tick-push`", rel)
			continue
		}
		if got := strings.TrimSpace(string(m[1])); got != "false" {
			t.Errorf("%s: `disable-tick-push` default = %q, want \"false\"", rel, got)
		}
	}
}
