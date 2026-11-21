package scheduler

// SCHED-GAP-16936 piece 1 — THE PRE-DISPATCH WRITE-SCOPE GATE.
//
// THE PROBLEM. A wave composes several worker units at once. Each unit is given
// a declared write scope (the paths/globs its brief may touch) and possibly a
// depends_on edge saying "this unit's work must land before that one's". When
// two units declare OVERLAPPING scopes and nothing orders them, both workers
// write the same files concurrently and the last writer silently destroys the
// first one's work — a lost update the fleet has no way to attribute after the
// fact, because the collision happened inside two worker sessions.
//
// THE RULE. Before anything is spawned, check every pair of units in the wave:
// two units whose declared scopes overlap MUST have a depends_on edge between
// them (directly, or transitively through the chain). If they do not, the
// dispatch is REFUSED before the spawn, and the refusal names BOTH lanes and the
// two overlapping paths. Overlapping scopes WITH an edge are legal and pass.
//
// WHERE THE DECLARATION COMES FROM. The lane's dispatch-target row
// (~/.hermes/coding-hermes/dispatch-targets.jsonl) grew two OPTIONAL keys:
//
//	{"lane": "foo", "agent": "...", "scope": ["internal/foo/", "cmd/fooctl/*.go"],
//	 "depends_on": ["foo-impl"]}
//
// The additive default is what keeps this gate safe to land: a lane that
// declares no scope has nothing to overlap with, so a fleet that has not
// declared scopes behaves EXACTLY as before (no refusal, no extra work beyond
// one cached file read per pass). A lane that declares a scope opts into the
// safety.
//
// WHY A LANE'S SCOPE IS NOT DERIVED FROM ITS WORKDIR. Several lanes legitimately
// share one checkout (the -qa / -sync / -pm satellites all point at their
// project's repo), so inferring "scope = workdir" would refuse the fleet's own
// normal shape. The scope is a DECLARATION, never an inference.
//
// THE COMPARISON IS CONSERVATIVE ON GLOBS. Two literal paths overlap when one is
// a directory prefix of the other. A glob is compared by its LITERAL ANCHOR
// directory (everything before the first metacharacter), so `internal/foo/*.go`
// anchors at `internal/foo` and overlaps a literal `internal/foo/bar.go`. That
// over-reports marginally (two disjoint globs in one directory can be reported
// as overlapping) and it is the right direction: a false refusal costs one wave
// pass, a false pass costs a worker's whole diff.

import (
	"fmt"
	"path"
	"strings"
)

// WaveScopeUnit is one unit of a wave: an id, the lane it runs on, the paths it
// declares it will write, and the units it depends on.
type WaveScopeUnit struct {
	// ID is the unit's identity inside the wave (the lane name for a lane
	// unit; a worker row id for a composed worker unit). depends_on edges
	// reference IDs.
	ID string
	// Lane is the lane name reported in a refusal. Empty means "same as ID".
	Lane string
	// Paths is the declared write scope: relative paths or globs.
	Paths []string
	// DependsOn names the unit IDs that must land before this one.
	DependsOn []string
}

// laneName is the display name for a refusal (both lanes are always named).
func (u WaveScopeUnit) laneName() string {
	if strings.TrimSpace(u.Lane) != "" {
		return u.Lane
	}
	return u.ID
}

// WaveScopeConflict is one refused pair: two units, the two overlapping paths,
// and no depends_on edge between them.
type WaveScopeConflict struct {
	A, B         WaveScopeUnit
	APath, BPath string
}

// Error names BOTH lanes and both paths — the brief's refusal requirement
// (a refusal that names one side leaves the operator guessing which wave member
// to re-scope).
func (c WaveScopeConflict) Error() string {
	return fmt.Sprintf("lane %q (scope %q) and lane %q (scope %q) overlap with no depends_on edge between them",
		c.A.laneName(), c.APath, c.B.laneName(), c.BPath)
}

// normalizeScopePath canonicalizes one declared scope entry. A declared scope is
// relative to the lane's checkout; the comparison never needs an absolute path.
func normalizeScopePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	// `internal/foo//bar` and `internal/foo/./bar` are the same file.
	if p != "" {
		if cleaned := path.Clean(p); cleaned != "." {
			p = cleaned
		}
	}
	return p
}

// normalizeScopePaths normalizes and de-duplicates a declared scope, dropping
// empty entries (an empty entry would otherwise overlap everything).
func normalizeScopePaths(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		n := normalizeScopePath(p)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// hasGlobMeta reports whether a scope entry is a pattern rather than a path.
func hasGlobMeta(p string) bool { return strings.ContainsAny(p, "*?[") }

// scopeAnchor is the literal directory a scope entry pins down: the entry
// itself for a literal path, or the directory before the first metacharacter
// for a glob (`internal/foo/*.go` → `internal/foo`, `*.go` → "").
func scopeAnchor(p string) string {
	idx := strings.IndexAny(p, "*?[")
	if idx < 0 {
		return p
	}
	anchor := p[:idx]
	if slash := strings.LastIndex(anchor, "/"); slash >= 0 {
		return strings.TrimSuffix(anchor[:slash], "/")
	}
	return ""
}

// pathContains reports whether parent addresses child or one of its ancestors.
// The empty parent is the checkout root and contains everything.
func pathContains(parent, child string) bool {
	if parent == "" {
		return true
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}

// scopePathsOverlap is the pairwise overlap rule documented in the file comment.
func scopePathsOverlap(a, b string) bool {
	a, b = normalizeScopePath(a), normalizeScopePath(b)
	if a == "" || b == "" {
		return false
	}
	if !hasGlobMeta(a) && !hasGlobMeta(b) {
		if a == b {
			return true
		}
		return pathContains(a, b) || pathContains(b, a)
	}
	aa, ba := scopeAnchor(a), scopeAnchor(b)
	if aa == ba {
		return true
	}
	return pathContains(aa, ba) || pathContains(ba, aa)
}

// waveScopeIndex maps unit IDs to their depends_on lists for reachability.
func waveScopeIndex(units []WaveScopeUnit) map[string][]string {
	index := make(map[string][]string, len(units))
	for _, u := range units {
		index[u.ID] = append(index[u.ID], u.DependsOn...)
	}
	return index
}

// orderedBefore reports whether an ordering edge exists between two units, in
// either direction: `from` reachable from `to` or the reverse, following
// depends_on transitively. A direct edge is the common case; transitivity is
// what makes a three-unit chain (a → b → c, a and c overlapping) legal, which is
// the honest reading of "there is an edge between them".
//
// The walk is bounded by the unit set, so a malformed cyclic depends_on cannot
// spin.
func orderedBefore(from, to string, index map[string][]string) bool {
	if from == to {
		return true
	}
	seen := make(map[string]bool, len(index))
	stack := []string{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, next := range index[cur] {
			if next == to {
				return true
			}
			if !seen[next] {
				stack = append(stack, next)
			}
		}
	}
	return false
}

// CheckWaveScopes returns every refused pair in the wave. An empty result means
// the wave is safe to dispatch.
func CheckWaveScopes(units []WaveScopeUnit) []WaveScopeConflict {
	normalized := make([]WaveScopeUnit, 0, len(units))
	for _, u := range units {
		u.Paths = normalizeScopePaths(u.Paths)
		normalized = append(normalized, u)
	}
	index := waveScopeIndex(normalized)
	var conflicts []WaveScopeConflict
	for i := 0; i < len(normalized); i++ {
		for j := i + 1; j < len(normalized); j++ {
			a, b := normalized[i], normalized[j]
			if a.ID == b.ID {
				continue
			}
			if orderedBefore(a.ID, b.ID, index) || orderedBefore(b.ID, a.ID, index) {
				// An ordering edge exists: the overlap is legal.
				continue
			}
			reported := false
			for _, pa := range a.Paths {
				for _, pb := range b.Paths {
					if !scopePathsOverlap(pa, pb) {
						continue
					}
					conflicts = append(conflicts, WaveScopeConflict{A: a, B: b, APath: pa, BPath: pb})
					reported = true
					break // one conflict per pair is enough to refuse it
				}
				if reported {
					break
				}
			}
		}
	}
	return conflicts
}

// EnsureWaveScopesDisjoint is CheckWaveScopes as a refusal: nil when the wave is
// safe, an error naming EVERY refused pair when it is not.
func EnsureWaveScopesDisjoint(units []WaveScopeUnit) error {
	conflicts := CheckWaveScopes(units)
	if len(conflicts) == 0 {
		return nil
	}
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		parts = append(parts, c.Error())
	}
	return fmt.Errorf("wave dispatch refused: %s", strings.Join(parts, "; "))
}

// waveScopeGate admits lanes one at a time across a single evaluation pass,
// carrying the units already admitted so each new candidate is checked against
// the wave the pass is building.
//
// A lane that declares no scope is admitted without any comparison (the
// additive default — see the file comment) and is not added to the wave.
type waveScopeGate struct {
	// targetsPath is resolved once per pass (an env override is process-wide;
	// resolving per lane would re-read the same value N times).
	targetsPath string
	units       []WaveScopeUnit
	// disabled short-circuits the gate for a test or an explicit opt-out.
	disabled bool
}

// newWaveScopeGate builds the per-pass gate over the default declaration file.
func newWaveScopeGate() *waveScopeGate {
	return &waveScopeGate{targetsPath: DefaultDispatchTargetsPath()}
}

// admit checks one lane against the wave admitted so far. It returns nil when
// the lane declares no scope (nothing to overlap) or when its scope is legal
// next to the already-admitted units, and a refusal error naming BOTH lanes
// otherwise. An admitted lane's unit joins the wave.
func (g *waveScopeGate) admit(lane string) error {
	if g == nil || g.disabled {
		return nil
	}
	unit, ok := resolveWaveScopeDeclaration(g.targetsPath, lane)
	if !ok {
		return nil
	}
	if err := EnsureWaveScopesDisjoint(append(append([]WaveScopeUnit{}, g.units...), unit)); err != nil {
		return err
	}
	g.units = append(g.units, unit)
	return nil
}

// admitted returns the units admitted so far (copy).
func (g *waveScopeGate) admitted() []WaveScopeUnit {
	if g == nil {
		return nil
	}
	return append([]WaveScopeUnit{}, g.units...)
}

// resolveWaveScopeDeclaration reads one lane's declared scope + depends_on from
// the dispatch-target file. A lane with no row, an unreadable file, or a row
// with no scope declares nothing (ok=false) — the fail-open default that keeps
// the gate inert for lanes that never opted in.
//
// The row's enabled flag is deliberately NOT consulted: a PARKED (disabled)
// target row still carries the lane's declaration, and the pass still needs it
// when that lane runs locally.
func resolveWaveScopeDeclaration(targetsPath, lane string) (WaveScopeUnit, bool) {
	lane = strings.TrimSpace(lane)
	if targetsPath == "" || lane == "" {
		return WaveScopeUnit{}, false
	}
	targets, err := cachedDispatchTargets(targetsPath)
	if err != nil || len(targets) == 0 {
		return WaveScopeUnit{}, false
	}
	t, ok := targets[lane]
	if !ok {
		return WaveScopeUnit{}, false
	}
	paths := normalizeScopePaths(t.Scope)
	if len(paths) == 0 {
		return WaveScopeUnit{}, false
	}
	deps := make([]string, 0, len(t.DependsOn))
	for _, d := range t.DependsOn {
		if d = strings.TrimSpace(d); d != "" {
			deps = append(deps, d)
		}
	}
	return WaveScopeUnit{ID: lane, Lane: lane, Paths: paths, DependsOn: deps}, true
}
