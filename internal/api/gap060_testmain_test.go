package api

// GAP-060 (load hygiene): arm the SCHEDULER_POLICY_SCRIPT stub for every test
// in this package. The pause/resume handlers fire the fleet.toml regen
// (SCHED-GAP-137b) through the REAL fleet-cooldown-policy.py when no test
// injects the runner seam — measured live during the GAP-060 baseline
// sampling (a python3 fleet-cooldown-policy.py --apply process under the
// repo, mid-suite). Internal-package tests (schedgap137b, schedgap180) swap
// the regenFleetTomlExec var directly; external-package tests cannot reach
// that seam, so the documented SCHEDULER_POLICY_SCRIPT override (see
// policyScriptPath in server_projects.go) points the regen at a stub in
// ops/testdata for the whole package instead. The stub ships executable in
// git; the single real-subprocess test that exercises the failure path
// (TestGap060_PolicyScriptFailureDoesNotPoisonPause) points at exit1.sh.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Never clobber an explicit operator override.
	if os.Getenv("SCHEDULER_POLICY_SCRIPT") == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", "ops", "testdata", "exit0.sh"))
		if err == nil {
			if _, err := os.Stat(abs); err == nil {
				_ = os.Setenv("SCHEDULER_POLICY_SCRIPT", abs)
			}
		}
	}
	os.Exit(m.Run())
}
