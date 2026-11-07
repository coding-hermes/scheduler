package docscheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stuckMatrix is the shape of docs/stuck-matrix.json that the guard cares about.
// Fields are deliberately the routing facts only — the prose stays in the md.
type stuckMatrix struct {
	Rule    string         `json:"rule"`
	Classes []stuckClass   `json:"classes"`
	extra   map[string]any `json:"-"`
}

type stuckClass struct {
	Symptom      string   `json:"symptom"`
	Signature    string   `json:"signature"`
	Reversible   *bool    `json:"reversible"`
	Resolver     string   `json:"resolver"`
	Commands     []string `json:"commands"`
	EscalateWhen string   `json:"escalate_when"`
}

// ownerTokens are resolver words that mean "a human has to do this".
var ownerTokens = []string{"owner", "bane", "human"}

func isOwner(to string) bool {
	low := strings.ToLower(to)
	for _, tok := range ownerTokens {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}

// loadMatrix reads docs/stuck-matrix.json from the repo root, wherever the test
// binary happens to run from (go test sets the working directory to the package
// dir, so walk up until the file exists).
func loadMatrix(t *testing.T) stuckMatrix {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		path := filepath.Join(dir, "docs", "stuck-matrix.json")
		if _, err := os.Stat(path); err == nil {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var m stuckMatrix
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("%s is not valid JSON: %v", path, err)
			}
			return m
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("docs/stuck-matrix.json not found above %s", dir)
	return stuckMatrix{}
}

// TestStuckMatrixIsRoutable is the machine half of SCHED-GAP-16999: the matrix
// only earns its place if no reversible blocker can quietly be routed to a
// human and no entry can lose the command that resolves it. Without this test,
// "ask the owner" is a documentation edit away from being the default again.
func TestStuckMatrixIsRoutable(t *testing.T) {
	m := loadMatrix(t)

	if strings.TrimSpace(m.Rule) == "" {
		t.Fatal("matrix has no top-level rule — the reversible-first rule must be stated where the classes are read")
	}
	if len(m.Classes) == 0 {
		t.Fatal("matrix has no classes")
	}

	ownerOnly := 0
	reversible := 0
	for i, c := range m.Classes {
		where := fmt.Sprintf("classes[%d]", i)
		if s := strings.TrimSpace(c.Symptom); s == "" {
			t.Errorf("%s: empty symptom", where)
		} else {
			where = fmt.Sprintf("%s (%s)", where, truncate(s, 48))
		}
		if strings.TrimSpace(c.Signature) == "" {
			t.Errorf("%s: empty signature — a symptom nobody can measure is not routable", where)
		}
		if strings.TrimSpace(c.EscalateWhen) == "" {
			t.Errorf("%s: empty escalate_when — escalation must be a measured trigger", where)
		}
		if c.Reversible == nil {
			t.Errorf("%s: 'reversible' must be present and boolean", where)
			continue
		}

		switch *c.Reversible {
		case true:
			reversible++
			if strings.TrimSpace(c.Resolver) == "" {
				t.Errorf("%s: reversible class has no resolver", where)
			} else if isOwner(c.Resolver) {
				t.Errorf("%s: reversible class routed to a human (%q) — reversible blockers are resolved by the fleet", where, c.Resolver)
			}
			if len(c.Commands) == 0 {
				t.Errorf("%s: reversible class has no runnable command (prose is not a resolver)", where)
			}
			for j, cmd := range c.Commands {
				if strings.TrimSpace(cmd) == "" {
					t.Errorf("%s: commands[%d] is empty", where, j)
				}
			}
		case false:
			ownerOnly++
			if !isOwner(c.Resolver) {
				t.Errorf("%s: irreversible class resolver %q does not name the owner — irreversible actions escalate to the owner", where, c.Resolver)
			}
		}
	}

	if reversible == 0 {
		t.Error("no reversible class — the matrix must tell lanes what they can resolve themselves")
	}
	if ownerOnly == 0 {
		t.Error("no irreversible class — every matrix needs the 'do not touch this' column")
	}
}

// TestStuckMatrixCoversKnownSymptoms pins the symptom classes the fleet has
// actually been blocked on. A class silently disappearing is the same regression
// as one being re-routed to the owner: the lane hits the symptom, finds nothing,
// and escalates by default.
func TestStuckMatrixCoversKnownSymptoms(t *testing.T) {
	m := loadMatrix(t)

	// Each needle must appear (case-insensitive) in some entry's symptom or
	// signature. These are the failures that produced owner-gated rows before the
	// matrix existed.
	required := map[string]string{
		"index.lock":       "stale git lock / D-state holder",
		"diverged":         "diverged or unpushed branch",
		"judge":            "gitreins judge FAIL/INCOMPLETE",
		"merge conflict":   "conflicting merge",
		"never spawns":     "capacity/cooldown starvation",
		"refusal":          "model/provider refusal",
		"already complete": "blocked on a completed row",
		"environment down": "service/container/host down",
		"release cut":      "release cut",
		"duplicate":        "duplicate rows",
	}

	haystack := strings.Builder{}
	for _, c := range m.Classes {
		haystack.WriteString(strings.ToLower(c.Symptom + " " + c.Signature + " " + c.EscalateWhen + "\n"))
	}
	text := haystack.String()

	for needle, label := range required {
		if !strings.Contains(text, strings.ToLower(needle)) {
			t.Errorf("matrix lost the %q class (%s) — a lane hitting it now escalates by default", label, needle)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
