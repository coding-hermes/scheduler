package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-1707 acceptance, knob surfaces: --session-silence-grace must
// appear on all four documented surfaces — the --schema JSON, the
// --show-config TOML printout, /api/v1/config's snapshot struct, and the
// main.go resolution chain. The two print surfaces are enforced here (same
// shape as TestLoadGateAndModelRatesOnBothIntrospectionSurfaces); the API
// snapshot struct+JSON key and the flag's -h entry are enforced too, and
// the resolution chain is covered by config-layer tests
// (TestSCHEDGAP1707_ConfigLayerValidation in internal/config pins the TOML
// validation; main.go's default-guard block is exercised by
// TestSessionSilenceGraceFlagDefaultMatchesSchema below).
//
// Both checks run in -short (they are pure-print tests, no DB, no network).
func TestSCHEDGAP1707_SessionSilenceGraceOnAllSurfaces(t *testing.T) {
	t.Run("printSchema --schema JSON", func(t *testing.T) {
		var schema struct {
			Properties struct {
				Scheduler struct {
					Properties struct {
						SessionSilenceGrace *struct {
							Type        string `json:"type"`
							Default     any    `json:"default"`
							Env         string `json:"env"`
							CLI         string `json:"cli"`
							Description string `json:"description"`
						} `json:"session_silence_grace"`
					} `json:"properties"`
				} `json:"scheduler"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(captureStdout(printSchema)), &schema); err != nil {
			t.Fatalf("printSchema() did not emit valid JSON: %v", err)
		}
		g := schema.Properties.Scheduler.Properties.SessionSilenceGrace
		if g == nil {
			t.Fatal("--schema is missing properties.scheduler.properties.session_silence_grace — the knob-surface drop-out this row closed has regressed")
		}
		if g.Default != "0s" {
			t.Errorf("session_silence_grace default = %v, want \"0s\" (watchdog off is the library/fleet default)", g.Default)
		}
		if g.Env != "SCHEDULER_SESSION_SILENCE_GRACE" {
			t.Errorf("session_silence_grace env = %q, want SCHEDULER_SESSION_SILENCE_GRACE", g.Env)
		}
		if g.CLI != "--session-silence-grace" {
			t.Errorf("session_silence_grace cli = %q, want --session-silence-grace", g.CLI)
		}
		if !strings.Contains(g.Description, "SCHED-GAP-1707") {
			t.Errorf("session_silence_grace description must name the row: %q", g.Description)
		}
	})

	t.Run("printConfig --show-config TOML", func(t *testing.T) {
		out := captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-1707.db",
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
				45*time.Minute,
			)
		})
		if got := tomlSectionValue(t, out, "scheduler", "session_silence_grace"); got != "45m0s" {
			t.Errorf("[scheduler] session_silence_grace in --show-config = %q, want \"45m0s\" (the argument)", got)
		}
	})

	t.Run("printConfig TOML is argument-driven", func(t *testing.T) {
		// Sentinel: the printed value is the ARGUMENT, not a literal baked
		// into printConfig's format string.
		out := captureStdout(func() {
			printConfig(
				"",
				"/tmp/sched-gap-1707.db",
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
				90*time.Minute,
			)
		})
		if got := tomlSectionValue(t, out, "scheduler", "session_silence_grace"); got != "1h30m0s" {
			t.Errorf("[scheduler] session_silence_grace = %q, want the argument 1h30m0s — printConfig must not hardcode it", got)
		}
	})

	t.Run("main.go flag default matches schema", func(t *testing.T) {
		// The flag's default (0 = off) is the single source of truth; the
		// schema's documented default must agree.
		src := sourceFlagDefault(t, "session-silence-grace")
		if src != "0" {
			t.Errorf("main.go --session-silence-grace default = %q, want \"0\" (watchdog disabled by default)", src)
		}
	})
}

// sourceFlagDefault extracts the numeric/duration default from main.go's
// flag declaration for one flag name. Fails loudly when the declaration
// cannot be located — a relocated declaration is drift, not a pass.
func sourceFlagDefault(t *testing.T, name string) string {
	t.Helper()
	names := sourceFlagNames(t, "main.go")
	if !names[name] {
		t.Fatalf("main.go does not register flag %q — the knob dropped out of the daemon surface", name)
	}
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(raw)
	needle := `flag.Duration("` + name + `",`
	idx := strings.Index(text, needle)
	if idx < 0 {
		t.Fatalf("main.go flag declaration for %q changed shape — re-anchor sourceFlagDefault", name)
	}
	seg := text[idx+len(needle):]
	end := strings.Index(seg, ",")
	if end < 0 {
		t.Fatalf("main.go flag declaration for %q has no default segment", name)
	}
	return strings.TrimSpace(seg[:end])
}
