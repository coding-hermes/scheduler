// Package config provides declarative fleet definition (TOML) support for
// seeding namespaces and projects at scheduler startup.
//
// A fleet.toml is loaded once at boot via the --config flag and upserted
// into the existing SQLite database. Upsert here means create-only: rows
// that already exist are left untouched so operator-made tweaks survive
// restarts.
package config

// FleetConfig is the top-level TOML structure decoded from a fleet.toml.
// The [[projects]] and [[namespaces]] array-of-tables slices allow the
// same project/namespace to be defined declaratively and in order.
type FleetConfig struct {
	Projects   []ProjectDef  `toml:"projects"`
	Namespaces []NamespaceDef `toml:"namespaces"`
}

// ProjectDef mirrors the subset of database.Project fields that are
// meaningful to set declaratively (see internal/database/models.go).
// Fields left at their zero value get defaults matching the db schema,
// applied in loader.go when materializing the row.
type ProjectDef struct {
	Name        string  `toml:"name"`
	RepoURL     string  `toml:"repo_url"`
	Workdir     string  `toml:"workdir"`
	Weight      int     `toml:"weight"`       // default 10 if <= 0
	Priority    int     `toml:"priority"`     // default 5 if <= 0
	CooldownS   int     `toml:"cooldown_s"`   // default 900 if <= 0
	DecayRate   float64 `toml:"decay_rate"`   // default 1.0 if <= 0
	Model       string  `toml:"model"`        // default "deepseek-v4-pro"
	Provider    string  `toml:"provider"`     // default "deepseek-foreman"
	Command     string  `toml:"command"`
	NamespaceID string  `toml:"namespace_id"` // optional FK → namespaces.id
	Deliver     string  `toml:"deliver"`
	Enabled     *bool   `toml:"enabled"`      // default true if nil
}

// NamespaceDef mirrors the subset of database.Namespace fields that are
// meaningful to set declaratively. ID is the only required field.
type NamespaceDef struct {
	ID          string `toml:"id"`
	Weight      int    `toml:"weight"`      // default 10 if <= 0
	Reserved    int    `toml:"reserved"`    // default 1 if <= 0
	HardCap     int    `toml:"hard_cap"`    // default 100 if <= 0
	Enabled     *bool  `toml:"enabled"`     // default true if nil
	Description string `toml:"description"`
}
