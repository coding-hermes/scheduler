package mcp_test

// CTL-003 — live behaviour coverage for the namespace / project-lifecycle /
// introspection tools added for MCP-API parity.
//
// mcp_api_parity_test.go proves every /api/v1 operation is COVERED by a tool
// in the live registry. Coverage is not behaviour, so this file drives each
// new tool through the real JSON-RPC endpoint against an in-memory DB and
// asserts what it actually does: the REST-mirrored guard ORDER (confirm →
// existence → enabled, reason → ticks → cooldown), the DB effects (rows
// created/updated/deleted/unassigned, disable and purge provenance events in
// the events table), the error classification, and the payload shapes
// (tick_get's two envelopes, queue ordering, metrics' per-block
// available=true|false honesty rule and its declared underivable field).
//
// project_spawn is exercised on its guard paths only: the happy path fires a
// real spawn session, which is out of scope for a unit test.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ctlCallTool drives one tools/call and returns the tool text or its JSON-RPC
// error message.
func ctlCallTool(t *testing.T, m *mcpTestServer, name string, args map[string]interface{}) (string, error) {
	t.Helper()
	params := map[string]interface{}{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params,
	})
	if resp.Error != nil {
		return "", errors.New(resp.Error.Message)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("%s: unmarshal result: %v (raw=%s)", name, err, b)
	}
	if len(parsed.Content) == 0 {
		t.Fatalf("%s: empty content (raw=%s)", name, b)
	}
	return parsed.Content[0].Text, nil
}

// ctlMustTool calls a tool, requires success, and returns the decoded object.
func ctlMustTool(t *testing.T, m *mcpTestServer, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	text, err := ctlCallTool(t, m, name, args)
	if err != nil {
		t.Fatalf("%s%v: unexpected error: %v", name, args, err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: non-object payload %q: %v", name, text, err)
	}
	return out
}

// ctlMustToolErr requires the tool to fail with a message containing want —
// the MCP stand-in for the REST handler's 400/404/409 classification.
func ctlMustToolErr(t *testing.T, m *mcpTestServer, name string, args map[string]interface{}, want string) {
	t.Helper()
	_, err := ctlCallTool(t, m, name, args)
	if err == nil {
		t.Fatalf("%s%v: want error containing %q, got success", name, args, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s%v: error %q does not contain %q", name, args, err.Error(), want)
	}
}

func ctlNum(t *testing.T, v interface{}, what string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: not a number: %T (%v)", what, v, v)
	}
	return f
}

// TestCTL003ParityToolsBehaviour walks the new tools end to end: namespaces
// lifecycle, project delete/bump/unbump, tick detail, and the config/queue/
// metrics reads.
func TestCTL003ParityToolsBehaviour(t *testing.T) {
	m := newMCPTestServer(t)
	m.loop.SetNoExecFallback(true)
	ctx := context.Background()

	// ── namespaces: create ────────────────────────────────────────────────
	created := ctlMustTool(t, m, "namespaces_create", map[string]interface{}{"id": "ns-a", "weight": 10, "max_concurrent": 2})
	if created["id"] != "ns-a" || ctlNum(t, created["weight"], "weight") != 10 {
		t.Fatalf("namespaces_create payload = %v", created)
	}
	ctlMustToolErr(t, m, "namespaces_create", map[string]interface{}{"id": "ns-a", "weight": 10}, "already exists")
	ctlMustToolErr(t, m, "namespaces_create", map[string]interface{}{"id": "ns-b"}, "weight must be greater than 0")
	ctlMustToolErr(t, m, "namespaces_create", map[string]interface{}{"weight": 10}, "id is required")

	// ── namespaces: list / get ────────────────────────────────────────────
	list := ctlMustTool(t, m, "namespaces_list", nil)
	if n := len(list["namespaces"].([]interface{})); n != 1 {
		t.Fatalf("namespaces_list = %v, want 1 entry", list)
	}
	if got := ctlMustTool(t, m, "namespaces_get", map[string]interface{}{"id": "ns-a"}); got["id"] != "ns-a" {
		t.Fatalf("namespaces_get payload = %v", got)
	}
	ctlMustToolErr(t, m, "namespaces_get", map[string]interface{}{"id": "nope"}, "namespace not found")
	ctlMustToolErr(t, m, "namespaces_get", nil, "id is required")

	// ── namespaces: update (top-level patch, and nested patch) ────────────
	upd := ctlMustTool(t, m, "namespaces_update", map[string]interface{}{"id": "ns-a", "max_concurrent": 3, "description": "verify"})
	if ctlNum(t, upd["max_concurrent"], "max_concurrent") != 3 || upd["description"] != "verify" {
		t.Fatalf("namespaces_update payload = %v", upd)
	}
	nested := ctlMustTool(t, m, "namespaces_update", map[string]interface{}{"id": "ns-a", "patch": map[string]interface{}{"weight": 22}})
	if ctlNum(t, nested["weight"], "weight") != 22 {
		t.Fatalf("nested patch ignored: %v", nested)
	}
	ctlMustToolErr(t, m, "namespaces_update", map[string]interface{}{"id": "ns-a", "admission_mode": "bogus"}, "invalid admission_mode")
	ctlMustToolErr(t, m, "namespaces_update", map[string]interface{}{"id": "ghost", "weight": 5}, "namespace not found")

	// ── namespaces: move / projects ───────────────────────────────────────
	mustCreateMCPProject(t, m.db, "proj-a")
	moved := ctlMustTool(t, m, "namespaces_move", map[string]interface{}{"id": "ns-a", "project": "proj-a"})
	if moved["namespace_id"] != "ns-a" {
		t.Fatalf("namespaces_move did not assign namespace: %v", moved)
	}
	ctlMustToolErr(t, m, "namespaces_move", map[string]interface{}{"id": "ns-a", "project": "ghost"}, "project not found")
	ctlMustToolErr(t, m, "namespaces_move", map[string]interface{}{"id": "ns-a"}, "project is required")
	projList := ctlMustTool(t, m, "namespaces_projects", map[string]interface{}{"id": "ns-a"})
	if n := len(projList["projects"].([]interface{})); n != 1 {
		t.Fatalf("namespaces_projects = %v, want 1 member", projList)
	}

	// ── namespaces: delete guards (confirm → enabled members → purge) ─────
	ctlMustToolErr(t, m, "namespaces_delete", map[string]interface{}{"id": "ns-a"}, "confirm=true is required")
	ctlMustToolErr(t, m, "namespaces_delete", map[string]interface{}{"id": "ns-a", "confirm": true}, "enabled project(s) assigned")
	ctlMustToolErr(t, m, "namespaces_delete", map[string]interface{}{"id": "ghost", "confirm": true}, "namespace not found")
	ctlMustTool(t, m, "fleet_pause", map[string]interface{}{"name": "proj-a"})
	if del := ctlMustTool(t, m, "namespaces_delete", map[string]interface{}{"id": "ns-a", "confirm": true}); del["status"] != "deleted" {
		t.Fatalf("namespaces_delete soft = %v", del)
	}
	// Soft delete retains the row (referential validity) but disables it and
	// unassigns its members.
	if after := ctlMustTool(t, m, "namespaces_get", map[string]interface{}{"id": "ns-a"}); after["enabled"] != false {
		t.Fatalf("soft-deleted namespace still enabled: %v", after)
	}
	if unassigned, err := database.GetProject(ctx, m.db, "proj-a"); err != nil || unassigned.NamespaceID != nil {
		t.Fatalf("member not unassigned: %+v err=%v", unassigned, err)
	}
	ctlMustTool(t, m, "namespaces_create", map[string]interface{}{"id": "ns-purge", "weight": 5})
	if purged := ctlMustTool(t, m, "namespaces_delete", map[string]interface{}{"id": "ns-purge", "confirm": true, "purge": true}); purged["status"] != "purged" {
		t.Fatalf("namespaces_delete purge = %v", purged)
	}
	ctlMustToolErr(t, m, "namespaces_get", map[string]interface{}{"id": "ns-purge"}, "namespace not found")

	// ── project_delete: guards, provenance event, purge ───────────────────
	mustCreateMCPProject(t, m.db, "proj-b")
	ctlMustToolErr(t, m, "project_delete", map[string]interface{}{"name": "proj-b"}, "confirm=true is required")
	ctlMustToolErr(t, m, "project_delete", map[string]interface{}{"name": "proj-b", "confirm": true}, "is enabled")
	ctlMustToolErr(t, m, "project_delete", map[string]interface{}{"name": "ghost", "confirm": true}, "project not found")
	ctlMustTool(t, m, "fleet_pause", map[string]interface{}{"name": "proj-b"})
	if softDel := ctlMustTool(t, m, "project_delete", map[string]interface{}{"name": "proj-b", "confirm": true}); softDel["status"] != "deleted" {
		t.Fatalf("project_delete soft = %v", softDel)
	}
	if pb, err := database.GetProject(ctx, m.db, "proj-b"); err != nil || pb.Enabled {
		t.Fatalf("soft delete did not disable proj-b: %+v err=%v", pb, err)
	}
	var disabledEvents int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE message LIKE 'project disabled: proj-b%'`).Scan(&disabledEvents); err != nil {
		t.Fatalf("event query: %v", err)
	}
	if disabledEvents == 0 {
		t.Fatal("project_delete soft path wrote no disable-provenance event")
	}
	mustCreateMCPProject(t, m.db, "proj-c")
	ctlMustTool(t, m, "fleet_pause", map[string]interface{}{"name": "proj-c"})
	ctlMustTool(t, m, "project_delete", map[string]interface{}{"name": "proj-c", "confirm": true, "purge": true})
	if _, err := database.GetProject(ctx, m.db, "proj-c"); !errors.Is(err, database.ErrProjectNotFound) {
		t.Fatalf("purge left the row: err=%v", err)
	}
	var purgeEvents int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE message = 'project purged (hard delete): proj-c'`).Scan(&purgeEvents); err != nil {
		t.Fatalf("purge event query: %v", err)
	}
	if purgeEvents == 0 {
		t.Fatal("project_delete purge wrote no audit event")
	}

	// ── project_bump / project_unbump ─────────────────────────────────────
	mustCreateMCPProject(t, m.db, "proj-d")
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "proj-d"}, "reason is required")
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "proj-d", "reason": "x", "ticks": 9}, "ticks must be 1..8")
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "proj-d", "reason": "x", "cooldown": 60}, "cooldown must be >=")
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "proj-b", "reason": "x"}, "is disabled")
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "ghost", "reason": "x"}, "project not found")
	bumped := ctlMustTool(t, m, "project_bump", map[string]interface{}{"name": "proj-d", "reason": "CTL-003 verify"})
	if bumped["bump_active"] != true || ctlNum(t, bumped["bump_remaining_ticks"], "bump_remaining_ticks") != 5 ||
		ctlNum(t, bumped["bump_cooldown_s"], "bump_cooldown_s") != 7200 {
		t.Fatalf("bump defaults wrong: %v", bumped)
	}
	ctlMustToolErr(t, m, "project_bump", map[string]interface{}{"name": "proj-d", "reason": "again"}, "bump already active")
	if cleared := ctlMustTool(t, m, "project_unbump", map[string]interface{}{"name": "proj-d"}); cleared["bump_active"] != false {
		t.Fatalf("unbump did not clear: %v", cleared)
	}
	ctlMustToolErr(t, m, "project_unbump", map[string]interface{}{"name": "proj-d"}, "no active bump")
	ctlMustToolErr(t, m, "project_unbump", map[string]interface{}{"name": "ghost"}, "project not found")
	var bumpEvents int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE message LIKE 'project bumped: proj-d%' OR message = 'bump manually cleared: proj-d'`).Scan(&bumpEvents); err != nil {
		t.Fatalf("bump event query: %v", err)
	}
	if bumpEvents != 2 {
		t.Fatalf("bump/unbump audit events = %d, want 2", bumpEvents)
	}

	// ── tick_get: serial tick vs worker-wave envelope ─────────────────────
	now := time.Now().UTC().Format(time.RFC3339)
	serial := &database.Tick{ID: "tick-serial-1", ProjectName: "proj-d", Status: database.StatusRunning, SpawnedAt: now}
	if err := database.CreateTick(ctx, m.db, serial); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	tickPayload := ctlMustTool(t, m, "tick_get", map[string]interface{}{"id": "tick-serial-1"})
	if tickPayload["id"] != "tick-serial-1" || tickPayload["status"] != "running" {
		t.Fatalf("tick_get payload = %v", tickPayload)
	}
	if _, hasWorkers := tickPayload["tick_workers"]; hasWorkers {
		t.Fatalf("serial tick must not carry tick_workers: %v", tickPayload)
	}
	mustCreateMCPProject(t, m.db, "wave-a")
	wave := &database.Tick{ID: "tick-wave-1", ProjectName: "wave-a", Status: database.StatusRunning, SpawnedAt: now, WorkerCount: 1}
	if err := database.CreateTick(ctx, m.db, wave); err != nil {
		t.Fatalf("CreateTick wave: %v", err)
	}
	if _, err := database.CreateTickWorker(ctx, m.db, &database.TickWorker{
		TickID: "tick-wave-1", TaskID: "TASK-1", Branch: "wt/task-1",
		Judge: "pass", Merge: "merged", State: "done",
	}); err != nil {
		t.Fatalf("CreateTickWorker: %v", err)
	}
	wavePayload := ctlMustTool(t, m, "tick_get", map[string]interface{}{"id": "tick-wave-1"})
	workers, ok := wavePayload["tick_workers"].([]interface{})
	if !ok || len(workers) != 1 {
		t.Fatalf("tick_get wave envelope = %v", wavePayload)
	}
	if worker, _ := workers[0].(map[string]interface{}); worker["task_id"] != "TASK-1" {
		t.Fatalf("tick_get worker row = %v", workers[0])
	}
	ctlMustToolErr(t, m, "tick_get", map[string]interface{}{"id": "ghost"}, "tick not found")
	ctlMustToolErr(t, m, "tick_get", nil, "id is required")

	// ── project_spawn: guard paths (the happy path fires a real session) ──
	ctlMustToolErr(t, m, "project_spawn", map[string]interface{}{"name": "ghost"}, "project not found")
	ctlMustToolErr(t, m, "project_spawn", nil, "name is required")
	mustCreateMCPProject(t, m.db, "spawn-a")
	queued := &database.Tick{ID: "tick-queued-1", ProjectName: "spawn-a", Status: database.StatusQueued, SpawnedAt: now}
	if err := database.CreateTick(ctx, m.db, queued); err != nil {
		t.Fatalf("CreateTick queued: %v", err)
	}
	// The scheduler's own duplicate-spawn refusal (ErrProjectRunning), which
	// proves the tool reaches Loop.SpawnNow rather than stopping at a lookup.
	ctlMustToolErr(t, m, "project_spawn", map[string]interface{}{"name": "spawn-a"}, "already has a tick in flight")

	// ── config_get ────────────────────────────────────────────────────────
	cfg := ctlMustTool(t, m, "config_get", nil)
	if ctlNum(t, cfg["weight_budget"], "weight_budget") != float64(m.loop.WeightBudget()) {
		t.Fatalf("config_get weight_budget = %v, want loop value %d", cfg["weight_budget"], m.loop.WeightBudget())
	}
	if cfg["paused"] != false {
		t.Fatalf("config_get paused = %v, want false", cfg["paused"])
	}
	if _, ok := cfg["db_path"]; !ok {
		t.Fatalf("config_get missing db_path: %v", cfg)
	}
	if omitted, _ := cfg["omitted"].([]interface{}); len(omitted) == 0 {
		t.Fatalf("config_get must list the omitted REST fields: %v", cfg)
	}

	// ── queue_get ─────────────────────────────────────────────────────────
	q := ctlMustTool(t, m, "queue_get", nil)
	entries, _ := q["queue"].([]interface{})
	if len(entries) == 0 || ctlNum(t, q["count"], "queue count") != float64(len(entries)) {
		t.Fatalf("queue_get returned nothing / count mismatch: %v", q)
	}
	var sawProjD bool
	for _, e := range entries {
		entry, _ := e.(map[string]interface{})
		if entry["project"] != "proj-d" {
			continue
		}
		sawProjD = true
		if ctlNum(t, entry["urgency"], "urgency") != ctlNum(t, entry["priority"], "priority") {
			t.Fatalf("queue_get urgency is not the documented priority-only fallback: %v", entry)
		}
	}
	if !sawProjD {
		t.Fatalf("queue_get omitted the enabled proj-d: %v", q)
	}

	// ── metrics_get ───────────────────────────────────────────────────────
	mt := ctlMustTool(t, m, "metrics_get", nil)
	for _, block := range []string{"spawns", "deferrals", "nudges", "ticks", "gateway", "outcomes"} {
		b, _ := mt[block].(map[string]interface{})
		if b == nil || b["available"] != true {
			t.Fatalf("metrics_get %s block = %v, want available=true", block, mt[block])
		}
	}
	dur, ok := mt["tick_duration_ms"].(map[string]interface{})
	if !ok || dur["count"] == nil {
		t.Fatalf("metrics_get tick_duration_ms = %v", mt["tick_duration_ms"])
	}
	ticksBlock, _ := mt["ticks"].(map[string]interface{})
	if _, has := ticksBlock["global_cap"]; has {
		t.Fatalf("metrics_get must not fabricate global_cap: %v", ticksBlock)
	}
	if unavailable, _ := mt["unavailable"].([]interface{}); len(unavailable) == 0 {
		t.Fatalf("metrics_get must declare the underivable field: %v", mt["unavailable"])
	}
	sources, _ := mt["sources"].(map[string]interface{})
	if len(sources) < 7 {
		t.Fatalf("metrics_get sources map incomplete: %v", sources)
	}
	// Two running rows (one serial, one wave) — active_ticks never counts
	// workers, so the wave tick contributes 1.
	if got := ctlNum(t, ticksBlock["active"], "active"); got != 2 {
		t.Fatalf("metrics_get active ticks = %v, want 2", got)
	}
	spawns, _ := mt["spawns"].(map[string]interface{})
	if got := ctlNum(t, spawns["total"], "spawns total"); got != 3 {
		t.Fatalf("metrics_get spawns total = %v, want 3 rows inside the window", got)
	}
}
