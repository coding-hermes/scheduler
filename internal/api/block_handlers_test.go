package api_test

// Handler-level integration tests for the JSONL-backed deploy blocks API
// (internal/api/block_handlers.go): GROUPS and TEMPLATES CRUD over
// /api/v1/groups* and /api/v1/templates*, plus the template→group DEPLOY
// action. The store internals are covered in internal/blocks; these tests
// exercise the HTTP surface end to end — status-code mapping
// (400/404/405/409), response bodies, dry-run plan vs live board appends,
// per-project failure isolation, torn-JSONL tolerance, and concurrent writes
// under the race detector.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── Harness ───────────────────────────────────────────────────────────────

// blocksTestServer is an apiTestServer that also exposes the temp directory
// backing the JSONL groups/templates store, so tests can corrupt the files
// directly (torn-line tolerance) without reaching inside the server.
type blocksTestServer struct {
	*apiTestServer
	storeDir string
}

func newBlocksTestServer(t *testing.T) *blocksTestServer {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// budget=0 keeps ForceEvaluate a no-op (no real spawning from tests).
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	srv := api.NewServer(db, loop)

	storeDir := t.TempDir()
	srv.SetBlocksStore(blocks.NewStore(
		filepath.Join(storeDir, "groups.jsonl"),
		filepath.Join(storeDir, "templates.jsonl"),
	))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &blocksTestServer{
		apiTestServer: &apiTestServer{db: db, loop: loop, server: srv, ts: ts},
		storeDir:      storeDir,
	}
}

func (b *blocksTestServer) groupsPath() string { return filepath.Join(b.storeDir, "groups.jsonl") }
func (b *blocksTestServer) templatesPath() string {
	return filepath.Join(b.storeDir, "templates.jsonl")
}

// doRaw sends a raw body so handlers can be exercised with JSON a struct
// round-trip would reject before it reaches the handler.
func (b *blocksTestServer) doRaw(t *testing.T, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(method, b.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]interface{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Logf("response body not JSON: %q", string(raw))
		}
	}
	return resp.StatusCode, parsed
}

// concurrentPosts fires one POST per body concurrently and returns the
// response statuses in request order (-1 = marshal failure, -2 = transport
// failure). Each goroutine writes only its own slice slot.
func concurrentPosts(baseURL, path string, bodies []interface{}) []int {
	statuses := make([]int, len(bodies))
	var wg sync.WaitGroup
	for i, body := range bodies {
		wg.Add(1)
		go func(i int, body interface{}) {
			defer wg.Done()
			payload, err := json.Marshal(body)
			if err != nil {
				statuses[i] = -1
				return
			}
			resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(payload))
			if err != nil {
				statuses[i] = -2
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			statuses[i] = resp.StatusCode
		}(i, body)
	}
	wg.Wait()
	return statuses
}

// ── Body builders ─────────────────────────────────────────────────────────

func groupBody(name string, projects ...string) map[string]interface{} {
	if projects == nil {
		projects = []string{}
	}
	return map[string]interface{}{
		"name":        name,
		"projects":    projects,
		"description": "group " + name,
	}
}

// templateBody builds a template request body; the default task list mirrors
// the two-task shape used across the fleet (with {PROJECT} substitution).
func templateBody(name string, titles ...string) map[string]interface{} {
	if len(titles) == 0 {
		titles = []string{"Audit {PROJECT} board hygiene", "Close stale rows on {PROJECT}"}
	}
	tasks := make([]map[string]interface{}, 0, len(titles))
	for _, title := range titles {
		tasks = append(tasks, map[string]interface{}{
			"title":  title,
			"labels": []string{"integration"},
		})
	}
	return map[string]interface{}{
		"name":        name,
		"description": "template " + name,
		"tasks":       tasks,
	}
}

// ── Fixture helpers ───────────────────────────────────────────────────────

func (b *blocksTestServer) mustCreateGroup(t *testing.T, name string, projects ...string) {
	t.Helper()
	status, resp := b.do(t, "POST", "/api/v1/groups", groupBody(name, projects...))
	if status != http.StatusCreated {
		t.Fatalf("create group %s: status = %d, want 201: %v", name, status, resp)
	}
}

func (b *blocksTestServer) mustCreateTemplate(t *testing.T, name string, titles ...string) {
	t.Helper()
	status, resp := b.do(t, "POST", "/api/v1/templates", templateBody(name, titles...))
	if status != http.StatusCreated {
		t.Fatalf("create template %s: status = %d, want 201: %v", name, status, resp)
	}
}

// mustCreateProject registers a scheduler project whose workdir is a real
// (or deliberately absent) path on this host — deploy resolves targets from
// the projects table, so the workdir column is what selects the board.
func (b *blocksTestServer) mustCreateProject(t *testing.T, name, workdir string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), b.db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   workdir,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// makeBoardDir creates a workdir holding a JSONL task board seeded with
// content ("" = the empty, valid board file) and returns workdir + board path.
func makeBoardDir(t *testing.T, seed string) (workdir, boardPath string) {
	t.Helper()
	workdir = t.TempDir()
	boardPath = filepath.Join(workdir, ".coding-hermes", "board", "tasks.jsonl")
	writeTestFile(t, boardPath, seed)
	return workdir, boardPath
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// appendTestFile appends raw text verbatim — used to reproduce a torn final
// line (a record with no trailing newline).
func appendTestFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// fileLines returns the non-empty lines of a JSONL file.
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(readTestFile(t, path), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func parseTestJSON(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

// jsonStrings converts a decoded JSON array of strings to a []string.
func jsonStrings(t *testing.T, v interface{}, field string) []string {
	t.Helper()
	raw, ok := v.([]interface{})
	if !ok {
		t.Fatalf("%s = %v (%T), want array", field, v, v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s contains %v (%T), want string", field, item, item)
		}
		out = append(out, s)
	}
	return out
}

// jsonNames extracts one field from a decoded JSON array of objects — used on
// the list endpoints, which return [{name: ...}, ...] records.
func jsonNames(t *testing.T, v interface{}, field, key string) []string {
	t.Helper()
	raw, ok := v.([]interface{})
	if !ok {
		t.Fatalf("%s = %v (%T), want array", field, v, v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("%s contains %v (%T), want object", field, item, item)
		}
		name, _ := m[key].(string)
		if name == "" {
			t.Fatalf("%s entry has no %q: %v", field, key, m)
		}
		out = append(out, name)
	}
	return out
}

// ── Deploy response helpers ───────────────────────────────────────────────

func deployProjects(t *testing.T, resp map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	raw, ok := resp["projects"].([]interface{})
	if !ok {
		t.Fatalf("deploy projects = %v (%T), want array", resp["projects"], resp["projects"])
	}
	out := make(map[string]map[string]interface{}, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("deploy outcome = %v (%T), want object", item, item)
		}
		name, _ := m["project"].(string)
		out[name] = m
	}
	return out
}

func summaryInt(t *testing.T, resp map[string]interface{}, key string) int {
	t.Helper()
	sum, ok := resp["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("summary = %v (%T), want object", resp["summary"], resp["summary"])
	}
	v, ok := sum[key].(float64)
	if !ok {
		t.Fatalf("summary.%s = %v (%T), want number", key, sum[key], sum[key])
	}
	return int(v)
}

// ── Groups: list ──────────────────────────────────────────────────────────

func TestBlocksAPI_ListGroupsEmpty(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "GET", "/api/v1/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if _, ok := resp["groups"].([]interface{}); !ok {
		t.Fatalf("groups = %v (%T), want an empty array (never null)", resp["groups"], resp["groups"])
	}
	if got := len(jsonNames(t, resp["groups"], "groups", "name")); got != 0 {
		t.Errorf("got %d groups, want 0", got)
	}
}

func TestBlocksAPI_ListGroupsPopulated(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "zeta-group", "p-zeta")
	b.mustCreateGroup(t, "alpha-group", "p-alpha")

	status, resp := b.do(t, "GET", "/api/v1/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	groups := jsonNames(t, resp["groups"], "groups", "name")
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	// Sorted by name for stable API output.
	if groups[0] != "alpha-group" || groups[1] != "zeta-group" {
		t.Errorf("group order = %v, want [alpha-group zeta-group]", groups)
	}
}

// ── Groups: create ────────────────────────────────────────────────────────

func TestBlocksAPI_CreateGroup(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "POST", "/api/v1/groups", groupBody("new-group", "p1", "p2"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", status, resp)
	}
	if resp["name"] != "new-group" {
		t.Errorf("name = %v, want new-group", resp["name"])
	}
	if got := jsonStrings(t, resp["projects"], "projects"); len(got) != 2 || got[0] != "p1" {
		t.Errorf("projects = %v, want [p1 p2]", got)
	}

	// Persisted as one JSONL record, file newline-terminated.
	lines := fileLines(t, b.groupsPath())
	if len(lines) != 1 {
		t.Fatalf("groups.jsonl lines = %d, want 1", len(lines))
	}
	rec := parseTestJSON(t, lines[0])
	if rec["name"] != "new-group" {
		t.Errorf("persisted name = %v, want new-group", rec["name"])
	}
	if got := jsonStrings(t, rec["projects"], "projects"); len(got) != 2 || got[1] != "p2" {
		t.Errorf("persisted projects = %v, want [p1 p2]", got)
	}
	if data := readTestFile(t, b.groupsPath()); !strings.HasSuffix(data, "\n") {
		t.Error("groups.jsonl does not end with a newline")
	}
}

func TestBlocksAPI_CreateGroupDuplicate(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "dup-group")
	status, resp := b.do(t, "POST", "/api/v1/groups", groupBody("dup-group"))
	if status != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "already exists") {
		t.Errorf("error = %q, want it to mention 'already exists'", msg)
	}
	// The rejected create left exactly one record behind.
	if got := len(fileLines(t, b.groupsPath())); got != 1 {
		t.Errorf("groups.jsonl lines = %d, want 1", got)
	}
}

func TestBlocksAPI_CreateGroupValidation(t *testing.T) {
	b := newBlocksTestServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":"","projects":[]}`},
		{"whitespace name", `{"name":"bad name","projects":[]}`},
		{"missing name", `{"projects":[]}`},
		{"invalid json", `{"name":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := b.doRaw(t, "POST", "/api/v1/groups", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %v", status, resp)
			}
			if msg, _ := resp["error"].(string); msg == "" {
				t.Errorf("error message empty: %v", resp)
			}
		})
	}
	// Nothing was persisted by any rejected request.
	if _, err := os.Stat(b.groupsPath()); !os.IsNotExist(err) {
		data := readTestFile(t, b.groupsPath())
		t.Errorf("rejected creates persisted data: %q", data)
	}
}

// ── Groups: get / update / delete ─────────────────────────────────────────

func TestBlocksAPI_GetGroup(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "get-group", "member-a", "member-b")

	status, resp := b.do(t, "GET", "/api/v1/groups/get-group", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["name"] != "get-group" {
		t.Errorf("name = %v, want get-group", resp["name"])
	}
	if got := jsonStrings(t, resp["projects"], "projects"); len(got) != 2 || got[1] != "member-b" {
		t.Errorf("projects = %v, want [member-a member-b]", got)
	}
	if resp["description"] != "group get-group" {
		t.Errorf("description = %v, want 'group get-group'", resp["description"])
	}
}

func TestBlocksAPI_GetGroupNotFound(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "GET", "/api/v1/groups/no-such-group", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", status, resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "not found") {
		t.Errorf("error = %q, want it to mention 'not found'", msg)
	}
}

func TestBlocksAPI_UpdateGroup(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "upd-group", "old-member")

	status, resp := b.do(t, "PUT", "/api/v1/groups/upd-group", map[string]interface{}{
		"projects":    []string{"new-member-1", "new-member-2"},
		"description": "repurposed",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["description"] != "repurposed" {
		t.Errorf("description = %v, want repurposed", resp["description"])
	}
	if got := jsonStrings(t, resp["projects"], "projects"); len(got) != 2 || got[0] != "new-member-1" {
		t.Errorf("projects = %v, want [new-member-1 new-member-2]", got)
	}

	// Read back through the API and from disk.
	status, resp = b.do(t, "GET", "/api/v1/groups/upd-group", nil)
	if status != http.StatusOK {
		t.Fatalf("GET after update: status = %d, want 200", status)
	}
	if got := jsonStrings(t, resp["projects"], "projects"); len(got) != 2 || got[1] != "new-member-2" {
		t.Errorf("persisted projects = %v, want [new-member-1 new-member-2]", got)
	}
	if got := len(fileLines(t, b.groupsPath())); got != 1 {
		t.Errorf("groups.jsonl lines after update = %d, want 1 (no append-duplication)", got)
	}
}

func TestBlocksAPI_UpdateGroupNotFound(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "PUT", "/api/v1/groups/no-such-group", map[string]interface{}{
		"description": "x",
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", status, resp)
	}
}

func TestBlocksAPI_DeleteGroup(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "del-group", "member")

	status, resp := b.do(t, "DELETE", "/api/v1/groups/del-group", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "deleted" || resp["group"] != "del-group" {
		t.Errorf("delete response = %v, want status=deleted group=del-group", resp)
	}
	if status, _ := b.do(t, "GET", "/api/v1/groups/del-group", nil); status != http.StatusNotFound {
		t.Errorf("GET after delete: status = %d, want 404", status)
	}
	// Second delete: the name is gone → 404, not a silent success.
	status, resp = b.do(t, "DELETE", "/api/v1/groups/del-group", nil)
	if status != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404: %v", status, resp)
	}
}

// ── Templates: list / create ──────────────────────────────────────────────

func TestBlocksAPI_ListTemplatesEmpty(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "GET", "/api/v1/templates", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if _, ok := resp["templates"].([]interface{}); !ok {
		t.Fatalf("templates = %v (%T), want an empty array (never null)", resp["templates"], resp["templates"])
	}
	if got := len(jsonNames(t, resp["templates"], "templates", "name")); got != 0 {
		t.Errorf("got %d templates, want 0", got)
	}
}

func TestBlocksAPI_ListTemplatesPopulated(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "beta-tpl")
	b.mustCreateTemplate(t, "alpha-tpl")

	status, resp := b.do(t, "GET", "/api/v1/templates", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	names := jsonNames(t, resp["templates"], "templates", "name")
	if len(names) != 2 || names[0] != "alpha-tpl" || names[1] != "beta-tpl" {
		t.Fatalf("templates = %v, want [alpha-tpl beta-tpl]", names)
	}
}

func TestBlocksAPI_CreateTemplate(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "POST", "/api/v1/templates", templateBody("new-tpl", "Only task on {PROJECT}"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", status, resp)
	}
	if resp["name"] != "new-tpl" {
		t.Errorf("name = %v, want new-tpl", resp["name"])
	}
	tasks, ok := resp["tasks"].([]interface{})
	if !ok || len(tasks) != 1 {
		t.Fatalf("tasks = %v, want 1 task", resp["tasks"])
	}

	lines := fileLines(t, b.templatesPath())
	if len(lines) != 1 {
		t.Fatalf("templates.jsonl lines = %d, want 1", len(lines))
	}
	rec := parseTestJSON(t, lines[0])
	if rec["name"] != "new-tpl" {
		t.Errorf("persisted name = %v, want new-tpl", rec["name"])
	}
	if tasks, ok := rec["tasks"].([]interface{}); !ok || len(tasks) != 1 {
		t.Errorf("persisted tasks = %v, want 1 task", rec["tasks"])
	}
}

func TestBlocksAPI_CreateTemplateDuplicate(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "dup-tpl")
	status, resp := b.do(t, "POST", "/api/v1/templates", templateBody("dup-tpl"))
	if status != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "already exists") {
		t.Errorf("error = %q, want it to mention 'already exists'", msg)
	}
	if got := len(fileLines(t, b.templatesPath())); got != 1 {
		t.Errorf("templates.jsonl lines = %d, want 1", got)
	}
}

func TestBlocksAPI_CreateTemplateValidation(t *testing.T) {
	b := newBlocksTestServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":"","tasks":[{"title":"x"}]}`},
		{"whitespace name", `{"name":"bad name","tasks":[{"title":"x"}]}`},
		{"no tasks", `{"name":"taskless","tasks":[]}`},
		{"blank task title", `{"name":"blanktitle","tasks":[{"title":"  "}]}`},
		{"invalid json", `{"name":"broken"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := b.doRaw(t, "POST", "/api/v1/templates", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %v", status, resp)
			}
			if msg, _ := resp["error"].(string); msg == "" {
				t.Errorf("error message empty: %v", resp)
			}
		})
	}
	if _, err := os.Stat(b.templatesPath()); !os.IsNotExist(err) {
		t.Errorf("rejected creates persisted data: %q", readTestFile(t, b.templatesPath()))
	}
}

// ── Templates: get / update / delete ──────────────────────────────────────

func TestBlocksAPI_GetTemplate(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "get-tpl", "Task one on {PROJECT}")

	status, resp := b.do(t, "GET", "/api/v1/templates/get-tpl", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["name"] != "get-tpl" {
		t.Errorf("name = %v, want get-tpl", resp["name"])
	}
	tasks, ok := resp["tasks"].([]interface{})
	if !ok || len(tasks) != 1 {
		t.Fatalf("tasks = %v, want 1 task", resp["tasks"])
	}
	first, _ := tasks[0].(map[string]interface{})
	if first["title"] != "Task one on {PROJECT}" {
		t.Errorf("task title = %v, want 'Task one on {PROJECT}'", first["title"])
	}
}

func TestBlocksAPI_GetTemplateNotFound(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "GET", "/api/v1/templates/no-such-template", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", status, resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "not found") {
		t.Errorf("error = %q, want it to mention 'not found'", msg)
	}
}

func TestBlocksAPI_UpdateTemplate(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "upd-tpl", "original task")

	status, resp := b.do(t, "PUT", "/api/v1/templates/upd-tpl", map[string]interface{}{
		"description": "rewritten",
		"tasks": []map[string]interface{}{
			{"title": "First on {PROJECT}"},
			{"title": "Second on {PROJECT}", "labels": []string{"hygiene"}},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["description"] != "rewritten" {
		t.Errorf("description = %v, want rewritten", resp["description"])
	}
	tasks, ok := resp["tasks"].([]interface{})
	if !ok || len(tasks) != 2 {
		t.Fatalf("tasks = %v, want 2", resp["tasks"])
	}

	status, resp = b.do(t, "GET", "/api/v1/templates/upd-tpl", nil)
	if status != http.StatusOK {
		t.Fatalf("GET after update: status = %d, want 200", status)
	}
	tasks, _ = resp["tasks"].([]interface{})
	if len(tasks) != 2 {
		t.Errorf("persisted tasks = %v, want 2", resp["tasks"])
	}
	if got := len(fileLines(t, b.templatesPath())); got != 1 {
		t.Errorf("templates.jsonl lines after update = %d, want 1", got)
	}
}

func TestBlocksAPI_UpdateTemplateNotFound(t *testing.T) {
	b := newBlocksTestServer(t)
	status, resp := b.do(t, "PUT", "/api/v1/templates/no-such-template", map[string]interface{}{
		"description": "x",
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", status, resp)
	}
}

func TestBlocksAPI_DeleteTemplate(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "del-tpl")

	status, resp := b.do(t, "DELETE", "/api/v1/templates/del-tpl", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "deleted" || resp["template"] != "del-tpl" {
		t.Errorf("delete response = %v, want status=deleted template=del-tpl", resp)
	}
	if status, _ := b.do(t, "GET", "/api/v1/templates/del-tpl", nil); status != http.StatusNotFound {
		t.Errorf("GET after delete: status = %d, want 404", status)
	}
	status, resp = b.do(t, "DELETE", "/api/v1/templates/del-tpl", nil)
	if status != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404: %v", status, resp)
	}
}

// ── Deploy: dry run ───────────────────────────────────────────────────────

// TestBlocksAPI_DeployDryRunWritesNothing verifies the plan is complete
// (would_append / error per project) while no board file is touched and no
// workdir tree is created.
func TestBlocksAPI_DeployDryRunWritesNothing(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "DEP-DRY")
	okWD, okBoard := makeBoardDir(t, "")
	goneWD := filepath.Join(t.TempDir(), "gone") // never created on disk
	b.mustCreateProject(t, "dry-ok", okWD)
	b.mustCreateProject(t, "dry-gone", goneWD)
	b.mustCreateGroup(t, "dry-grp", "dry-ok", "dry-gone", "dry-unknown")

	before := readTestFile(t, okBoard)
	status, resp := b.do(t, "POST", "/api/v1/groups/dry-grp/deploy", map[string]interface{}{
		"template": "DEP-DRY",
		"dry_run":  true,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", resp["dry_run"])
	}
	if resp["group"] != "dry-grp" || resp["template"] != "DEP-DRY" {
		t.Errorf("response group/template = %v/%v, want dry-grp/DEP-DRY", resp["group"], resp["template"])
	}
	out := deployProjects(t, resp)
	if len(out) != 3 {
		t.Fatalf("outcomes = %d, want 3 (batch covers every member)", len(out))
	}
	if got := out["dry-ok"]["status"]; got != "would_append" {
		t.Errorf("dry-ok status = %v, want would_append (%v)", got, out["dry-ok"]["reason"])
	}
	if ids := jsonStrings(t, out["dry-ok"]["task_ids"], "task_ids"); len(ids) != 2 {
		t.Errorf("dry-ok task_ids = %v, want 2 planned ids", ids)
	}
	if got := out["dry-gone"]["status"]; got != "error" {
		t.Errorf("dry-gone status = %v, want error (workdir missing)", got)
	}
	if got := out["dry-unknown"]["status"]; got != "error" {
		t.Errorf("dry-unknown status = %v, want error (not in projects table)", got)
	}
	if s := summaryInt(t, resp, "appended"); s != 1 {
		t.Errorf("summary.appended = %d, want 1", s)
	}
	if s := summaryInt(t, resp, "errors"); s != 2 {
		t.Errorf("summary.errors = %d, want 2", s)
	}
	if s := summaryInt(t, resp, "task_rows"); s != 2 {
		t.Errorf("summary.task_rows = %d, want 2", s)
	}
	if after := readTestFile(t, okBoard); after != before {
		t.Errorf("dry run wrote to the board file:\nbefore: %q\nafter:  %q", before, after)
	}
	if _, err := os.Stat(goneWD); !os.IsNotExist(err) {
		t.Errorf("dry run created the missing workdir %s", goneWD)
	}
}

// ── Deploy: live append ───────────────────────────────────────────────────

// TestBlocksAPI_DeployLiveAppendsForemanRows verifies a real deploy puts
// canonical, foreman-valid pending rows on every member's board.
func TestBlocksAPI_DeployLiveAppendsForemanRows(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "DEP-LIVE") // default 2-task body
	wd1, board1 := makeBoardDir(t, "")
	wd2, board2 := makeBoardDir(t, "")
	b.mustCreateProject(t, "live-a", wd1)
	b.mustCreateProject(t, "live-b", wd2)
	b.mustCreateGroup(t, "live-grp", "live-a", "live-b")

	status, resp := b.do(t, "POST", "/api/v1/groups/live-grp/deploy", map[string]interface{}{
		"template": "DEP-LIVE",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["dry_run"] != false {
		t.Errorf("dry_run = %v, want false", resp["dry_run"])
	}
	out := deployProjects(t, resp)
	if len(out) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(out))
	}

	date := time.Now().UTC().Format("20060102")
	nullKeys := []string{
		"depends_on", "blocks", "primary_model", "primary_provider",
		"fallback_model", "fallback_provider", "reasoning", "dispatched_at",
		"completed_at", "exit_code", "commit_hash", "files_changed",
		"lines_added", "lines_removed", "guard_result", "ci_result",
		"worker_summary", "blocked_reason", "review_notes", "blocked_since",
	}
	boards := map[string]string{"live-a": board1, "live-b": board2}
	for project, board := range boards {
		o := out[project]
		if o == nil {
			t.Fatalf("no outcome for %s: %v", project, resp)
		}
		if got := o["status"]; got != "appended" {
			t.Fatalf("%s status = %v, want appended (%v)", project, got, o["reason"])
		}
		ids := jsonStrings(t, o["task_ids"], "task_ids")
		if len(ids) != 2 {
			t.Fatalf("%s task_ids = %v, want 2", project, ids)
		}
		lines := fileLines(t, board)
		if len(lines) != 2 {
			t.Fatalf("%s board lines = %d, want 2", project, len(lines))
		}
		for i, line := range lines {
			row := parseTestJSON(t, line)
			wantID := fmt.Sprintf("DEP-LIVE-%s-%s-%02d", date, project, i+1)
			if row["id"] != wantID {
				t.Errorf("%s row %d id = %v, want %v", project, i, row["id"], wantID)
			}
			if ids[i] != wantID {
				t.Errorf("%s reported task id = %v, want %v", project, ids[i], wantID)
			}
			// Canonical 31-key shape: every key present (nulls allowed).
			if len(row) != 31 {
				t.Errorf("%s row %d has %d keys, want the canonical 31", project, i, len(row))
			}
			for _, k := range nullKeys {
				if _, ok := row[k]; !ok {
					t.Errorf("%s row %d missing key %q", project, i, k)
				}
			}
			if row["status"] != "pending" || row["worker_status"] != "pending" {
				t.Errorf("%s row %d statuses = %v/%v, want pending/pending",
					project, i, row["status"], row["worker_status"])
			}
			if row["priority"] != "P2" {
				t.Errorf("%s row %d priority = %v, want P2 (default)", project, i, row["priority"])
			}
			wantTitles := []string{
				"Audit " + project + " board hygiene",
				"Close stale rows on " + project,
			}
			if row["title"] != wantTitles[i] {
				t.Errorf("%s row %d title = %v, want %v", project, i, row["title"], wantTitles[i])
			}
			note, _ := row["foreman_note"].(string)
			if !strings.Contains(note, "DEP-LIVE") || !strings.Contains(note, "live-grp") {
				t.Errorf("%s row %d foreman_note = %q, want template+group provenance", project, i, note)
			}
		}
	}
	if s := summaryInt(t, resp, "projects"); s != 2 {
		t.Errorf("summary.projects = %d, want 2", s)
	}
	if s := summaryInt(t, resp, "appended"); s != 2 {
		t.Errorf("summary.appended = %d, want 2", s)
	}
	if s := summaryInt(t, resp, "task_rows"); s != 4 {
		t.Errorf("summary.task_rows = %d, want 4", s)
	}
	if s := summaryInt(t, resp, "errors"); s != 0 {
		t.Errorf("summary.errors = %d, want 0", s)
	}
}

// ── Deploy: failure isolation ─────────────────────────────────────────────

// TestBlocksAPI_DeployPerProjectFailureDoesNotAbortBatch verifies every
// broken member is reported in the response errors while the healthy members
// still get their rows — the batch never aborts.
func TestBlocksAPI_DeployPerProjectFailureDoesNotAbortBatch(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "DEP-PARTIAL")
	goodWD, goodBoard := makeBoardDir(t, "")
	noBoardWD := t.TempDir() // exists, but has no .coding-hermes board
	b.mustCreateProject(t, "part-good", goodWD)
	b.mustCreateProject(t, "part-noboard", noBoardWD)
	b.mustCreateProject(t, "part-gone", filepath.Join(t.TempDir(), "missing"))
	b.mustCreateGroup(t, "part-grp", "part-good", "part-noboard", "part-gone", "part-unknown")

	status, resp := b.do(t, "POST", "/api/v1/groups/part-grp/deploy", map[string]interface{}{
		"template": "DEP-PARTIAL",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-project failures never abort): %v", status, resp)
	}
	out := deployProjects(t, resp)
	if len(out) != 4 {
		t.Fatalf("outcomes = %d, want 4", len(out))
	}
	if got := out["part-good"]["status"]; got != "appended" {
		t.Errorf("part-good status = %v, want appended (%v)", got, out["part-good"]["reason"])
	}
	for _, project := range []string{"part-noboard", "part-gone", "part-unknown"} {
		o := out[project]
		if o == nil {
			t.Fatalf("no outcome for %s", project)
		}
		if got := o["status"]; got != "error" {
			t.Errorf("%s status = %v, want error", project, got)
		}
		if reason, _ := o["reason"].(string); reason == "" {
			t.Errorf("%s has no failure reason", project)
		}
		if ids, ok := o["task_ids"]; ok && ids != nil {
			t.Errorf("%s errored but reported task_ids = %v", project, ids)
		}
	}
	if s := summaryInt(t, resp, "projects"); s != 4 {
		t.Errorf("summary.projects = %d, want 4", s)
	}
	if s := summaryInt(t, resp, "appended"); s != 1 {
		t.Errorf("summary.appended = %d, want 1", s)
	}
	if s := summaryInt(t, resp, "errors"); s != 3 {
		t.Errorf("summary.errors = %d, want 3", s)
	}
	// The healthy member really got its rows despite the other failures.
	if got := len(fileLines(t, goodBoard)); got != 2 {
		t.Errorf("part-good board lines = %d, want 2", got)
	}
	// Exactly one INFO api event records the deploy (provenance).
	evs, err := database.ListEvents(context.Background(), b.db, "", "api", 50, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	deploys := 0
	for _, ev := range evs {
		if strings.HasPrefix(ev.Message, "template deploy: DEP-PARTIAL") {
			deploys++
		}
	}
	if deploys != 1 {
		t.Errorf("template deploy events = %d, want exactly 1", deploys)
	}
}

// ── Deploy: request errors ────────────────────────────────────────────────

func TestBlocksAPI_DeployErrors(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateTemplate(t, "DEP-ERR")
	b.mustCreateGroup(t, "err-grp", "err-member")
	b.mustCreateGroup(t, "empty-grp")

	// Unknown group → 404.
	status, resp := b.do(t, "POST", "/api/v1/groups/no-such-group/deploy", map[string]interface{}{
		"template": "DEP-ERR",
	})
	if status != http.StatusNotFound {
		t.Errorf("unknown group: status = %d, want 404: %v", status, resp)
	}
	// Unknown template → 404.
	status, resp = b.do(t, "POST", "/api/v1/groups/err-grp/deploy", map[string]interface{}{
		"template": "no-such-template",
	})
	if status != http.StatusNotFound {
		t.Errorf("unknown template: status = %d, want 404: %v", status, resp)
	}
	// Missing template field → 400.
	status, resp = b.do(t, "POST", "/api/v1/groups/err-grp/deploy", map[string]interface{}{})
	if status != http.StatusBadRequest {
		t.Errorf("missing template: status = %d, want 400: %v", status, resp)
	} else if msg, _ := resp["error"].(string); !strings.Contains(msg, "template is required") {
		t.Errorf("missing template error = %q, want 'template is required'", msg)
	}
	// Whitespace template → 400.
	status, resp = b.do(t, "POST", "/api/v1/groups/err-grp/deploy", map[string]interface{}{
		"template": "   ",
	})
	if status != http.StatusBadRequest {
		t.Errorf("blank template: status = %d, want 400: %v", status, resp)
	}
	// Invalid body → 400.
	status, resp = b.doRaw(t, "POST", "/api/v1/groups/err-grp/deploy", `{"template":`)
	if status != http.StatusBadRequest {
		t.Errorf("invalid body: status = %d, want 400: %v", status, resp)
	}
	// Group with no members → 400 (nothing to deploy to).
	status, resp = b.do(t, "POST", "/api/v1/groups/empty-grp/deploy", map[string]interface{}{
		"template": "DEP-ERR",
	})
	if status != http.StatusBadRequest {
		t.Errorf("empty group: status = %d, want 400: %v", status, resp)
	} else if msg, _ := resp["error"].(string); !strings.Contains(msg, "no projects") {
		t.Errorf("empty group error = %q, want it to mention 'no projects'", msg)
	}
	// Deploy is POST-only; unknown sub-routes are 404.
	if status, _ := b.do(t, "GET", "/api/v1/groups/err-grp/deploy", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET deploy: status = %d, want 405", status)
	}
	if status, _ := b.do(t, "GET", "/api/v1/groups/err-grp/bogus", nil); status != http.StatusNotFound {
		t.Errorf("unknown sub-route: status = %d, want 404", status)
	}
}

// ── Torn JSONL tolerance at the handler level ─────────────────────────────

// TestBlocksAPI_TornJSONLLinesTolerated verifies a torn final line (crash
// mid-write signature: no trailing newline) never fails a list call, and that
// the next mutation heals the file.
func TestBlocksAPI_TornJSONLLinesTolerated(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateGroup(t, "torn-group", "member")
	b.mustCreateTemplate(t, "torn-template", "Task on {PROJECT}")

	// Append partial records with no trailing newline.
	appendTestFile(t, b.groupsPath(), `{"name":"torn-frag","projects":["x"],"descrip`)
	appendTestFile(t, b.templatesPath(), `{"name":"torn-tpl","tasks":[{"tit`)

	status, resp := b.do(t, "GET", "/api/v1/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("GET groups on torn file: status = %d, want 200: %v", status, resp)
	}
	groups := jsonNames(t, resp["groups"], "groups", "name")
	if len(groups) != 1 || groups[0] != "torn-group" {
		t.Errorf("groups on torn file = %v, want [torn-group] (fragment skipped)", groups)
	}

	status, resp = b.do(t, "GET", "/api/v1/templates", nil)
	if status != http.StatusOK {
		t.Fatalf("GET templates on torn file: status = %d, want 200: %v", status, resp)
	}
	templates := jsonNames(t, resp["templates"], "templates", "name")
	if len(templates) != 1 || templates[0] != "torn-template" {
		t.Errorf("templates on torn file = %v, want [torn-template] (fragment skipped)", templates)
	}

	// The torn fragment is not addressable as a record.
	if status, _ := b.do(t, "GET", "/api/v1/groups/torn-frag", nil); status != http.StatusNotFound {
		t.Errorf("GET torn fragment: status = %d, want 404", status)
	}

	// The next mutation rewrites the file and drops the fragment.
	if status, resp := b.do(t, "POST", "/api/v1/groups", groupBody("healed-group", "member")); status != http.StatusCreated {
		t.Fatalf("create after torn tail: status = %d, want 201: %v", status, resp)
	}
	data := readTestFile(t, b.groupsPath())
	if strings.Contains(data, "torn-frag") {
		t.Errorf("groups.jsonl still carries the torn fragment after a write: %q", data)
	}
	if !strings.HasSuffix(data, "\n") {
		t.Error("groups.jsonl does not end with a newline after a write")
	}
	status, resp = b.do(t, "GET", "/api/v1/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("GET groups after healing: status = %d, want 200", status)
	}
	if got := jsonNames(t, resp["groups"], "groups", "name"); len(got) != 2 {
		t.Errorf("groups after healing = %v, want 2", got)
	}
}

// ── Concurrency (-race) ───────────────────────────────────────────────────

// TestBlocksAPI_ConcurrentGroupCreatesRace drives concurrent POSTs through
// the JSONL store: distinct names must all land, and same-name contention
// must resolve to exactly one create (the rest 409) with the file left
// parseable — the read-modify-write lock must hold under -race.
func TestBlocksAPI_ConcurrentGroupCreatesRace(t *testing.T) {
	b := newBlocksTestServer(t)
	const n = 12

	bodies := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		bodies = append(bodies, groupBody(fmt.Sprintf("race-group-%02d", i), "member"))
	}
	for i, st := range concurrentPosts(b.ts.URL, "/api/v1/groups", bodies) {
		if st != http.StatusCreated {
			t.Errorf("concurrent create %d: status = %d, want 201", i, st)
		}
	}

	status, resp := b.do(t, "GET", "/api/v1/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("GET groups: status = %d, want 200", status)
	}
	if got := jsonNames(t, resp["groups"], "groups", "name"); len(got) != n {
		t.Errorf("groups after %d concurrent creates = %d, want %d (no lost updates)", n, len(got), n)
	}

	// Same-name contention: the store lock must let exactly one create win.
	dups := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		dups = append(dups, groupBody("race-dup"))
	}
	created, conflicts := 0, 0
	for _, st := range concurrentPosts(b.ts.URL, "/api/v1/groups", dups) {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("same-name concurrent create: unexpected status %d", st)
		}
	}
	if created != 1 || conflicts != n-1 {
		t.Errorf("same-name creates: created=%d conflicts=%d, want 1/%d", created, conflicts, n-1)
	}

	// Every line is still one complete JSON record.
	for i, line := range fileLines(t, b.groupsPath()) {
		if rec := parseTestJSON(t, line); rec["name"] == nil {
			t.Errorf("groups.jsonl line %d has no name: %v", i+1, rec)
		}
	}
}
