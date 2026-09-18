package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

func (s *Server) toolFleetStatus(ctx context.Context) (string, error) {
	projects, _ := database.ListProjects(ctx, s.db, true)
	activeTicks := 0
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&activeTicks)
	// ADV-R09/G8: budget flows from the loop (the --budget/SCHEDULER_BUDGET/
	// TOML resolution) — never a literal. The MCP tool description says
	// "budget" in weight units; the field keeps its name for compatibility.
	budget := 100
	if s.loop != nil {
		budget = s.loop.WeightBudget()
	}
	return jsonString(map[string]interface{}{
		"total_projects": len(projects),
		"active_ticks":   activeTicks,
		"budget":         budget,
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
		CreatedAt: s.clock().Now().UTC().Format(time.RFC3339),
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

// ── Deploy blocks (groups / templates / deploy) + events ───────────────────
//
// These mirror the /api/v1/groups*, /api/v1/templates* and /api/v1/events
// REST routes (internal/api/block_handlers.go) over MCP so an agent can
// manage deploy blocks without a second transport. Behaviour contract is
// identical: JSONL store semantics, per-project deploy outcomes that never
// abort the batch, one INFO event per deploy.

// blocksStoreRequired returns the configured store or a readable error.
// Mirrors api.Server.blocksStoreRequired (503 there, tool error here).
func (s *Server) blocksStoreRequired() (*blocks.Store, error) {
	if s.blocksStore == nil {
		return nil, fmt.Errorf("groups/templates store not configured — daemon must be started with the default <db dir> JSONL paths or --groups-file/--templates-file")
	}
	return s.blocksStore, nil
}

// friendlyBlocksError maps store sentinel errors (ErrNotFound / ErrExists)
// to readable messages; other errors pass through verbatim.
func friendlyBlocksError(err error) error {
	switch {
	case errors.Is(err, blocks.ErrNotFound):
		return fmt.Errorf("not found: %s", err)
	case errors.Is(err, blocks.ErrExists):
		return fmt.Errorf("already exists: %s", err)
	default:
		return err
	}
}

// getBoolArg reads a boolean argument (JSON booleans arrive as bool).
func getBoolArg(args map[string]interface{}, key string) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// getTypedArg unmarshals a nested argument into a typed value via a JSON
// round-trip. MCP arguments arrive as decoded interface{} trees, so typed
// structs (patches, task lists) need this re-encode step.
func getTypedArg(dst interface{}, args map[string]interface{}, key string) error {
	v, ok := args[key]
	if !ok {
		return fmt.Errorf("%s is required", key)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: marshal: %w", key, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// decodeArgsInto unmarshals the whole argument map into dst (unknown keys
// are ignored). This is the primary path for groups_create/templates_create,
// whose tool schema carries the record fields at the top level.
func decodeArgsInto(dst interface{}, args map[string]interface{}) error {
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("arguments: marshal: %w", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}

// getInt64Arg reads a 64-bit integer argument (JSON numbers arrive as
// float64). Returns 0 when absent — callers treat that as the default.
func getInt64Arg(args map[string]interface{}, key string) int64 {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		}
	}
	return 0
}

func (s *Server) toolGroupsList(ctx context.Context) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	groups, err := st.ListGroups()
	if err != nil {
		return "", err
	}
	if groups == nil {
		groups = []blocks.Group{}
	}
	return jsonString(map[string]interface{}{"groups": groups}), nil
}

func (s *Server) toolGroupsGet(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	g, err := st.GetGroup(name)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(g), nil
}

func (s *Server) toolGroupsCreate(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	var g blocks.Group
	if err := decodeArgsInto(&g, args); err != nil {
		return "", err
	}
	if g.Projects == nil {
		g.Projects = []string{}
	}
	if err := blocks.ValidateGroup(g); err != nil {
		return "", err
	}
	if err := st.CreateGroup(g); err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(g), nil
}

func (s *Server) toolGroupsUpdate(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	var patch blocks.GroupUpdate
	if err := getTypedArg(&patch, args, "patch"); err != nil {
		return "", err
	}
	updated, err := st.UpdateGroup(name, patch)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(updated), nil
}

func (s *Server) toolGroupsDelete(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := st.DeleteGroup(name); err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(map[string]string{"status": "deleted", "group": name}), nil
}

func (s *Server) toolTemplatesList(ctx context.Context) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	templates, err := st.ListTemplates()
	if err != nil {
		return "", err
	}
	if templates == nil {
		templates = []blocks.Template{}
	}
	return jsonString(map[string]interface{}{"templates": templates}), nil
}

func (s *Server) toolTemplatesGet(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	tpl, err := st.GetTemplate(name)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(tpl), nil
}

func (s *Server) toolTemplatesCreate(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	var tpl blocks.Template
	if err := decodeArgsInto(&tpl, args); err != nil {
		return "", err
	}
	if err := blocks.ValidateTemplate(tpl); err != nil {
		return "", err
	}
	if err := st.CreateTemplate(tpl); err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(tpl), nil
}

func (s *Server) toolTemplatesUpdate(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	var patch blocks.TemplateUpdate
	if err := getTypedArg(&patch, args, "patch"); err != nil {
		return "", err
	}
	updated, err := st.UpdateTemplate(name, patch)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(updated), nil
}

func (s *Server) toolTemplatesDelete(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := st.DeleteTemplate(name); err != nil {
		return "", friendlyBlocksError(err)
	}
	return jsonString(map[string]string{"status": "deleted", "template": name}), nil
}

// toolGroupsDeploy mirrors api.Server.deployGroup (internal/api/
// block_handlers.go) exactly in behaviour: get group → get template →
// reject empty groups → resolve member workdirs from the projects table →
// dedupe by name → blocks.Deploy with a provenance ForemanNote → one INFO
// event (component "mcp"). Deploy does file IO on member boards, so — like
// the API handler, which uses context.Background() — it deliberately runs
// off the 10s tools/call deadline instead of inheriting it.
func (s *Server) toolGroupsDeploy(ctx context.Context, args map[string]interface{}) (string, error) {
	st, err := s.blocksStoreRequired()
	if err != nil {
		return "", err
	}
	groupName := getStringArg(args, "group")
	templateName := getStringArg(args, "template")
	if groupName == "" {
		return "", fmt.Errorf("group is required")
	}
	if templateName == "" {
		return "", fmt.Errorf("template is required")
	}
	dryRun := getBoolArg(args, "dry_run")

	group, err := st.GetGroup(groupName)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	template, err := st.GetTemplate(templateName)
	if err != nil {
		return "", friendlyBlocksError(err)
	}
	if len(group.Projects) == 0 {
		return "", fmt.Errorf("group %s has no projects — add members before deploying", group.Name)
	}

	// The deploy work runs on context.Background() (see doc comment): board
	// file IO must not die at the caller's 10s tools/call deadline, matching
	// the API handler which never binds the request context either.
	deployCtx := context.Background()
	projects, err := database.ListProjects(deployCtx, s.db, false)
	if err != nil {
		return "", err
	}
	workdirByProject := make(map[string]string, len(projects))
	for _, p := range projects {
		workdirByProject[p.Name] = p.Workdir
	}
	targets := make([]blocks.ProjectTarget, 0, len(group.Projects))
	seen := map[string]bool{}
	for _, member := range group.Projects {
		if seen[member] {
			continue
		}
		seen[member] = true
		targets = append(targets, blocks.ProjectTarget{Name: member, Workdir: workdirByProject[member]})
	}

	note := fmt.Sprintf("deployed via scheduler template %s to group %s", template.Name, group.Name)
	res := blocks.Deploy(blocks.DeployRequest{
		Group:       group,
		Template:    template,
		Projects:    targets,
		DryRun:      dryRun,
		ForemanNote: note,
	})

	// One event-log entry per deploy (INFO, component "mcp"). A failure to
	// record it is logged but never fails the deploy — the boards already
	// hold the authoritative result (same contract as the API handler).
	mode := ""
	if dryRun {
		mode = ", dry run"
	}
	ev := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message: fmt.Sprintf("template deploy: %s → group %s (%d projects, %d task rows%s)",
			template.Name, group.Name, res.Summary.Projects, res.Summary.TaskRows, mode),
		Details: blocks.DeployErrorDetail(res),
	}
	if err := database.LogEvent(deployCtx, s.db, ev); err != nil {
		log.Printf("WARN: deploy event-log entry failed: %v", err)
	}

	return jsonString(res), nil
}

// toolEventsList exposes the event log with an SQL-side incremental cursor
// (id > since). severity/component filters are applied in Go on top of the
// cursor query — ListEventsAfterID keeps the id predicate in SQL, which is
// the point of the tool (cheap tail polling).
func (s *Server) toolEventsList(ctx context.Context, args map[string]interface{}) (string, error) {
	limit := getIntArg(args, "limit")
	if limit == 0 {
		limit = 100
	}
	severity := getStringArg(args, "severity")
	component := getStringArg(args, "component")
	since := getInt64Arg(args, "since")

	events, err := database.ListEventsAfterID(ctx, s.db, since, limit)
	if err != nil {
		return "", err
	}
	if severity != "" || component != "" {
		filtered := events[:0]
		for _, e := range events {
			if severity != "" && string(e.Severity) != severity {
				continue
			}
			if component != "" && e.Component != component {
				continue
			}
			filtered = append(filtered, e)
		}
		events = filtered
	}
	if events == nil {
		events = []database.Event{}
	}
	return jsonString(map[string]interface{}{"events": events, "count": len(events)}), nil
}
