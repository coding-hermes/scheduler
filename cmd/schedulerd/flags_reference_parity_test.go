package main

// DOC-4 — docs/reference/flags.md flag parity guard.
//
// The 2026-09-27 drift this guard exists for: cmd/schedulerd/main.go
// registered 47 flags while docs/reference/flags.md — the agent-facing copy
// of the flag table, distinct from README.md's operator-facing copy —
// documented 36. Eleven operator-facing knobs had no row (the SCHED-GAP-117
// gateway response deadline, the SCHED-GAP-125 load gate, the ADV-R08 slot
// patience, the ADV-R11 spawn memory cap, tasks pacing, the deploy
// groups/templates files, model rates, public-url, --sim-idle and
// --verify-board), so an agent reading the reference table hit exactly those
// behaviours with no discoverable knob. readme_flag_parity_test.go already
// pinned the README copy (SCHED-GAP-194); this file pins the reference copy
// to the same source so the two tables can no longer drift independently.
//
// Shape mirrors readme_flag_parity_test.go: a pure checker that both the live
// test and a self-test drive, so a regex regression cannot turn the guard
// into a permanently-green no-op. The registered side is the SAME parser —
// flagRegistrationRe + sourceFlagNames — so there is exactly one
// registration-shape regex to re-anchor when main.go's declaration style
// changes.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// flagsRefHeaderRe matches the flags.md table header row.
var flagsRefHeaderRe = regexp.MustCompile(`^\|[[:space:]]*Flag[[:space:]]*\|[[:space:]]*Default[[:space:]]*\|[[:space:]]*Description[[:space:]]*\|$`)

// flagsRefTable returns the row lines of the flag table in flags.md: the
// lines between the `| Flag | Default | Description |` header and the first
// line that is not a table row, so a backticked identifier in later prose or
// code blocks cannot satisfy the parity check.
func flagsRefTable(doc string) ([]string, bool) {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if !flagsRefHeaderRe.MatchString(strings.TrimSpace(line)) {
			continue
		}
		var rows []string
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(strings.TrimSpace(l), "|") {
				return rows, true
			}
			rows = append(rows, l)
		}
		return rows, true
	}
	return nil, false
}

// flagsRefParityFindings is the pure checker both the live test and the
// self-test drive. It compares the flag-name set declared in main.go against
// the docs/reference/flags.md table, returning one human-readable finding per
// divergence (empty = parity). Set equality is checked in BOTH directions: a
// registered flag with no documented row AND a documented row with no
// registration are both drift. Both `--name` and `-name` spellings are
// accepted in the flag column (this file and README.md spell differently).
func flagsRefParityFindings(registered map[string]bool, doc string) []string {
	var findings []string

	rows, ok := flagsRefTable(doc)
	if !ok {
		findings = append(findings, `docs/reference/flags.md has no "| Flag | Default | Description |" table — the flag table is missing`)
	} else {
		documented := make(map[string]bool)
		seen := make(map[string]int)
		for _, line := range rows {
			if strings.HasPrefix(strings.TrimSpace(line), "|-") {
				continue // the |---|---| separator row
			}
			m := readmeFlagRowRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			documented[name] = true
			seen[name]++
			if seen[name] == 2 {
				findings = append(findings, fmt.Sprintf("docs/reference/flags.md lists flag %q more than once — duplicate row", name))
			}
			if !registered[name] {
				findings = append(findings, fmt.Sprintf("docs/reference/flags.md documents flag %q which cmd/schedulerd/main.go does not register — remove it or fix the name", name))
			}
		}
		if len(documented) == 0 {
			findings = append(findings, `docs/reference/flags.md flag table has no "| `+"`-flag`"+` | ..." rows — the table is missing or malformed`)
		}
		var missing []string
		for name := range registered {
			if !documented[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			findings = append(findings, fmt.Sprintf("flag %q is registered by cmd/schedulerd/main.go but has no row in docs/reference/flags.md — every flag -h prints must be documented", name))
		}
	}

	sort.Strings(findings)
	return findings
}

// TestFlagsReferenceParity is the DOC-4 guard: the docs/reference/flags.md
// table must name exactly the flags cmd/schedulerd/main.go registers — the
// same set `schedulerd -h` prints.
func TestFlagsReferenceParity(t *testing.T) {
	registered := sourceFlagNames(t, "main.go")

	path := filepath.Join("..", "..", "docs", "reference", "flags.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	findings := flagsRefParityFindings(registered, string(raw))
	if len(findings) == 0 {
		return
	}
	t.Errorf("DOC-4 flags.md/flag parity: %d divergences between docs/reference/flags.md and the %d flags declared in main.go:", len(findings), len(registered))
	for _, f := range findings {
		t.Errorf("  DRIFT %s", f)
	}
}

// TestFlagsReferenceParity_SelfCheck proves the checker is not vacuous: on
// synthetic clean content it must stay silent; on synthetic drift (one
// undocumented registered flag, one rogue documented flag, a missing table, a
// header with no rows) it must fire. Without this a regex regression could
// leave the guard permanently green.
func TestFlagsReferenceParity_SelfCheck(t *testing.T) {
	registered := map[string]bool{"db": true, "budget": true, "slot-patience": true}

	const header = "| Flag | Default | Description |\n|------|---------|-------------|\n"
	clean := "title\n\n" +
		header +
		"| `--db` | `~/scheduler.db` | SQLite database path |\n" +
		"| `-budget` | `100` | Weight budget |\n" +
		"| `--slot-patience` | `5m0s` | Slot wait |\n" +
		"\n## Later section\n\nProse mentioning `--budget` outside the table.\n"
	if findings := flagsRefParityFindings(registered, clean); len(findings) != 0 {
		t.Fatalf("self-check: clean content produced findings %v — checker is over-firing", findings)
	}

	// Drift: the slot-patience row is replaced by a phantom one — a
	// registered-but-undocumented flag AND a documented-but-unregistered
	// flag in a single blob, so one run exercises BOTH directions of the
	// set equality.
	drift := strings.ReplaceAll(clean, "| `--slot-patience` | `5m0s` | Slot wait |\n", "| `-phantom_flag` | `0` | not registered anywhere |\n")
	findings := flagsRefParityFindings(registered, drift)
	want := map[string]bool{
		`docs/reference/flags.md documents flag "phantom_flag" which cmd/schedulerd/main.go does not register — remove it or fix the name`:                 true,
		`flag "slot-patience" is registered by cmd/schedulerd/main.go but has no row in docs/reference/flags.md — every flag -h prints must be documented`: true,
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

	// A flags.md whose table vanished (renamed header, moved table) must
	// fail as a missing table, not pass silently.
	if findings := flagsRefParityFindings(registered, "title\n\nNo table here.\n"); len(findings) != 1 {
		t.Fatalf("self-check: missing-table content produced %v, want exactly one missing-table finding", findings)
	}

	// A header with zero rows must fail as malformed, not pass silently —
	// the no-rows finding must be present (the missing-flag findings it
	// accompanies are correct too).
	empty := flagsRefParityFindings(registered, "title\n\n"+header+"## next\n")
	const wantNoRows = `docs/reference/flags.md flag table has no "| ` + "`-flag`" + ` | ..." rows — the table is missing or malformed`
	hasNoRows := false
	for _, f := range empty {
		if f == wantNoRows {
			hasNoRows = true
		}
	}
	if !hasNoRows {
		t.Fatalf("self-check: empty-table content produced %v, want it to include the no-rows finding", empty)
	}
}
