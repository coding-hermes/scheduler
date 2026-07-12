package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProjectNotFound is returned when a project lookup or update targets a
// name that does not exist in the projects table.
var ErrProjectNotFound = errors.New("project not found")

// nowUTC returns the current time as a UTC RFC3339 string — the canonical
// timestamp format stored in all TEXT timestamp columns.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// CreateProject inserts a new project row. CreatedAt and UpdatedAt are set
// to the current UTC time if the caller left them zero-valued.
func CreateProject(ctx context.Context, db *sql.DB, p *Project) error {
	if p.CreatedAt == "" {
		p.CreatedAt = nowUTC()
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = p.CreatedAt
	}
	const q = `INSERT INTO projects
(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, q,
		p.Name, p.RepoURL, p.Workdir, p.Weight, p.Priority, p.CooldownS,
		p.DecayRate, p.Model, p.Provider, boolToInt(p.Enabled),
		p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create project %q: %w", p.Name, err)
	}
	return nil
}

// GetProject loads a single project by name. Returns ErrProjectNotFound if
// no row matches.
func GetProject(ctx context.Context, db *sql.DB, name string) (*Project, error) {
	const q = `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at
FROM projects WHERE name = ?`
	var p Project
	var enabled int
	err := db.QueryRowContext(ctx, q, name).Scan(
		&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
		&p.DecayRate, &p.Model, &p.Provider, &enabled,
		&p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get project %q: %w", name, err)
	}
	p.Enabled = enabled != 0
	return &p, nil
}

// ListProjects returns projects. If enabledOnly is true, only enabled=1
// rows are returned. Results are ordered by name for stable output.
func ListProjects(ctx context.Context, db *sql.DB, enabledOnly bool) ([]Project, error) {
	q := `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at
FROM projects`
	if enabledOnly {
		q += " WHERE enabled = 1"
	}
	q += " ORDER BY name ASC"

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		var enabled int
		if err := rows.Scan(
			&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
			&p.DecayRate, &p.Model, &p.Provider, &enabled,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.Enabled = enabled != 0
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	return out, nil
}

// ProjectUpdates holds the optional fields for a partial project update.
// Only non-nil fields are written. Pointer types distinguish "unset" from
// "set to zero value".
type ProjectUpdates struct {
	RepoURL   *string
	Workdir   *string
	Weight    *int
	Priority  *int
	CooldownS *int
	DecayRate *float64
	Model     *string
	Provider  *string
	Enabled   *bool
}

// UpdateProject applies the given updates to the project named name. Only
// the fields present in updates are modified; UpdatedAt is always refreshed.
func UpdateProject(ctx context.Context, db *sql.DB, name string, updates ProjectUpdates) error {
	setClauses := []string{"updated_at = ?"}
	args := []any{nowUTC()}

	if updates.RepoURL != nil {
		setClauses = append(setClauses, "repo_url = ?")
		args = append(args, *updates.RepoURL)
	}
	if updates.Workdir != nil {
		setClauses = append(setClauses, "workdir = ?")
		args = append(args, *updates.Workdir)
	}
	if updates.Weight != nil {
		setClauses = append(setClauses, "weight = ?")
		args = append(args, *updates.Weight)
	}
	if updates.Priority != nil {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, *updates.Priority)
	}
	if updates.CooldownS != nil {
		setClauses = append(setClauses, "cooldown_s = ?")
		args = append(args, *updates.CooldownS)
	}
	if updates.DecayRate != nil {
		setClauses = append(setClauses, "decay_rate = ?")
		args = append(args, *updates.DecayRate)
	}
	if updates.Model != nil {
		setClauses = append(setClauses, "model = ?")
		args = append(args, *updates.Model)
	}
	if updates.Provider != nil {
		setClauses = append(setClauses, "provider = ?")
		args = append(args, *updates.Provider)
	}
	if updates.Enabled != nil {
		setClauses = append(setClauses, "enabled = ?")
		args = append(args, boolToInt(*updates.Enabled))
	}

	args = append(args, name)
	q := "UPDATE projects SET " + strings.Join(setClauses, ", ") + " WHERE name = ?"

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update project %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for update project %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, name)
	}
	return nil
}

// DeleteProject soft-deletes a project by setting enabled=0. The row is
// retained so historical ticks remain referentially valid.
func DeleteProject(ctx context.Context, db *sql.DB, name string) error {
	return UpdateProject(ctx, db, name, ProjectUpdates{Enabled: boolPtr(false)})
}

// boolToInt converts a bool to SQLite's INTEGER representation.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// boolPtr returns a pointer to b — a convenience for ProjectUpdates callers.
func boolPtr(b bool) *bool { return &b }
