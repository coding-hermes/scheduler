package mcp_test

// CTL-003 — MCP/API parity guard.
//
// The 2026-09-03 drift this guard exists for: openapi.json exposed
// groups/templates/deploy/events routes while the MCP tool registry
// covered a different subset. This test fails CI whenever an API
// operation ships without an MCP tool (or an explicit, hygiene-guarded
// skip-list entry), so the REST and MCP surfaces cannot drift apart
// silently again.
//
// Enumeration is LIVE on both sides — there is deliberately no static
// path list (a static list cannot catch new routes):
//   - API surface: GET /api/v1/openapi.json from a wired api.Server —
//     the same bytes the daemon serves, so every new route lands here
//     automatically the moment it is documented in the spec.
//   - MCP surface: JSON-RPC tools/list against a wired mcp.Server —
//     the registry as clients actually see it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// parityOp is one API operation (path + HTTP method) under the parity
// contract.
type parityOp struct {
	Method string
	Path   string
}

func (op parityOp) String() string { return op.Method + " " + op.Path }

// apiToolCoverage is the parity contract: every openapi operation that an
// MCP tool covers maps here to the covering tool name(s). An operation
// absent from this map must either be in paritySkipList (dashboard/human
// routes only) or the guard fails. Keep entries aligned with the tools var
// in server.go; a renamed tool fails the coverage_map_tools_exist subtest
// instead of silently un-covering its route.
var apiToolCoverage = map[string][]string{
	"GET /api/v1/status":                  {"fleet_status"},
	"GET /api/v1/projects":                {"fleet_projects"},
	"POST /api/v1/projects":               {"fleet_add"},
	"GET /api/v1/projects/{name}":         {"fleet_project_detail"},
	"PUT /api/v1/projects/{name}":         {"fleet_set_weight", "fleet_set_priority", "fleet_set_cooldown", "fleet_set_decay"},
	"POST /api/v1/projects/{name}/pause":  {"fleet_pause"},
	"POST /api/v1/projects/{name}/resume": {"fleet_resume"},
	"GET /api/v1/ticks":                   {"fleet_ticks"},
	"POST /api/v1/evaluate":               {"fleet_evaluate"},
	"POST /api/v1/pause":                  {"fleet_pause_scheduler"},
	"POST /api/v1/resume":                 {"fleet_resume_scheduler"},
	"GET /api/v1/events":                  {"events_list"},
	"GET /api/v1/groups":                  {"groups_list"},
	"POST /api/v1/groups":                 {"groups_create"},
	"GET /api/v1/groups/{name}":           {"groups_get"},
	"PUT /api/v1/groups/{name}":           {"groups_update"},
	"DELETE /api/v1/groups/{name}":        {"groups_delete"},
	"POST /api/v1/groups/{name}/deploy":   {"groups_deploy"},
	"GET /api/v1/templates":               {"templates_list"},
	"POST /api/v1/templates":              {"templates_create"},
	"GET /api/v1/templates/{name}":        {"templates_get"},
	"PUT /api/v1/templates/{name}":        {"templates_update"},
	"DELETE /api/v1/templates/{name}":     {"templates_delete"},
}

// paritySkipList exempts NON-control routes from the parity contract.
// ONLY dashboard/human/infra routes are skip-eligible; a data or control
// route (projects, namespaces, ticks, events, groups, templates, deploy,
// pause, resume, evaluate, config, status, queue, metrics) must NEVER be
// added here — ship the MCP tool instead. The skip_list_hygiene subtest
// enforces this mechanically so the escape hatch cannot be widened by
// convenience.
var paritySkipList = map[string]string{
	"/api/v1/health":       "human/ops daemon-health probe (uptime, DB ping, gateway error count); fleet_status covers fleet-level status, not daemon health",
	"/api/v1/openapi.json": "the OpenAPI document itself — served for humans and codegen, not a fleet operation",
	"/mcp":                 "the MCP JSON-RPC endpoint itself (never enumerated via openapi.json; listed so skip entries stay self-documenting)",
}

// allowedSkipKey reports whether a skip-list key is a dashboard/human/infra
// route per the CTL-003 brief. Anything else (in particular any /api/v1
// control route) makes skip_list_hygiene fail.
func allowedSkipKey(key string) bool {
	switch key {
	case "/", "/health", "/queue", "/ticks", "/mcp",
		"/api/v1/health", "/api/v1/openapi.json":
		return true
	}
	for _, prefix := range []string{"/projects/", "/namespaces/", "/dashboard/"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// newParityStack wires the two live servers the guard enumerates: an
// api.Server (same construction as the api package's own tests) and an
// mcp.Server, both over one in-memory DB.
func newParityStack(t *testing.T) (apiURL, mcpURL string) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// budget=0 keeps Pick empty so no test can spawn a real foreman; the
	// exec fallback stays off for the same reason (mirrors api tests).
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)

	apiSrv := api.NewServer(db, loop)
	storeDir := t.TempDir()
	apiSrv.SetBlocksStore(blocks.NewStore(
		filepath.Join(storeDir, "groups.jsonl"),
		filepath.Join(storeDir, "templates.jsonl"),
	))
	apiTS := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(apiTS.Close)

	mcpSrv := mcpserver.NewServer(db, loop)
	mcpTS := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(mcpTS.Close)

	return apiTS.URL, mcpTS.URL
}

// fetchOpenAPIOps enumerates the live API surface from the served
// openapi.json. Only real HTTP method entries count (path items may also
// carry non-method keys like parameters/summary).
func fetchOpenAPIOps(t *testing.T, baseURL string) []parityOp {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET openapi.json: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET openapi.json: status %d: %s", resp.StatusCode, body)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	// openapi.json method keys are lowercase ("get", "post") — keep this
	// filter map lowercase to match (http.Method* constants are uppercase).
	methods := map[string]bool{
		"get": true, "post": true, "put": true,
		"delete": true, "patch": true, "head": true,
		"options": true,
	}
	var ops []parityOp
	for path, item := range spec.Paths {
		for m := range item {
			// openapi.json method keys are lowercase ("get", "post");
			// normalize before the filter and the parity key.
			lm := strings.ToLower(m)
			if methods[lm] {
				ops = append(ops, parityOp{Method: strings.ToUpper(lm), Path: path})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
	return ops
}

// fetchMCPToolNames enumerates the live MCP registry through the JSON-RPC
// tools/list surface — the same view an MCP client gets.
func fetchMCPToolNames(t *testing.T, baseURL string) map[string]bool {
	t.Helper()
	reqBody, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if err != nil {
		t.Fatalf("marshal tools/list: %v", err)
	}
	resp, err := http.Post(baseURL+"/mcp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /mcp tools/list: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read tools/list response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp tools/list: status %d: %s", resp.StatusCode, body)
	}
	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse tools/list response: %v", err)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Fatalf("tools/list returned zero tools — registry enumeration is broken")
	}
	names := make(map[string]bool, len(parsed.Result.Tools))
	for _, tl := range parsed.Result.Tools {
		names[tl.Name] = true
	}
	return names
}

// parityUncovered is the pure parity check: every operation must be covered
// by at least one live registry tool (via apiToolCoverage) or skipped via
// paritySkipList. Returns the uncovered operations, sorted, each annotated
// with the reason it failed coverage.
func parityUncovered(ops []parityOp, registry map[string]bool, coverage map[string][]string, skips map[string]string) []string {
	var out []string
	for _, op := range ops {
		key := op.String()
		if toolNames, ok := coverage[key]; ok {
			live := false
			for _, tn := range toolNames {
				if registry[tn] {
					live = true
					break
				}
			}
			if live {
				continue
			}
			out = append(out, fmt.Sprintf("%s (mapped tool(s) %s absent from the live registry)", key, strings.Join(toolNames, ", ")))
			continue
		}
		if _, ok := skips[op.Path]; ok {
			continue
		}
		out = append(out, key+" — no MCP tool covers this operation")
	}
	sort.Strings(out)
	return out
}

// TestMCPAPIParity is the CTL-003 guard: every /api/v1 operation must be
// covered by an MCP tool or an explicit, hygiene-guarded skip-list entry.
func TestMCPAPIParity(t *testing.T) {
	apiURL, mcpURL := newParityStack(t)
	ops := fetchOpenAPIOps(t, apiURL)
	registry := fetchMCPToolNames(t, mcpURL)

	t.Run("enumeration_is_live", func(t *testing.T) {
		if len(ops) < 20 {
			t.Fatalf("enumerated %d openapi operations, want >= 20 — the live enumeration source looks broken", len(ops))
		}
		for _, op := range ops {
			if !strings.HasPrefix(op.Path, "/api/v1/") {
				t.Errorf("enumerated non-API path %s — enumeration should come from the /api/v1 openapi document", op.Path)
			}
		}
	})

	t.Run("coverage_map_tools_exist", func(t *testing.T) {
		for key, toolNames := range apiToolCoverage {
			for _, tn := range toolNames {
				if !registry[tn] {
					t.Errorf("coverage map entry %q names tool %q which does not exist in the live registry — a tool was renamed without updating the parity map", key, tn)
				}
			}
		}
	})

	t.Run("coverage_map_has_no_stale_entries", func(t *testing.T) {
		live := make(map[string]bool, len(ops))
		for _, op := range ops {
			live[op.String()] = true
		}
		for key := range apiToolCoverage {
			if !live[key] {
				t.Errorf("coverage map entry %q matches no openapi operation — the route was removed/renamed without updating the parity map", key)
			}
		}
	})

	t.Run("every_operation_covered_or_skipped", func(t *testing.T) {
		uncovered := parityUncovered(ops, registry, apiToolCoverage, paritySkipList)
		if len(uncovered) == 0 {
			return
		}
		t.Errorf("CTL-003 MCP/API parity: %d uncovered API operations — every new control/data route MUST ship with an MCP tool (skip-list is for dashboard/human routes only):", len(uncovered))
		for _, u := range uncovered {
			t.Errorf("  UNCOVERED %s", u)
		}
	})

	t.Run("decoy_route_is_detected", func(t *testing.T) {
		decoy := parityOp{Method: http.MethodGet, Path: "/api/v1/__decoy_route__"}
		withDecoy := append(append([]parityOp{}, ops...), decoy)
		uncovered := parityUncovered(withDecoy, registry, apiToolCoverage, paritySkipList)
		for _, u := range uncovered {
			if strings.HasPrefix(u, decoy.String()) {
				return // the guard sees the decoy — enumeration feeds the check
			}
		}
		t.Fatalf("self-check FAILED: decoy %s was not reported uncovered — the enumeration is not feeding the parity check", decoy)
	})

	t.Run("skip_list_hygiene", func(t *testing.T) {
		for key, reason := range paritySkipList {
			if !allowedSkipKey(key) {
				t.Errorf("skip-list entry %q is not a dashboard/human/infra route — control/data routes are NOT skip-eligible (CTL-003); ship the MCP tool instead", key)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("skip-list entry %q has an empty reason — every skip entry needs a one-line documented reason", key)
			}
		}
	})
}
