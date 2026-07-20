package mcp

import (
	"context"
	"fmt"
	"strconv"

	"github.com/coding-herms/scheduler/internal/database"
)

func (s *Server) toolFleetStatus(ctx context.Context) (string, error) {
	projects, _ := database.ListProjects(ctx, s.db, true)
	activeTicks := 0
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&activeTicks)
	return jsonString(map[string]interface{}{
		"total_projects": len(projects),
		"active_ticks":   activeTicks,
		"budget":         100,
	}), nil
}

func (s *Server) toolFleetProjects(ctx context.Context) (string, error) {
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		return "", err
	}
	return jsonString(map[string]interface{}{"projects": projects}), nil
}

func (s *Server) toolFleetProjectDetail(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		return "", err
	}
	// Get last 5 ticks.
	rows, _ := s.db.QueryContext(ctx, `SELECT id, status, outcome, spawned_at, completed_at, commits, files_changed 
		FROM ticks WHERE project_name=? ORDER BY spawned_at DESC LIMIT 5`, name)
	type tickSummary struct {
		ID, Status, Outcome, SpawnedAt, CompletedAt string
		Commits, FilesChanged                       int
	}
	var ticks []tickSummary
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ts tickSummary
			rows.Scan(&ts.ID, &ts.Status, &ts.Outcome, &ts.SpawnedAt, &ts.CompletedAt, &ts.Commits, &ts.FilesChanged)
			ticks = append(ticks, ts)
		}
	}
	return jsonString(map[string]interface{}{"project": p, "recent_ticks": ticks}), nil
}

func (s *Server) toolFleetSetWeight(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	w := getIntArg(args, "weight")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if w < 1 || w > 100 {
		return "", fmt.Errorf("weight must be 1-100, got %d", w)
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Weight: &w}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "weight": strconv.Itoa(w)}), nil
}

func (s *Server) toolFleetSetPriority(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	p := getIntArg(args, "priority")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if p < 1 || p > 10 {
		return "", fmt.Errorf("priority must be 1-10, got %d", p)
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Priority: &p}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "priority": strconv.Itoa(p)}), nil
}

func (s *Server) toolFleetSetCooldown(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	c := getIntArg(args, "cooldown")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{CooldownS: &c}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "cooldown_s": strconv.Itoa(c)}), nil
}

func (s *Server) toolFleetSetDecay(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	d := getFloatArg(args, "decay")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{DecayRate: &d}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "updated", "project": name, "decay": fmt.Sprintf("%.2f", d)}), nil
}

func (s *Server) toolFleetPause(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(false)}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "paused", "project": name}), nil
}

func (s *Server) toolFleetResume(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "resumed", "project": name}), nil
}

func (s *Server) toolFleetAdd(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	repo := getStringArg(args, "repo")
	workdir := getStringArg(args, "workdir")
	weight := getIntArg(args, "weight")
	if name == "" || repo == "" || workdir == "" {
		return "", fmt.Errorf("name, repo, and workdir are required")
	}
	if weight == 0 {
		weight = 10
	}
	p := &database.Project{
		Name:    name,
		RepoURL: repo,
		Workdir: workdir,
		Weight:  weight,
	}
	if err := database.CreateProject(ctx, s.db, p); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "added", "project": name}), nil
}

func (s *Server) toolFleetTicks(ctx context.Context, args map[string]interface{}) (string, error) {
	project := getStringArg(args, "project")
	limit := getIntArg(args, "limit")
	if limit == 0 {
		limit = 20
	}
	q := "SELECT id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed FROM ticks"
	var queryArgs []interface{}
	if project != "" {
		q += " WHERE project_name = ?"
		queryArgs = append(queryArgs, project)
	}
	q += " ORDER BY spawned_at DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)

	rows, err := s.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type tickRow struct {
		ID, ProjectName, Status, Outcome, SpawnedAt, CompletedAt string
		ExitCode, Commits, FilesChanged                          int
	}
	var ticks []tickRow
	for rows.Next() {
		var t tickRow
		rows.Scan(&t.ID, &t.ProjectName, &t.Status, &t.Outcome, &t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged)
		ticks = append(ticks, t)
	}
	return jsonString(map[string]interface{}{"ticks": ticks, "count": len(ticks)}), nil
}

func (s *Server) toolFleetEvaluate() (string, error) {
	s.loop.ForceEvaluate()
	return jsonString(map[string]string{"status": "evaluation triggered"}), nil
}

func (s *Server) toolFleetPauseScheduler() (string, error) {
	s.loop.Pause()
	return jsonString(map[string]string{"status": "scheduler paused"}), nil
}

func (s *Server) toolFleetResumeScheduler() (string, error) {
	s.loop.Resume()
	return jsonString(map[string]string{"status": "scheduler resumed"}), nil
}
