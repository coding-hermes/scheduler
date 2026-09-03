package config

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func findProject(cfg *FleetConfig, name string) *ProjectDef {
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == name {
			return &cfg.Projects[i]
		}
	}
	return nil
}

func findNamespace(cfg *FleetConfig, id string) *NamespaceDef {
	for i := range cfg.Namespaces {
		if cfg.Namespaces[i].ID == id {
			return &cfg.Namespaces[i]
		}
	}
	return nil
}

func TestLoadFleetConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")

	tomlContent := `[[namespaces]]
id = "coding-hermes"
weight = 70
reserved = 10
hard_cap = 0
enabled = true
description = "Main coding-hermes fleet"

[[projects]]
name = "helix"
repo_url = "https://github.com/totalwindupflightsystems/helix"
workdir = "/home/kara/helix"
weight = 10
priority = 5
cooldown_s = 900
decay_rate = 1.0
model = "deepseek-v4-pro"
provider = "deepseek-foreman"
namespace_id = "coding-hermes"
deliver = "telegram:-1003310984808:12345"
enabled = true
`
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}

	if len(cfg.Namespaces) != 1 {
		t.Fatalf("expected 1 namespace, got %d", len(cfg.Namespaces))
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(cfg.Projects))
	}

	// Verify namespace.
	ns := findNamespace(cfg, "coding-hermes")
	if ns == nil {
		t.Fatal("namespace coding-hermes not found")
		return
	}
	if ns.Weight != 70 {
		t.Errorf("namespace weight: expected 70, got %d", ns.Weight)
	}
	if ns.Reserved != 10 {
		t.Errorf("namespace reserved: expected 10, got %d", ns.Reserved)
	}
	if ns.Description != "Main coding-hermes fleet" {
		t.Errorf("namespace description mismatch: %q", ns.Description)
	}
	if ns.Enabled == nil || !*ns.Enabled {
		t.Error("namespace enabled should be true")
	}

	// Verify project.
	p := findProject(cfg, "helix")
	if p == nil {
		t.Fatal("project helix not found")
		return
	}
	if p.RepoURL != "https://github.com/totalwindupflightsystems/helix" {
		t.Errorf("project repo_url mismatch: %q", p.RepoURL)
	}
	if p.Workdir != "/home/kara/helix" {
		t.Errorf("project workdir mismatch: %q", p.Workdir)
	}
	if p.Weight != 10 {
		t.Errorf("project weight: expected 10, got %d", p.Weight)
	}
	if p.Priority != 5 {
		t.Errorf("project priority: expected 5, got %d", p.Priority)
	}
	if p.CooldownS != 900 {
		t.Errorf("project cooldown_s: expected 900, got %d", p.CooldownS)
	}
	if p.Model != "deepseek-v4-pro" {
		t.Errorf("project model: expected deepseek-v4-pro, got %q", p.Model)
	}
	if p.Provider != "deepseek-foreman" {
		t.Errorf("project provider: expected deepseek-foreman, got %q", p.Provider)
	}
	if p.NamespaceID != "coding-hermes" {
		t.Errorf("project namespace_id: expected coding-hermes, got %q", p.NamespaceID)
	}
	if p.Deliver != "telegram:-1003310984808:12345" {
		t.Errorf("project deliver: %q", p.Deliver)
	}
	if p.Enabled == nil || !*p.Enabled {
		t.Error("project enabled should be true")
	}
}

func TestApplyFleetConfig(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	cfg := &FleetConfig{
		Namespaces: []NamespaceDef{
			{
				ID:          "coding-hermes",
				Weight:      70,
				Reserved:    10,
				HardCap:     0,
				Description: "Main fleet",
			},
		},
		Projects: []ProjectDef{
			{
				Name:        "helix",
				RepoURL:     "https://github.com/totalwindupflightsystems/helix",
				Workdir:     "/home/kara/helix",
				Weight:      10,
				Priority:    5,
				CooldownS:   900,
				DecayRate:   1.0,
				Model:       "deepseek-v4-pro",
				Provider:    "deepseek-foreman",
				NamespaceID: "coding-hermes",
				Deliver:     "telegram:-1003310984808:12345",
			},
		},
	}

	ctx := context.Background()
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// Verify namespace was created.
	ns, err := database.GetNamespace(ctx, db, "coding-hermes")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.Weight != 70 {
		t.Errorf("namespace weight: expected 70, got %d", ns.Weight)
	}
	if ns.Reserved != 10 {
		t.Errorf("namespace reserved: expected 10, got %d", ns.Reserved)
	}

	// Verify project was created.
	p, err := database.GetProject(ctx, db, "helix")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.RepoURL != "https://github.com/totalwindupflightsystems/helix" {
		t.Errorf("project repo_url: %q", p.RepoURL)
	}
	if p.Workdir != "/home/kara/helix" {
		t.Errorf("project workdir: %q", p.Workdir)
	}
	if p.Weight != 10 {
		t.Errorf("project weight: expected 10, got %d", p.Weight)
	}
	if p.Model != "deepseek-v4-pro" {
		t.Errorf("project model: %q", p.Model)
	}
	if p.Provider != "deepseek-foreman" {
		t.Errorf("project provider: %q", p.Provider)
	}
	if p.NamespaceID == nil || *p.NamespaceID != "coding-hermes" {
		t.Errorf("project namespace_id: expected coding-hermes, got %v", p.NamespaceID)
	}
	if p.Deliver != "telegram:-1003310984808:12345" {
		t.Errorf("project deliver: %q", p.Deliver)
	}

	// Idempotency: re-applying should skip existing rows (no error).
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (idempotent): %v", err)
	}

	// Verify count didn't double.
	projects, err := database.ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("expected 1 project after idempotent apply, got %d", len(projects))
	}
}

// TestApplyFleetConfig_PinOverridesExisting — fleet.toml entries PIN cooldown/
// model/enabled on existing projects (survives daemon restart + foreman
// self-pause). Bane 2026-07-31: API PUTs revert on restart; fleet.toml is the
// durable pin (supervisor skill line 424).
func TestApplyFleetConfig_PinOverridesExisting(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{Name: "pinned", RepoURL: "https://github.com/example/pinned", Workdir: "/home/kara/pinned"},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// Simulate foreman self-pause + API drift: push it to 43200s.
	drifty := 43200
	model := "deepseek-v4-pro"
	if err := database.UpdateProject(ctx, db, "pinned", database.ProjectUpdates{
		CooldownS: &drifty, Model: &model,
	}); err != nil {
		t.Fatalf("UpdateProject (simulate drift): %v", err)
	}

	// Re-apply fleet config — the pin must override the drift.
	cfg.Projects[0].CooldownS = 900
	cfg.Projects[0].Model = "deepseek-v4-flash"
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}

	p, err := database.GetProject(ctx, db, "pinned")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.CooldownS != 900 {
		t.Errorf("pin cooldown: expected 900, got %d (drift won)", p.CooldownS)
	}
	if p.Model != "deepseek-v4-flash" {
		t.Errorf("pin model: expected deepseek-v4-flash, got %q (drift won)", p.Model)
	}
}

func TestApplyFleetConfigDefaults(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// Project with most fields omitted — defaults should apply.
	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name:    "minimal",
				RepoURL: "https://github.com/example/minimal",
				Workdir: "/home/kara/minimal",
			},
		},
	}

	ctx := context.Background()
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	p, err := database.GetProject(ctx, db, "minimal")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.Weight != 10 {
		t.Errorf("default weight: expected 10, got %d", p.Weight)
	}
	if p.Priority != 5 {
		t.Errorf("default priority: expected 5, got %d", p.Priority)
	}
	if p.CooldownS != 7200 {
		t.Errorf("default cooldown: expected 7200 (2h baseline per 3-speed policy), got %d", p.CooldownS)
	}
	if p.DecayRate != 1.0 {
		t.Errorf("default decay_rate: expected 1.0, got %f", p.DecayRate)
	}
	// Bane 2026-08-27: unset model/provider stay EMPTY so the spawn chain
	// resolves them (namespace model_chain → global env) — the legacy
	// placeholder shadowed the namespace tier.
	if p.Model != "" {
		t.Errorf("default model: expected empty (chain-resolved), got %q", p.Model)
	}
	if p.Provider != "" {
		t.Errorf("default provider: expected empty (chain-resolved), got %q", p.Provider)
	}
	if !p.Enabled {
		t.Error("default enabled should be true")
	}
}

func TestLoadFleetConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")

	// Project with explicitly disabled + defaults zeroed out.
	tomlContent := `[[projects]]
name = "disabled-proj"
repo_url = "https://github.com/example/disabled"
workdir = "/home/kara/disabled"
weight = 0
priority = 0
cooldown_s = 0
decay_rate = 0.0
model = ""
provider = ""
enabled = false
`
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}

	p := findProject(cfg, "disabled-proj")
	if p == nil {
		t.Fatal("project disabled-proj not found")
		return
	}
	if p.Enabled == nil || *p.Enabled {
		t.Error("project enabled should be false")
	}
	if p.DecayRate != 0.0 {
		t.Errorf("zero decay_rate should stay 0.0, got %f", p.DecayRate)
	}
}

func TestLoadFleetConfigEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")

	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig empty: %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(cfg.Projects))
	}
	if len(cfg.Namespaces) != 0 {
		t.Errorf("expected 0 namespaces, got %d", len(cfg.Namespaces))
	}
}

func TestLoadFleetConfigMissingFile(t *testing.T) {
	_, err := LoadFleetConfig("/nonexistent/path/fleet.toml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// Ensure sql import used.
var _ = sql.ErrNoRows

// TestApplyFleetConfig_FallbackChainPins (SCHED-GAP-064): fleet.toml pins
// the fallback tiers + no_global_fallback flag on EXISTING projects (durable
// pin, same contract as model/provider), while empty fallback keys leave an
// API-assigned fallback untouched (GatewayKey-style conditional pin).
func TestApplyFleetConfig_FallbackChainPins(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Create the project first, then pin via fleet config.
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "fallback-pinned", RepoURL: "https://github.com/example/fallback-pinned",
		Workdir: "/home/kara/fallback-pinned", Weight: 10, Priority: 5, CooldownS: 900,
		Model: "old-model", Provider: "old-provider", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "fallback-pinned", RepoURL: "https://github.com/example/fallback-pinned",
				Workdir: "/home/kara/fallback-pinned",
				Model:   "deepseek-v4-flash", Provider: "deepseek-foreman",
				FallbackModel: "deepseek-v4-pro", FallbackProvider: "deepseek-foreman",
				NoGlobalFallback: true,
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (pin): %v", err)
	}

	p, err := database.GetProject(ctx, db, "fallback-pinned")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.FallbackModel != "deepseek-v4-pro" || p.FallbackProvider != "deepseek-foreman" {
		t.Errorf("pinned fallback tiers = (%q, %q), want (deepseek-v4-pro, deepseek-foreman)",
			p.FallbackModel, p.FallbackProvider)
	}
	if !p.NoGlobalFallback {
		t.Error("pinned no_global_fallback = false, want true")
	}

	// An API-assigned fallback must survive a re-pin with a keyless entry.
	fm := "api-assigned-fallback"
	if err := database.UpdateProject(ctx, db, "fallback-pinned", database.ProjectUpdates{FallbackModel: &fm}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	cfg.Projects[0].FallbackModel = "" // keyless entry
	cfg.Projects[0].NoGlobalFallback = false
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	p, err = database.GetProject(ctx, db, "fallback-pinned")
	if err != nil {
		t.Fatalf("GetProject after re-pin: %v", err)
	}
	if p.FallbackModel != "api-assigned-fallback" {
		t.Errorf("FallbackModel after keyless re-pin = %q, want api-assigned-fallback (conditional pin must not clear it)", p.FallbackModel)
	}
	if p.NoGlobalFallback {
		t.Error("NoGlobalFallback = true after re-pin with flag false, want false")
	}
}

// TestApplyFleetConfig_IdleChainPins (SCHED-GAP-065): fleet.toml pins the
// idle tiers on EXISTING projects (GatewayKey-style conditional pin), while
// keyless entries leave an API-assigned idle lane untouched. Also verifies
// the TOML parse of the new keys end-to-end.
func TestApplyFleetConfig_IdleChainPins(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Create the project first, then pin via fleet config.
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "idle-pinned", RepoURL: "https://github.com/example/idle-pinned",
		Workdir: "/home/kara/idle-pinned", Weight: 10, Priority: 5, CooldownS: 900,
		Model: "old-model", Provider: "old-provider", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "idle-pinned", RepoURL: "https://github.com/example/idle-pinned",
				Workdir: "/home/kara/idle-pinned",
				Model:   "deepseek-v4-flash", Provider: "deepseek-foreman",
				IdleModel: "deepseek-v4-flash", IdleProvider: "opencode-go",
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (pin): %v", err)
	}

	p, err := database.GetProject(ctx, db, "idle-pinned")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.IdleModel != "deepseek-v4-flash" || p.IdleProvider != "opencode-go" {
		t.Errorf("pinned idle tiers = (%q, %q), want (deepseek-v4-flash, opencode-go)",
			p.IdleModel, p.IdleProvider)
	}

	// An API-assigned idle lane must survive a re-pin with a keyless entry.
	im := "api-assigned-idle"
	if err := database.UpdateProject(ctx, db, "idle-pinned", database.ProjectUpdates{IdleModel: &im}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	cfg.Projects[0].IdleModel = "" // keyless entry
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	p, err = database.GetProject(ctx, db, "idle-pinned")
	if err != nil {
		t.Fatalf("GetProject after re-pin: %v", err)
	}
	if p.IdleModel != "api-assigned-idle" {
		t.Errorf("IdleModel after keyless re-pin = %q, want api-assigned-idle (conditional pin must not clear it)", p.IdleModel)
	}
	// The provider key is still set in the entry, so it re-pins.
	if p.IdleProvider != "opencode-go" {
		t.Errorf("IdleProvider after re-pin = %q, want opencode-go (set key re-pins)", p.IdleProvider)
	}
}

// TestApplyFleetConfig_AdaptiveCooldownPins covers the fleet.toml config
// surface for adaptive cooldown: explicit keys land on the row, a keyless
// entry pins the feature OFF (fleet.toml is the durable on/off switch, same
// doctrine as enabled/cooldown_s), and enabling with only the flag resolves
// floor/ceiling/threshold to effective values.
func TestApplyFleetConfig_AdaptiveCooldownPins(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	adaptive := true
	cfg := &FleetConfig{
		Projects: []ProjectDef{
			{
				Name: "adaptive-on", RepoURL: "https://github.com/example/adaptive-on",
				Workdir: "/home/kara/adaptive-on", CooldownS: 3600,
				AdaptiveCooldown:    &adaptive,
				CooldownFloorS:      7200,
				CooldownCeilingS:    100000,
				NoProgressThreshold: 3,
			},
			{
				Name: "adaptive-defaults", RepoURL: "https://github.com/example/adaptive-defaults",
				Workdir: "/home/kara/adaptive-defaults", CooldownS: 1800,
				AdaptiveCooldown: &adaptive, // no numeric keys — built-in defaults
			},
			{
				// Keyless entry: adaptive must pin OFF (default).
				Name: "adaptive-absent", RepoURL: "https://github.com/example/adaptive-absent",
				Workdir: "/home/kara/adaptive-absent", CooldownS: 900,
			},
		},
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// Explicit values land verbatim (floor overrides the cooldown snapshot).
	p, err := database.GetProject(ctx, db, "adaptive-on")
	if err != nil {
		t.Fatalf("GetProject(adaptive-on): %v", err)
	}
	if !p.AdaptiveCooldown || p.CooldownFloorS != 7200 || p.CooldownCeilingS != 100000 || p.NoProgressThreshold != 3 {
		t.Errorf("adaptive-on policy = (%v, floor=%d, ceiling=%d, threshold=%d), want (true, 7200, 100000, 3)",
			p.AdaptiveCooldown, p.CooldownFloorS, p.CooldownCeilingS, p.NoProgressThreshold)
	}

	// Flag-only enablement resolves to effective defaults on the row.
	p, err = database.GetProject(ctx, db, "adaptive-defaults")
	if err != nil {
		t.Fatalf("GetProject(adaptive-defaults): %v", err)
	}
	if !p.AdaptiveCooldown {
		t.Error("adaptive-defaults adaptive_cooldown = false, want true")
	}
	if p.CooldownFloorS != 1800 {
		t.Errorf("adaptive-defaults floor = %d, want 1800 (default = fleet cooldown_s)", p.CooldownFloorS)
	}
	if p.CooldownCeilingS != database.DefaultAdaptiveCooldownCeilingS {
		t.Errorf("adaptive-defaults ceiling = %d, want %d (built-in weekly)", p.CooldownCeilingS, database.DefaultAdaptiveCooldownCeilingS)
	}
	if p.NoProgressThreshold != database.DefaultAdaptiveCooldownThreshold {
		t.Errorf("adaptive-defaults threshold = %d, want %d (built-in)", p.NoProgressThreshold, database.DefaultAdaptiveCooldownThreshold)
	}

	// Keyless entries pin the flag off.
	p, err = database.GetProject(ctx, db, "adaptive-absent")
	if err != nil {
		t.Fatalf("GetProject(adaptive-absent): %v", err)
	}
	if p.AdaptiveCooldown {
		t.Error("adaptive-absent adaptive_cooldown = true, want false (absent key = off by default)")
	}
	if p.CooldownFloorS != 0 {
		t.Errorf("adaptive-absent floor = %d, want 0 (policy only materialized when enabled)", p.CooldownFloorS)
	}

	// Re-pin with the flag absent flips a previously-on project back OFF —
	// fleet.toml is the durable switch, mirroring enabled/cooldown_s.
	p, err = database.GetProject(ctx, db, "adaptive-on")
	if err != nil {
		t.Fatalf("GetProject(adaptive-on): %v", err)
	}
	off := false
	cfg.Projects[0].AdaptiveCooldown = &off
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin off): %v", err)
	}
	p, err = database.GetProject(ctx, db, "adaptive-on")
	if err != nil {
		t.Fatalf("GetProject after re-pin: %v", err)
	}
	if p.AdaptiveCooldown {
		t.Error("adaptive_cooldown = true after re-pin with adaptive_cooldown = false, want false")
	}
}
