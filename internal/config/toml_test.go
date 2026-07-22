package config

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
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
	if p.CooldownS != 900 {
		t.Errorf("default cooldown: expected 900, got %d", p.CooldownS)
	}
	if p.DecayRate != 1.0 {
		t.Errorf("default decay_rate: expected 1.0, got %f", p.DecayRate)
	}
	if p.Model != "your-model-name" {
		t.Errorf("default model: expected your-model-name, got %q", p.Model)
	}
	if p.Provider != "your-provider-name" {
		t.Errorf("default provider: expected your-provider-name, got %q", p.Provider)
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
