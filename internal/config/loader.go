package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/coding-herms/scheduler/internal/database"
)

// Sensible defaults matching the projects/namespaces schema; applied when
// the corresponding TOML field is omitted or set to its zero value.
const (
	defaultProjectWeight    = 10
	defaultProjectPriority  = 5
	defaultProjectCooldown  = 900
	defaultProjectDecayRate = 1.0
	defaultProjectModel     = "deepseek-v4-pro"
	defaultProjectProvider  = "deepseek-foreman"

	defaultNamespaceWeight   = 10
	defaultNamespaceReserved = 1
	defaultNamespaceHardCap  = 100
)

// LoadFleetConfig reads and decodes the TOML file at path into a FleetConfig.
// It validates that every project has a Name and every namespace has an ID,
// returning an error aggregating all violations so operators can fix the file
// in one pass.
func LoadFleetConfig(path string) (*FleetConfig, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	var cfg FleetConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}

	var errs []error
	for i, p := range cfg.Projects {
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("projects[%d]: name is required", i))
		}
	}
	for i, n := range cfg.Namespaces {
		if n.ID == "" {
			errs = append(errs, fmt.Errorf("namespaces[%d]: id is required", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyFleetConfig seeds the namespaces and projects defined in cfg into db.
// Rows that already exist are left untouched — this is a create-only upsert,
// never an overwrite, so operator-made tweaks survive restarts.
//
// Namespaces are applied first so that any project referencing one by id
// resolves cleanly.
func ApplyFleetConfig(ctx context.Context, db *sql.DB, cfg *FleetConfig) error {
	for _, nd := range cfg.Namespaces {
		if _, err := database.GetNamespace(ctx, db, nd.ID); err == nil {
			log.Printf("Config: namespace %q already exists, skipped", nd.ID)
			continue
		} else if !errors.Is(err, database.ErrNamespaceNotFound) {
			return fmt.Errorf("lookup namespace %q: %w", nd.ID, err)
		}
		ns := namespaceFromDef(nd)
		if err := database.CreateNamespace(ctx, db, ns); err != nil {
			return fmt.Errorf("create namespace %q: %w", nd.ID, err)
		}
		log.Printf("Config: imported namespace %q", nd.ID)
	}

	for _, pd := range cfg.Projects {
		if _, err := database.GetProject(ctx, db, pd.Name); err == nil {
			log.Printf("Config: project %q already exists, skipped", pd.Name)
			continue
		} else if !errors.Is(err, database.ErrProjectNotFound) {
			return fmt.Errorf("lookup project %q: %w", pd.Name, err)
		}
		p := projectFromDef(pd)
		if err := database.CreateProject(ctx, db, p); err != nil {
			return fmt.Errorf("create project %q: %w", pd.Name, err)
		}
		log.Printf("Config: imported project %q", pd.Name)
	}
	return nil
}

// projectFromDef materializes a *database.Project from a ProjectDef,
// substituting schema-matching defaults for zero-valued fields.
func projectFromDef(pd ProjectDef) *database.Project {
	weight := pd.Weight
	if weight <= 0 {
		weight = defaultProjectWeight
	}
	priority := pd.Priority
	if priority <= 0 {
		priority = defaultProjectPriority
	}
	cooldown := pd.CooldownS
	if cooldown <= 0 {
		cooldown = defaultProjectCooldown
	}
	decay := pd.DecayRate
	if decay <= 0 {
		decay = defaultProjectDecayRate
	}
	model := pd.Model
	if model == "" {
		model = defaultProjectModel
	}
	provider := pd.Provider
	if provider == "" {
		provider = defaultProjectProvider
	}
	enabled := true
	if pd.Enabled != nil {
		enabled = *pd.Enabled
	}

	p := &database.Project{
		Name:      pd.Name,
		RepoURL:   pd.RepoURL,
		Workdir:   pd.Workdir,
		Weight:    weight,
		Priority:  priority,
		CooldownS: cooldown,
		DecayRate: decay,
		Model:     model,
		Provider:  provider,
		Command:   pd.Command,
		Deliver:   pd.Deliver,
		Enabled:   enabled,
	}
	if pd.NamespaceID != "" {
		nsID := pd.NamespaceID
		p.NamespaceID = &nsID
	}
	return p
}

// namespaceFromDef materializes a *database.Namespace from a NamespaceDef,
// substituting schema-matching defaults for zero-valued fields.
func namespaceFromDef(nd NamespaceDef) *database.Namespace {
	weight := nd.Weight
	if weight <= 0 {
		weight = defaultNamespaceWeight
	}
	reserved := nd.Reserved
	if reserved <= 0 {
		reserved = defaultNamespaceReserved
	}
	hardCap := nd.HardCap
	if hardCap <= 0 {
		hardCap = defaultNamespaceHardCap
	}
	enabled := true
	if nd.Enabled != nil {
		enabled = *nd.Enabled
	}
	return &database.Namespace{
		ID:          nd.ID,
		Weight:      weight,
		Reserved:    reserved,
		HardCap:     hardCap,
		Enabled:     enabled,
		Description: nd.Description,
	}
}
