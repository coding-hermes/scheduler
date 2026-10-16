package scheduler

import (
	"database/sql"
	"fmt"
	"strings"

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

func (s *Spawner) fixedForeman(project PackedProject) bool {
	return s.foremanRoutingMode == "fixed" && project.LaneClass == LaneClassForeman
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
