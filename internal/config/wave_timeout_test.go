package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §4.3 (SCHED-GAP-111): 4h hard ceiling on wave_tick_timeout ───────

// writeFleetToml writes a minimal fleet.toml declaring one namespace whose
// wave_tick_timeout is the given raw string.
func writeFleetToml(t *testing.T, waveTickTimeout string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	content := `[[namespaces]]
id = "h3"
weight = 10
wave_enabled = true
wave_tick_timeout = "` + waveTickTimeout + `"

[[projects]]
name = "wave-proj"
workdir = "/tmp/wave-proj"
namespace_id = "h3"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfig_RejectsWaveTickTimeoutAboveCeiling (§12): "5h" → field-named
// error naming the namespace and the field; "4h" is accepted (the ceiling is
// inclusive); "garbage" is rejected as a config error.
func TestConfig_RejectsWaveTickTimeoutAboveCeiling(t *testing.T) {
	// > 4h → rejected, field-named, never clamped.
	path := writeFleetToml(t, "5h")
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("LoadFleetConfig accepted wave_tick_timeout=5h — the 4h ceiling is not enforced")
	}
	for _, want := range []string{"namespaces[h3].wave_tick_timeout", "exceeds the 4h ceiling", "5h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q (field-named ceiling error)", err.Error(), want)
		}
	}

	// 4h exactly → accepted (inclusive ceiling).
	path = writeFleetToml(t, "4h")
	if _, err := LoadFleetConfig(path); err != nil {
		t.Errorf("LoadFleetConfig rejected wave_tick_timeout=4h (ceiling must be inclusive): %v", err)
	}

	// Unparseable non-empty → config error, field-named.
	path = writeFleetToml(t, "garbage")
	_, err = LoadFleetConfig(path)
	if err == nil {
		t.Fatal("LoadFleetConfig accepted wave_tick_timeout=garbage")
	}
	if !strings.Contains(err.Error(), "namespaces[h3].wave_tick_timeout") {
		t.Errorf("error %q missing field name", err.Error())
	}
}

// TestConfig_RejectsWaveTickTimeoutAboveCeiling_LongUnits: unit variants
// above the ceiling are rejected too (4h1s, 1440000000001ns...), and the
// recommended 3h passes.
func TestConfig_RejectsWaveTickTimeoutAboveCeiling_LongUnits(t *testing.T) {
	for _, raw := range []string{"4h1s", "4h0m1s", "5h0m0s", "14400000000001ns"} {
		path := writeFleetToml(t, raw)
		if _, err := LoadFleetConfig(path); err == nil {
			t.Errorf("LoadFleetConfig accepted wave_tick_timeout=%s", raw)
		}
	}
	path := writeFleetToml(t, "3h") // recommended value (1.5x base)
	if _, err := LoadFleetConfig(path); err != nil {
		t.Errorf("LoadFleetConfig rejected the recommended 3h: %v", err)
	}
}

// TestRootConfigValidate_RejectsWaveTickTimeoutAboveCeiling: the root TOML
// path (LoadRootConfig + Validate) enforces the identical contract.
func TestRootConfigValidate_RejectsWaveTickTimeoutAboveCeiling(t *testing.T) {
	path := writeFleetToml(t, "5h")
	root, err := LoadRootConfig(path)
	if err != nil {
		t.Fatalf("LoadRootConfig (decode-only): %v", err)
	}
	err = root.Validate()
	if err == nil {
		t.Fatal("RootConfig.Validate accepted wave_tick_timeout=5h")
	}
	if !strings.Contains(err.Error(), "namespaces[h3].wave_tick_timeout") ||
		!strings.Contains(err.Error(), "exceeds the 4h ceiling") {
		t.Errorf("error %q missing field name or ceiling text", err.Error())
	}
}

// TestApplyFleetConfig_RejectsWaveTickTimeoutAboveCeiling: the DB-write path
// re-checks the ceiling even when the caller built the FleetConfig in code.
func TestApplyFleetConfig_RejectsWaveTickTimeoutAboveCeiling(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	on := true
	cfg := &FleetConfig{
		Namespaces: []NamespaceDef{
			{ID: "h3", WaveEnabled: &on, WaveTickTimeout: "9h", WaveWorkersCap: 3},
		},
	}
	err = ApplyFleetConfig(t.Context(), db, cfg)
	if err == nil {
		t.Fatal("ApplyFleetConfig accepted wave_tick_timeout=9h — an over-ceiling value reached the DB")
	}
	if !strings.Contains(err.Error(), "namespaces[h3].wave_tick_timeout") {
		t.Errorf("error %q missing field name", err.Error())
	}
	// Prove nothing was written.
	if ns, err := database.GetNamespace(t.Context(), db, "h3"); err == nil {
		t.Errorf("namespace h3 was created despite rejection: %+v", ns)
	}
}

// TestValidateWaveTickTimeout_EmptyInherits: empty is always valid (inherit).
func TestValidateWaveTickTimeout_EmptyInherits(t *testing.T) {
	if err := validateWaveTickTimeout("h3", ""); err != nil {
		t.Errorf("empty wave_tick_timeout rejected: %v", err)
	}
	if WaveTickTimeoutCeiling != 4*time.Hour {
		t.Errorf("WaveTickTimeoutCeiling = %v, want 4h", WaveTickTimeoutCeiling)
	}
}
