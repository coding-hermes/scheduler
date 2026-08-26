package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
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
		ID           string `json:"id"`
		Status       string `json:"status"`
		Outcome      string `json:"outcome"`
		SpawnedAt    string `json:"spawned_at"`
		CompletedAt  string `json:"completed_at"`
		Commits      int    `json:"commits"`
		FilesChanged int    `json:"files_changed"`
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
	// Log cooldown mutation for audit trail (COOLDOWN-REVERSION investigation).
	_ = database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message:   fmt.Sprintf("toolFleetSetCooldown: %s → %ds", name, c),
		Details:   fmt.Sprintf(`{"cooldown_s":%d,"tool":"toolFleetSetCooldown"}`, c),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return jsonString(map[string]string{"status": "updated", "project": name, "cooldown_s": strconv.Itoa(c)}), nil
}

func (s *Server) toolFleetSetDecay(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	d := getFloatArg(args, "decay")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	// Mirror the HTTP API guard (internal/api/server_projects.go): decay_rate
	// <= 0 makes urgency flat (priority × 1^0) so the packer never picks the
	// project — a silent permanent starvation. Foremen must not be able to do
	// this to themselves via MCP. Proven: dexdat-memory starved 87h with a
	// valid namespace + 900s cooldown because decay_rate was 0; 7 enabled
	// projects were found at decay=0 (2026-08-01) set through this unguarded
	// MCP path after the HTTP guard shipped (bc438e6).
	if d <= 0 {
		return "", fmt.Errorf("decay must be > 0 (0 causes permanent starvation — urgency never grows)")
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
	// repo_url is accepted as an alias for repo (REST-style name) so agents
	// that learned the REST dialect (POST /api/v1/projects uses repo_url)
	// don't fail the call. repo remains the canonical/primary name.
	repo := getStringArg(args, "repo")
	if repo == "" {
		repo = getStringArg(args, "repo_url")
	}
	workdir := getStringArg(args, "workdir")
	weight := getIntArg(args, "weight")
	if name == "" || repo == "" || workdir == "" {
		return "", fmt.Errorf("name, repo (or repo_url), and workdir are required")
	}
	// Mirror the REST create defaults (internal/api/server_projects.go): a
	// minimal {name, repo, workdir} body must satisfy the CHECK constraints.
	// weight=0 is ambiguous — either the arg was omitted or explicitly set
	// to 0 — so only default when the key is absent; an explicit 0 is
	// rejected below as out of range.
	if _, ok := args["weight"]; !ok {
		weight = 10
	}
	if weight < 1 || weight > 100 {
		return "", fmt.Errorf("weight must be 1-100, got %d", weight)
	}
	p := &database.Project{
		Name:      name,
		RepoURL:   repo,
		Workdir:   workdir,
		Weight:    weight,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
	}
	if err := database.CreateProject(ctx, s.db, p); err != nil {
		return "", friendlyCreateError(name, err)
	}
	return jsonString(map[string]string{"status": "added", "project": name}), nil
}

// friendlyCreateError maps database.CreateProject failures to human-readable
// messages so a raw sqlite error never surfaces through the MCP tool. Mirrors
// the REST handler's 409/400 mapping (internal/api/server_projects.go).
func friendlyCreateError(name string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint"):
		return fmt.Errorf("project %q already exists", name)
	case strings.Contains(msg, "already registered by enabled project"):
		return fmt.Errorf("cannot add project %q: %s", name, msg)
	case strings.Contains(msg, "CHECK constraint failed"):
		return fmt.Errorf("invalid project fields: weight must be 1..100; priority 1..10; decay_rate > 0")
	default:
		return fmt.Errorf("failed to create project %q: %s", name, msg)
	}
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
		ID           string `json:"id"`
		ProjectName  string `json:"project_name"`
		Status       string `json:"status"`
		Outcome      string `json:"outcome"`
		SpawnedAt    string `json:"spawned_at"`
		CompletedAt  string `json:"completed_at"`
		ExitCode     int    `json:"exit_code"`
		Commits      int    `json:"commits"`
		FilesChanged int    `json:"files_changed"`
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
