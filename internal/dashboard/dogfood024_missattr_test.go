package dashboard

import (
	"context"
	"os/exec"
	"sync/atomic"
	"testing"
)

// TestDogfood024_MissAttribution pins the DOGFOOD-024 tickWork memo contract:
// after the TTL was raised 60s → 24h, a second (warm) fleet render must pay
// ZERO git execs — the memo serves every fixed (immutable-history) window.
// Skips without DOGFOOD_DB, so CI (-short) stays hermetic; the fleet DB copy
// provides the ~4.8k-window render surface this contract needs to bite.
func TestDogfood024_MissAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("dogfood measurement (needs DOGFOOD_DB fleet copy)")
	}
	gen := newDogfoodGenT(t)
	ctx := context.Background()

	var misses atomic.Int64
	orig := tickWorkRunner
	tickWorkRunner = func(cmd *exec.Cmd) ([]byte, error) {
		misses.Add(1)
		return cmd.Output()
	}
	defer func() { tickWorkRunner = orig }()

	gen.collect(ctx) // render 1 (cold)
	first := misses.Load()
	misses.Store(0)
	gen.collect(ctx) // render 2 (warm)
	second := misses.Load()
	t.Logf("render1 execs: %d, render2 execs: %d", first, second)
	if second > 0 {
		t.Errorf("warm render still paid %d git execs (want 0)", second)
	}
}
