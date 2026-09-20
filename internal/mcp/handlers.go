package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
	"github.com/coding-hermes/scheduler/internal/version"
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, status, outcome, spawned_at, completed_at, commits, files_changed 
		FROM ticks WHERE project_name=? ORDER BY spawned_at DESC LIMIT 5`, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
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
	for rows.Next() {
		var (
			ts           tickSummary
			outcome      sql.NullString
			spawnedAt    sql.NullString
			completedAt  sql.NullString
			commits      sql.NullInt64
			filesChanged sql.NullInt64
		)
		// Nullable columns scan into sql.Null* — an in-flight tick has NULL
		// outcome/completed_at/commits/files_changed, and a scan error here used
		// to be discarded (SCHED-GAP-175b): the row was still appended, so every
		// column from the first NULL onward silently rendered as "", 0 while
		// REST GET /api/v1/ticks carried the real timestamp.
		if err := rows.Scan(&ts.ID, &ts.Status,
			&outcome, &spawnedAt, &completedAt, &commits, &filesChanged); err != nil {
			return "", fmt.Errorf("scan recent tick row: %w", err)
		}
		if outcome.Valid {
			ts.Outcome = outcome.String
		}
		if spawnedAt.Valid {
			ts.SpawnedAt = spawnedAt.String
		}
		if completedAt.Valid {
			ts.CompletedAt = completedAt.String
		}
		if commits.Valid {
			ts.Commits = int(commits.Int64)
		}
		if filesChanged.Valid {
			ts.FilesChanged = int(filesChanged.Int64)
		}
		ticks = append(ticks, ts)
	}
	if err := rows.Err(); err != nil {
		return "", err
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

// tickRow is the fleet_ticks wire shape. Field names/tags are the S06
// snake_case contract shared with /api/v1/ticks; nullable columns render as
// "" / 0 here exactly the way the REST surface's COALESCE does.
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
	var ticks []tickRow
	for rows.Next() {
		var (
			t            tickRow
			outcome      sql.NullString
			spawnedAt    sql.NullString
			completedAt  sql.NullString
			exitCode     sql.NullInt64
			commits      sql.NullInt64
			filesChanged sql.NullInt64
		)
		// Nullable columns scan into sql.Null* — an in-flight tick has NULL
		// outcome/completed_at/exit_code, and a scan error here used to be
		// discarded (SCHED-GAP-175): the row was still appended, so every
		// column from the first NULL onward silently rendered as "", 0.
		if err := rows.Scan(&t.ID, &t.ProjectName, &t.Status,
			&outcome, &spawnedAt, &completedAt, &exitCode, &commits, &filesChanged); err != nil {
			return "", fmt.Errorf("scan tick row: %w", err)
		}
		if outcome.Valid {
			t.Outcome = outcome.String
		}
		if spawnedAt.Valid {
			t.SpawnedAt = spawnedAt.String
		}
		if completedAt.Valid {
			t.CompletedAt = completedAt.String
		}
		if exitCode.Valid {
			t.ExitCode = int(exitCode.Int64)
		}
		if commits.Valid {
			t.Commits = int(commits.Int64)
		}
		if filesChanged.Valid {
			t.FilesChanged = int(filesChanged.Int64)
		}
		ticks = append(ticks, t)
	}
	if err := rows.Err(); err != nil {
		return "", err
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

// ── CTL-003: namespaces, project lifecycle, fleet introspection ───────────
//
// Every tool below mirrors one REST operation 1:1 (internal/api) so the two
// surfaces cannot drift apart — the contract is enforced by
// mcp_api_parity_test.go (TestMCPAPIParity). Like the fleet_* tools they read
// and write the SQLite DB and drive the Loop DIRECTLY; none of them calls the
// REST API over HTTP.
//
// Error-mapping note: MCP has no HTTP status codes. Where the REST handler
// distinguishes 400 / 404 / 409, the tool returns an error whose message
// carries the same decision (not-found vs refused-while-enabled vs missing
// confirmation) so an agent can branch on the text exactly as it would on the
// status code.

// requireLoop returns the scheduler loop or a readable error. The tools that
// drive scheduling need it; the daemon always builds the MCP server with a
// loop, but a nil loop must fail loudly instead of panicking inside a tool.
func (s *Server) requireLoop() (*scheduler.Loop, error) {
	if s.loop == nil {
		return nil, fmt.Errorf("scheduler loop not attached to this MCP server — scheduling control is unavailable")
	}
	return s.loop, nil
}

// logDisableEventMCP records a GAP-044 disable-provenance entry in the events
// table, mirroring api.logDisableEvent for the MCP disable paths (pause,
// project_delete soft delete). The message shape is identical to the REST
// surface's so an operator greps both with one pattern; Component is "mcp"
// because that is the transport that wrote the row, and via names the tool.
func logDisableEventMCP(ctx context.Context, db *sql.DB, name, by, reason, at, via string) {
	details, _ := json.Marshal(map[string]string{
		"project":         name,
		"disabled_by":     by,
		"disabled_reason": reason,
		"disabled_at":     at,
		"via":             via,
	})
	_ = database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message:   fmt.Sprintf("project disabled: %s (%s)", name, by),
		Details:   string(details),
	})
}

// ── namespaces ────────────────────────────────────────────────────────────

// toolNamespacesList mirrors GET /api/v1/namespaces: every namespace row
// (enabled and disabled), ordered by id.
func (s *Server) toolNamespacesList(ctx context.Context) (string, error) {
	namespaces, err := database.ListNamespaces(ctx, s.db, false)
	if err != nil {
		return "", err
	}
	if namespaces == nil {
		namespaces = []database.Namespace{}
	}
	return jsonString(map[string]interface{}{"namespaces": namespaces}), nil
}

// toolNamespacesGet mirrors GET /api/v1/namespaces/{id}: one namespace row,
// or a not-found error.
func (s *Server) toolNamespacesGet(ctx context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	ns, err := database.GetNamespace(ctx, s.db, id)
	if err != nil {
		if errors.Is(err, database.ErrNamespaceNotFound) {
			return "", fmt.Errorf("namespace not found: %s", id)
		}
		return "", err
	}
	return jsonString(ns), nil
}

// toolNamespacesCreate mirrors POST /api/v1/namespaces: the arguments ARE the
// namespace row (same field names as the REST body — id, weight, reserved,
// hard_cap, max_concurrent, enabled, description, default_prompt,
// model_chain, wave_*, admission_mode, load_gate). id and a positive weight
// are required; a duplicate id is refused.
func (s *Server) toolNamespacesCreate(ctx context.Context, args map[string]interface{}) (string, error) {
	var ns database.Namespace
	if err := decodeArgsInto(&ns, args); err != nil {
		return "", err
	}
	if ns.ID == "" {
		return "", fmt.Errorf("id is required")
	}
	if ns.Weight <= 0 {
		return "", fmt.Errorf("weight must be greater than 0")
	}
	if err := database.CreateNamespace(ctx, s.db, &ns); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return "", fmt.Errorf("namespace %q already exists", ns.ID)
		}
		return "", err
	}
	return jsonString(ns), nil
}

// toolNamespacesUpdate mirrors PUT /api/v1/namespaces/{id}: a partial update
// where only the supplied fields are written (the patch shape is the REST
// body's — weight, reserved, hard_cap, max_concurrent, enabled, description,
// default_prompt, model_chain, wave_*, admission_mode, load_gate). The patch
// may also be passed as a nested "patch" object. Returns the updated row.
func (s *Server) toolNamespacesUpdate(ctx context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	var patch database.NamespacePatch
	if _, ok := args["patch"]; ok {
		if err := getTypedArg(&patch, args, "patch"); err != nil {
			return "", err
		}
	} else if err := decodeArgsInto(&patch, args); err != nil {
		return "", err
	}
	if err := database.UpdateNamespace(ctx, s.db, id, patch); err != nil {
		if errors.Is(err, database.ErrNamespaceNotFound) {
			return "", fmt.Errorf("namespace not found: %s", id)
		}
		// SCHED-GAP-124/125: invalid admission_mode / load_gate surface
		// verbatim (400-class in REST).
		return "", err
	}
	ns, err := database.GetNamespace(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	return jsonString(ns), nil
}

// toolNamespacesDelete mirrors DELETE /api/v1/namespaces/{id}. Guards, in the
// REST handler's order: the namespace must exist; confirm must be true (a bare
// delete is refused); a namespace with ENABLED member projects is refused so a
// stray delete can never silently unassign live fleet members. Without purge
// the namespace is soft-deleted (enabled=false, members unassigned, row
// retained); with purge=true the row is hard-deleted and an INFO purge event
// is logged. purge alone (without confirm) is refused exactly like a bare
// delete.
func (s *Server) toolNamespacesDelete(ctx context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	purge := getBoolArg(args, "purge")
	// Existence checked before the confirm gate: an unknown id fails on any
	// delete variant (bare, confirm, or purge).
	if _, err := database.GetNamespace(ctx, s.db, id); err != nil {
		if errors.Is(err, database.ErrNamespaceNotFound) {
			return "", fmt.Errorf("namespace not found: %s", id)
		}
		return "", err
	}
	// Confirm flag checked before the enabled-member guard: even a namespace
	// with enabled members must not be touched without explicit confirmation.
	if !getBoolArg(args, "confirm") {
		return "", fmt.Errorf("confirm=true is required — this soft-deletes the namespace (enabled=false); add purge=true to permanently remove the row")
	}
	members, err := database.ListProjectsByNamespace(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	var enabledNames []string
	for _, m := range members {
		if m.Enabled {
			enabledNames = append(enabledNames, m.Name)
		}
	}
	if len(enabledNames) > 0 {
		return "", fmt.Errorf("namespace has enabled project(s) assigned — pause or move them first: %s", strings.Join(enabledNames, ", "))
	}
	if purge {
		if err := database.PurgeNamespace(ctx, s.db, id); err != nil {
			return "", err
		}
		// The row is gone, so a soft-delete entry makes no sense; log a
		// purge audit event instead (same shape as the REST handler).
		_ = database.LogEvent(ctx, s.db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "mcp",
			Message:   fmt.Sprintf("namespace purged (hard delete): %s", id),
			Details:   `{"namespace":"` + id + `","action":"purge","via":"MCP namespaces_delete (confirm=true, purge=true)"}`,
		})
		return jsonString(map[string]string{"status": "purged", "namespace": id}), nil
	}
	if err := database.DeleteNamespace(ctx, s.db, id); err != nil {
		return "", err
	}
	return jsonString(map[string]string{"status": "deleted", "namespace": id}), nil
}

// toolNamespacesProjects mirrors GET /api/v1/namespaces/{id}/projects: the
// member projects of a namespace (enabled and disabled alike).
func (s *Server) toolNamespacesProjects(ctx context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	projects, err := database.ListProjectsByNamespace(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	if projects == nil {
		projects = []database.Project{}
	}
	return jsonString(map[string]interface{}{
		"namespace_id": id,
		"projects":     projects,
	}), nil
}

// toolNamespacesMove mirrors POST /api/v1/namespaces/{id}/move: assign one
// project to the namespace. Faithful to the REST handler — it does NOT
// pre-validate that the namespace exists (the project row's namespace_id is
// simply set, and the namespace list is the authority on what exists); an
// unknown project is a not-found error. Returns the updated project.
func (s *Server) toolNamespacesMove(ctx context.Context, args map[string]interface{}) (string, error) {
	nsID := getStringArg(args, "id")
	if nsID == "" {
		return "", fmt.Errorf("id is required")
	}
	project := getStringArg(args, "project")
	if project == "" {
		return "", fmt.Errorf("project is required")
	}
	if err := database.UpdateProject(ctx, s.db, project, database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", project)
		}
		return "", err
	}
	p, err := database.GetProject(ctx, s.db, project)
	if err != nil {
		return "", err
	}
	return jsonString(p), nil
}

// ── project lifecycle ─────────────────────────────────────────────────────

// toolProjectDelete mirrors DELETE /api/v1/projects/{name}. Guards, in the
// REST handler's order: confirm must be true (checked first, so even an
// enabled project is never touched without it); the project must exist; an
// ENABLED project is refused (pause it first). Without purge the project is
// soft-deleted (enabled=false, row retained, disable-provenance stamped by the
// DB layer and mirrored into the events table); with purge=true the row is
// hard-deleted and an INFO purge event is logged.
func (s *Server) toolProjectDelete(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	purge := getBoolArg(args, "purge")
	if !getBoolArg(args, "confirm") {
		return "", fmt.Errorf("confirm=true is required — this soft-deletes the project (enabled=false); add purge=true to permanently remove the row")
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", name)
		}
		return "", err
	}
	if p.Enabled {
		return "", fmt.Errorf("project %s is enabled — pause it first (fleet_pause or project_delete after pausing) before deleting", name)
	}
	if purge {
		if err := database.PurgeProject(ctx, s.db, name); err != nil {
			return "", err
		}
		// The row is gone, so a disable-provenance event makes no sense;
		// log a purge audit entry instead.
		_ = database.LogEvent(ctx, s.db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "mcp",
			Message:   fmt.Sprintf("project purged (hard delete): %s", name),
			Details:   `{"project":"` + name + `","action":"purge","via":"MCP project_delete (confirm=true, purge=true)"}`,
		})
		return jsonString(map[string]string{"status": "purged", "project": name}), nil
	}
	if err := database.DeleteProject(ctx, s.db, name); err != nil {
		return "", err
	}
	// GAP-044: the DELETE soft-delete stamps provenance (api-delete, with
	// COALESCE legacy backfill) — mirror it into the events table from the
	// row's actually-stamped values.
	if updated, err := database.GetProject(ctx, s.db, name); err == nil {
		logDisableEventMCP(ctx, s.db, name, updated.DisabledBy, updated.DisabledReason, updated.DisabledAt, "MCP project_delete (confirm=true)")
	}
	return jsonString(map[string]string{"status": "deleted", "project": name}), nil
}

// toolProjectSpawn mirrors POST /api/v1/projects/{name}/spawn: enqueue a tick
// for the project through Loop.SpawnNow, returning the REAL stored tick id
// (resolvable immediately via tick_get). An unknown project is a not-found
// error; a project that already holds a running/queued tick is refused with
// the scheduler's ErrProjectRunning message (409-class in REST), so the
// manual spawn path can never double-spawn a project mid-tick.
func (s *Server) toolProjectSpawn(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	loop, err := s.requireLoop()
	if err != nil {
		return "", err
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", name)
		}
		return "", err
	}
	tickID, err := loop.SpawnNow(*p)
	if err != nil {
		if errors.Is(err, scheduler.ErrProjectRunning) {
			return "", fmt.Errorf("spawn refused: %w", err)
		}
		return "", err
	}
	return jsonString(map[string]string{
		"status":  "spawned",
		"project": name,
		"tick_id": tickID,
	}), nil
}

// toolProjectBump mirrors POST /api/v1/projects/{name}/bump (SCHED-GAP-107):
// temporarily accelerate a project to a small cooldown for at most
// database.MaxBumpTicks ticks, then auto-revert. Validation order is the REST
// handler's: project must exist, be ENABLED, and have no active bump; reason
// is REQUIRED (an unattributed speed-up is unauditable); ticks default to
// database.DefaultBumpTicks and must be 1..MaxBumpTicks; cooldown defaults to
// database.DefaultBumpCooldown and must be >= database.MinBumpCooldown (the
// cooldown law floor applies to bumps too). Every bump writes an audit event.
// Returns the updated project.
func (s *Server) toolProjectBump(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", name)
		}
		return "", err
	}
	if !p.Enabled {
		return "", fmt.Errorf("project %s is disabled — resume it before bumping (a paused project cannot consume bump ticks)", name)
	}
	if p.BumpActive {
		return "", fmt.Errorf("bump already active on %s (%d tick(s) remaining, reason %q) — let it expire or call project_unbump", name, p.BumpRemainingTicks, p.BumpReason)
	}
	reason := ""
	if v, ok := args["reason"]; ok {
		if str, ok := v.(string); ok {
			reason = strings.TrimSpace(str)
		}
	}
	if reason == "" {
		return "", fmt.Errorf("reason is required — a bump is an auditable speed-up, name why")
	}
	// Presence-aware defaults: an omitted field takes the default, while an
	// explicit out-of-range value is rejected (mirrors the REST pointer
	// semantics for the JSON body's nullable ticks/cooldown).
	ticks := database.DefaultBumpTicks
	if _, ok := args["ticks"]; ok {
		ticks = getIntArg(args, "ticks")
	}
	if ticks < 1 || ticks > database.MaxBumpTicks {
		return "", fmt.Errorf("ticks must be 1..%d (got %d)", database.MaxBumpTicks, ticks)
	}
	cooldown := database.DefaultBumpCooldown
	if _, ok := args["cooldown"]; ok {
		cooldown = getIntArg(args, "cooldown")
	}
	if cooldown < database.MinBumpCooldown {
		return "", fmt.Errorf("cooldown must be >= %ds — the 6h cooldown law floor applies to bumps too (got %d)", database.MinBumpCooldown, cooldown)
	}
	updated, err := database.BumpProject(ctx, s.db, name, ticks, cooldown, reason)
	if err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", name)
		}
		return "", err
	}
	// Audit trail: every bump gets an events-table entry with who/why (same
	// detail shape as the REST handler).
	details, _ := json.Marshal(map[string]any{
		"project":  name,
		"ticks":    ticks,
		"cooldown": cooldown,
		"reason":   reason,
		"saved": map[string]int{
			"cooldown_s":         updated.BumpSavedCooldownS,
			"cooldown_floor_s":   updated.BumpSavedFloorS,
			"cooldown_ceiling_s": updated.BumpSavedCeilingS,
			"no_progress_ticks":  updated.BumpSavedNoProgress,
		},
	})
	_ = database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message:   fmt.Sprintf("project bumped: %s (%d ticks @ %ds — %s)", name, ticks, cooldown, reason),
		Details:   string(details),
	})
	return jsonString(updated), nil
}

// toolProjectUnbump mirrors POST /api/v1/projects/{name}/unbump: manually
// abort an active bump (Phase A only — restore the saved pre-bump state; no
// adaptive re-evaluation runs, because an explicit cancel wants the pre-bump
// state verbatim). Unknown project → not-found; no active bump → error.
// Returns the restored project row.
func (s *Server) toolProjectUnbump(ctx context.Context, args map[string]interface{}) (string, error) {
	name := getStringArg(args, "name")
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if err := database.ClearBump(ctx, s.db, name); err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			return "", fmt.Errorf("project not found: %s", name)
		}
		if errors.Is(err, database.ErrNoActiveBump) {
			return "", fmt.Errorf("no active bump to clear on %s", name)
		}
		return "", err
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		return "", err
	}
	_ = database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message:   "bump manually cleared: " + name,
		Details:   `{"project":"` + name + `","via":"MCP project_unbump"}`,
	})
	return jsonString(p), nil
}

// ── ticks, config, queue, metrics ─────────────────────────────────────────

// toolTickGet mirrors GET /api/v1/ticks/{id} (SCHED-GAP-112 / S12 §9.4):
// the tick row, plus tick_workers when the tick dispatched a worker wave.
// Embedding the Tick guarantees the wave envelope carries every Tick field,
// and a serial tick (no worker rows) is written as the bare Tick — the same
// two shapes the REST route serves.
func (s *Server) toolTickGet(ctx context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	tick, err := database.GetTick(ctx, s.db, id)
	if err != nil {
		if errors.Is(err, database.ErrTickNotFound) {
			return "", fmt.Errorf("tick not found: %s", id)
		}
		return "", err
	}
	workers, err := database.ListTickWorkersByTick(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	if len(workers) == 0 {
		return jsonString(tick), nil
	}
	return jsonString(struct {
		*database.Tick
		TickWorkers []database.TickWorker `json:"tick_workers"`
	}{tick, workers}), nil
}

// toolConfigGet covers GET /api/v1/config. The REST handler serves a
// startup-time snapshot captured in main.go (SCHED-GAP-034), which the MCP
// server does not receive, so this tool returns the HONEST subset that is
// derivable from the live Loop and the DB — never a fabricated value:
//
//	db_path                  the attached SQLite file ("" for an in-memory DB)
//	weight_budget            Loop.WeightBudget() — the same budget authority
//	                         chain value /api/v1/config reports
//	paused                   Loop.IsPaused()
//	gateway_response_timeout Loop.GatewayResponseTimeout() (SCHED-GAP-117)
//	version                  build identity (internal/version)
//
// The "omitted" array names every REST field this tool cannot derive
// (listen, min/max_interval, num_levels, max_concurrent, tick_timeout,
// auto_disable_*, gateway.*, duckbrain, ...) so a consumer sees the absence
// explicitly instead of guessing from a zero.
func (s *Server) toolConfigGet(ctx context.Context) (string, error) {
	out := map[string]interface{}{
		"source":         "mcp subset — derived from the live Loop + DB; the full startup snapshot is served by GET /api/v1/config",
		"db_path":        s.databasePath(ctx),
		"version":        version.Current(),
		"weight_budget":  nil,
		"paused":         nil,
		"omitted":        []string{"listen", "min_interval", "max_interval", "num_levels", "max_concurrent", "tick_timeout", "slot_patience", "tasks_pacing", "load_gate_threshold", "spawn_mem_limit_mb", "model_rates_file", "namespace_mode", "auto_disable_failure_rate", "auto_disable_window", "auto_disable_min_ticks", "failure_window", "gateway", "duckbrain"},
		"omitted_reason": "these live in the main.go-resolved config snapshot (api.Server.SetResolvedConfig), which the MCP server is not given; read GET /api/v1/config for them",
	}
	if s.loop != nil {
		out["weight_budget"] = s.loop.WeightBudget()
		out["paused"] = s.loop.IsPaused()
		out["gateway_response_timeout"] = s.loop.GatewayResponseTimeout().String()
	}
	return jsonString(out), nil
}

// databasePath reports the file backing the attached SQLite connection
// (PRAGMA database_list, schema "main"), or "" when the source is an in-memory
// database. It exists so config_get can report the real DB path without
// re-plumbing the daemon's resolved config.
func (s *Server) databasePath(ctx context.Context) string {
	rows, err := s.db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			continue
		}
		if name == "main" {
			return file
		}
	}
	return ""
}

// queueEntry is the MCP queue element — the same field names as the REST
// /api/v1/queue element (queueItem in internal/api/server_helpers.go).
type queueEntry struct {
	Project   string  `json:"project"`
	Urgency   float64 `json:"urgency"`
	Weight    int     `json:"weight"`
	Priority  int     `json:"priority"`
	CooldownS int     `json:"cooldown_s"`
	Enabled   bool    `json:"enabled"`
}

// toolQueueGet mirrors GET /api/v1/queue: the enabled projects ordered by
// urgency, descending. The REST route scores with the engine's
// UrgencyCalculator, which it builds from the resolved-config interval range —
// the same snapshot the MCP server does not hold (see toolConfigGet), so the
// scores here are the api handler's documented fallback: priority-only. The
// payload states that in urgency_source, and the ordering itself is real
// (priority DESC from SQL, stable across the score sort).
func (s *Server) toolQueueGet(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, COALESCE(weight,0), COALESCE(priority,0), COALESCE(cooldown_s,0), COALESCE(enabled,1) FROM projects WHERE enabled = 1 ORDER BY priority DESC LIMIT 200`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var items []queueEntry
	for rows.Next() {
		var it queueEntry
		if err := rows.Scan(&it.Project, &it.Weight, &it.Priority, &it.CooldownS, &it.Enabled); err != nil {
			return "", err
		}
		it.Urgency = float64(it.Priority)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Urgency > items[j].Urgency
	})
	if items == nil {
		items = []queueEntry{}
	}
	return jsonString(map[string]interface{}{
		"queue":          items,
		"count":          len(items),
		"urgency_source": "priority-only fallback — the MCP server holds no resolved-config interval range, so the engine-formula urgency served by GET /api/v1/queue is unavailable here",
	}), nil
}

// mcpMetricsWindow is the lookback for every WINDOWED metrics block; it
// matches the REST endpoint's 24h window (SCHED-GAP-156).
const mcpMetricsWindow = 24 * time.Hour

// mcpMetricsWindowLabel is the wire string for mcpMetricsWindow.
const mcpMetricsWindowLabel = "24h"

// metricsUnavailable builds the honesty shape for a block whose real source is
// missing or whose query failed — the block reports available=false with the
// reason instead of an invented zero (identical contract to /api/v1/metrics).
func metricsUnavailable(block, reason string) map[string]interface{} {
	log.Printf("metrics (mcp): %s unavailable: %s", block, reason)
	return map[string]interface{}{
		"available": false,
		"reason":    reason,
	}
}

// mcpMetricsSources names the exact query/counter behind every metrics block
// this tool serves, so a consumer can tell a measured number from an
// unavailable one without reading the source (mirror of the REST endpoint's
// sources map, compacted to one entry per block).
func mcpMetricsSources() map[string]string {
	return map[string]string{
		"spawns": "ticks table, rows with spawned_at inside the window (julianday(spawned_at) >= julianday(cutoff)): total = every such row; " +
			"by_namespace = LEFT JOIN projects on project_name grouped by namespace_id (\"-\" when the project row is missing or its namespace_id is empty); " +
			"by_outcome = rows whose outcome is set, one key per CHECK-constraint value. Window: " + mcpMetricsWindowLabel + ".",
		"deferrals": "Loop.AdmissionCounters() — per-process SCHED-GAP-155 admission counters, monotonic since daemon boot and reset by a restart " +
			"(no persisted equivalent exists); admitted_by_namespace and passes are the two bookkeeping entries. available=false when no Loop is attached.",
		"nudges": "ticks table, SUM(nudge_count) grouped by orphan_reason for rows with nudge_count > 0 (empty orphan_reason -> \"unknown\"). " +
			"Persisted across restarts, so this block is all-time rather than windowed.",
		"ticks": "ticks table (active = COUNT(status='running'), the same number /api/v1/status reports as active_ticks; queued = COUNT(status='queued')); " +
			"by_namespace = one entry per namespaces row with its max_concurrent cap plus the live running/queued counts, and a \"-\" entry for ticks whose " +
			"project has no namespace; cooldown_expired_unscheduled = enabled projects with no running/queued tick whose wall-clock cooldown has elapsed " +
			"(bump-aware: an active bump's cooldown owns the gate; it is a SQL mirror of the packer's gate and does not model blackout windows, tasks-mode " +
			"post-tick pacing or dynamic intervals, so it can overcount slightly). global_cap is deliberately absent — it comes from the resolved-config " +
			"snapshot, which is api-only; see the top-level \"unavailable\" list.",
		"tick_duration_ms": "ticks table: ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000) milliseconds for rows completed inside the " +
			"window (completed_at >= spawned_at); p50/p90/p99 are nearest-rank on the ascending sample (idx = ceil(pct/100*n) - 1 clamped to [0, n-1]) " +
			"and are null with count=0 when the window holds no completed tick.",
		"gateway": "ticks table: rows spawned inside the window whose error text matches the harness drain class (error LIKE '%503%' OR '%draining%'). " +
			"The in-memory gateway-error counter behind /api/v1/status gateway_errors counts EVERY transient gateway failure and does not classify drains, " +
			"so it cannot source this number and is deliberately not used.",
		"outcomes": "ticks table: rows spawned inside the window with outcome='committed' AND commits=0 — a tick that recorded a commit outcome while " +
			"landing no commit at all. Window: " + mcpMetricsWindowLabel + ".",
	}
}

// mcpMetricsUnavailableFields names the REST metrics fields the MCP surface
// cannot derive, so an absent field reads as "unknown here" rather than a
// fabricated zero.
func mcpMetricsUnavailableFields() []string {
	return []string{
		"ticks.global_cap — served from the startup resolved-config snapshot (api.Server.SetResolvedConfig), which the MCP server is not given",
	}
}

// toolMetricsGet mirrors GET /api/v1/metrics (SCHED-GAP-156): one read-only
// request answering spawns by namespace/outcome, deferrals by reason, orphan
// nudges by path, active/queued ticks vs caps, cooldown-expired-unscheduled,
// tick duration p50/p90/p99, gateway drain-503s and zero-output committed
// ticks. The identical SQL and the identical honesty rule apply — a block
// whose real source is absent reports available=false with a reason, never a
// zero. Read-only: no write, no migration, no state mutation.
func (s *Server) toolMetricsGet(ctx context.Context) (string, error) {
	now := s.clock().Now().UTC()
	// RFC3339 cutoff for the windowed blocks: every predicate compares with
	// julianday() because spawned_at/completed_at are RFC3339 TEXT and the
	// fleet holds both UTC ("...Z") and local-offset rows.
	cutoff := now.Add(-mcpMetricsWindow).Format(time.RFC3339)
	return jsonString(map[string]interface{}{
		"generated_at":     now.Format(time.RFC3339),
		"uptime_s":         int64(s.clock().Since(s.started).Seconds()),
		"sources":          mcpMetricsSources(),
		"unavailable":      mcpMetricsUnavailableFields(),
		"spawns":           s.metricsSpawns(ctx, cutoff),
		"deferrals":        s.metricsDeferrals(),
		"nudges":           s.metricsNudges(ctx),
		"ticks":            s.metricsTicks(ctx),
		"tick_duration_ms": s.metricsTickDurations(ctx, cutoff),
		"gateway":          s.metricsGateway(ctx, cutoff),
		"outcomes":         s.metricsOutcomes(ctx, cutoff),
	}), nil
}

// metricsSpawns answers question 1: spawn volume per namespace and per outcome
// over the window. An empty by_namespace map is written as {} (never null).
func (s *Server) metricsSpawns(ctx context.Context, cutoff string) map[string]interface{} {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE julianday(spawned_at) >= julianday(?)`,
		cutoff).Scan(&total); err != nil {
		return metricsUnavailable("spawns", "query failed: "+err.Error())
	}

	byNamespace := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(p.namespace_id, ''), '-') AS ns, COUNT(*)
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE julianday(t.spawned_at) >= julianday(?)
GROUP BY ns
ORDER BY ns`, cutoff)
	if err != nil {
		return metricsUnavailable("spawns", "namespace query failed: "+err.Error())
	}
	for rows.Next() {
		var ns string
		var n int
		if err := rows.Scan(&ns, &n); err != nil {
			rows.Close()
			return metricsUnavailable("spawns", "namespace scan failed: "+err.Error())
		}
		byNamespace[ns] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return metricsUnavailable("spawns", "namespace query failed: "+err.Error())
	}
	rows.Close()

	// The four values the ticks.outcome CHECK constraint allows are always
	// present (0 = the query found none), so a consumer can read
	// by_outcome.committed without a presence check.
	byOutcome := map[string]int{"committed": 0, "dry_run": 0, "failed": 0, "timeout": 0}
	orows, err := s.db.QueryContext(ctx, `
SELECT outcome, COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?) AND outcome IS NOT NULL
GROUP BY outcome`, cutoff)
	if err != nil {
		return metricsUnavailable("spawns", "outcome query failed: "+err.Error())
	}
	for orows.Next() {
		var outcome string
		var n int
		if err := orows.Scan(&outcome, &n); err != nil {
			orows.Close()
			return metricsUnavailable("spawns", "outcome scan failed: "+err.Error())
		}
		if _, known := byOutcome[outcome]; known {
			byOutcome[outcome] = n
		}
	}
	if err := orows.Err(); err != nil {
		orows.Close()
		return metricsUnavailable("spawns", "outcome query failed: "+err.Error())
	}
	orows.Close()

	return map[string]interface{}{
		"available":    true,
		"window":       mcpMetricsWindowLabel,
		"total":        total,
		"by_namespace": byNamespace,
		"by_outcome":   byOutcome,
	}
}

// metricsDeferrals answers question 2 from the SCHED-GAP-155 admission
// counters. Those counters live in the Loop's memory only, so a server built
// without a Loop reports the block unavailable with the reason instead of an
// invented zero map.
func (s *Server) metricsDeferrals() map[string]interface{} {
	if s.loop == nil {
		return metricsUnavailable("deferrals",
			"no scheduler Loop attached to this MCP server: the SCHED-GAP-155 admission counters are per-process in-memory state owned by the Loop; nothing is persisted, so there is no source to query")
	}
	counters := s.loop.AdmissionCounters()
	byReason := make(map[string]int, len(counters))
	admitted := map[string]int{}
	passes := 0
	for key, n := range counters {
		switch {
		case key == "passes":
			passes = n
		case strings.HasPrefix(key, "admitted:"):
			admitted[strings.TrimPrefix(key, "admitted:")] = n
		default:
			byReason[key] = n
		}
	}
	return map[string]interface{}{
		"available":             true,
		"by_reason":             byReason,
		"admitted_by_namespace": admitted,
		"passes":                passes,
	}
}

// metricsNudges answers question 3: orphan re-nudges grouped by the drop path
// that orphaned the tick. The source is persisted (ticks.nudge_count +
// ticks.orphan_reason), so this block is all-time.
func (s *Server) metricsNudges(ctx context.Context) map[string]interface{} {
	byPath := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(orphan_reason, ''), 'unknown') AS path, SUM(nudge_count)
FROM ticks
WHERE nudge_count > 0
GROUP BY path
ORDER BY path`)
	if err != nil {
		return metricsUnavailable("nudges", "query failed: "+err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var n int
		if err := rows.Scan(&path, &n); err != nil {
			return metricsUnavailable("nudges", "scan failed: "+err.Error())
		}
		byPath[path] = n
	}
	if err := rows.Err(); err != nil {
		return metricsUnavailable("nudges", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available": true,
		"by_path":   byPath,
	}
}

// metricsTicks answers questions 4 and 5: the live occupancy gauges against
// both caps, and the cooldown-expired-but-unscheduled count. global_cap is
// deliberately ABSENT (see mcpMetricsUnavailableFields) — this server holds no
// resolved-config snapshot, and emitting a zero would claim "no global cap".
func (s *Server) metricsTicks(ctx context.Context) map[string]interface{} {
	var active int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&active); err != nil {
		return metricsUnavailable("ticks", "active query failed: "+err.Error())
	}

	var queued int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE status = 'queued'`).Scan(&queued); err != nil {
		return metricsUnavailable("ticks", "queued query failed: "+err.Error())
	}

	byNamespace := map[string]map[string]int{}
	namespaces, err := database.ListNamespaces(ctx, s.db, false)
	if err != nil {
		return metricsUnavailable("ticks", "namespace query failed: "+err.Error())
	}
	for _, ns := range namespaces {
		byNamespace[ns.ID] = map[string]int{"active": 0, "queued": 0, "cap": ns.MaxConcurrent}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(p.namespace_id, ''), '-') AS ns, t.status, COUNT(*)
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE t.status IN ('running', 'queued')
GROUP BY ns, t.status`)
	if err != nil {
		return metricsUnavailable("ticks", "namespace occupancy query failed: "+err.Error())
	}
	for rows.Next() {
		var ns, status string
		var n int
		if err := rows.Scan(&ns, &status, &n); err != nil {
			rows.Close()
			return metricsUnavailable("ticks", "namespace occupancy scan failed: "+err.Error())
		}
		entry, ok := byNamespace[ns]
		if !ok {
			// A namespace can be missing from the namespaces table (project
			// row points at a deleted/unknown id) — report the occupancy
			// with no cap rather than dropping it.
			entry = map[string]int{"active": 0, "queued": 0, "cap": 0}
			byNamespace[ns] = entry
		}
		switch status {
		case "running":
			entry["active"] = n
		case "queued":
			entry["queued"] = n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return metricsUnavailable("ticks", "namespace occupancy query failed: "+err.Error())
	}
	rows.Close()

	// Question 5. A project is counted when it is enabled, owns no live tick
	// (running or queued — a queued row already owns a future slot) and its
	// wall-clock cooldown has elapsed. An active bump owns the gate, exactly
	// like the packer's selection paths.
	var cooldownExpired int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM projects p
WHERE p.enabled = 1
  AND NOT EXISTS (SELECT 1 FROM ticks t WHERE t.project_name = p.name AND t.status IN ('running', 'queued'))
  AND (
        p.last_tick_completed IS NULL
     OR p.last_tick_completed = ''
     OR (julianday('now') - julianday(p.last_tick_completed)) * 86400.0
            >= CASE WHEN COALESCE(p.bump_active, 0) = 1 AND COALESCE(p.bump_cooldown_s, 0) > 0
                    THEN p.bump_cooldown_s ELSE p.cooldown_s END
      )`).Scan(&cooldownExpired); err != nil {
		return metricsUnavailable("ticks", "cooldown query failed: "+err.Error())
	}

	return map[string]interface{}{
		"available":                    true,
		"active":                       active,
		"queued":                       queued,
		"by_namespace":                 byNamespace,
		"cooldown_expired_unscheduled": cooldownExpired,
	}
}

// metricsTickDurations answers question 6. Returns either the duration block
// or the unavailable shape (interface{} so both marshal identically).
func (s *Server) metricsTickDurations(ctx context.Context, cutoff string) interface{} {
	rows, err := s.db.QueryContext(ctx, `
SELECT CAST(ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000.0) AS INTEGER)
FROM ticks
WHERE completed_at IS NOT NULL AND completed_at <> ''
  AND spawned_at IS NOT NULL AND spawned_at <> ''
  AND julianday(completed_at) >= julianday(?)
  AND julianday(completed_at) >= julianday(spawned_at)`, cutoff)
	if err != nil {
		return metricsUnavailable("tick_duration_ms", "query failed: "+err.Error())
	}
	defer rows.Close()

	var samples []int
	for rows.Next() {
		var ms int
		if err := rows.Scan(&ms); err != nil {
			return metricsUnavailable("tick_duration_ms", "scan failed: "+err.Error())
		}
		samples = append(samples, ms)
	}
	if err := rows.Err(); err != nil {
		return metricsUnavailable("tick_duration_ms", "query failed: "+err.Error())
	}

	block := map[string]interface{}{
		"count":  len(samples),
		"window": mcpMetricsWindowLabel,
		"p50":    nil,
		"p90":    nil,
		"p99":    nil,
	}
	if len(samples) > 0 {
		sort.Ints(samples)
		block["p50"] = nearestRankPercentile(samples, 50)
		block["p90"] = nearestRankPercentile(samples, 90)
		block["p99"] = nearestRankPercentile(samples, 99)
	}
	return block
}

// nearestRankPercentile returns the nearest-rank percentile of an ASCENDING
// sample: idx = ceil(pct/100 * n) - 1, clamped to [0, n-1] (the definition
// pinned by SCHED-GAP-156 §2.1). Integer arithmetic keeps a float product like
// 0.9*10 from landing on the wrong side of an integer boundary. Callers must
// pass a non-empty sample.
func nearestRankPercentile(ascending []int, pct int) int {
	n := len(ascending)
	if n == 0 {
		return 0
	}
	idx := (pct*n+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx > n-1 {
		idx = n - 1
	}
	return ascending[idx]
}

// metricsGateway answers question 7: drain-class 503s from the ticks table's
// error text (the only persisted, drain-classifying source).
func (s *Server) metricsGateway(ctx context.Context, cutoff string) map[string]interface{} {
	var drains int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND error IS NOT NULL AND error <> ''
  AND (error LIKE '%503%' OR error LIKE '%draining%')`, cutoff).Scan(&drains); err != nil {
		return metricsUnavailable("gateway", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available": true,
		"drain_503": drains,
		"window":    mcpMetricsWindowLabel,
	}
}

// metricsOutcomes answers question 8: ticks that recorded outcome='committed'
// while committing nothing.
func (s *Server) metricsOutcomes(ctx context.Context, cutoff string) map[string]interface{} {
	var zeroOutput int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND outcome = 'committed'
  AND COALESCE(commits, 0) = 0`, cutoff).Scan(&zeroOutput); err != nil {
		return metricsUnavailable("outcomes", "query failed: "+err.Error())
	}
	return map[string]interface{}{
		"available":             true,
		"zero_output_committed": zeroOutput,
		"window":                mcpMetricsWindowLabel,
	}
}
