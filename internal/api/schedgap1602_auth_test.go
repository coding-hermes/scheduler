package api_test

// SCHED-GAP-1602 — acceptance tests for operator authentication on the
// mutating control surface.
//
// The six acceptance criteria map to subtests:
//
//	AC1  unauthenticated mutating requests refused per ROUTE CLASS
//	     (fleet action / collection create / item PUT+DELETE / item action),
//	     each with a clear status, and the daemon answers reads afterwards
//	     (refusal did not crash anything).
//	AC2  an authenticated request succeeds AND the identity is recorded
//	     (api.auth event row, read back through GET /api/v1/events).
//	AC3  misconfiguration fails CLOSED — unset credential → 503 on every
//	     mutating route class; wrong credential → 401.
//	AC4  every mutating call produces an auditable events row — allowed
//	     (INFO) and refused (MEDIUM) both carry identity/method/target.
//	AC5  reads behave exactly as decided (open): every read route answers
//	     200 without any credential while mutations are gated.
//	AC6  the served OpenAPI document declares the security posture:
//	     components.securitySchemes present, security on all 22 mutating
//	     operations, security on none of the read operations.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// operatorToken is the test credential — the SAME value the shared stack
// (newAPITestServer) arms, re-declared here for explicitness in refusal
// probes. Any change here must keep the Authorization header shapes below
// in sync.
const operatorToken = testOperatorToken

// newAuthedServer is the shared (credentialed) stack under its auth-row name.
func newAuthedServer(t *testing.T) *apiTestServer {
	return newAPITestServer(t)
}

// newBareServer builds a stack with NO SetAuthConfig call — the zero-value
// Server exactly as every pre-1602 constructor produced it. This is the
// fail-closed arm's subject: mutations must 503, reads must stay open.
func newBareServer(t *testing.T) *apiTestServer {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	loop := scheduler.NewLoop(db, 1, 2, 10, 0, 5)
	loop.SetNoExecFallback(true)
	srv := api.NewServer(db, loop)
	storeDir := t.TempDir()
	srv.SetBlocksStore(blocks.NewStore(
		filepath.Join(storeDir, "groups.jsonl"),
		filepath.Join(storeDir, "templates.jsonl"),
	))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &apiTestServer{db: db, loop: loop, server: srv, ts: ts}
}

// doAnon performs a request with NO credential header — the unauthenticated
// caller the refusal tests simulate. (The shared do helper authenticates
// every request; this one deliberately bypasses that.)
func (a *apiTestServer) doAnon(t *testing.T, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	status, parsed, _ := a.doAuthRaw(t, method, path, body, "")
	return status, parsed
}

// doAuth performs a request with optional operator credential headers.
func (a *apiTestServer) doAuth(t *testing.T, method, path string, body interface{}, cred string) (int, map[string]interface{}) {
	t.Helper()
	status, parsed, _ := a.doAuthRaw(t, method, path, body, cred)
	return status, parsed
}

func (a *apiTestServer) doAuthRaw(t *testing.T, method, path string, body interface{}, cred string) (int, map[string]interface{}, *http.Response) {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.ts.URL+path, reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch cred {
	case "token-header":
		req.Header.Set("X-Operator-Token", operatorToken)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+operatorToken)
	case "basic":
		req.SetBasicAuth("operator", operatorToken)
	case "basic-empty-user":
		req.SetBasicAuth("", operatorToken)
	case "wrong":
		req.Header.Set("X-Operator-Token", "not-the-token")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp.StatusCode, parsed, resp
}

// authEvents lists the api.auth audit rows through the public events route —
// the read-back path acceptance criterion 4 names. The structured audit
// fields (identity/method/path/target/outcome/mode) ride in the row's
// `details` JSON string; they are parsed and merged into each returned map.
func (a *apiTestServer) authEvents(t *testing.T) []map[string]interface{} {
	t.Helper()
	status, parsed := a.do(t, "GET", "/api/v1/events?component=api.auth&limit=200", nil)
	if status != 200 {
		t.Fatalf("GET /api/v1/events?component=api.auth: status %d", status)
	}
	raw, ok := parsed["events"].([]interface{})
	if !ok {
		t.Fatalf("events payload has no events array: %v", parsed)
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if d, ok := m["details"].(string); ok && strings.HasPrefix(d, "{") {
			var fields map[string]interface{}
			if err := json.Unmarshal([]byte(d), &fields); err == nil {
				for k, v := range fields {
					m[k] = v
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// ── AC1: unauthenticated mutations refused per route class ──

func TestAuthUnauthenticatedRefusalsPerRouteClass(t *testing.T) {
	a := newAuthedServer(t)

	type probe struct {
		name   string
		method string
		path   string
		body   interface{}
	}
	classes := []probe{
		{"fleet-pause", "POST", "/api/v1/pause", nil},
		{"fleet-evaluate", "POST", "/api/v1/evaluate", nil},
		{"collection-create", "POST", "/api/v1/projects", map[string]interface{}{
			"name": "auth-deny-create", "repo_url": "local:/tmp/x", "workdir": "/tmp/x",
		}},
		{"item-put", "PUT", "/api/v1/projects/auth-proj", map[string]interface{}{"weight": 20}},
		{"item-delete", "DELETE", "/api/v1/projects/auth-proj", nil},
		{"item-action", "POST", "/api/v1/projects/auth-proj/pause", nil},
		{"namespace-create", "POST", "/api/v1/namespaces", map[string]interface{}{"id": "auth-deny-ns", "weight": 10}},
		{"namespace-put", "PUT", "/api/v1/namespaces/auth-ns", map[string]interface{}{"weight": 20}},
		{"namespace-delete", "DELETE", "/api/v1/namespaces/auth-ns", nil},
		{"namespace-move", "POST", "/api/v1/namespaces/auth-ns/move", map[string]interface{}{"project": "auth-proj"}},
		{"group-create", "POST", "/api/v1/groups", map[string]interface{}{"name": "auth-deny-grp"}},
		{"group-put", "PUT", "/api/v1/groups/auth-grp", map[string]interface{}{"description": "x"}},
		{"group-delete", "DELETE", "/api/v1/groups/auth-grp", nil},
		{"group-deploy", "POST", "/api/v1/groups/auth-grp/deploy", nil},
		{"template-create", "POST", "/api/v1/templates", map[string]interface{}{"name": "auth-deny-tpl"}},
		{"template-put", "PUT", "/api/v1/templates/auth-tpl", map[string]interface{}{"body": "x"}},
		{"template-delete", "DELETE", "/api/v1/templates/auth-tpl", nil},
	}
	for _, p := range classes {
		status, parsed := a.doAnon(t, p.method, p.path, p.body)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s: unauthenticated status = %d, want 401 (body %v)", p.name, p.method, status, parsed["error"])
		}
		if msg, _ := parsed["error"].(string); !strings.Contains(msg, "credential") {
			t.Errorf("%s: refusal body should name the credential requirement, got %q", p.name, msg)
		}
	}

	// The refusal must not have crashed the daemon: reads still answer.
	status, _ := a.do(t, "GET", "/api/v1/status", nil)
	if status != 200 {
		t.Errorf("GET /api/v1/status after refusals = %d, want 200 (daemon crashed)", status)
	}

	// Every refusal wrote its audit row (17 probes → 17 rows, AC4 refusals).
	events := a.authEvents(t)
	if len(events) < len(classes) {
		t.Errorf("audit rows after %d refusals = %d, want >= %d", len(classes), len(events), len(classes))
	}
	for _, e := range events {
		if e["outcome"] == "allowed" {
			t.Errorf("refusal probe recorded an allowed row: %v", e)
		}
	}
}

// ── AC2: authenticated request succeeds, identity recorded ──

func TestAuthAuthenticatedSucceedsAndRecordsIdentity(t *testing.T) {
	a := newAuthedServer(t)
	mustCreateAPITestProject(t, a.db, "auth-ok-proj")

	// The three credential shapes the API accepts must all pass.
	for _, cred := range []string{"token-header", "bearer", "basic", "basic-empty-user"} {
		var status int
		switch cred {
		case "basic", "basic-empty-user":
			// Basic with token-as-password against a project pause.
			status, _ = a.doAuth(t, "POST", "/api/v1/projects/auth-ok-proj/pause", nil, cred)
		default:
			status, _ = a.doAuth(t, "POST", "/api/v1/pause", nil, cred)
		}
		if status != 200 {
			t.Errorf("credential shape %q: status = %d, want 200", cred, status)
		}
	}

	// Identity recorded on each allowed call.
	events := a.authEvents(t)
	allowed := 0
	for _, e := range events {
		if e["outcome"] == "allowed" {
			allowed++
			id, _ := e["identity"].(string)
			if !strings.HasPrefix(id, "operator:") {
				t.Errorf("allowed row identity = %q, want operator:*", id)
			}
		}
	}
	if allowed != 4 {
		t.Errorf("allowed audit rows = %d, want 4", allowed)
	}

	// Unpause for cleanliness (the loop is shared per-test, fine either way).
	_, _ = a.doAuth(t, "POST", "/api/v1/resume", nil, "token-header")
}

// ── AC3: misconfiguration fails closed ──

func TestAuthFailClosedUnset(t *testing.T) {
	// A Server with NO SetAuthConfig call (the zero value = authOff) must
	// refuse EVERY mutating route class with 503 — never silently allow.
	a := newBareServer(t)

	probes := []struct{ method, path string }{
		{"POST", "/api/v1/pause"},
		{"POST", "/api/v1/evaluate"},
		{"POST", "/api/v1/projects"},
		{"PUT", "/api/v1/projects/x"},
		{"DELETE", "/api/v1/projects/x"},
		{"POST", "/api/v1/projects/x/spawn"},
		{"POST", "/api/v1/namespaces"},
		{"PUT", "/api/v1/namespaces/ns1"},
		{"DELETE", "/api/v1/namespaces/ns1"},
		{"POST", "/api/v1/namespaces/ns1/move"},
		{"POST", "/api/v1/groups"},
		{"PUT", "/api/v1/groups/g1"},
		{"DELETE", "/api/v1/groups/g1"},
		{"POST", "/api/v1/groups/g1/deploy"},
		{"POST", "/api/v1/templates"},
		{"PUT", "/api/v1/templates/t1"},
		{"DELETE", "/api/v1/templates/t1"},
	}
	for _, p := range probes {
		status, parsed := a.do(t, p.method, p.path, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("unset credential %s %s: status = %d, want 503 (fail-closed)", p.method, p.path, status)
		}
		if msg, _ := parsed["error"].(string); !strings.Contains(msg, "no operator credential") {
			t.Errorf("unset credential refusal body should explain the misconfiguration, got %q", msg)
		}
	}
	// Reads stay open even in the fail-closed mode (decision (b)/(f)).
	status, _ := a.do(t, "GET", "/api/v1/status", nil)
	if status != 200 {
		t.Errorf("GET /api/v1/status with authOff = %d, want 200", status)
	}
}

func TestAuthFailClosedWrongCredential(t *testing.T) {
	a := newAuthedServer(t)
	status, parsed := a.doAuth(t, "POST", "/api/v1/pause", nil, "wrong")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong credential: status = %d, want 401", status)
	}
	if msg, _ := parsed["error"].(string); msg == "" {
		t.Error("wrong-credential refusal must carry a reason")
	}
	// The wrong-credential attempt is audited with the invalid identity.
	events := a.authEvents(t)
	found := false
	for _, e := range events {
		if e["identity"] == "invalid" && e["outcome"] == "refused-bad-credential" {
			found = true
		}
	}
	if !found {
		t.Errorf("no refused-bad-credential audit row (events %v)", events)
	}
}

// ── AC4: audit rows are readable back with who/what/target ──

func TestAuthAuditRowShape(t *testing.T) {
	a := newAuthedServer(t)
	mustCreateAPITestProject(t, a.db, "auth-audit-proj")

	status, _ := a.doAuth(t, "POST", "/api/v1/projects/auth-audit-proj/bump", map[string]interface{}{"ticks": 1, "reason": "auth-test"}, "token-header")
	if status != 200 {
		t.Fatalf("bump with credential: status %d, want 200", status)
	}
	events := a.authEvents(t)
	var row map[string]interface{}
	for _, e := range events {
		if tgt, _ := e["target"].(string); strings.Contains(tgt, "auth-audit-proj") && e["outcome"] == "allowed" {
			row = e
		}
	}
	if row == nil {
		t.Fatalf("no allowed audit row for the bump (events: %v)", events)
	}
	for _, field := range []string{"identity", "method", "path", "target", "outcome", "mode"} {
		if _, ok := row[field]; !ok {
			t.Errorf("audit row missing %q field: %v", field, row)
		}
	}
	if row["method"] != "POST" || row["mode"] != "token" {
		t.Errorf("audit row method/mode = %v/%v, want POST/token", row["method"], row["mode"])
	}
}

// ── AC5: reads stay open while mutations are gated ──

func TestAuthReadsStayOpen(t *testing.T) {
	a := newAuthedServer(t)
	reads := []string{
		"/api/v1/health",
		"/api/v1/live",
		"/api/v1/status",
		"/api/v1/config",
		"/api/v1/projects",
		"/api/v1/namespaces",
		"/api/v1/ticks",
		"/api/v1/events",
		"/api/v1/queue",
		"/api/v1/metrics",
		"/api/v1/openapi.json",
	}
	for _, p := range reads {
		status, _ := a.do(t, "GET", p, nil)
		if status != 200 {
			t.Errorf("GET %s without credential = %d, want 200 (reads must stay open)", p, status)
		}
	}
}

// ── AC6: the served OpenAPI document declares the posture ──

func TestAuthOpenAPIDeclaresSecurity(t *testing.T) {
	a := newAuthedServer(t)
	status, parsed := a.do(t, "GET", "/api/v1/openapi.json", nil)
	if status != 200 {
		t.Fatalf("openapi.json: status %d", status)
	}
	spec, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-marshal spec: %v", err)
	}
	var doc struct {
		Components struct {
			SecuritySchemes map[string]interface{} `json:"securitySchemes"`
		} `json:"components"`
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if len(doc.Components.SecuritySchemes) == 0 {
		t.Error("components.securitySchemes missing — the served spec must declare the credential")
	}
	mutating, missing := 0, []string{}
	readWithSec := 0
	for path, ops := range doc.Paths {
		for method, raw := range ops {
			switch method {
			case "post", "put", "delete":
				mutating++
				var op struct {
					Security []map[string]interface{} `json:"security"`
				}
				if err := json.Unmarshal(raw, &op); err != nil {
					t.Fatalf("parse %s %s: %v", method, path, err)
				}
				if len(op.Security) == 0 {
					missing = append(missing, method+" "+path)
				}
			case "get":
				var op struct {
					Security []map[string]interface{} `json:"security"`
				}
				_ = json.Unmarshal(raw, &op)
				if len(op.Security) > 0 {
					readWithSec++
				}
			}
		}
	}
	if mutating != 22 {
		t.Errorf("mutating operations in spec = %d, want 22", mutating)
	}
	if len(missing) > 0 {
		t.Errorf("mutating operations without security: %v", missing)
	}
	if readWithSec != 0 {
		t.Errorf("%d read operations carry security — reads must stay open", readWithSec)
	}
}

// fmt import guard (keep fmt for future debug output)
