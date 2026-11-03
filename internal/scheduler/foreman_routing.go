package scheduler

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// laneClasses keeps ownership classification independent of admission, pack
// eligibility and namespace membership (including disabled satellite names).
func laneClasses(projects []database.Project) map[string]string {
	all := make(map[string]bool, len(projects))
	for _, p := range projects {
		all[p.Name] = true
	}
	classes := make(map[string]string, len(projects))
	for _, p := range projects {
		classes[p.Name] = ClassifyLane(p, all)
	}
	return classes
}

func loadLaneClasses(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT name, COALESCE(parent, '') FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []database.Project
	for rows.Next() {
		var p database.Project
		if err := rows.Scan(&p.Name, &p.Parent); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return laneClasses(projects), nil
}

func (l *Loop) laneClassForProject(p database.Project) (string, error) {
	classes, err := loadLaneClasses(l.db)
	if err != nil {
		return "", fmt.Errorf("classify spawn lane %s: %w", p.Name, err)
	}
	all := make(map[string]bool, len(classes))
	for name := range classes {
		all[name] = true
	}
	return ClassifyLane(p, all), nil
}

// SetForemanRoutingMode applies the boot-resolved routing policy to the loop's
// spawner without changing process environment. Call before starting any work;
// the policy remains fixed for the lifetime of the running loop.
func (l *Loop) SetForemanRoutingMode(mode string) error {
	resolved, err := config.ParseForemanRoutingMode(mode)
	if err != nil {
		return err
	}
	l.spawner.foremanRoutingMode = resolved
	return nil
}

func (s *Spawner) fixedForeman(project PackedProject) bool {
	return s.routingMode(project) == "fixed" && project.LaneClass == LaneClassForeman
}

// routingMode resolves the orchestration policy for one spawn.
//
// Bane 2026-11-03: this is the SAME layered shape as the model chains —
// lane > namespace > global — so a project that hard-codes its model can be
// pinned WITHOUT flipping the whole daemon:
//
//	projects.routing_mode    'fixed' | 'router'   (explicit per-lane choice)
//	namespaces.routing_mode  'fixed' | 'router'   (whole namespace pinned)
//	foremanRoutingMode       the process-wide env default (boot-resolved)
//
// An unrecognised value is treated as inherit rather than as 'router': the
// failure mode of a typo must be \"keeps inheriting\", never \"silently
// unpins a foreman that the operator hard-coded on purpose\".
func (s *Spawner) routingMode(project PackedProject) string {
	if v := normalizeRoutingMode(project.RoutingMode); v != "" {
		return v
	}
	if v := normalizeRoutingMode(project.NamespaceRoutingMode); v != "" {
		return v
	}
	if s.foremanRoutingMode == "fixed" {
		return "fixed"
	}
	return "router"
}

// normalizeRoutingMode accepts only the two explicit policy words; everything
// else (empty, whitespace, a typo) means \"no opinion at this tier\".
func normalizeRoutingMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "fixed":
		return "fixed"
	case "router":
		return "router"
	default:
		return ""
	}
}

func validateFixedForemanRoute(project PackedProject) error {
	var missing []string
	if strings.TrimSpace(project.Model) == "" {
		missing = append(missing, "model")
	}
	if strings.TrimSpace(project.Provider) == "" {
		missing = append(missing, "provider")
	}
	if len(missing) > 0 {
		return fmt.Errorf("fixed foreman route for %s: missing project %s; configure both project model and provider (global defaults are not used)", project.Name, strings.Join(missing, " and "))
	}
	return nil
}
