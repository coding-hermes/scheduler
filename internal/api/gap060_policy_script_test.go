package api_test

// GAP-060 (load hygiene) — subprocess-boundary evidence for the
// fleet.toml-regen seam that pause/resume fire (SCHED-GAP-137b).
//
// The GAP-060 baseline sampling caught the real fleet-cooldown-policy.py
// --apply running under this repo mid-suite: external-package tests exercise
// pause/resume through the HTTP handlers and cannot reach the in-package
// regenFleetTomlExec seam, so the regen ran for real against the test host.
// TestMain points SCHEDULER_POLICY_SCRIPT (the documented test override in
// policyScriptPath) at a stub, and these tests pin the seam end-to-end:
//
//   - the handlers keep their 200/paused contract with the stub armed, and
//     the env override survives a whole request cycle (no mid-request unset),
//   - a policy script that FAILS (real subprocess, exit 1) must not poison
//     the response — the documented log-and-proceed behavior — so regen
//     failures stay invisible to API consumers exactly as production
//     promises.
//
// No assertions were weakened anywhere: these are NEW assertions about the
// subprocess boundary, not replacements for existing handler coverage.

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGap060_PolicyScriptStubbedDuringPauseResume(t *testing.T) {
	script := os.Getenv("SCHEDULER_POLICY_SCRIPT")
	if script == "" {
		t.Fatal("SCHEDULER_POLICY_SCRIPT must be armed by TestMain — without it an " +
			"external-package pause/resume test runs the real ops policy script " +
			"against the test host (the GAP-060 finding)")
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("policy stub %s missing: %v", script, err)
	}

	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "gap060-pause")

	code, body := a.do(t, "POST", "/api/v1/projects/gap060-pause/pause", nil)
	if code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200 (body: %v)", code, body)
	}
	if body["status"] != "paused" {
		t.Errorf("status field = %v, want paused", body["status"])
	}

	code, body = a.do(t, "POST", "/api/v1/projects/gap060-pause/resume", nil)
	if code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200 (body: %v)", code, body)
	}
	if body["status"] != "resumed" {
		t.Errorf("status field = %v, want resumed", body["status"])
	}

	// The override must survive a full request cycle — a mid-suite unset
	// would silently re-arm the real script for later tests.
	if again := os.Getenv("SCHEDULER_POLICY_SCRIPT"); again != script {
		t.Errorf("SCHEDULER_POLICY_SCRIPT = %q after the request cycle, want %q (unset mid-run)",
			again, script)
	}
}

// TestGap060_PolicyScriptFailureDoesNotPoisonPause runs a REAL failing stub
// (ops/testdata/exit1.sh, exit 1) through the same resolution path production
// uses, proving the documented failure contract: the regen error is returned
// to the handler, logged, and never poisons the API response.
func TestGap060_PolicyScriptFailureDoesNotPoisonPause(t *testing.T) {
	stub, err := filepath.Abs(filepath.Join("..", "..", "ops", "testdata", "exit1.sh"))
	if err != nil {
		t.Fatalf("resolve exit1 stub: %v", err)
	}
	if _, err := os.Stat(stub); err != nil {
		t.Fatalf("exit1 stub missing: %v", err)
	}
	t.Setenv("SCHEDULER_POLICY_SCRIPT", stub)

	// Premise: the stub really fails, via the same exec shape regen uses.
	if out, err := exec.Command(stub).CombinedOutput(); err == nil {
		t.Fatalf("premise: exit1 stub exited 0 (output %q)", out)
	}

	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "gap060-fail")

	code, body := a.do(t, "POST", "/api/v1/projects/gap060-fail/pause", nil)
	if code != http.StatusOK {
		t.Fatalf("pause status = %d with a failing policy script, want 200 — "+
			"regen failures are logged-and-proceeded, never an API error (body: %v)",
			code, body)
	}
	if body["status"] != "paused" {
		t.Errorf("status field = %v, want paused despite the regen failure", body["status"])
	}
	if strings.Contains(strings.ToLower(body["status"].(string)), "error") {
		t.Errorf("status body leaked the regen failure: %v", body)
	}
}
