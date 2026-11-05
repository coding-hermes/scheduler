package mcp

// hooks_tools.go — CTL-003 parity for S13 lane hooks.
//
// The daemon serves /api/v1/hooks, /api/v1/hooks/plan, /api/v1/hooks/reload and
// PUT/DELETE /api/v1/hooks/{id}. CTL-003 says every control/data route ships an
// MCP tool (the skip-list is for dashboard/human routes only), so these five
// tools mirror the five operations. They read and write the SAME *lanehook.Set
// the tick path uses, so an agent editing a hook sees exactly what the next tick
// will run — no second copy of the truth, and no cached shadow.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

// SetHooks installs the armed definition set (called by main.go once the daemon
// has loaded --hooks-file). A nil set leaves every hooks tool answering an
// explicit configuration error rather than an empty list that would read as
// "no hooks exist".
func (s *Server) SetHooks(set *lanehook.Set) { s.hooks = set }

func hooksTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "hooks_list",
			Description: "S13 lane hooks: the armed PRE/POST step definitions — the hooks file path, the ordering mode, and every step (id, phase, level, target, priority, command, args, timeout_s, on_failure, enabled). `error` is present when the last reload was rejected (the previous set stays armed). Mirrors GET /api/v1/hooks.",
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			Name:        "hooks_plan",
			Description: "Resolve the PRE/POST plan one lane would actually run, using the same resolver the tick path uses (lanehook.Resolve). Mirrors GET /api/v1/hooks/plan. `lane` is required; pass `workdir`/`repo` to substitute {workdir}/{repo} exactly as a real tick would.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"lane":      map[string]interface{}{"type": "string", "description": "Lane name, e.g. code-factory-qa"},
					"namespace": map[string]interface{}{"type": "string", "description": "Namespace id for namespace-level hooks"},
					"workdir":   map[string]interface{}{"type": "string", "description": "Worktree path substituted for {workdir}"},
					"repo":      map[string]interface{}{"type": "string", "description": "Source repository substituted for {repo}"},
				},
				"required": []string{"lane"},
			},
		},
		{
			Name:        "hooks_reload",
			Description: "Re-read the hooks file now (the daemon also reloads on mtime/size change). Mirrors POST /api/v1/hooks/reload. A rejected edit reports the error and the PREVIOUS set stays armed — a broken edit can never disarm a running fleet.",
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			Name:        "hooks_upsert",
			Description: "Create or replace one hook step by id, writing through to the hooks file (the file stays the single source of truth) and reloading the armed set. Mirrors PUT /api/v1/hooks/{id}. The step is validated by lanehook.Validate: an unknown {placeholder}, a relative command or a bad phase/level is refused before anything is written.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "Step id (path parameter on the REST surface)"},
					"step": map[string]interface{}{
						"type":        "object",
						"description": "Step body: phase (pre|post), level (global|namespace|lane), target, priority, command (absolute path), args[] (may use {lane},{project},{role},{namespace},{workdir},{repo},{tick},{phase},{hook}), timeout_s, on_failure (open|closed), enabled",
					},
				},
				"required": []string{"id", "step"},
			},
		},
		{
			Name:        "hooks_delete",
			Description: "Delete one hook step by id, writing through to the hooks file and reloading the armed set. Mirrors DELETE /api/v1/hooks/{id}. An unknown id is a named not-found error, not a silent success.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "Step id to delete"},
				},
				"required": []string{"id"},
			},
		},
	}
}

// hooksConfigured is the shared fail-closed precondition.
func (s *Server) hooksConfigured() error {
	if s.hooks == nil {
		return fmt.Errorf("hooks not configured (no --hooks-file)")
	}
	return nil
}

func (s *Server) toolHooksList(_ context.Context, _ map[string]interface{}) (string, error) {
	if err := s.hooksConfigured(); err != nil {
		return "", err
	}
	out := map[string]interface{}{
		"path":  s.hooks.Path(),
		"order": string(s.hooks.Order()),
		"steps": s.hooks.Steps(),
	}
	if err := s.hooks.LastError(); err != nil {
		out["error"] = err.Error()
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) toolHooksPlan(_ context.Context, args map[string]interface{}) (string, error) {
	if err := s.hooksConfigured(); err != nil {
		return "", err
	}
	lane := getStringArg(args, "lane")
	if lane == "" {
		return "", fmt.Errorf("lane is required")
	}
	skip := []string{}
	if spec, ok := s.hooks.Lane(lane); ok {
		skip = spec.SkipHooks
	}
	plan, err := s.hooks.PlanFor(lane, getStringArg(args, "namespace"), getStringArg(args, "workdir"), getStringArg(args, "repo"), skip)
	if err != nil {
		return "", err
	}
	out := map[string]interface{}{"lane": lane, "pre": plan.Pre, "post": plan.Post}
	if spec, ok := s.hooks.Lane(lane); ok {
		out["lane_spec"] = spec
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) toolHooksReload(_ context.Context, _ map[string]interface{}) (string, error) {
	if err := s.hooksConfigured(); err != nil {
		return "", err
	}
	changed, err := s.hooks.ReloadIfChanged()
	if err != nil {
		return "", fmt.Errorf("reload rejected (previous set stays armed): %w", err)
	}
	b, err := json.MarshalIndent(map[string]interface{}{
		"changed": changed,
		"steps":   len(s.hooks.Steps()),
		"path":    s.hooks.Path(),
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) toolHooksUpsert(_ context.Context, args map[string]interface{}) (string, error) {
	if err := s.hooksConfigured(); err != nil {
		return "", err
	}
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	raw, ok := args["step"]
	if !ok {
		return "", fmt.Errorf("step is required")
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("step is not JSON-encodable: %w", err)
	}
	var step lanehook.Step
	if err := json.Unmarshal(body, &step); err != nil {
		return "", fmt.Errorf("invalid step: %w", err)
	}
	step.ID = id
	if step.Level == lanehook.LevelGlobal && step.Target == "" {
		step.Target = "*"
	}
	f, err := lanehook.LoadFile(s.hooks.Path())
	if err != nil {
		return "", fmt.Errorf("current file rejected: %w", err)
	}
	prev, err := f.Upsert(step)
	if err != nil {
		return "", err
	}
	if err := lanehook.SaveFile(s.hooks.Path(), f); err != nil {
		return "", err
	}
	if _, err := s.hooks.ReloadIfChanged(); err != nil {
		return "", fmt.Errorf("saved but not reloaded: %w", err)
	}
	out := map[string]interface{}{"id": id, "created": prev == nil, "steps": len(s.hooks.Steps())}
	if prev != nil {
		out["replaced"] = prev
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) toolHooksDelete(_ context.Context, args map[string]interface{}) (string, error) {
	if err := s.hooksConfigured(); err != nil {
		return "", err
	}
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	f, err := lanehook.LoadFile(s.hooks.Path())
	if err != nil {
		return "", fmt.Errorf("current file rejected: %w", err)
	}
	n := f.Delete(id)
	if n == 0 {
		return "", fmt.Errorf("no hook with id %s", id)
	}
	if err := lanehook.SaveFile(s.hooks.Path(), f); err != nil {
		return "", err
	}
	if _, err := s.hooks.ReloadIfChanged(); err != nil {
		return "", fmt.Errorf("saved but not reloaded: %w", err)
	}
	b, err := json.MarshalIndent(map[string]interface{}{"deleted": n, "id": id, "steps": len(s.hooks.Steps())}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
