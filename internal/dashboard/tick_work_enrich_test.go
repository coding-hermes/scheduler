package dashboard

import (
	"context"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// The call-site-level RED proof for SCHED-GAP-1623: the fleet enrichment
// (enrichProjects, the fleet render's per-lane pass) must classify its tick
// samples through the memo, so ONE enrichment of many completed samples pays
// ONE git exec per unique window — and a SECOND enrichment of the same
// samples pays ZERO. With the fix reverted (tickSamplesFromCompleted calling
// raw tickWork directly, the pre-fix wiring), the second enrichment re-execs
// every sample and this test fails.

// TestEnrichProjectsSecondPassZeroExec drives the real enrichProjects over a
// set of completed samples and counts git execs across two passes.
func TestEnrichProjectsSecondPassZeroExec(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	wd := initGitRepo(t)
	calls, restore := countingGitRunner(t)
	defer restore()

	realRunner := tickWorkRunner
	tickWorkRunner = func(cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		if realRunner != nil {
			return realRunner(cmd)
		}
		return cmd.Output()
	}
	defer func() { tickWorkRunner = realRunner }()

	// Four completed samples: three fixed windows (the memoizable class),
	// one running-tick window (empty completed_at — never memoized).
	samples := []completedSample{
		{spawnedAt: pastRFC3339(48), completedAt: pastRFC3339(40)},
		{spawnedAt: pastRFC3339(36), completedAt: pastRFC3339(30)},
		{spawnedAt: pastRFC3339(24), completedAt: pastRFC3339(20)},
		{spawnedAt: pastRFC3339(12), completedAt: ""},
	}
	projects := []FleetRow{{Name: "p1", Workdir: wd}}
	samplesByProject := map[string][]completedSample{"p1": samples}

	pass := func() (first, second int) {
		g := NewGenerator(nil, nil)
		g.enrichProjects(projects, samplesByProject, map[string]tickHealth{}, nil)
		first = int(calls.Load())
		g.enrichProjects(projects, samplesByProject, map[string]tickHealth{}, nil)
		second = int(calls.Load())
		return first, second
	}
	first, second := pass()
	if first == 0 {
		t.Fatal("first enrichment ran zero git execs — samples never reached tickWork")
	}
	if second != first {
		t.Fatalf("second enrichment cost %d more git execs (first pass %d) — memo bypassed; the per-render subprocess storm is back", second-first, first)
	}
	if second-first > len(samples) {
		t.Fatalf("per-render exec count unbounded: first %d, second %d", first, second)
	}
}

// TestEnrichProjectsFirstPassBoundedExec: the FIRST pass must already be
// bounded — at most one exec per sample plus at most one per running-tick
// re-derivation (moving windows never memoize). Guards the memo against
// accidentally re-executing fixed windows within one pass.
func TestEnrichProjectsFirstPassBoundedExec(t *testing.T) {
	resetTickWorkCache()
	defer resetTickWorkCache()
	wd := initGitRepo(t)

	realRunner := tickWorkRunner
	var calls atomic.Int64
	tickWorkRunner = func(cmd *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		if realRunner != nil {
			return realRunner(cmd)
		}
		return cmd.Output()
	}
	defer func() { tickWorkRunner = realRunner }()

	// The same fixed window appears in the sample list three times
	// (e.g. three lanes sharing a workdir's history in one pass via
	// duplicated samples) — deduped by the memo to one exec.
	samples := []completedSample{
		{spawnedAt: pastRFC3339(48), completedAt: pastRFC3339(40)},
		{spawnedAt: pastRFC3339(48), completedAt: pastRFC3339(40)},
		{spawnedAt: pastRFC3339(48), completedAt: pastRFC3339(40)},
	}
	projects := []FleetRow{{Name: "p1", Workdir: wd}}
	g := NewGenerator(nil, nil)
	g.enrichProjects(projects, map[string][]completedSample{"p1": samples}, map[string]tickHealth{}, nil)
	if got := calls.Load(); got > 1 {
		t.Fatalf("identical fixed windows cost %d execs in one pass, want 1 (memo dedupe)", got)
	}
	_ = context.Background
}

// pastRFC3339 returns RFC3339 for (now - h hours). Shared by the enrichment
// tests; h may be fractional.
func pastRFC3339(h float64) string {
	return clock.Real().Now().Add(-time.Duration(h * float64(3600*1e9))).Format(time.RFC3339)
}
