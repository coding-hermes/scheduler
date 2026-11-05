package lanehook

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func step(id string, phase Phase, level Level, target string, pri int) Step {
	return Step{ID: id, Phase: phase, Level: level, Target: target, Priority: pri,
		Command: "/bin/true", OnFailure: FailOpen}
}

func ids(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.ID
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v want %v", got, want)
		}
	}
}

// T1: PRE is global(asc) -> namespace(asc) -> lane(asc).
func TestResolvePreLevelFirst(t *testing.T) {
	set := []Step{
		step("lane-b", PhasePre, LevelLane, "cf-qa", 20),
		step("lane-a", PhasePre, LevelLane, "cf-qa", 5),
		step("ns-b", PhasePre, LevelNamespace, "cf", 20),
		step("ns-a", PhasePre, LevelNamespace, "cf", 10),
		step("global-b", PhasePre, LevelGlobal, "*", 40),
		step("global-a", PhasePre, LevelGlobal, "*", 10),
	}
	plan, err := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(plan.Pre), []string{"global-a", "global-b", "ns-a", "ns-b", "lane-a", "lane-b"})
}

// T2: POST is lane(desc) -> namespace(desc) -> global(desc).
func TestResolvePostReversed(t *testing.T) {
	set := []Step{
		step("lane-low", PhasePost, LevelLane, "cf-qa", 10),
		step("lane-high", PhasePost, LevelLane, "cf-qa", 90),
		step("ns-mid", PhasePost, LevelNamespace, "cf", 40),
		step("global-low", PhasePost, LevelGlobal, "*", 5),
	}
	plan, err := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(plan.Post), []string{"lane-high", "lane-low", "ns-mid", "global-low"})
}

// T3: flat ordering uses one priority space across levels.
func TestResolveFlat(t *testing.T) {
	set := []Step{
		step("lane-10", PhasePre, LevelLane, "cf-qa", 10),
		step("global-40", PhasePre, LevelGlobal, "*", 40),
		step("ns-20", PhasePre, LevelNamespace, "cf", 20),
	}
	plan, err := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderFlat, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(plan.Pre), []string{"lane-10", "ns-20", "global-40"})
}

// T4: duplicate (phase, level, target, id) is a load error.
func TestValidateDuplicate(t *testing.T) {
	set := []Step{
		step("dup", PhasePre, LevelLane, "cf-qa", 10),
		step("dup", PhasePre, LevelLane, "cf-qa", 20),
	}
	if err := Validate(set); err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
}

// A same id at two different levels is legal: both run.
func TestValidateSameIDDifferentLevel(t *testing.T) {
	set := []Step{
		step("fetch", PhasePre, LevelGlobal, "*", 10),
		step("fetch", PhasePre, LevelLane, "cf-qa", 10),
	}
	if err := Validate(set); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// T5: disabled steps and skip_hooks remove a step; an unknown skip id is fine.
func TestResolveSkipAndDisabled(t *testing.T) {
	no := false
	d := step("disabled", PhasePre, LevelLane, "cf-qa", 10)
	d.Enabled = &no
	set := []Step{
		d,
		step("kept", PhasePre, LevelLane, "cf-qa", 20),
		step("skipped", PhasePre, LevelLane, "cf-qa", 30),
	}
	plan, err := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, []string{"skipped", "no-such-id"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(plan.Pre), []string{"kept"})
}

// Only the matching target is included.
func TestResolveTargets(t *testing.T) {
	set := []Step{
		step("g", PhasePre, LevelGlobal, "*", 1),
		step("other-ns", PhasePre, LevelNamespace, "other", 1),
		step("other-lane", PhasePre, LevelLane, "cf-docs", 1),
		step("this-lane", PhasePre, LevelLane, "cf-qa", 1),
	}
	plan, _ := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, nil)
	eq(t, ids(plan.Pre), []string{"g", "this-lane"})
}

// T13: resolution is deterministic.
func TestResolveDeterministic(t *testing.T) {
	set := []Step{
		step("b", PhasePre, LevelLane, "cf-qa", 10),
		step("a", PhasePre, LevelLane, "cf-qa", 10),
	}
	p1, _ := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, nil)
	p2, _ := Resolve(set, "cf-qa", "cf", "/tmp/wd", "/tmp/repo", OrderLevelFirst, nil)
	eq(t, ids(p1.Pre), ids(p2.Pre))
	eq(t, ids(p1.Pre), []string{"a", "b"}) // tie broken by id
}

// Role derivation.
func TestRoleFor(t *testing.T) {
	cases := map[string][2]string{
		"code-factory":              {"code-factory", "primary"},
		"code-factory-qa":           {"code-factory", "qa"},
		"crier-readme":              {"crier", "readme"},
		"code-factory-duckbrain-qa": {"code-factory-duckbrain", "qa"},
	}
	for lane, want := range cases {
		p, r := RoleFor(lane)
		if p != want[0] || r != want[1] {
			t.Fatalf("%s: got (%s,%s) want (%s,%s)", lane, p, r, want[0], want[1])
		}
	}
}

// Placeholder expansion never quotes and passes values through verbatim.
func TestExpand(t *testing.T) {
	env := Env{TickID: "t1", Lane: "cf-qa", Role: "qa", Workdir: "/w", Repo: "/r", Phase: PhasePre}
	got := Expand([]string{"--lane", "{lane}", "--tick", "{tick}"}, env)
	if len(got) != 4 || got[1] != "cf-qa" || got[3] != "t1" {
		t.Fatalf("unexpected expansion: %v", got)
	}
}

// T11: a broken edit keeps the previous set armed.
func TestSetReloadKeepsPreviousOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.toml")
	good := `
[[hook]]
id = "a"
phase = "pre"
level = "global"
target = "*"
priority = 10
command = "/bin/true"
`
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Steps()) != 1 {
		t.Fatalf("want 1 step, got %d", len(set.Steps()))
	}
	if err := os.WriteFile(path, []byte("this is not toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := set.ReloadIfChanged()
	if err == nil || changed {
		t.Fatalf("want error and no change, got changed=%v err=%v", changed, err)
	}
	if len(set.Steps()) != 1 {
		t.Fatalf("previous set must stay armed, got %d steps", len(set.Steps()))
	}
}

// A missing file is not an error and yields an empty plan.
func TestLoadMissingFile(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if len(f.Hooks) != 0 {
		t.Fatalf("want no hooks, got %d", len(f.Hooks))
	}
	plan, err := Resolve(f.Hooks, "x", "", "", "", f.Order(), nil)
	if err != nil || !plan.Empty() {
		t.Fatalf("want empty plan, got %v err=%v", plan, err)
	}
}

// T15: a zero-step plan is free — no receipts, no events.
func TestPlanEmptyIsFree(t *testing.T) {
	var events int
	r := NewExecRunner(nil, func(ctx context.Context, sev, msg string, d map[string]any) { events++ })
	if err := r.Pre(context.Background(), Env{TickID: "t"}, Plan{}); err != nil {
		t.Fatal(err)
	}
	r.Post(context.Background(), Env{TickID: "t"}, Plan{})
	if events != 0 {
		t.Fatalf("want no events, got %d", events)
	}
}
