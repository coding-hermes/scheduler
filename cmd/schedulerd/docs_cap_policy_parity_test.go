package main

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

// capPolicyGate is the invariant checker that owns the namespace cap policy in
// code. It is the single machine authority: the docs must agree with THESE
// constants, not with each other and not with whatever the live DB happens to
// hold (DOC-24 shipped when three pages stated 8/1, 16/1 and 9/12 for one
// field while the checker enforced 16 / flat-1).
const capPolicyGate = "ops/check-fleet-invariants.py"

var (
	foremanCapConstRe = regexp.MustCompile(`(?m)^FOREMAN_CAP_EXPECTED\s*=\s*(\d+)`)
	satPolicyBlockRe  = regexp.MustCompile(`(?s)SATELLITE_CAP_POLICY\s*=\s*\{(.*?)\}`)
	satPolicyEntryRe  = regexp.MustCompile(`"([a-z0-9][a-z0-9-]*)"\s*:\s*(\d+)`)

	// The two cap-table row shapes a docs page may use to state policy. Both
	// are parsed for their NUMBER, which is then compared with the gate; the
	// guard never hardcodes 16/1, so a re-leveled policy fails here instead of
	// silently drifting away from the docs again.
	capTableForemanRowRe   = regexp.MustCompile(`(?m)^\|\s*` + "`coding-hermes`" + ` \(foremen\)\s*\|\s*(\d+)\s*\|`)
	capTableSatelliteRowRe = regexp.MustCompile(`(?m)^\|\s*Each satellite namespace \(([^)]*)\)\s*\|\s*(\d+)\s*\|`)
	backtickedNameRe       = regexp.MustCompile("`([^`]+)`")
)

// retiredCapClaims are PROSE assertions of a retired cap policy. The stale
// table values are caught by the number comparison above; these catch the
// sentences that told an operator to "fix" the fleet the wrong way. Both
// phrasings shipped in DOC-24 (docs/api.md's foremen arithmetic and
// docs/design-decisions.md naming SATELLITE_CAP_POLICY as the home of 9/12,
// when that map is flat 1).
var retiredCapClaims = []string{
	"the per-family values live in `SATELLITE_CAP_POLICY`",
	"(`coding-hermes` = 8 + at most one per satellite",
}

// TestDocsCapPolicyParity fails the build when a documentation surface states a
// namespace cap value that disagrees with the invariants checker, when the
// retired cap prose returns, or when the reconciliation markers DOC-24 added
// (one authoritative page + explicit "superseded" pointers) are dropped.
//
// Scope is the NAMESPACE cap policy (foremen R3.2 = 16, satellites R3.3 =
// flat 1, both ruled 2026-10-02 in docs/scheduler-rules.md §3). The global
// --max-concurrent dial is a different knob with no DB column and is checked as
// flag↔TOML parity by the gate itself, not here.
//
// Sibling precedent for the shape (a claim-based grep gate over the doc tree):
// cmd/schedulerd/root_toml_docs_parity_test.go and
// internal/clock/stdlib_guard_test.go.
func TestDocsCapPolicyParity(t *testing.T) {
	root := repoRoot(t)
	foremanCap, policy := loadCapPolicy(t, root)
	satCap := uniformSatelliteCap(t, policy)

	wantNamespaces := make([]string, 0, len(policy))
	for ns := range policy {
		wantNamespaces = append(wantNamespaces, ns)
	}
	sort.Strings(wantNamespaces)

	scanned, rows := 0, 0
	for _, rel := range docSurfaces(t, root) {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("cannot read %s: %v", rel, err)
		}
		scanned++
		text := string(body)

		// Every stated foremen cap must equal the gate's constant.
		for _, loc := range capTableForemanRowRe.FindAllStringSubmatchIndex(text, -1) {
			got, _ := strconv.Atoi(text[loc[2]:loc[3]])
			rows++
			if got != foremanCap {
				t.Errorf("%s:%d states the foremen namespace cap as %d, but %s has "+
					"FOREMAN_CAP_EXPECTED = %d (R3.2, ruled 2026-10-02) — state the policy "+
					"the checker enforces, or fix the checker and every page together",
					rel, lineAt(text, loc[0]), got, capPolicyGate, foremanCap)
			}
		}

		// The satellite row must carry the flat policy value for exactly the
		// namespaces the gate asserts.
		for _, loc := range capTableSatelliteRowRe.FindAllStringSubmatchIndex(text, -1) {
			got, _ := strconv.Atoi(text[loc[4]:loc[5]])
			rows++
			listed := backtickedNameRe.FindAllString(text[loc[2]:loc[3]], -1)
			names := make([]string, 0, len(listed))
			for _, n := range listed {
				names = append(names, strings.Trim(n, "`"))
			}
			sort.Strings(names)
			if got != satCap {
				t.Errorf("%s:%d states the satellite namespace cap as %d, but %s's "+
					"SATELLITE_CAP_POLICY is flat %d (R3.3, ruled 2026-10-02)",
					rel, lineAt(text, loc[0]), got, capPolicyGate, satCap)
			}
			if strings.Join(names, ",") != strings.Join(wantNamespaces, ",") {
				t.Errorf("%s:%d lists satellite namespaces [%s], but %s's "+
					"SATELLITE_CAP_POLICY asserts [%s]",
					rel, lineAt(text, loc[0]), strings.Join(names, ", "),
					capPolicyGate, strings.Join(wantNamespaces, ", "))
			}
		}

		for _, claim := range retiredCapClaims {
			if idx := strings.Index(text, claim); idx >= 0 {
				t.Errorf("%s:%d re-introduces the retired cap claim %q — R3.3 "+
					"(2026-10-02) put every satellite namespace back at 1 and "+
					"SATELLITE_CAP_POLICY is flat 1; the 9/12 raise is history",
					rel, lineAt(text, idx), claim)
			}
		}
	}

	if scanned == 0 {
		t.Fatal("no documentation surface was readable — the guard checked nothing " +
			"(a parity test that scans zero files is a phantom pass)")
	}
	if rows == 0 {
		t.Fatal("no namespace cap table row was found on any documentation surface — " +
			"the number comparison checked nothing")
	}

	// The authoritative page must still state the policy numbers and name the
	// rulings that produced them.
	authoritative := readDoc(t, root, "docs/fleet-config-model.md")
	for _, want := range []string{
		"authoritative statement of the namespace cap",
		fmt.Sprintf("`max_concurrent` = **%d**", foremanCap),
		fmt.Sprintf("`max_concurrent = %d`", satCap),
		"R3.2",
		"R3.3",
	} {
		if !strings.Contains(authoritative, want) {
			t.Errorf("docs/fleet-config-model.md (the authoritative cap-policy page) no longer "+
				"carries %q — it must state the R3.2/R3.3 policy derived from %s",
				want, capPolicyGate)
		}
	}

	// The superseded entry must stay marked superseded: this is the page that
	// used to hand an operator the wrong number with no hint it was retired.
	decisions := readDoc(t, root, "docs/design-decisions.md")
	for _, want := range []string{"SCHED-GAP-215", "SUPERSEDED by R3.3"} {
		if !strings.Contains(decisions, want) {
			t.Errorf("docs/design-decisions.md no longer contains %q — the SCHED-GAP-215 "+
				"entry must stay marked SUPERSEDED by R3.3 (2026-10-02) so a reader is "+
				"never handed 9/12 as current policy", want)
		}
	}

	// And the API page must point at the authority + the check, instead of
	// standing as a second, independently-drifting source of truth.
	api := readDoc(t, root, "docs/api.md")
	for _, want := range []string{"fleet-config-model.md", capPolicyGate} {
		if !strings.Contains(api, want) {
			t.Errorf("docs/api.md no longer references %q — the cap table must point at "+
				"the authoritative policy page and at the checker that enforces it", want)
		}
	}
}

// loadCapPolicy parses the two cap-policy constants out of the invariants
// checker. A parse failure is fatal: a guard that silently compares against
// zero values would pass every doc.
func loadCapPolicy(t *testing.T, root string) (int, map[string]int) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, capPolicyGate))
	if err != nil {
		t.Fatalf("cannot read %s: %v", capPolicyGate, err)
	}
	src := string(body)

	m := foremanCapConstRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("cannot find FOREMAN_CAP_EXPECTED in %s — the docs parity guard has no "+
			"authority to compare against", capPolicyGate)
	}
	foremanCap, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("FOREMAN_CAP_EXPECTED in %s is not an integer: %v", capPolicyGate, err)
	}

	block := satPolicyBlockRe.FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("cannot find the SATELLITE_CAP_POLICY map in %s", capPolicyGate)
	}
	policy := map[string]int{}
	for _, e := range satPolicyEntryRe.FindAllStringSubmatch(block[1], -1) {
		v, err := strconv.Atoi(e[2])
		if err != nil {
			t.Fatalf("SATELLITE_CAP_POLICY entry %q in %s is not an integer: %v",
				e[1], capPolicyGate, err)
		}
		policy[e[1]] = v
	}
	if len(policy) == 0 {
		t.Fatalf("SATELLITE_CAP_POLICY in %s parsed to zero entries — refusing a phantom pass",
			capPolicyGate)
	}
	if foremanCap <= 0 {
		t.Fatalf("FOREMAN_CAP_EXPECTED in %s is %d; a non-positive cap is not a policy value",
			capPolicyGate, foremanCap)
	}
	return foremanCap, policy
}

// uniformSatelliteCap returns the single flat satellite cap the docs are
// allowed to state. R3.3 made the map uniform; if a future ruling re-levels one
// family, the docs need per-family rows and this guard has to grow with them
// rather than picking one value and blessing it.
func uniformSatelliteCap(t *testing.T, policy map[string]int) int {
	t.Helper()
	flat := -1
	names := make([]string, 0, len(policy))
	for ns := range policy {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		if flat == -1 {
			flat = policy[ns]
			continue
		}
		if policy[ns] != flat {
			t.Fatalf("SATELLITE_CAP_POLICY in %s is no longer uniform (e.g. %s=%d vs %d): "+
				"R3.3's flat-1 ruling was re-leveled, so docs must state per-family cap rows "+
				"and this parity guard must compare them per namespace",
				capPolicyGate, ns, policy[ns], flat)
		}
	}
	return flat
}

// docSurfaces lists the documentation surfaces the guard scans: README.md plus
// every markdown page under docs/ and docs/reference/.
func docSurfaces(t *testing.T, root string) []string {
	t.Helper()
	surfaces := []string{"README.md"}
	for _, dir := range []string{"docs", filepath.Join("docs", "reference")} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("cannot read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			surfaces = append(surfaces, filepath.Join(dir, e.Name()))
		}
	}
	return surfaces
}

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	return string(body)
}

// lineAt converts a byte offset into a 1-based line number for the error text.
func lineAt(text string, idx int) int {
	if idx < 0 || idx > len(text) {
		return 0
	}
	return 1 + strings.Count(text[:idx], "\n")
}
