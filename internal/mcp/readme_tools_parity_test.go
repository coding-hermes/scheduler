package mcp_test

// SCHED-GAP-188 — README/MCP-registry parity guard.
//
// The 2026-09-20 drift this guard exists for: README.md documented "14 tools"
// and listed only the 12-14 fleet_* rows while the live registry served 41
// tools (groups/templates/deploy, events_list, namespaces_*, the project
// lifecycle tools, and the config/queue/metrics/tick reads). Any capability
// or security inventory built from the docs therefore understated the
// daemon's write surface. This test fails CI the moment the README's MCP
// Tools table or its stated tool counts drift from the registry clients
// actually see.
//
// The registry side is LIVE — the same in-process JSON-RPC tools/list the
// CTL-003 parity guard enumerates (fetchMCPToolNames in
// mcp_api_parity_test.go) — so a tool added or renamed in server.go lands
// here automatically. The README side is parsed from the repository's
// README.md, located by walking up from the test's working directory so the
// test runs under `go test ./internal/mcp/` without the daemon.

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

// readmeDiagramCountRe matches the architecture diagram line, e.g.
// "│  /mcp      → MCP server (41 tools)            │".
var readmeDiagramCountRe = regexp.MustCompile(`MCP server \((\d+) tools\)`)

// readmeProseCountRe matches the MCP Server prose, e.g.
// "...via the 41 tools listed in [MCP Tools](#mcp-tools)".
var readmeProseCountRe = regexp.MustCompile(`the (\d+) tools listed in \[MCP Tools\]`)

// readmeToolRowRe matches one MCP Tools table row: | `tool_name` | description |.
var readmeToolRowRe = regexp.MustCompile("^[[:space:]]*\\|[[:space:]]*`([a-z_]+)`[[:space:]]*\\|")

// readmeRepoRoot walks up from the test's working directory until it finds
// README.md, so the guard works from any package directory inside the repo.
func readmeRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("README.md not found in any parent directory — run the test from within the repository checkout")
		}
		dir = parent
	}
}

// readmeMCPToolsSection returns the body of the "## MCP Tools" section
// (everything up to the next "## " heading) so stray backticked identifiers
// elsewhere in the README cannot satisfy the table check.
func readmeMCPToolsSection(readme string) (string, bool) {
	lines := strings.Split(readme, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## MCP Tools" {
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

// readmeMCPParityFindings is the pure checker both the live test and the
// decoy self-test drive. It compares the registry's tool-name set against
// the README MCP Tools table and the two stated counts, returning one
// human-readable finding per divergence (empty = parity).
func readmeMCPParityFindings(registry map[string]bool, readme string) []string {
	var findings []string

	// ── tool-name set equality (both directions) ────────────────────────
	section, ok := readmeMCPToolsSection(readme)
	if !ok {
		findings = append(findings, `README has no "## MCP Tools" section — the tool table is missing`)
	} else {
		documented := make(map[string]bool)
		seen := make(map[string]int)
		for _, line := range strings.Split(section, "\n") {
			m := readmeToolRowRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			documented[name] = true
			seen[name]++
			if seen[name] == 2 {
				findings = append(findings, fmt.Sprintf("README MCP Tools table lists %q more than once — duplicate row", name))
			}
			if !registry[name] {
				findings = append(findings, fmt.Sprintf("README lists tool %q which does not exist in the MCP registry — remove it or fix the name", name))
			}
		}
		if len(documented) == 0 {
			findings = append(findings, "README MCP Tools section has no \"| `tool` | ...\" table rows — the table is missing or malformed")
		}
		var missing []string
		for name := range registry {
			if !documented[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			findings = append(findings, fmt.Sprintf("registry tool %q is missing from the README MCP Tools table — every live tool must be documented", name))
		}
	}

	// ── stated counts must equal the live registry size ─────────────────
	want := len(registry)
	for _, spec := range []struct {
		label string
		re    *regexp.Regexp
	}{
		{"architecture diagram tool count", readmeDiagramCountRe},
		{"MCP Server prose tool count", readmeProseCountRe},
	} {
		matches := spec.re.FindAllStringSubmatch(readme, -1)
		if len(matches) != 1 {
			findings = append(findings, fmt.Sprintf("expected exactly 1 %s statement in README, found %d — keep one canonical count per surface so this guard can check it", spec.label, len(matches)))
			continue
		}
		got, err := strconv.Atoi(matches[0][1])
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s is not a number: %q", spec.label, matches[0][1]))
			continue
		}
		if got != want {
			findings = append(findings, fmt.Sprintf("%s states %d tools but the MCP registry serves %d — update README or investigate the registry change", spec.label, got, want))
		}
	}

	sort.Strings(findings)
	return findings
}

// TestReadmeMCPToolsParity is the SCHED-GAP-188 guard: the README MCP Tools
// table must name exactly the registry's tools (set equality, both
// directions) and the stated counts must equal the live registry size.
func TestReadmeMCPToolsParity(t *testing.T) {
	m := newMCPTestServer(t)
	registry := fetchMCPToolNames(t, m.ts.URL)
	if len(registry) == 0 {
		t.Fatalf("tools/list returned zero tools — registry enumeration is broken")
	}

	path := filepath.Join(readmeRepoRoot(t), "README.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	findings := readmeMCPParityFindings(registry, string(raw))
	if len(findings) == 0 {
		return
	}
	t.Errorf("SCHED-GAP-188 README/MCP parity: %d divergences between README.md and the live MCP registry (%d tools):", len(findings), len(registry))
	for _, f := range findings {
		t.Errorf("  DRIFT %s", f)
	}
}

// TestReadmeMCPToolsParity_SelfCheck proves the checker is not vacuous: on
// synthetic pre-drift content it must flag an undocumented registry tool, a
// rogue documented tool, and both wrong counts — and on clean content it
// must stay silent. Without this subtest a regex regression could turn the
// guard into a permanently-green no-op.
func TestReadmeMCPToolsParity_SelfCheck(t *testing.T) {
	registry := map[string]bool{"fleet_status": true, "fleet_add": true, "rogue_future_tool": true}

	clean := "intro\n" +
		"## MCP Tools\n" +
		"\n" +
		"All 3 tools served by `POST /mcp`:\n" +
		"\n" +
		"| Tool | Description |\n" +
		"|------|-------------|\n" +
		"| `fleet_status` | status |\n" +
		"| `fleet_add` | add |\n" +
		"| `rogue_future_tool` | documented |\n" +
		"\n" +
		"## MCP Server\n\n" +
		"via the 3 tools listed in [MCP Tools](#mcp-tools)\n" +
		"MCP server (3 tools)\n"
	if findings := readmeMCPParityFindings(registry, clean); len(findings) != 0 {
		t.Fatalf("self-check: clean content produced findings %v — checker is over-firing", findings)
	}

	preDrift := strings.ReplaceAll(clean, "| `rogue_future_tool` | documented |\n", "")
	preDrift = strings.ReplaceAll(preDrift, "All 3 tools", "All 2 tools")
	preDrift = strings.ReplaceAll(preDrift, "the 3 tools listed", "the 2 tools listed")
	preDrift = strings.ReplaceAll(preDrift, "MCP server (3 tools)", "MCP server (2 tools)")
	findings := readmeMCPParityFindings(registry, preDrift)
	want := map[string]bool{
		`registry tool "rogue_future_tool" is missing from the README MCP Tools table — every live tool must be documented`:               true,
		"architecture diagram tool count states 2 tools but the MCP registry serves 3 — update README or investigate the registry change": true,
		"MCP Server prose tool count states 2 tools but the MCP registry serves 3 — update README or investigate the registry change":     true,
	}
	if len(findings) != len(want) {
		t.Fatalf("self-check: want %d findings, got %d: %v", len(want), len(findings), findings)
	}
	for _, f := range findings {
		if !want[f] {
			t.Fatalf("self-check: unexpected finding %q (want set: %v)", f, want)
		}
		delete(want, f)
	}
}
