package config

import (
	"os"
	"path/filepath"
	"testing"
)

// REMOTE-003 §1 acceptance #3: the default scheduler id is the hostname,
// and SCHEDULER_ID overrides it. Proven at BOTH config layers: the field
// survives the LoadConfig pipeline (defaults + TOML + env) and the env
// layer wins over TOML.

// TestSchedulerIDDefaultEmpty means "host-derived": the config layer leaves
// the field empty so the daemon (main.go) resolves the short hostname at
// boot. An identity baked into the config default would defeat the
// host-derived contract (two hosts sharing a config template would share an
// identity).
func TestSchedulerIDDefaultEmpty(t *testing.T) {
	t.Setenv("SCHEDULER_ID", "")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.ID != "" {
		t.Errorf("default Scheduler.ID = %q, want \"\" (the daemon resolves the short hostname when unset)", cfg.Scheduler.ID)
	}
}

// TestSchedulerIDTOMLLayer proves the [scheduler] id TOML key loads.
func TestSchedulerIDTOMLLayer(t *testing.T) {
	t.Setenv("SCHEDULER_ID", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	body := "[scheduler]\nid = \"box-toml\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.ID != "box-toml" {
		t.Errorf("TOML Scheduler.ID = %q, want box-toml", cfg.Scheduler.ID)
	}
}

// TestSchedulerIDEnvOverride proves the SCHEDULER_ID env layer wins over
// the TOML layer (precedence: TOML < env).
func TestSchedulerIDEnvOverride(t *testing.T) {
	t.Setenv("SCHEDULER_ID", "box-env")
	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	body := "[scheduler]\nid = \"box-toml\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Scheduler.ID != "box-env" {
		t.Errorf("Scheduler.ID = %q with SCHEDULER_ID set, want box-env (env > TOML)", cfg.Scheduler.ID)
	}
}
