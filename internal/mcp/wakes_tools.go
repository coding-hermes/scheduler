package mcp

// wakes_tools.go — CTL-003 parity for SCHED-GAP-17002 wake tables.
//
// The daemon serves /api/v1/wakes (GET/POST), /api/v1/wakes/{id}
// (GET/DELETE) and /api/v1/wakes/{id}/fire (POST). CTL-003 says every
// control/data route ships an MCP tool, so these five mirror the five
// operations. They read and write the SAME wakes table the runner fires
// from — an agent scheduling a wake sees exactly what the daemon will
// honour, with no second copy of the truth.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SetWakeFirer installs the wake-table runner's external-fire entry (called
// by main.go next to the API wiring). nil = wakes_fire answers an explicit
// configuration error rather than pretending to fire.
func (s *Server) SetWakeFirer(fn func(ctx context.Context, id string) (any, bool, error)) {
	s.wakeFirer = fn
}

func wakeTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "wakes_list",
			Description: "Wake tables (SCHED-GAP-17002): list persisted wake-ups — promises to run a lane at a future time or on an external event — newest-first. Pass `state` (pending|firing|fired|cancelled|skipped) to filter; rows are never deleted, so the full history is the audit. Mirrors GET /api/v1/wakes.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"state": map[string]interface{}{"type": "string", "description": "Optional state filter"},
				},
			},
		},
		{
			Name:        "wakes_get",
			Description: "One wake row: project, kind, when, payload, state, created_by/at, fired_at and the tick id it produced. Mirrors GET /api/v1/wakes/{id}.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "Wake id (wk-…)"},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "wakes_create",
			Description: "Schedule a wake — a persisted promise to run one lane later. EXACTLY ONE scheduling clause: `at` (RFC3339 one-shot), `in_minutes` (one-shot), `daily` (local HH:MM, re-arms each day) or `external` (fires only when wakes_fire is called). A fired wake spawns through the operator path (manual bypass, provenance wake_table); a blackout window defers it; a busy lane skips it (recorded, retried for dailies). Mirrors POST /api/v1/wakes.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"project":    map[string]interface{}{"type": "string", "description": "Lane name (must exist and be enabled)"},
					"at":         map[string]interface{}{"type": "string", "description": "RFC3339 one-shot instant, e.g. 2026-11-09T02:00:00-05:00"},
					"in_minutes": map[string]interface{}{"type": "integer", "description": "One-shot N minutes from now"},
					"daily":      map[string]interface{}{"type": "string", "description": "Local HH:MM, recurring"},
					"external":   map[string]interface{}{"type": "boolean", "description": "true = fire via wakes_fire when an external event happens"},
					"payload":    map[string]interface{}{"type": "string", "description": "Opaque; carried onto the wake event and the tick"},
					"note":       map[string]interface{}{"type": "string"},
					"created_by": map[string]interface{}{"type": "string", "description": "Who is scheduling this (defaults to mcp)"},
				},
				"required": []string{"project"},
			},
		},
		{
			Name:        "wakes_cancel",
			Description: "Cancel a PENDING wake (a state change — the record stays as the audit). Mirrors DELETE /api/v1/wakes/{id}. 409-equivalent when the wake already fired or was cancelled.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "Wake id to cancel"},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "wakes_fire",
			Description: "Fire an EXTERNAL wake now — the external-event entry (\"my deploy finished, run the QA lane\"). Only pending external wakes fire; the spawn rides the operator path with wake_table provenance. Mirrors POST /api/v1/wakes/{id}/fire.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "string", "description": "Wake id to fire"},
				},
				"required": []string{"id"},
			},
		},
	}
}

func (s *Server) toolWakesList(_ context.Context, args map[string]interface{}) (string, error) {
	state := getStringArg(args, "state")
	wakes, err := database.ListWakes(context.Background(), s.db, state)
	if err != nil {
		return "", err
	}
	return marshalJSONIndent(map[string]interface{}{"wakes": wakes, "count": len(wakes)})
}

func (s *Server) toolWakesGet(_ context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	w, err := database.GetWake(context.Background(), s.db, id)
	if err != nil {
		return "", fmt.Errorf("wake not found: %s", id)
	}
	return marshalJSONIndent(w)
}

func (s *Server) toolWakesCreate(ctx context.Context, args map[string]interface{}) (string, error) {
	project := getStringArg(args, "project")
	if project == "" {
		return "", fmt.Errorf("project is required")
	}
	p, err := database.GetProject(ctx, s.db, project)
	if err != nil {
		return "", fmt.Errorf("project not found: %s", project)
	}
	if !p.Enabled {
		return "", fmt.Errorf("project is disabled — a wake for a disabled lane would be a promise the runner has to skip")
	}

	w := database.Wake{
		Project:   project,
		Payload:   getStringArg(args, "payload"),
		Note:      getStringArg(args, "note"),
		CreatedBy: getStringArg(args, "created_by"),
	}
	if w.CreatedBy == "" {
		w.CreatedBy = "mcp"
	}

	clauses := 0
	now := s.clock().Now()
	if at := getStringArg(args, "at"); at != "" {
		t, perr := time.Parse(time.RFC3339, at)
		if perr != nil {
			return "", fmt.Errorf("at must be RFC3339: %v", perr)
		}
		if !t.After(now) {
			return "", fmt.Errorf("at is in the past — a wake that fires immediately is a spawn request, use the spawn tool")
		}
		w.Kind, w.AtTime = database.WakeKindAt, &t
		clauses++
	}
	if v, ok := args["in_minutes"].(float64); ok {
		if v <= 0 {
			return "", fmt.Errorf("in_minutes must be positive")
		}
		t := now.Add(time.Duration(v) * time.Minute)
		w.Kind, w.AtTime = database.WakeKindAt, &t
		clauses++
	}
	if daily := getStringArg(args, "daily"); daily != "" {
		w.Kind = database.WakeKindDaily
		w.DailyHHMM = daily
		clauses++
	}
	if v, ok := args["external"].(bool); ok && v {
		w.Kind = database.WakeKindExternal
		clauses++
	}
	if clauses != 1 {
		return "", fmt.Errorf("exactly one of at | in_minutes | daily | external is required")
	}
	if w.Kind == database.WakeKindDaily {
		first, err := firstDailyFromHHMM(w.DailyHHMM, now)
		if err != nil {
			return "", err
		}
		w.AtTime = &first
	}

	id, err := database.CreateWake(ctx, s.db, &w, now)
	if err != nil {
		return "", err
	}
	stored, err := database.GetWake(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	return marshalJSONIndent(map[string]interface{}{"status": "scheduled", "id": id, "wake": stored})
}

func (s *Server) toolWakesCancel(_ context.Context, args map[string]interface{}) (string, error) {
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	ok, err := database.CancelWake(context.Background(), s.db, id, "mcp")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("wake %s is not pending — only a pending wake can be cancelled", id)
	}
	return marshalJSONIndent(map[string]interface{}{"status": "cancelled", "id": id})
}

func (s *Server) toolWakesFire(ctx context.Context, args map[string]interface{}) (string, error) {
	if s.wakeFirer == nil {
		return "", fmt.Errorf("wake-table runner not wired — cannot fire")
	}
	id := getStringArg(args, "id")
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	w, ok, err := s.wakeFirer(ctx, id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("wake %s is not pending (already fired, cancelled, or unknown)", id)
	}
	return marshalJSONIndent(map[string]interface{}{"status": "fired", "wake": w})
}

// firstDailyFromHHMM validates a local "HH:MM" and returns its first
// occurrence strictly after now (today, or tomorrow once passed).
func firstDailyFromHHMM(hhmm string, now time.Time) (time.Time, error) {
	var h, m int
	if n, err := fmt.Sscanf(hhmm, "%02d:%02d", &h, &m); err != nil || n != 2 || h > 23 || m > 59 || len(hhmm) != 5 {
		return time.Time{}, fmt.Errorf(`daily must be local "HH:MM", got %q`, hhmm)
	}
	first := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !first.After(now) {
		first = first.AddDate(0, 0, 1)
	}
	return first, nil
}

func marshalJSONIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
