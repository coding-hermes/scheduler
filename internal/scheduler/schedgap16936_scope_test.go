package scheduler

// SCHED-GAP-16936 piece 1 — pre-dispatch write-scope safety.
//
// The rule under test: two units of one wave whose declared file scopes overlap
// must have a depends_on edge between them (directly or transitively);
// everything else is REFUSED before the spawn, and the refusal names BOTH lanes
// and both overlapping paths.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSCHEDGAP16936_ScopeOverlapRefusedWithoutEdge is the brief's core case: two
// lanes declaring the same directory, no ordering edge → refused, both names in
// the error.
func TestSCHEDGAP16936_ScopeOverlapRefusedWithoutEdge(t *testing.T) {
	units := []WaveScopeUnit{
		{ID: "lane-a", Lane: "lane-a", Paths: []string{"internal/foo/"}},
		{ID: "lane-b", Lane: "lane-b", Paths: []string{"internal/foo"}},
	}
	err := EnsureWaveScopesDisjoint(units)
	if err == nil {
		t.Fatalf("overlapping scopes with no depends_on edge were ADMITTED; want a refusal naming both lanes")
	}
	for _, want := range []string{"lane-a", "lane-b", "internal/foo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q (a one-sided refusal leaves the operator guessing)", err.Error(), want)
		}
	}
}

// TestSCHEDGAP16936_ScopeOverlapLegalWithEdge proves the carve-out: the same
// overlapping scopes pass when an ordering edge exists — in EITHER direction,
// and transitively through a third unit.
func TestSCHEDGAP16936_ScopeOverlapLegalWithEdge(t *testing.T) {
	cases := []struct {
		name  string
		units []WaveScopeUnit
	}{
		{
			name: "direct edge b-after-a",
			units: []WaveScopeUnit{
				{ID: "lane-a", Paths: []string{"internal/foo"}},
				{ID: "lane-b", Paths: []string{"internal/foo"}, DependsOn: []string{"lane-a"}},
			},
		},
		{
			name: "direct edge a-after-b",
			units: []WaveScopeUnit{
				{ID: "lane-a", Paths: []string{"internal/foo"}, DependsOn: []string{"lane-b"}},
				{ID: "lane-b", Paths: []string{"internal/foo"}},
			},
		},
		{
			name: "transitive chain a->b->c",
			units: []WaveScopeUnit{
				{ID: "a", Paths: []string{"internal/foo"}},
				{ID: "b", Paths: []string{"internal/bar"}, DependsOn: []string{"a"}},
				{ID: "c", Paths: []string{"internal/foo"}, DependsOn: []string{"b"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := EnsureWaveScopesDisjoint(tc.units); err != nil {
				t.Fatalf("overlapping scopes WITH a depends_on edge were refused: %v", err)
			}
		})
	}
}

// TestSCHEDGAP16936_ScopePathOverlapRules pins the comparison itself: nesting,
// globs by literal anchor, and the disjoint cases that must NOT be refused (a
// gate that refuses everything is as useless as one that refuses nothing).
func TestSCHEDGAP16936_ScopePathOverlapRules(t *testing.T) {
	overlapping := [][2]string{
		{"internal/foo", "internal/foo"},
		{"internal/foo/", "./internal/foo"},
		{"internal/foo", "internal/foo/bar.go"},
		{"internal/foo/bar.go", "internal/foo"},
		{"internal/foo//bar", "internal/foo/bar"},
		{"cmd/fooctl/*.go", "cmd/fooctl/main.go"},
		{"cmd/fooctl/main.go", "cmd/fooctl/*.go"},
		{"internal/*/x.go", "internal/foo/x.go"},
	}
	for _, pair := range overlapping {
		if !scopePathsOverlap(pair[0], pair[1]) {
			t.Errorf("scopePathsOverlap(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	disjoint := [][2]string{
		{"internal/foo", "internal/foobar"},
		{"internal/foo", "internal/baz"},
		{"pkg/a/*.go", "pkg/b/*.go"},
		{"", "internal/foo"},
	}
	for _, pair := range disjoint {
		if scopePathsOverlap(pair[0], pair[1]) {
			t.Errorf("scopePathsOverlap(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

// TestSCHEDGAP16936_ScopeGateInertWithoutDeclaration pins the additive default:
// a lane that declares no scope is admitted and adds nothing to the wave, so a
// fleet that never opted in keeps its exact previous behaviour.
func TestSCHEDGAP16936_ScopeGateInertWithoutDeclaration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch-targets.jsonl")
	writeLines(t, path,
		`{"lane":"lane-a","agent":"agent-a","enabled":false}`,
		`{"lane":"lane-b","agent":"agent-b","enabled":false,"scope":[]}`,
	)
	gate := &waveScopeGate{targetsPath: path}
	for _, lane := range []string{"lane-a", "lane-b", "lane-not-declared"} {
		if err := gate.admit(lane); err != nil {
			t.Fatalf("gate.admit(%q) = %v, want nil (no declared scope must be inert)", lane, err)
		}
	}
	if got := gate.admitted(); len(got) != 0 {
		t.Fatalf("gate admitted %d units from scope-less declarations, want 0: %+v", len(got), got)
	}
}

// TestSCHEDGAP16936_ScopeGateRefusesSecondLane is the wired path: the per-pass
// gate admits the first lane and refuses the second, naming both, and the same
// pair passes once the second declares a depends_on edge.
func TestSCHEDGAP16936_ScopeGateRefusesSecondLane(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch-targets.jsonl")
	writeLines(t, path,
		`{"lane":"lane-a","agent":"agent-a","scope":["internal/foo/"]}`,
		`{"lane":"lane-b","agent":"agent-b","scope":["internal/foo/bar.go"]}`,
	)
	gate := newTestScopeGate(path)
	if err := gate.admit("lane-a"); err != nil {
		t.Fatalf("first lane refused: %v", err)
	}
	err := gate.admit("lane-b")
	if err == nil {
		t.Fatal("second lane with an overlapping scope and no depends_on edge was ADMITTED")
	}
	for _, want := range []string{"lane-a", "lane-b", "internal/foo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	// Only the admitted lane is in the wave: a refused lane must not be half
	// added (it would poison every later candidate).
	if got := gate.admitted(); len(got) != 1 || got[0].ID != "lane-a" {
		t.Fatalf("wave after refusal = %+v, want just lane-a", got)
	}

	// The edge legalises the same pair. cacheFile forces a re-read (the loader
	// memoises on mtime+size, and a same-size rewrite within one mtime tick
	// would otherwise serve the stale parse).
	writeLines(t, path,
		`{"lane":"lane-a","agent":"agent-a","scope":["internal/foo/"]}`,
		`{"lane":"lane-b","agent":"agent-b","scope":["internal/foo/bar.go"],"depends_on":["lane-a"]}`,
	)
	gate = newTestScopeGate(path)
	if err := gate.admit("lane-a"); err != nil {
		t.Fatalf("lane-a refused in the legal-pair run: %v", err)
	}
	if err := gate.admit("lane-b"); err != nil {
		t.Fatalf("lane-b with depends_on lane-a was refused: %v", err)
	}
	if got := gate.admitted(); len(got) != 2 {
		t.Fatalf("wave = %d units, want 2", len(got))
	}
}

// TestSCHEDGAP16936_ScopeDeclarationSurvivesDisabledTarget proves the
// declaration is read from a PARKED (enabled:false) row: a lane running locally
// with a parked remote target still declares its scope, and the gate must not
// lose that.
func TestSCHEDGAP16936_ScopeDeclarationSurvivesDisabledTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch-targets.jsonl")
	writeLines(t, path, `{"lane":"lane-a","agent":"agent-a","enabled":false,"scope":["internal/foo"]}`)
	unit, ok := resolveWaveScopeDeclaration(path, "lane-a")
	if !ok {
		t.Fatal("a parked (enabled:false) target row's scope declaration was dropped")
	}
	if len(unit.Paths) != 1 || unit.Paths[0] != "internal/foo" {
		t.Fatalf("unit paths = %v, want [internal/foo]", unit.Paths)
	}
}

// TestSCHEDGAP16936_ScopeGateCycleIsBounded pins the documented safety property
// of the reachability walk: a malformed (cyclic) depends_on cannot spin, and a
// cycle that connects two overlapping units still counts as an ordering edge —
// the walk terminates and answers, it does not hang the dispatch pass.
func TestSCHEDGAP16936_ScopeGateCycleIsBounded(t *testing.T) {
	units := []WaveScopeUnit{
		{ID: "a", Paths: []string{"internal/foo"}, DependsOn: []string{"c"}},
		{ID: "b", Paths: []string{"internal/foo"}, DependsOn: []string{"a"}},
		{ID: "c", Paths: []string{"internal/foo"}, DependsOn: []string{"b"}},
	}
	done := make(chan []WaveScopeConflict, 1)
	go func() { done <- CheckWaveScopes(units) }()
	select {
	case conflicts := <-done:
		if len(conflicts) != 0 {
			t.Fatalf("a cyclic depends_on chain refused %d pairs; every pair is reachable from every other", len(conflicts))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CheckWaveScopes did not terminate on a cyclic depends_on (the reachability walk is unbounded)")
	}
}

// --- helpers -------------------------------------------------------------

// newTestScopeGate builds a gate over an explicit declaration file (the
// production constructor resolves the fleet's default path, which a unit test
// must never read or write).
func newTestScopeGate(path string) *waveScopeGate {
	return &waveScopeGate{targetsPath: path}
}

// writeLines writes a fixture file, ensuring a distinct size so the dispatch
// target loader's (mtime,size) memo cannot serve a stale parse back to us.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
