package mcp_test

// SCHED-GAP-166 — endpoints-table parity guard (docs/reference/endpoints.md).
//
// The 2026-09-18 drift this guard exists for: the endpoint table carried in
// AGENTS.md then (now docs/reference/endpoints.md) listed 15 rows while the
// daemon served a larger route set. Four live
// routes were missing entirely — /api/v1/groups, /api/v1/templates,
// /api/v1/metrics and /api/v1/events/stream — and the table carried no MCP
// surface statement at all, so an agent building a capability or security
// inventory from that table understated the daemon's write surface.
//
// SCHED-GAP-1580 added the second half of the same guard: the daemon's own
// SERVED OpenAPI 3.x document (/api/v1/openapi.json) is now checked against
// the same route surface, because that document — not this table — is what a
// muster consumer (docs/reference/openapi.md) generates its CLI verbs and MCP
// tools from.
//
// Both sides of the comparison are IN-REPO, never a live daemon process:
//
//   - the documented side is parsed out of docs/reference/endpoints.md, located
//     by walking up from the test's working directory (same idiom as
//     readmeRepoRoot in readme_tools_parity_test.go), so the guard runs under
//     `go test ./internal/mcp/` with no daemon running;
//   - the route side is walked from the SOURCE registrations themselves —
//     `mux.HandleFunc(…)`/`mux.Handle(…)` string literals in internal/api/*.go
//     (the /api/v1/* surface) and cmd/schedulerd/main.go (the HTML pages and
//     /mcp). A route added to one of those registrations and not documented
//     here fails this test, and so does a documented row whose registration
//     was removed.
//
// The stated MCP tool count is checked against the same live in-process
// registry the CTL-003 and SCHED-GAP-188 guards enumerate
// (fetchMCPToolNames in mcp_api_parity_test.go), so a tool added to
// internal/mcp/server.go lands here automatically.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// agentsRouteRowRe matches one row of the endpoints table and
// captures the route cell — e.g. "| `/api/v1/groups` | Deploy groups … |".
// The cell cannot contain a backtick, so a row whose DESCRIPTION is
// backticked (the /api/v1/projects/{name} sub-route row) still yields only
// the route as a capture.
var agentsRouteRowRe = regexp.MustCompile("^[[:space:]]*\\|[[:space:]]*`([^`]+)`[[:space:]]*\\|")

// agentsMCPCountRe matches the stated MCP tool count in the Endpoints
// section's MCP surface paragraph, e.g. "`POST /mcp` serves **41 tools**".
var agentsMCPCountRe = regexp.MustCompile(`serves \*\*(\d+) tools\*\*`)

// routePatternRe extracts the path literal from a mux registration in Go
// source: mux.HandleFunc("/api/v1/health", …) / mux.Handle("GET /queue", …).
var routePatternRe = regexp.MustCompile(`mux\.(?:HandleFunc|Handle)\(\s*"([^"]+)"`)

// nonRouteRegistrations are mux registrations that are deliberately NOT
// routes of their own and therefore never get a documented row: "/api/"
// mounts the API handler that serves the documented /api/v1/* paths, and
// "/mcp/" is the trailing-slash alias of the documented "/mcp".
var nonRouteRegistrations = map[string]bool{"/api/": true, "/mcp/": true}

// Re-anchor floors. A regex that stops matching the table's row shape, or a
// source walk that finds nothing, must fail LOUDLY instead of comparing an
// empty set against an empty set and passing. The documented-row floor is
// deliberately LOW: it only proves the row regex still matches the table
// shape (a broken regex yields ~0 rows, not a handful). Whether the table is
// COMPLETE is not the floor's job — that is exactly what the route-set
// comparison below proves, and a floor set near the expected row count would
// mask the drift findings behind a generic "too few rows" message.
const (
	minDocumentedRows = 10
	minLiveExact      = 15
	minLiveSubtree    = 4
)

// routeSets is the in-repo route registry, split by registration shape.
// exact holds patterns registered at their final path ("/api/v1/health");
// subtree holds trailing-slash subtree patterns ("/api/v1/projects/") whose
// deeper paths ({name}/pause, {name}/deploy, …) are handled inside one
// handler and spelled out individually in the documentation.
type routeSets struct {
	exact   map[string]bool
	subtree map[string]bool
}

// agentsRepoRoot walks up from the test's working directory until it finds
// AGENTS.md, so the guard works from any package directory inside the repo.
func agentsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("AGENTS.md not found in any parent directory — run the test from within the repository checkout")
		}
		dir = parent
	}
}

// agentsEndpointSection returns the body of the "## Endpoints" section
// (everything up to the next "## " heading) so backticked identifiers in the
// flags, clock and tier tables elsewhere in the docs cannot satisfy — or
// pollute — the route check.
func agentsEndpointSection(agents string) (string, bool) {
	lines := strings.Split(agents, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Endpoints" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	return strings.Join(lines[start:], "\n"), true
}

// normalizeRoute puts a route into the form both sides of the comparison
// share: any leading HTTP-method token is dropped, a query string is dropped
// ("/ticks?page=N" documents the path "/ticks"), and a trailing slash is
// trimmed except on the root path.
func normalizeRoute(raw string) string {
	route := strings.TrimSpace(raw)
	if i := strings.Index(route, " "); i > 0 && !strings.HasPrefix(route, "/") {
		route = strings.TrimSpace(route[i+1:])
	}
	if i := strings.Index(route, "?"); i >= 0 {
		route = route[:i]
	}
	for len(route) > 1 && strings.HasSuffix(route, "/") {
		route = strings.TrimSuffix(route, "/")
	}
	return route
}

// parseAgentsEndpointRoutes returns the normalized route column of every row
// in the Endpoints section.
func parseAgentsEndpointRoutes(section string) []string {
	var routes []string
	for _, line := range strings.Split(section, "\n") {
		m := agentsRouteRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if route := normalizeRoute(m[1]); route != "" {
			routes = append(routes, route)
		}
	}
	return routes
}

// routeSetsFromSources builds the live registry from mux registration
// literals in the given Go sources.
func routeSetsFromSources(t *testing.T, paths []string) routeSets {
	t.Helper()
	live := routeSets{exact: map[string]bool{}, subtree: map[string]bool{}}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range routePatternRe.FindAllStringSubmatch(string(src), -1) {
			raw := m[1]
			if nonRouteRegistrations[raw] {
				continue
			}
			route := normalizeRoute(raw)
			if route == "" {
				continue
			}
			if strings.HasSuffix(raw, "/") && !strings.HasSuffix(route, "/") {
				live.subtree[route] = true
			} else {
				live.exact[route] = true
			}
		}
	}
	return live
}

// liveNames returns every registered route, sorted, with the exact and
// subtree sets de-duplicated.
func (r routeSets) liveNames() []string {
	seen := map[string]bool{}
	for name := range r.exact {
		seen[name] = true
	}
	for name := range r.subtree {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// underSubtree reports whether d is a deeper path inside one of the subtree
// registrations (e.g. "/api/v1/projects/{name}/pause" under
// "/api/v1/projects/").
func underSubtree(live routeSets, d string) bool {
	for sub := range live.subtree {
		if strings.HasPrefix(d, sub+"/") {
			return true
		}
	}
	return false
}

// documentedUnder reports whether the documentation spells out a deeper path
// inside the subtree route l (e.g. "/api/v1/groups/{name}/deploy" covers the
// "/api/v1/groups/" subtree registration).
func documentedUnder(documentedd map[string]bool, l string) bool {
	for d := range documentedd {
		if strings.HasPrefix(d, l+"/") {
			return true
		}
	}
	return false
}

// agentsEndpointParityFindings is the pure checker both the live test and the
// self-check drive. It compares the documented route set against the live
// registry in BOTH directions, returning one human-readable finding per
// divergence (empty = parity).
func agentsEndpointParityFindings(documented []string, live routeSets) []string {
	var findings []string

	documented = append([]string(nil), documented...)
	documentedSet := make(map[string]bool, len(documented))
	for _, d := range documented {
		documentedSet[d] = true
	}

	// ── direction 1: every documented row must exist in the registry ────
	for _, d := range documented {
		if live.exact[d] || live.subtree[d] || underSubtree(live, d) {
			continue
		}
		findings = append(findings, fmt.Sprintf("docs/reference/endpoints.md documents route %q which has no live registration — remove the row or fix the path", d))
	}

	// ── direction 2: every live registration must be documented ─────────
	for _, l := range live.liveNames() {
		if documentedSet[l] {
			continue
		}
		// A subtree registration is satisfied by a documented deeper path
		// (the sub-routes it serves). An exact registration is not: its
		// path must appear as its own row.
		if live.subtree[l] && documentedUnder(documentedSet, l) {
			continue
		}
		findings = append(findings, fmt.Sprintf("live route %q is missing from the docs/reference/endpoints.md route table — every registered route must have a row", l))
	}

	sort.Strings(findings)
	return findings
}

// sourcePathsForRoutes returns the Go sources that register routes, skipping
// test files (test muxes are not the daemon's surface).
func sourcePathsForRoutes(t *testing.T, root string) []string {
	t.Helper()
	apiFiles, err := filepath.Glob(filepath.Join(root, "internal", "api", "*.go"))
	if err != nil {
		t.Fatalf("glob internal/api: %v", err)
	}
	var paths []string
	for _, p := range apiFiles {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return append(paths, filepath.Join(root, "cmd", "schedulerd", "main.go"))
}

// TestAgentsEndpointTableParity is the SCHED-GAP-166 guard: the docs/reference/endpoints.md
// route table must name every route registered in the daemon's source
// (set equality, both directions) and the stated MCP tool count must equal
// the live registry size.
func TestAgentsEndpointTableParity(t *testing.T) {
	root := agentsRepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "docs", "reference", "endpoints.md"))
	if err != nil {
		t.Fatalf("read docs/reference/endpoints.md: %v", err)
	}
	agents := string(raw)

	section, ok := agentsEndpointSection(agents)
	if !ok {
		t.Fatalf(`docs/reference/endpoints.md has no "## Endpoints" section — the route table is missing`)
	}
	documented := parseAgentsEndpointRoutes(section)
	if len(documented) < minDocumentedRows {
		t.Fatalf("parsed only %d route rows from the docs/reference/endpoints.md Endpoints section (want >= %d) — the table or the row regex is broken", len(documented), minDocumentedRows)
	}

	live := routeSetsFromSources(t, sourcePathsForRoutes(t, root))
	if len(live.exact) < minLiveExact {
		t.Fatalf("source walk found only %d exact route registrations (want >= %d) — the registration regex no longer matches internal/api/*.go and cmd/schedulerd/main.go", len(live.exact), minLiveExact)
	}
	if len(live.subtree) < minLiveSubtree {
		t.Fatalf("source walk found only %d subtree (trailing-slash) registrations (want >= %d) — the registration regex is stale", len(live.subtree), minLiveSubtree)
	}

	if findings := agentsEndpointParityFindings(documented, live); len(findings) > 0 {
		t.Errorf("SCHED-GAP-166 endpoint-table/route-registry parity: %d divergences between docs/reference/endpoints.md and the %d live route registrations:", len(findings), len(live.liveNames()))
		for _, f := range findings {
			t.Errorf("  DRIFT %s", f)
		}
	}

	// ── stated MCP tool count must equal the live registry size ─────────
	m := newMCPTestServer(t)
	registry := fetchMCPToolNames(t, m.ts.URL)
	if len(registry) == 0 {
		t.Fatalf("tools/list returned zero tools — registry enumeration is broken")
	}
	matches := agentsMCPCountRe.FindAllStringSubmatch(section, -1)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 MCP tool-count statement in the docs/reference/endpoints.md Endpoints section, found %d — keep one canonical count per surface so this guard can check it", len(matches))
	}
	got, err := strconv.Atoi(matches[0][1])
	if err != nil {
		t.Fatalf("stated MCP tool count is not a number: %q", matches[0][1])
	}
	if got != len(registry) {
		t.Errorf("docs/reference/endpoints.md states %d MCP tools but the registry serves %d — update the table or investigate the registry change", got, len(registry))
	}
}

// TestAgentsEndpointTableParity_SelfCheck proves the guard bites in BOTH
// directions and is not vacuous. The documented/live sets are synthetic —
// nothing on disk is touched — so this runs as an ordinary unit test:
//
//   - the real shape passes;
//   - a REMOVED documented row for a live route fails (a route added to the
//     source and not documented);
//   - a stale documented row with no registration fails (a row whose route
//     was deleted);
//   - a subtree registration is satisfied by a documented deeper path but an
//     exact registration is not.
//
// The parse path is checked separately on synthetic markdown, so a regex
// regression that silently stops matching table rows also fails here.
func TestAgentsEndpointTableParity_SelfCheck(t *testing.T) {
	live := routeSets{
		exact: map[string]bool{
			"/":                     true,
			"/health":               true,
			"/healthz":              true,
			"/api/v1/health":        true,
			"/api/v1/status":        true,
			"/api/v1/metrics":       true,
			"/api/v1/events":        true,
			"/api/v1/events/stream": true,
			"/api/v1/groups":        true,
			"/api/v1/projects":      true,
			"/api/v1/templates":     true,
			"/api/v1/ticks":         true,
			"/mcp":                  true,
		},
		subtree: map[string]bool{
			"/api/v1/projects":   true,
			"/api/v1/namespaces": true,
			"/api/v1/groups":     true,
			"/api/v1/ticks":      true,
		},
	}
	real := []string{
		"/", "/health", "/healthz",
		"/api/v1/health", "/api/v1/status", "/api/v1/metrics",
		"/api/v1/events", "/api/v1/events/stream",
		"/api/v1/groups", "/api/v1/projects", "/api/v1/templates", "/api/v1/ticks",
		"/api/v1/projects/{name}", "/api/v1/projects/{name}/deploy",
		"/api/v1/namespaces/{id}", "/api/v1/ticks/{id}",
		"/mcp",
	}

	cases := []struct {
		name       string
		documented []string
		live       routeSets
		wantErr    bool
	}{{
		name:       "real shape passes",
		documented: real,
		live:       live,
		wantErr:    false,
	}, {
		name:       "removed row for a live route fails",
		documented: removeRoute(real, "/api/v1/metrics"),
		live:       live,
		wantErr:    true,
	}, {
		name:       "stale documented row fails",
		documented: append(append([]string(nil), real...), "/api/v1/retired-route"),
		live:       live,
		wantErr:    true,
	}, {
		name: "subtree registration covered by a deeper documented path passes",
		documented: []string{
			"/api/v1/groups/{name}/deploy", "/api/v1/namespaces/{id}",
			"/", "/health", "/healthz",
			"/api/v1/health", "/api/v1/status", "/api/v1/metrics",
			"/api/v1/events", "/api/v1/events/stream",
			"/api/v1/groups", "/api/v1/projects", "/api/v1/projects/{name}",
			"/api/v1/templates", "/api/v1/ticks",
			"/mcp",
		},
		live:    live,
		wantErr: false,
	}, {
		name: "exact registration is NOT covered by a deeper documented path",
		documented: []string{
			"/", "/health", "/healthz",
			"/api/v1/health", "/api/v1/status", "/api/v1/metrics",
			// "/api/v1/events" itself is dropped; only its deeper twin is
			// documented — an exact route must have its own row.
			"/api/v1/events/stream",
			"/api/v1/groups", "/api/v1/projects", "/api/v1/{name}",
			"/api/v1/projects/{name}/deploy", "/api/v1/namespaces/{id}",
			"/api/v1/templates", "/api/v1/ticks", "/api/v1/ticks/{id}",
			"/mcp",
		},
		live:    live,
		wantErr: true,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := agentsEndpointParityFindings(tc.documented, tc.live)
			if tc.wantErr && len(findings) == 0 {
				t.Fatalf("self-check: expected findings, got none — the guard is blind in this direction")
			}
			if !tc.wantErr && len(findings) != 0 {
				t.Fatalf("self-check: expected parity, got findings %v — checker is over-firing", findings)
			}
		})
	}

	// The removed-row case must fail for the RIGHT reason (the missing live
	// route), not merely because some finding appeared.
	findings := agentsEndpointParityFindings(removeRoute(real, "/api/v1/metrics"), live)
	want := `live route "/api/v1/metrics" is missing from the docs/reference/endpoints.md route table — every registered route must have a row`
	if len(findings) != 1 || findings[0] != want {
		t.Fatalf("self-check: removed-row findings = %v, want exactly [%q]", findings, want)
	}
	// And the stale-row case likewise.
	stale := append(append([]string(nil), real...), "/api/v1/retired-route")
	findings = agentsEndpointParityFindings(stale, live)
	wantStale := `docs/reference/endpoints.md documents route "/api/v1/retired-route" which has no live registration — remove the row or fix the path`
	if len(findings) != 1 || findings[0] != wantStale {
		t.Fatalf("self-check: stale-row findings = %v, want exactly [%q]", findings, wantStale)
	}

	// ── parse path: synthetic markdown tables ──────────────────────────
	clean := "## Endpoints\n\n" +
		"| Route | Purpose |\n" +
		"|-------|---------|\n" +
		"| `/` | Fleet dashboard |\n" +
		"| `/ticks?page=N` | Paginated tick history |\n" +
		"| `/api/v1/health` | Health (JSON) |\n" +
		"| `/api/v1/metrics` | Fleet metrics (JSON) |\n" +
		"| `/mcp` | MCP JSON-RPC endpoint |\n" +
		"\n" +
		"`POST /mcp` serves **41 tools**\n" +
		"\n" +
		"## Next Section\n" +
		"| `--db` | not a route |\n"
	section, ok := agentsEndpointSection(clean)
	if !ok {
		t.Fatalf("self-check: synthetic markdown has no Endpoints section")
	}
	got := parseAgentsEndpointRoutes(section)
	want2 := []string{"/", "/ticks", "/api/v1/health", "/api/v1/metrics", "/mcp"}
	if strings.Join(got, ",") != strings.Join(want2, ",") {
		t.Fatalf("self-check: parsed routes = %v, want %v (query stripped, flags table excluded)", got, want2)
	}
	if m := agentsMCPCountRe.FindAllStringSubmatch(section, -1); len(m) != 1 || m[0][1] != "41" {
		t.Fatalf("self-check: MCP count matches = %v, want one match of \"41\"", m)
	}
	// A table with a row removed must yield the shorter route set.
	trimmed := strings.Replace(clean, "| `/api/v1/metrics` | Fleet metrics (JSON) |\n", "", 1)
	section2, _ := agentsEndpointSection(trimmed)
	if got2 := parseAgentsEndpointRoutes(section2); len(got2) != len(got)-1 {
		t.Fatalf("self-check: row removal not observed by the parser: %v -> %v", got, got2)
	}
	if _, ok := agentsEndpointSection("## Other\n\n| `/x` | y |\n"); ok {
		t.Fatalf("self-check: section lookup matched markdown with no Endpoints heading")
	}
}

// removeRoute returns routes without the named entry.
func removeRoute(routes []string, drop string) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		if r == drop {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ─── SCHED-GAP-1580 — the served OpenAPI 3.x contract ───────────────────────
//
// The muster integration (docs/muster-scope.md → docs/reference/openapi.md)
// consumes the daemon's OWN OpenAPI document, which makes that document a
// served contract like any other: it gets the same two-direction guard the
// endpoint table above gets.
//
//   - every path in the spec must be a route the daemon actually serves,
//     probed against the real *http.ServeMux the Handler() returns; and a path
//     that lives inside a trailing-slash subtree registration must additionally
//     name itself in the handler sources, so an invented nested path cannot
//     hide behind a subtree pattern that would match anything;
//   - every /api/v1 route documented in docs/reference/endpoints.md — which
//     TestAgentsEndpointTableParity already pins to the route registrations in
//     BOTH directions — must be described by the spec.
//
// That second leg is the drift this guard was written for: /api/v1/cadence,
// /api/v1/peers/events and /api/v1/peers/{id}/events were served and documented
// while the OpenAPI document did not describe them, so a muster consumer
// silently lost three capabilities (and the spec's own /api/v1/openapi.json was
// undescribed too). The api package's TestAPI_OpenAPI_Success could not catch
// it: it compares the spec against a hand-maintained path list, which is exactly
// the artifact that drifts.
//
// It also pins what a consumer's LOADER depends on, because each of these
// fails the whole document rather than one operation:
//
//   - an operationId on every operation (muster derives both its CLI verb names
//     and its MCP tool names from it, so an id-less operation is named by
//     convention instead of by contract);
//   - an absolute servers[] URL (a relative one leaves a standalone consumer
//     with no host to call);
//   - no top-level key outside the OpenAPI vocabulary and the x- extension
//     space — the SPEC-ROB-001 defect class, where a top-level "security-note"
//     sibling made kin-openapi's doc.Validate reject the entire document
//     ("extra sibling fields: [security-note]") and muster generate ZERO tools.
//     The prose lives in "x-security-note" now.

const (
	// Re-anchor floors for the served spec. Same purpose as the route floors
	// above: a broken fetch or a decode that silently yields an empty document
	// must fail loudly instead of comparing empty against empty.
	minSpecPaths      = 40
	minSpecOperations = 45
)

// openapiTopLevelKeys is the OpenAPI 3.0 vocabulary, plus the vendor-extension
// space. Anything else at the document root is the SPEC-ROB-001 defect.
var openapiTopLevelKeys = map[string]bool{
	"openapi": true, "info": true, "jsonSchemaDialect": true, "servers": true,
	"paths": true, "webhooks": true, "components": true, "security": true,
	"tags": true, "externalDocs": true,
}

// openapiGatedReads is the allowlist of NON-mutating operations that carry the
// operator-credential marker. The mutation is the danger, not the observation
// (internal/api/auth.go ROUTE SPLIT) — but a peer-registry read is
// anti-enumeration gated (SCHED-GAP-1602), so these four are deliberate. The
// list is pinned in both directions: a read that grows a marker fails here, and
// a mutating operation that loses one fails below.
var openapiGatedReads = map[string]bool{
	"get /api/v1/peers":                true,
	"get /api/v1/peers/events":         true,
	"get /api/v1/peers/{id}/events":    true,
	"get /api/v1/federation/catalogue": true,
}

// openapiOperation is one operation reduced to the properties the guard checks.
type openapiOperation struct {
	ID          string
	HasSecurity bool
}

// openapiSpec is the parsed view of the served document.
type openapiSpec struct {
	TopLevelKeys []string
	Servers      []string
	// Paths maps a path template to its operations by lower-case method.
	Paths map[string]map[string]openapiOperation
}

// openapiOperationRef is one (path, method, operation) triple.
type openapiOperationRef struct {
	Path   string
	Method string
	Op     openapiOperation
}

// operations returns every operation in the document in a stable order.
func (s openapiSpec) operations() []openapiOperationRef {
	var refs []openapiOperationRef
	for path, methods := range s.Paths {
		for method, op := range methods {
			refs = append(refs, openapiOperationRef{Path: path, Method: method, Op: op})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Path != refs[j].Path {
			return refs[i].Path < refs[j].Path
		}
		return refs[i].Method < refs[j].Method
	})
	return refs
}

// parseOpenAPISpec decodes the served document into the guard's view. The
// top-level keys are decoded separately from the typed fields so an unknown
// sibling field is SEEN rather than silently dropped by the decoder.
func parseOpenAPISpec(t *testing.T, body []byte) openapiSpec {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("served OpenAPI document is not a JSON object: %v", err)
	}
	var typed struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]struct {
			OperationID string          `json:"operationId"`
			Security    json.RawMessage `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("served OpenAPI document does not decode: %v", err)
	}
	spec := openapiSpec{
		Paths: map[string]map[string]openapiOperation{},
	}
	for k := range root {
		spec.TopLevelKeys = append(spec.TopLevelKeys, k)
	}
	sort.Strings(spec.TopLevelKeys)
	for _, s := range typed.Servers {
		spec.Servers = append(spec.Servers, s.URL)
	}
	for path, methods := range typed.Paths {
		ops := map[string]openapiOperation{}
		for method, raw := range methods {
			ops[strings.ToLower(method)] = openapiOperation{
				ID:          raw.OperationID,
				HasSecurity: len(raw.Security) > 0 && string(raw.Security) != "null",
			}
		}
		spec.Paths[path] = ops
	}
	return spec
}

// deriveOperationID is the document's operationId rule, restated here so the
// guard fails when an id stops being derivable from its route: operationId =
// lower-camel(method + request path), where every path segment — brace-stripped
// for parameters, split on '-', '_' and '.' — contributes a capitalized part.
// ("get", "/api/v1/projects/{name}/pause") → "getApiV1ProjectsNamePause".
func deriveOperationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		seg = strings.Trim(seg, "{}")
		for _, part := range strings.FieldsFunc(seg, func(r rune) bool {
			return r == '-' || r == '_' || r == '.'
		}) {
			b.WriteString(strings.ToUpper(part[:1]))
			b.WriteString(part[1:])
		}
	}
	return b.String()
}

// concreteProbePath turns a spec path into a requestable one by giving every
// {param} a value — the mux matches on shape, not on the value.
func concreteProbePath(specPath string) string {
	segs := strings.Split(specPath, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			seg = "probe"
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

// muxServes reports whether the mux has a route pattern for the request path.
// net/http documents that Handler returns an empty pattern when nothing
// matches, which is exactly the question the spec must answer "yes" to.
func muxServes(mux *http.ServeMux, path string) bool {
	req, err := http.NewRequest(http.MethodGet, "http://spec-parity.invalid"+path, nil)
	if err != nil {
		return false
	}
	_, pattern := mux.Handler(req)
	return pattern != ""
}

// openapiPathParamSegments returns the literal (non-parameter) sub-resource
// segments of a spec path that sits inside a trailing-slash subtree
// registration — the segments a handler must name to serve it. A path that is
// its own registration (or is only parameters below the subtree, e.g.
// /api/v1/projects/{name}) has none.
func openapiPathParamSegments(subtree, specPath string) []string {
	rest := strings.TrimPrefix(specPath, subtree)
	var literals []string
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || (strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")) {
			continue
		}
		literals = append(literals, seg)
	}
	return literals
}

// apiSourceLiterals collects every double-quoted string literal in the API
// sources, so a sub-resource segment is only accepted when the handler really
// mentions it.
func apiSourceLiterals(t *testing.T, paths []string) []string {
	t.Helper()
	literalRe := regexp.MustCompile(`"([^"\n]*)"`)
	var literals []string
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range literalRe.FindAllStringSubmatch(string(src), -1) {
			literals = append(literals, m[1])
		}
	}
	return literals
}

// openapiSpecFindings is the pure checker the live test and the self-check
// both drive: empty findings = the served document describes the served
// surface, and refuses to be loaded by a strict OpenAPI validator in no way.
func openapiSpecFindings(
	spec openapiSpec,
	live routeSets,
	apiLiterals []string,
	documented []string,
	served func(path string) bool,
) []string {
	var findings []string

	// ── direction 0: the document must be loadable at all ───────────────
	for _, key := range spec.TopLevelKeys {
		if openapiTopLevelKeys[key] || strings.HasPrefix(key, "x-") {
			continue
		}
		findings = append(findings, fmt.Sprintf("the served OpenAPI document carries top-level key %q, which is not OpenAPI vocabulary and not an x- extension — a strict validator (kin-openapi doc.Validate, used by muster) rejects the WHOLE document with \"extra sibling fields: [%s]\" and generates zero tools", key, key))
	}
	if len(spec.Servers) == 0 {
		findings = append(findings, "the served OpenAPI document declares no servers[] — a standalone consumer (muster --spec <url>) has no host to call for any operation")
	}
	for _, srv := range spec.Servers {
		if !strings.HasPrefix(srv, "http://") && !strings.HasPrefix(srv, "https://") {
			findings = append(findings, fmt.Sprintf("servers[] url %q is not absolute — muster cannot dereference a relative server URL", srv))
		}
	}

	// ── direction 1: every spec path must be a route the daemon serves ──
	seenIDs := map[string]string{}
	for _, ref := range spec.operations() {
		opID := ref.Op.ID
		if opID == "" {
			findings = append(findings, fmt.Sprintf("spec %s %s has no operationId — muster names the CLI verb and the MCP tool from it, so the name would be a consumer convention instead of contract", ref.Method, ref.Path))
		} else {
			if want := deriveOperationID(ref.Method, ref.Path); opID != want {
				findings = append(findings, fmt.Sprintf("spec %s %s operationId %q is not the derived name %q (rule: lower-camel(method + path))", ref.Method, ref.Path, opID, want))
			}
			if prev, dup := seenIDs[opID]; dup {
				findings = append(findings, fmt.Sprintf("operationId %q is used twice (%s and %s %s) — tool names must be unique per consumer", opID, prev, ref.Method, ref.Path))
			} else {
				seenIDs[opID] = ref.Method + " " + ref.Path
			}
		}
	}
	for path := range spec.Paths {
		if !served(concreteProbePath(path)) {
			findings = append(findings, fmt.Sprintf("the served OpenAPI document describes path %q but the daemon's route table does not serve it — the document has drifted off the real surface", path))
			continue
		}
		if live.exact[path] {
			continue
		}
		// Inside a subtree registration the pattern matches anything, so the
		// sub-resource must name itself in the handler sources.
		for subtree := range live.subtree {
			if !strings.HasPrefix(path, subtree+"/") {
				continue
			}
			for _, seg := range openapiPathParamSegments(subtree, path) {
				found := false
				for _, lit := range apiLiterals {
					if strings.Contains(lit, seg) {
						found = true
						break
					}
				}
				if !found {
					findings = append(findings, fmt.Sprintf("the served OpenAPI document describes %q under the %q subtree registration, but no API source names the %q sub-resource — that path looks served while the handler 404s it", path, subtree, seg))
				}
			}
		}
	}

	// ── direction 2: every documented route must be described ───────────
	described := map[string]bool{}
	for path := range spec.Paths {
		described[path] = true
	}
	for _, d := range documented {
		if !strings.HasPrefix(d, "/api/v1/") || described[d] {
			continue
		}
		findings = append(findings, fmt.Sprintf("docs/reference/endpoints.md documents %q but the served OpenAPI document does not describe it — an OpenAPI consumer cannot see a capability the daemon serves", d))
	}

	// ── direction 3: the credential markers are the served classification ─
	for _, ref := range spec.operations() {
		mutating := ref.Method == "post" || ref.Method == "put" || ref.Method == "delete" || ref.Method == "patch"
		switch {
		case mutating && !ref.Op.HasSecurity:
			findings = append(findings, fmt.Sprintf("spec %s %s is a MUTATING operation with no security marker — the document advertises it as callable while the daemon refuses it", ref.Method, ref.Path))
		case !mutating && ref.Op.HasSecurity && !openapiGatedReads[ref.Method+" "+ref.Path]:
			findings = append(findings, fmt.Sprintf("spec %s %s is a gated READ with no entry in openapiGatedReads — either the marker is wrong or the allowlist must name it (a read gate is anti-enumeration, not observation doctrine)", ref.Method, ref.Path))
		}
	}

	sort.Strings(findings)
	return findings
}

// servedOpenAPISpec boots the real api.Server and returns its served OpenAPI
// document plus the *http.ServeMux the same Handler() serves, so the parity
// comparison is against the document a consumer actually fetches over the route
// (not a copy of the constant that could drift from it).
func servedOpenAPISpec(t *testing.T) (openapiSpec, *http.ServeMux) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	t.Cleanup(loop.Stop)

	handler := api.NewServer(db, loop).Handler()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("api.Server.Handler() returned %T, want *http.ServeMux — the route probe cannot inspect it", handler)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET /api/v1/openapi.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return parseOpenAPISpec(t, body), mux
}

// TestServedOpenAPISpecParity is the SCHED-GAP-1580 guard: the OpenAPI 3.x
// document the daemon SERVES must describe the surface the daemon serves, in
// both directions, and must stay loadable by a strict validator.
func TestServedOpenAPISpecParity(t *testing.T) {
	root := agentsRepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "docs", "reference", "endpoints.md"))
	if err != nil {
		t.Fatalf("read docs/reference/endpoints.md: %v", err)
	}
	section, ok := agentsEndpointSection(string(raw))
	if !ok {
		t.Fatalf(`docs/reference/endpoints.md has no "## Endpoints" section — the route table is missing`)
	}
	documented := parseAgentsEndpointRoutes(section)
	if len(documented) < minDocumentedRows {
		t.Fatalf("parsed only %d route rows from docs/reference/endpoints.md (want >= %d)", len(documented), minDocumentedRows)
	}

	spec, mux := servedOpenAPISpec(t)
	if len(spec.Paths) < minSpecPaths {
		t.Fatalf("the served OpenAPI document parsed to only %d paths (want >= %d) — the document or the decode is broken", len(spec.Paths), minSpecPaths)
	}
	if ops := len(spec.operations()); ops < minSpecOperations {
		t.Fatalf("the served OpenAPI document parsed to only %d operations (want >= %d) — the document or the decode is broken", ops, minSpecOperations)
	}

	live := routeSetsFromSources(t, sourcePathsForRoutes(t, root))
	if len(live.exact) < minLiveExact {
		t.Fatalf("source walk found only %d exact route registrations (want >= %d)", len(live.exact), minLiveExact)
	}
	literals := apiSourceLiterals(t, sourcePathsForRoutes(t, root))

	findings := openapiSpecFindings(spec, live, literals, documented,
		func(path string) bool { return muxServes(mux, path) })
	if len(findings) > 0 {
		t.Errorf("SCHED-GAP-1580 served-OpenAPI/route parity: %d divergences across %d paths / %d operations:",
			len(findings), len(spec.Paths), len(spec.operations()))
		for _, f := range findings {
			t.Errorf("  DRIFT %s", f)
		}
	}
}

// cloneOpenAPISpec deep-copies the parsed document so a self-check case can
// mutate one field without leaking that mutation into the next case (the
// Paths maps are references).
func cloneOpenAPISpec(s openapiSpec) openapiSpec {
	out := openapiSpec{
		TopLevelKeys: append([]string(nil), s.TopLevelKeys...),
		Servers:      append([]string(nil), s.Servers...),
		Paths:        make(map[string]map[string]openapiOperation, len(s.Paths)),
	}
	for path, methods := range s.Paths {
		ops := make(map[string]openapiOperation, len(methods))
		for method, op := range methods {
			ops[method] = op
		}
		out.Paths[path] = ops
	}
	return out
}

// TestServedOpenAPISpecParity_SelfCheck proves the guard bites in every
// direction and is not vacuous. All inputs are synthetic — nothing on disk is
// touched — so this runs as an ordinary unit test.
func TestServedOpenAPISpecParity_SelfCheck(t *testing.T) {
	live := routeSets{
		exact: map[string]bool{
			"/api/v1/health": true,
			"/api/v1/pause":  true,
		},
		subtree: map[string]bool{"/api/v1/projects": true},
	}
	literals := []string{"GET only", "POST only", "/api/v1/projects/", "pause", "resume"}

	spec := openapiSpec{
		TopLevelKeys: []string{"components", "info", "openapi", "paths", "servers", "x-security-note"},
		Servers:      []string{"http://127.0.0.1:9090"},
		Paths: map[string]map[string]openapiOperation{
			"/api/v1/health":             {"get": {ID: "getApiV1Health"}},
			"/api/v1/pause":              {"post": {ID: "postApiV1Pause", HasSecurity: true}},
			"/api/v1/projects/{name}":    {"get": {ID: "getApiV1ProjectsName"}},
			"/api/v1/projects/{n}/pause": {"post": {ID: "postApiV1ProjectsNPause", HasSecurity: true}},
		},
	}
	servedReal := func(path string) bool {
		if _, ok := live.exact[path]; ok {
			return true
		}
		for sub := range live.subtree {
			if strings.HasPrefix(path, sub+"/") {
				return true
			}
		}
		return false
	}
	real := []string{"/api/v1/health", "/api/v1/pause", "/api/v1/projects/{name}"}

	if findings := openapiSpecFindings(cloneOpenAPISpec(spec), live, literals, real, servedReal); len(findings) != 0 {
		t.Fatalf("self-check: real shape reported findings %v — checker is over-firing", findings)
	}

	cases := []struct {
		name    string
		mutate  func(openapiSpec) openapiSpec
		live    routeSets
		docs    []string
		served  func(string) bool
		wantSub string
	}{{
		name:    "illegal top-level sibling fails",
		mutate:  func(s openapiSpec) openapiSpec { s.TopLevelKeys = append(s.TopLevelKeys, "security-note"); return s },
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "extra sibling fields",
	}, {
		name:    "relative servers url fails",
		mutate:  func(s openapiSpec) openapiSpec { s.Servers = []string{"/api/v1"}; return s },
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "is not absolute",
	}, {
		name: "missing operationId fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/health"] = map[string]openapiOperation{"get": {}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "has no operationId",
	}, {
		name: "non-derived operationId fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/health"] = map[string]openapiOperation{"get": {ID: "checkHealth"}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "is not the derived name",
	}, {
		name: "duplicate operationId fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/pause"] = map[string]openapiOperation{"post": {ID: "getApiV1Health"}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "is used twice",
	}, {
		name: "spec path the daemon does not serve fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/retired"] = map[string]openapiOperation{"get": {ID: "getApiV1Retired"}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "does not serve it",
	}, {
		name: "invented sub-resource under a subtree fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/projects/{name}/frobnicate"] = map[string]openapiOperation{"post": {ID: "postApiV1ProjectsNameFrobnicate", HasSecurity: true}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: `no API source names the "frobnicate" sub-resource`,
	}, {
		name:    "documented route missing from the spec fails",
		mutate:  func(s openapiSpec) openapiSpec { return s },
		live:    live,
		docs:    append(append([]string(nil), real...), "/api/v1/cadence"),
		served:  servedReal,
		wantSub: "endpoints.md documents",
	}, {
		name: "mutating operation without a security marker fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/pause"] = map[string]openapiOperation{"post": {ID: "postApiV1Pause"}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "no security marker",
	}, {
		name: "unclassified gated read fails",
		mutate: func(s openapiSpec) openapiSpec {
			s.Paths["/api/v1/health"] = map[string]openapiOperation{"get": {ID: "getApiV1Health", HasSecurity: true}}
			return s
		},
		live:    live,
		docs:    real,
		served:  servedReal,
		wantSub: "no entry in openapiGatedReads",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := openapiSpecFindings(tc.mutate(cloneOpenAPISpec(spec)), tc.live, literals, tc.docs, tc.served)
			if len(findings) == 0 {
				t.Fatalf("self-check: expected a finding containing %q, got none — the guard is blind here", tc.wantSub)
			}
			found := false
			for _, f := range findings {
				if strings.Contains(f, tc.wantSub) {
					found = true
				}
			}
			if !found {
				t.Fatalf("self-check: findings %v do not mention %q", findings, tc.wantSub)
			}
		})
	}

	// The derivation rule itself is pinned, so a "fix" that changes the rule
	// has to change this table too.
	for _, tc := range []struct{ method, path, want string }{
		{"get", "/api/v1/health", "getApiV1Health"},
		{"get", "/api/v1/live", "getApiV1Live"},
		{"get", "/api/v1/features/prune-candidates", "getApiV1FeaturesPruneCandidates"},
		{"get", "/api/v1/openapi.json", "getApiV1OpenapiJson"},
		{"post", "/api/v1/projects/{name}/pause", "postApiV1ProjectsNamePause"},
		{"delete", "/api/v1/projects/{name}", "deleteApiV1ProjectsName"},
		{"post", "/api/v1/namespaces/{id}/move", "postApiV1NamespacesIdMove"},
		{"get", "/api/v1/peers/{id}/events", "getApiV1PeersIdEvents"},
	} {
		if got := deriveOperationID(tc.method, tc.path); got != tc.want {
			t.Errorf("deriveOperationID(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}

	// The probe helper must turn a template into a requestable path.
	if got := concreteProbePath("/api/v1/projects/{name}/pause"); got != "/api/v1/projects/probe/pause" {
		t.Errorf("concreteProbePath = %q, want /api/v1/projects/probe/pause", got)
	}

	// And the "is it really served" probe must agree with a real mux: a
	// registered path matches, an unregistered sibling does not.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", func(http.ResponseWriter, *http.Request) {})
	if !muxServes(mux, "/api/v1/projects/probe/pause") {
		t.Error("muxServes: a path under a subtree registration must read as served")
	}
	if muxServes(mux, "/api/v1/cadence") {
		t.Error("muxServes: an unregistered path must not read as served")
	}
}
