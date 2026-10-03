package scheduler

// SCHED-GAP-1710 — the lane→target file's semantics.
//
// The file is the ONLY place a lane becomes remote, so its failure modes matter
// as much as its success path: a missing file must mean "no remote lanes"
// (the additive default), an explicit false must park a target, and an
// unroutable or ambiguous row must be REFUSED loudly rather than quietly
// turning a lane back into a local one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTargetsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write targets: %v", err)
	}
	return path
}

func TestLoadDispatchTargets_MissingFileMeansNoRemoteLanes(t *testing.T) {
	resetDispatchTargetsCache()
	targets, err := LoadDispatchTargets(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("missing file = %v, want no error (the additive default)", err)
	}
	if len(targets) != 0 {
		t.Fatalf("missing file produced %d targets, want 0", len(targets))
	}
}

func TestLoadDispatchTargets_Semantics(t *testing.T) {
	resetDispatchTargetsCache()
	path := writeTargetsFile(t, strings.Join([]string{
		`# a comment is not a target`,
		``,
		`{"lane":"helix","agent":"helix","workdir":"/agent/helix"}`,
		`{"lane":"asce","agent":"asce","enabled":false}`,
	}, "\n")+"\n")

	targets, err := LoadDispatchTargets(path)
	if err != nil {
		t.Fatalf("LoadDispatchTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want 2 rows", targets)
	}
	if got := targets["helix"]; got.Agent != "helix" || got.Workdir != "/agent/helix" || !got.IsEnabled() {
		t.Errorf("helix target = %+v", got)
	}
	if got := targets["asce"]; got.IsEnabled() {
		t.Errorf("asce target = %+v, want an explicit park (enabled=false)", got)
	}

	// A parked target is NOT remote.
	if _, ok, _ := LookupDispatchTarget(path, "asce"); ok {
		t.Error("a parked (enabled=false) lane resolved as remote")
	}
	if tgt, ok, _ := LookupDispatchTarget(path, "helix"); !ok || tgt.Agent != "helix" {
		t.Errorf("helix lookup = %+v ok=%v, want the declared target", tgt, ok)
	}
	if _, ok, _ := LookupDispatchTarget(path, "unknown-lane"); ok {
		t.Error("an undeclared lane resolved as remote — the additive default is local")
	}
}

func TestLoadDispatchTargets_RefusesUnroutableAndAmbiguousRows(t *testing.T) {
	resetDispatchTargetsCache()
	path := writeTargetsFile(t, strings.Join([]string{
		`{"lane":"helix","agent":"helix"}`,
		`{"lane":"empty-agent"}`,
		`{"agent":"no-lane"}`,
		`{"lane":"helix","agent":"someone-else"}`,
	}, "\n")+"\n")

	targets, err := LoadDispatchTargets(path)
	if err == nil {
		t.Fatal("a file with unroutable/ambiguous rows parsed with no reported problem")
	}
	if len(targets) != 1 || targets["helix"].Agent != "helix" {
		t.Fatalf("targets = %v, want only the first, valid helix row", targets)
	}
	for _, want := range []string{"line 2", "line 3", "line 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestResolveDispatchReferences — a dispatched unit always carries a workdir,
// and a board reference inside it when the target names none: an agent handed
// work it cannot locate is not a hand-out.
func TestResolveDispatchReferences(t *testing.T) {
	board, workdir := resolveDispatchReferences(DispatchTarget{Agent: "a"}, PackedProject{Name: "helix", Workdir: "/lane/helix"})
	if workdir != "/lane/helix" || board != filepath.Join("/lane/helix", ".coding-hermes", "board", "tasks.jsonl") {
		t.Errorf("derived refs = (%q, %q)", board, workdir)
	}

	board, workdir = resolveDispatchReferences(
		DispatchTarget{Agent: "a", Board: "/agent/board.jsonl", Workdir: "/agent/helix"},
		PackedProject{Name: "helix", Workdir: "/control/helix"})
	if workdir != "/agent/helix" || board != "/agent/board.jsonl" {
		t.Errorf("explicit refs must win, got (%q, %q)", board, workdir)
	}
}
