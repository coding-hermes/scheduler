package dashboard

// SCHED-GAP-1601 — operator console integration tests (in-package: they pin
// unexported seams — the action registry, the proxy's source-level confirm
// path, and the API's auth config type).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

const (
	controlTestUser  = "operator"
	controlTestToken = "sched-gap-1601-operator-token"
)

// newControlStack wires the REAL stack main.go builds — dashboard Generator +
// api.Server + loop over one in-memory DB, the control seam pointed at the
// API handler — with the operator gate armed in BASIC mode: the browser path
// the auth row designates (401 + WWW-Authenticate → native prompt). The DB
// is returned so tests can seed lanes the API handler (and therefore the
// proxy) will actually see.
func newControlStack(t *testing.T) (*Generator, *api.Server, *sql.DB) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)

	apiSrv := api.NewServer(db, loop)
	apiSrv.SetAuthConfig(api.ResolveAuthConfig("", controlTestUser, controlTestToken))
	apiSrv.SetBlocksStore(blocks.NewStore(
		filepath.Join(t.TempDir(), "groups.jsonl"),
		filepath.Join(t.TempDir(), "templates.jsonl"),
	))

	gen := NewGenerator(nil, nil)
	gen.SetControlAPIHandler(apiSrv.Handler())
	gen.SetFleetPaused(loop.IsPaused)
	return gen, apiSrv, db
}

// postControl posts a urlencoded instruction through the same mux shape
// main.go registers, returning the proxy's response.
func postControl(t *testing.T, gen *Generator, form url.Values, credHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dashboard/control", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range credHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/dashboard/control", gen.ControlProxy)
	mux.ServeHTTP(rec, req)
	return rec
}

// fetchOpenAPIOps re-derives the LIVE API operation set from the served
// openapi.json (the acceptance-mandated source of truth), returning
// "METHOD /path" keys.
func fetchOpenAPIOps(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET openapi.json: %v", err)
	}
	defer resp.Body.Close()
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	ops := map[string]bool{}
	for path, item := range spec.Paths {
		norm := normalizeOpPath(path)
		for m := range item {
			switch strings.ToLower(m) {
			case "get", "post", "put", "delete", "patch":
				ops[strings.ToUpper(strings.ToLower(m))+" "+norm] = true
			}
		}
	}
	return ops
}

// normalizeOpPath makes console path templates (%s) and openapi path
// templates ({name}/{id}) comparable: both normalize the single parameter
// slot to {t}.
func normalizeOpPath(p string) string {
	p = strings.ReplaceAll(p, "%s", "{t}")
	p = strings.ReplaceAll(p, "{name}", "{t}")
	p = strings.ReplaceAll(p, "{id}", "{t}")
	return p
}

// TestControlVocabularyParity is the brief's (e): the console's operations
// are pinned to the guarded API set. Every action's METHOD+PATH must be an
// operation the live openapi.json offers, and the registry must be complete
// against the rendering slices (no registered-but-unrenderable action — the
// "unlisted gap" the brief forbids).
func TestControlVocabularyParity(t *testing.T) {
	_, apiSrv, _ := newControlStack(t)
	live := fetchOpenAPIOps(t, apiSrv.Handler())

	if err := controlRegistryComplete(); err != nil {
		t.Fatalf("console registry is incomplete against its render lists: %v", err)
	}
	for id, a := range controlActions {
		op := a.Method + " " + normalizeOpPath(a.Path)
		if !live[op] {
			t.Errorf("console action %q maps to %q which the LIVE openapi.json does not offer — no invented verbs allowed", id, op)
		}
	}
}

// TestControlRegistryCovers22 documents, as a test, the brief's (a) decision:
// which of the 22 API mutations the console exposes and which it deliberately
// does not. The map below IS the written decision — a change to either side
// without editing this map fails the test.
func TestControlRegistryCovers22(t *testing.T) {
	// The 22 mutating operations (method + openapi path).
	all22 := map[string]string{
		"POST /api/v1/projects":               "excluded — lane creation is a fleet-bootstrap act (repo/workdir paths); the console renders lanes, it does not onboard hosts. Documented exclusion.",
		"PUT /api/v1/projects/{name}":         "exposed — project_update on the lane page",
		"DELETE /api/v1/projects/{name}":      "exposed — project_delete (confirm-gated)",
		"POST /api/v1/projects/{name}/pause":  "exposed — project_pause",
		"POST /api/v1/projects/{name}/resume": "exposed — project_resume",
		"POST /api/v1/projects/{name}/spawn":  "exposed — project_spawn (long-running: reported as accepted)",
		"POST /api/v1/projects/{name}/bump":   "exposed — project_bump (reason required)",
		"POST /api/v1/projects/{name}/unbump": "exposed — project_unbump",
		"POST /api/v1/namespaces":             "exposed — namespace_create (on the blocks/console page)",
		"PUT /api/v1/namespaces/{id}":         "exposed — namespace_update",
		"DELETE /api/v1/namespaces/{id}":      "exposed — namespace_delete (confirm-gated)",
		"POST /api/v1/namespaces/{id}/move":   "exposed — namespace_move",
		"POST /api/v1/groups":                 "exposed — group_create",
		"PUT /api/v1/groups/{name}":           "exposed — group_update",
		"DELETE /api/v1/groups/{name}":        "exposed — group_delete (confirm-gated)",
		"POST /api/v1/groups/{name}/deploy":   "exposed — group_deploy (confirm-gated, dry-run-first)",
		"POST /api/v1/templates":              "exposed — template_create",
		"PUT /api/v1/templates/{name}":        "exposed — template_update",
		"DELETE /api/v1/templates/{name}":     "exposed — template_delete (confirm-gated)",
		"POST /api/v1/evaluate":               "exposed — global_evaluate",
		"POST /api/v1/pause":                  "exposed — global_pause (confirm-gated, fleet-wide)",
		"POST /api/v1/resume":                 "exposed — global_resume",
	}
	// The console's own method+path set (normalized the same way).
	consoleOps := map[string]bool{}
	for _, a := range controlActions {
		consoleOps[a.Method+" "+normalizeOpPath(a.Path)] = true
	}
	excluded := 0
	for op, decision := range all22 {
		exposed := consoleOps[normalizeOpPath(op)]
		switch {
		case exposed && strings.HasPrefix(decision, "excluded"):
			t.Errorf("%s is marked excluded but the console registers it", op)
		case !exposed && strings.HasPrefix(decision, "exposed"):
			t.Errorf("%s is marked exposed but the console has no action for it", op)
		}
		if !exposed {
			excluded++
		}
	}
	if excluded != 1 {
		t.Errorf("expected exactly 1 documented exclusion (lane creation), got %d — update the decision map when the surface changes", excluded)
	}
	if len(all22) != 22 {
		t.Errorf("the 22-op census drifted: map holds %d entries — re-derive from openapi.json", len(all22))
	}
}

// TestControlProxy_AuthGateForwarded proves the proxy is a dumb pipe for
// credentials and failures:
//   - no credential → the API's 401 + WWW-Authenticate forwarded verbatim;
//   - wrong credential → the API's 401 (bad credential) forwarded;
//   - correct credential → 200 and the REAL lane state flips (paused row).
func TestControlProxy_AuthGateForwarded(t *testing.T) {
	gen, _, db := newControlStack(t)
	if err := database.CreateProject(nil2ctx(), db, &database.Project{
		Name: "authlane", RepoURL: "https://example.com/a", Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("seed lane: %v", err)
	}

	// No credential: the API refuses with 401 + challenge; the proxy must
	// forward the challenge so the browser's basic prompt can fire.
	rec := postControl(t, gen, url.Values{
		"action": {"project_pause"}, "target": {"authlane"},
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: got %d, want 401 forwarded", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no credential: WWW-Authenticate challenge must be forwarded for the browser prompt")
	}

	// Wrong credential: still 401, no success, state unchanged.
	rec = postControl(t, gen, url.Values{
		"action": {"project_pause"}, "target": {"authlane"},
	}, map[string]string{"Authorization": "Basic " + basicAuth("operator", "wrong")})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad credential: got %d, want 401 forwarded", rec.Code)
	}

	// Correct credential: the REAL mutation lands (acceptance criterion 1,
	// exercised at the API level the browser reaches).
	rec = postControl(t, gen, url.Values{
		"action": {"project_pause"}, "target": {"authlane"},
	}, map[string]string{"Authorization": "Basic " + basicAuth("operator", controlTestToken)})
	if rec.Code != http.StatusOK {
		t.Fatalf("good credential: got %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	p, err := database.GetProject(nil2ctx(), db, "authlane")
	if err != nil {
		t.Fatalf("reload lane: %v", err)
	}
	if p.Enabled {
		t.Fatalf("lane still enabled after pause — the proxied mutation did not land")
	}
}

// TestControlProxy_FailedActionSurfacesAPIRefusal is acceptance criterion 4:
// a FAILED action surfaces the API's message — forced with a REAL refusal:
// bump a lane twice; the second bump 409s ("bump already active") inside the
// API and the proxy forwards that status + message verbatim.
func TestControlProxy_FailedActionSurfacesAPIRefusal(t *testing.T) {
	gen, _, db := newControlStack(t)
	if err := database.CreateProject(nil2ctx(), db, &database.Project{
		Name: "bumplane", RepoURL: "https://example.com/b", Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("seed lane: %v", err)
	}
	cred := map[string]string{"Authorization": "Basic " + basicAuth(controlTestUser, controlTestToken)}

	// First bump: succeeds (200) — this is the state the second bump refuses.
	rec := postControl(t, gen, url.Values{
		"action": {"project_bump"}, "target": {"bumplane"},
		"reason": {"first bump"}, "ticks": {"2"},
	}, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("first bump: got %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}

	// Second bump: the API refuses 409 (bump already active) and the proxy
	// forwards the refusal verbatim — the console cannot hide a failure.
	rec = postControl(t, gen, url.Values{
		"action": {"project_bump"}, "target": {"bumplane"},
		"reason": {"second bump"},
	}, cred)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second bump: got %d, want the API's 409 forwarded — body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse refusal body: %v", err)
	}
	if !strings.Contains(strings.ToLower(body.Error), "bump already active") {
		t.Errorf("refusal body should name the bump conflict, got: %q", body.Error)
	}
}

// TestControlProxy_AuditRowWritten is acceptance criterion 5: each action
// writes an auditable record — the API's own api.auth events row, read back
// through the same events helper /api/v1/events serves.
func TestControlProxy_AuditRowWritten(t *testing.T) {
	gen, apiSrv, db := newControlStack(t)
	if err := database.CreateProject(nil2ctx(), db, &database.Project{
		Name: "auditlane", RepoURL: "https://example.com/c", Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("seed lane: %v", err)
	}
	rec := postControl(t, gen, url.Values{
		"action": {"project_resume"}, "target": {"auditlane"},
	}, map[string]string{"X-Operator-Token": controlTestToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	events, err := database.ListEventsRecent(nil2ctx(), db, 50)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Component == "api.auth" &&
			strings.Contains(e.Message, "allowed") &&
			strings.Contains(e.Message, "project auditlane resume") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no api.auth audit row for the proxied action — the console bypassed the API's audit point")
	}
	_ = apiSrv
}

// TestControlProxy_UnwiredFailsClosed proves the fail-closed arm: with no
// in-process API handler wired, the proxy refuses (503) and forwards nothing.
func TestControlProxy_UnwiredFailsClosed(t *testing.T) {
	gen := NewGenerator(nil, nil)
	rec := postControl(t, gen, url.Values{"action": {"global_resume"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired proxy: got %d, want 503 (fail-closed)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "control API not wired") {
		t.Fatalf("unwired proxy body should name the refusal, got: %s", rec.Body.String())
	}
}

// TestControlProxy_UnknownActionRefused proves the console cannot invent a
// verb: an action outside the registry is refused before any API call.
func TestControlProxy_UnknownActionRefused(t *testing.T) {
	gen, _, _ := newControlStack(t)
	rec := postControl(t, gen, url.Values{"action": {"nuke_everything"}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown action") {
		t.Fatalf("body should name the unknown action, got: %s", rec.Body.String())
	}
}

// TestControlProxy_ConfirmRequiredBeforeAPI proves destructive/fleet-wide
// actions refuse WITHOUT the confirm flag — a stray click cannot reach the
// API at all (the refusal names the console gate, not the auth gate).
func TestControlProxy_ConfirmRequiredBeforeAPI(t *testing.T) {
	gen, _, _ := newControlStack(t)
	for _, action := range []string{"project_delete", "namespace_delete", "group_delete", "template_delete", "global_pause", "group_deploy"} {
		form := url.Values{"action": {action}, "target": {"some-target"}}
		if action == "global_pause" {
			form.Del("target")
		}
		rec := postControl(t, gen, form, map[string]string{"X-Operator-Token": controlTestToken})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s without confirm: got %d, want 400 (console-side refusal)", action, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "operator credential") {
			t.Fatalf("%s without confirm reached the API auth gate — the console gate did not run first", action)
		}
	}
}

// TestControlProxy_ConfirmUsesAPIConvention proves the confirm path reuses
// the API's own ?confirm=true convention (not a second scheme) and NEVER
// sends purge=true (soft-delete only behind a modal click).
func TestControlProxy_ConfirmUsesAPIConvention(t *testing.T) {
	req, err := buildAPIRequest(nil2ctx(), controlRequest{
		Action: "project_delete", Target: "somelane", Confirm: true,
	})
	if err != nil {
		t.Fatalf("build confirm request: %v", err)
	}
	if !strings.Contains(req.URL.RawQuery, "confirm=true") {
		t.Fatalf("confirm path must carry the API's confirm=true convention, got %q", req.URL.RawQuery)
	}
	if strings.Contains(req.URL.RawQuery, "purge") {
		t.Fatalf("the console must NEVER send purge=true (soft-delete only), got %q", req.URL.RawQuery)
	}
}

// TestControlNeverHandlesCredentials is a source scan of the console driver:
// the JS must not touch web storage, cookies or credential headers — the
// browser's own basic-auth cache is the only credential path (the auth row's
// "worse than no auth" ruling).
func TestControlNeverHandlesCredentials(t *testing.T) {
	raw := pageTemplate + blocksConsoleSrc()
	for _, bad := range []string{"localStorage.setItem('token", "sessionStorage", "document.cookie", "X-Operator-Token", "Authorization"} {
		if strings.Contains(raw, bad) {
			t.Errorf("console JS source contains %q — the console must never handle credentials itself", bad)
		}
	}
}

// ── helpers ──

// nil2ctx is context.Background() spelled short for the seed helpers.
func nil2ctx() context.Context { return context.Background() }

// basicAuth encodes a user:password pair for the Authorization header.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// TestControlButtonsRenderOnPages proves the strips actually render on the
// three console-bearing pages (the fleet overview's global strip, the lane
// page's strip, the namespace page's strip) with their result regions.
func TestControlButtonsRenderOnPages(t *testing.T) {
	gen, _, db := newControlStack(t)
	// The generator renders pages from THIS db (the same one the API
	// handler reads), so the strips and the data agree.
	gen.SetDB(db)
	gen.SetBlocksStore(blocks.NewStore(
		filepath.Join(t.TempDir(), "groups.jsonl"),
		filepath.Join(t.TempDir(), "templates.jsonl"),
	))
	if err := database.CreateProject(nil2ctx(), db, &database.Project{
		Name: "renderlane", RepoURL: "https://example.com/r", Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true, NamespaceID: nil,
	}); err != nil {
		t.Fatalf("seed lane: %v", err)
	}
	if err := database.CreateNamespace(nil2ctx(), db, &database.Namespace{
		ID: "nsx", Weight: 50, Enabled: true,
	}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	// Lane page.
	var sb strings.Builder
	if err := gen.GenerateProjectDetail(&sb, "renderlane"); err != nil {
		t.Fatalf("render project detail: %v", err)
	}
	page := sb.String()
	for _, want := range []string{"data-action=\"project_pause\"", "data-action=\"project_resume\"", "data-action=\"project_spawn\"", "data-action=\"project_delete\"", "ctl-result"} {
		if !strings.Contains(page, want) {
			t.Errorf("lane page missing control markup %q", want)
		}
	}

	// Namespace page.
	sb.Reset()
	if err := gen.GenerateNamespaceView(&sb, "nsx"); err != nil {
		t.Fatalf("render namespace view: %v", err)
	}
	page = sb.String()
	for _, want := range []string{"data-action=\"namespace_update\"", "data-action=\"namespace_move\"", "data-action=\"namespace_delete\""} {
		if !strings.Contains(page, want) {
			t.Errorf("namespace page missing control markup %q", want)
		}
	}

	// Overview page: global strip + paused badge.
	sb.Reset()
	if err := gen.Generate(&sb); err != nil {
		t.Fatalf("render overview: %v", err)
	}
	page = sb.String()
	for _, want := range []string{"data-action=\"global_pause\"", "data-action=\"global_resume\"", "data-action=\"global_evaluate\"", "fleetPausedBadge"} {
		if !strings.Contains(page, want) {
			t.Errorf("overview page missing control markup %q", want)
		}
	}

	// Blocks page renders actions + lists.
	sb.Reset()
	if err := gen.GenerateBlocksConsole(&sb, nil); err != nil {
		t.Fatalf("render blocks console: %v", err)
	}
	page = sb.String()
	for _, want := range []string{"data-action=\"group_deploy\"", "data-action=\"template_create\"", "Deploy Blocks"} {
		if !strings.Contains(page, want) {
			t.Errorf("blocks page missing control markup %q", want)
		}
	}
}
