package main

// SCHED-GAP-194 — README/help flag parity guard.
//
// The 2026-09-20 drift this guard exists for: `schedulerd -h` exposed 43 flags
// while the README "### Flags" table documented 29. Fourteen operator-facing
// knobs had no row at all — the SCHED-GAP-117 gateway-turn deadline, the
// SCHED-GAP-125 load gate, the ADV-R08 slot patience, the ADV-R11 spawn memory
// cap, the deploy groups/templates files, the SCHED-GAP-089 zombie-reaper pair,
// the DuckBrain cadence, --sim-idle and --version — so an operator hitting
// exactly those behaviours (hung gateway POSTs, load-deferred spawns) had no
// discoverable knob. This is the third recurrence of the class (SCHED-GAP-002
// and SCHED-GAP-024 fixed it in August; 11+ flags landed since with no row),
// hence the table is now PINNED to the flag declarations in main.go.
//
// Shape mirrors internal/mcp/readme_tools_parity_test.go: a pure checker that
// both the live test and a decoy self-test drive, so a regex regression cannot
// turn the guard into a permanently-green no-op.
//
// The registered side is parsed from main.go's SOURCE rather than a live flag
// set: the flags are declared inside main(), which a test in this package
// cannot reach without booting the daemon. CommandLine is the set `-h` prints
// (flag.PrintDefaults) and every registration there is a
// flag.<Kind>("name", ...) literal with the name first, so the source is the
// same surface the binary exposes.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// flagRegistrationRe matches one flag declaration in main.go, e.g.
// flag.String("db", os.ExpandEnv(...), "SQLite database path") or
// flag.Int64("spawn-mem-limit-mb", 0, "Per-spawn RLIMIT_AS ...").
var flagRegistrationRe = regexp.MustCompile(`flag\.(?:String|Bool|Int|Int64|Duration|Float64|Uint|Uint64)\(\s*"([A-Za-z0-9][A-Za-z0-9_-]*)"`)

// readmeFlagRowRe matches one README "### Flags" table row, e.g.
// | `-db` | `~/.hermes/coding-hermes/scheduler.db` | SQLite database path |
// Both spellings are accepted: README.md uses -duckbrain-ns, AGENTS.md uses
// --duckbrain-ns (same idiom as checkMarkdownFlagDefault in
// show_config_test.go).
var readmeFlagRowRe = regexp.MustCompile("^[[:space:]]*\\|[[:space:]]*`--?([A-Za-z0-9][A-Za-z0-9_-]*)`[[:space:]]*\\|")

// minRegisteredFlags is a re-anchor floor: the daemon ships 43 flags, so a
// regex that stops matching main.go's declaration shape (a reformatted
// declaration, a renamed helper) must fail LOUDLY instead of silently
// comparing an empty set against an empty set and passing.
const minRegisteredFlags = 30

// sourceFlagNames returns the flag-name set declared in the given main.go
// source file — the surface `schedulerd -h` prints.
func sourceFlagNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	names := make(map[string]bool)
	for _, m := range flagRegistrationRe.FindAllStringSubmatch(string(src), -1) {
		names[m[1]] = true
	}
	if len(names) < minRegisteredFlags {
		t.Fatalf("%s: found only %d flag registrations (want >= %d) — flagRegistrationRe no longer matches this file's declaration shape; re-anchor it", path, len(names), minRegisteredFlags)
	}
	return names
}

// readmeFlagSection returns the body of the "### Flags" section (everything up
// to the next heading) so a backticked identifier elsewhere in the README
// cannot satisfy the table check.
func readmeFlagSection(readme string) (string, bool) {
	lines := strings.Split(readme, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "### Flags" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "##") {
			return strings.Join(lines[start:i], "\n"), true
		}
	}
	return strings.Join(lines[start:], "\n"), true
}

// readmeFlagParityFindings is the pure checker both the live test and the
// decoy self-test drive. It compares the flag-name set declared in main.go
// against the README "### Flags" table, returning one human-readable finding
// per divergence (empty = parity). Set equality is checked in BOTH directions:
// a registered flag with no documented row AND a documented row with no
// registration are both drift.
func readmeFlagParityFindings(registered map[string]bool, readme string) []string {
	var findings []string

	section, ok := readmeFlagSection(readme)
	if !ok {
		findings = append(findings, `README has no "### Flags" section — the flag table is missing`)
	} else {
		documented := make(map[string]bool)
		seen := make(map[string]int)
		for _, line := range strings.Split(section, "\n") {
			m := readmeFlagRowRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			documented[name] = true
			seen[name]++
			if seen[name] == 2 {
				findings = append(findings, fmt.Sprintf("README ### Flags table lists %q more than once — duplicate row", name))
			}
			if !registered[name] {
				findings = append(findings, fmt.Sprintf("README documents flag %q which cmd/schedulerd/main.go does not register — remove it or fix the name", name))
			}
		}
		if len(documented) == 0 {
			findings = append(findings, `README ### Flags section has no "| `+"`-flag`"+` | ..." table rows — the table is missing or malformed`)
		}
		var missing []string
		for name := range registered {
			if !documented[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			findings = append(findings, fmt.Sprintf("flag %q is registered by cmd/schedulerd/main.go but has no row in the README ### Flags table — every flag -h prints must be documented", name))
		}
	}

	sort.Strings(findings)
	return findings
}

// TestReadmeFlagParity is the SCHED-GAP-194 guard: the README "### Flags"
// table must name exactly the flags cmd/schedulerd/main.go registers — the
// same set `schedulerd -h` prints.
func TestReadmeFlagParity(t *testing.T) {
	registered := sourceFlagNames(t, "main.go")

	path := filepath.Join("..", "..", "README.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	findings := readmeFlagParityFindings(registered, string(raw))
	if len(findings) == 0 {
		return
	}
	t.Errorf("SCHED-GAP-194 README/flag parity: %d divergences between README.md and the %d flags declared in main.go:", len(findings), len(registered))
	for _, f := range findings {
		t.Errorf("  DRIFT %s", f)
	}
}

// TestReadmeFlagParity_SelfCheck proves the checker is not vacuous: on
// synthetic pre-drift content it must flag an undocumented registered flag, a
// rogue documented flag and the missing section — and on clean content it must
// stay silent. Without this a regex regression could leave the guard
// permanently green.
func TestReadmeFlagParity_SelfCheck(t *testing.T) {
	registered := map[string]bool{"db": true, "budget": true, "slot-patience": true, "rogue_future_flag": true}

	clean := "intro\n" +
		"### Flags\n" +
		"\n" +
		"| Flag | Default | Description |\n" +
		"|------|---------|-------------|\n" +
		"| `-db` | `~/scheduler.db` | SQLite database path |\n" +
		"| `-budget` | `100` | Weight budget |\n" +
		"| `--slot-patience` | `5m0s` | Slot wait |\n" +
		"| `-rogue_future_flag` | `0` | documented |\n" +
		"\n" +
		"Declarative fleet seeding via TOML.\n"
	if findings := readmeFlagParityFindings(registered, clean); len(findings) != 0 {
		t.Fatalf("self-check: clean content produced findings %v — checker is over-firing", findings)
	}

	// Pre-drift: the slot-patience row is replaced by a phantom one — a
	// registered-but-undocumented flag AND a documented-but-unregistered flag
	// in a single blob, so one run exercises BOTH directions of the set
	// equality.
	preDrift := strings.ReplaceAll(clean, "| `--slot-patience` | `5m0s` | Slot wait |\n", "| `-phantom_flag` | `0` | not registered anywhere |\n")
	findings := readmeFlagParityFindings(registered, preDrift)
	want := map[string]bool{
		`README documents flag "phantom_flag" which cmd/schedulerd/main.go does not register — remove it or fix the name`:                                     true,
		`flag "slot-patience" is registered by cmd/schedulerd/main.go but has no row in the README ### Flags table — every flag -h prints must be documented`: true,
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

	// A README whose Flags section vanished (renamed heading, moved table)
	// must fail as a missing section, not pass silently.
	if findings := readmeFlagParityFindings(registered, "intro\n\n### Something Else\n"); len(findings) != 1 {
		t.Fatalf("self-check: missing-section content produced %v, want exactly one missing-section finding", findings)
	}
}
