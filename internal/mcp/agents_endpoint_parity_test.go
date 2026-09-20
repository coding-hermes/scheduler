package mcp_test

// SCHED-GAP-166 — AGENTS.md endpoint-table parity guard.
//
// The 2026-09-18 drift this guard exists for: AGENTS.md's Endpoints table
// listed 15 rows while the daemon served a larger route set. Four live
// routes were missing entirely — /api/v1/groups, /api/v1/templates,
// /api/v1/metrics and /api/v1/events/stream — and the table carried no MCP
// surface statement at all, so an agent building a capability or security
// inventory from AGENTS.md understated the daemon's write surface.
//
// Both sides of the comparison are IN-REPO, never a live daemon process:
//
//   - the AGENTS.md side is parsed out of the repository's AGENTS.md, located
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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// agentsRouteRowRe matches one row of the AGENTS.md Endpoints table and
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
// routes of their own and therefore never get an AGENTS.md row: "/api/"
// mounts the API handler that serves the documented /api/v1/* paths, and
// "/mcp/" is the trailing-slash alias of the documented "/mcp".
var nonRouteRegistrations = map[string]bool{"/api/": true, "/mcp/": true}

// Re-anchor floors. A regex that stops matching AGENTS.md's row shape, or a
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
// flags, clock and tier tables elsewhere in AGENTS.md cannot satisfy — or
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
		findings = append(findings, fmt.Sprintf("AGENTS.md documents route %q which has no live registration — remove the row or fix the path", d))
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
		findings = append(findings, fmt.Sprintf("live route %q is missing from the AGENTS.md Endpoints table — every registered route must have a row", l))
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

// TestAgentsEndpointTableParity is the SCHED-GAP-166 guard: the AGENTS.md
// Endpoints table must name every route registered in the daemon's source
// (set equality, both directions) and the stated MCP tool count must equal
// the live registry size.
func TestAgentsEndpointTableParity(t *testing.T) {
	root := agentsRepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	agents := string(raw)

	section, ok := agentsEndpointSection(agents)
	if !ok {
		t.Fatalf(`AGENTS.md has no "## Endpoints" section — the route table is missing`)
	}
	documented := parseAgentsEndpointRoutes(section)
	if len(documented) < minDocumentedRows {
		t.Fatalf("parsed only %d route rows from the AGENTS.md Endpoints section (want >= %d) — the table or the row regex is broken", len(documented), minDocumentedRows)
	}

	live := routeSetsFromSources(t, sourcePathsForRoutes(t, root))
	if len(live.exact) < minLiveExact {
		t.Fatalf("source walk found only %d exact route registrations (want >= %d) — the registration regex no longer matches internal/api/*.go and cmd/schedulerd/main.go", len(live.exact), minLiveExact)
	}
	if len(live.subtree) < minLiveSubtree {
		t.Fatalf("source walk found only %d subtree (trailing-slash) registrations (want >= %d) — the registration regex is stale", len(live.subtree), minLiveSubtree)
	}

	if findings := agentsEndpointParityFindings(documented, live); len(findings) > 0 {
		t.Errorf("SCHED-GAP-166 AGENTS.md/route-registry parity: %d divergences between AGENTS.md and the %d live route registrations:", len(findings), len(live.liveNames()))
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
		t.Fatalf("expected exactly 1 MCP tool-count statement in the AGENTS.md Endpoints section, found %d — keep one canonical count per surface so this guard can check it", len(matches))
	}
	got, err := strconv.Atoi(matches[0][1])
	if err != nil {
		t.Fatalf("stated MCP tool count is not a number: %q", matches[0][1])
	}
	if got != len(registry) {
		t.Errorf("AGENTS.md states %d MCP tools but the registry serves %d — update AGENTS.md or investigate the registry change", got, len(registry))
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
		"/", "/health",
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
			"/", "/health",
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
			"/", "/health",
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
	want := `live route "/api/v1/metrics" is missing from the AGENTS.md Endpoints table — every registered route must have a row`
	if len(findings) != 1 || findings[0] != want {
		t.Fatalf("self-check: removed-row findings = %v, want exactly [%q]", findings, want)
	}
	// And the stale-row case likewise.
	stale := append(append([]string(nil), real...), "/api/v1/retired-route")
	findings = agentsEndpointParityFindings(stale, live)
	wantStale := `AGENTS.md documents route "/api/v1/retired-route" which has no live registration — remove the row or fix the path`
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
