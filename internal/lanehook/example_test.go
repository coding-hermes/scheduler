package lanehook

import (
	"path/filepath"
	"testing"
)

// The shipped example file must load and resolve exactly as documented: PRE
// runs lease -> fetch -> worktree-ensure, POST runs quiesce -> merge -> prune.
func TestExampleFileResolves(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "hooks", "hooks.example.toml")
	f, err := LoadFile(path)
	if err != nil {
		t.Fatalf("example hooks.toml does not load: %v", err)
	}
	if len(f.Hooks) == 0 {
		t.Fatal("example declares no hooks")
	}
	plan, err := Resolve(f.Hooks, "code-factory-qa", "code-factory", "/home/kara/proj-wt-code-factory-qa",
		"/home/kara/code-factory", f.Order(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// lease-acquire was removed from the example: it named
	// ~/.hermes/hooks/lease-acquire.sh, a script install-hooks.sh does not
	// ship, so a verbatim copy of the example would have vetoed EVERY tick
	// (on_failure=closed) on a host that installed exactly what the repo
	// provides. The example now only declares hooks whose commands exist here.
	eq(t, ids(plan.Pre), []string{"repo-fetch", "worktree-ensure"})
	eq(t, ids(plan.Post), []string{"quiesce-check", "lane-merge", "worktree-prune"})

	if spec, ok := f.Lanes["code-factory-qa"]; !ok || spec.Repo == "" || spec.Base != "main" {
		t.Fatalf("lane spec missing or incomplete: %+v (ok=%v)", spec, ok)
	}
	// A lane with no LANE-level declarations still inherits the GLOBAL list
	// (that is the point of a global hook) but must pick up no lane steps.
	other, err := Resolve(f.Hooks, "crier-qa", "coding-hermes", "/home/kara/crier", "/home/kara/crier", f.Order(), nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(other.Pre), []string{"repo-fetch"})
	if len(other.Post) != 0 {
		t.Fatalf("undeclared lane must have no POST steps, got %v", ids(other.Post))
	}
}
