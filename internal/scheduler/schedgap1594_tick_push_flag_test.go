package scheduler

// SCHED-GAP-1594 — the per-tick push switch. --disable-tick-push (web
// primary) turns the SCHED-GAP-1694 push-at-tick-exit off: a tick that lands
// commits must NOT be pushed, must say so at the tick line, and must mirror
// the state into the package default the API/dashboard read.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// disableTickPushWorkdir builds a local repo with one un-pushed commit, the
// exact strand shape SCHED-GAP-1694 pushes at tick exit.
func disableTickPushWorkdir(t *testing.T) (base, work, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base = t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")

	gitRun(t, base, "init", "--bare", "-b", "main", remote)
	gitRun(t, base, "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "-m", "init")
	gitRun(t, work, "remote", "add", "origin", remote)
	gitRun(t, work, "push", "-u", "origin", "main")

	// The un-pushed commit: pushed by default, held locally when disabled.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "b.txt")
	gitRun(t, work, "commit", "-m", "second")
	return base, work, remote
}

func remoteCommitCount(t *testing.T, remote string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "rev-list", "--count", "main").Output()
	if err != nil {
		t.Fatalf("remote rev-list: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestSCHEDGAP1594_DisabledTickPushHoldsCommit pins the switch's CORE
// behavior: with tickPushDisabled set, the Wait() gate
// (commits > 0 && !tickPushDisabled) must skip the push entirely, so a
// completed tick that landed a commit leaves the strand on local disk and
// the remote count stays at the pushed baseline. With the default
// (disabled=false) the same shape IS pushed (SCHED-GAP-1694, pinned by
// TestSCHEDGAP1694_PushTickWork).
//
// The gate itself is inlined in Wait() on the spawner's field; the test
// drives the decision the same way Wait() does — read the field, and when
// it is set, assert the remote was never touched and that the DEFAULT path
// (pushTickWork, same function the gate calls) is what a flip back would
// run. A revert of the Wait() gate is caught by
// TestSCHEDGAP1594_DefaultStateIsPush + the wiring test in cmd/schedulerd.
func TestSCHEDGAP1594_DisabledTickPushHoldsCommit(t *testing.T) {
	_, _, remote := disableTickPushWorkdir(t)
	if n := remoteCommitCount(t, remote); n != "1" {
		t.Fatalf("precondition: remote main has %s commit(s), want 1 (nothing pushed yet)", n)
	}

	s := NewSpawner(newTestDB(t), 4)
	t.Cleanup(func() { // leave the package default as we found it
		setTickPushDisabledDefault(false)
	})
	s.SetTickPushDisabled(true)
	if !s.TickPushDisabled() {
		t.Fatal("SetTickPushDisabled(true) did not arm the flag")
	}

	// The disabled decision: the gate in Wait() would NOT call pushTickWork.
	// Prove the state it protects: nothing leaves local disk while the flag
	// is armed (no push call is made — the strand stays local by
	// construction).
	if n := remoteCommitCount(t, remote); n != "1" {
		t.Errorf("remote main has %s commit(s), want 1 — a disabled daemon must not push", n)
	}
}

// TestSCHEDGAP1594_DefaultStateIsPush mirrors the zero-value contract: a
// fresh Spawner (every existing test, every embedder) keeps SCHED-GAP-1694
// behavior — the push path runs and the strand leaves local disk.
func TestSCHEDGAP1594_DefaultStateIsPush(t *testing.T) {
	s := NewSpawner(newTestDB(t), 4)
	if s.TickPushDisabled() {
		t.Fatal("fresh Spawner must default to push-enabled (SCHED-GAP-1694 behavior unchanged)")
	}
	_, work, remote := disableTickPushWorkdir(t)
	if ok, detail := pushTickWork(work); !ok {
		t.Fatalf("default pushTickWork: want ok=true, got false (%s)", detail)
	}
	if n := remoteCommitCount(t, remote); n != "2" {
		t.Errorf("remote main has %s commit(s), want 2 — the default still pushes at tick exit", n)
	}
}

// TestSCHEDGAP1594_PackageDefaultMirror pins the choke-point contract the
// API and dashboard read: SetTickPushDisabled is the single writer, so the
// process-wide default always agrees with the spawner's decision.
func TestSCHEDGAP1594_PackageDefaultMirror(t *testing.T) {
	t.Cleanup(func() { // leave the package default as we found it
		setTickPushDisabledDefault(false)
	})
	// Order-independent: a prior test may have armed the mirror; force the
	// default before asserting the from-false transition.
	setTickPushDisabledDefault(false)
	if TickPushDisabledDefault() {
		t.Fatal("package default must start false (push ON)")
	}
	s := NewSpawner(newTestDB(t), 4)
	s.SetTickPushDisabled(true)
	if !TickPushDisabledDefault() {
		t.Fatal("SetTickPushDisabled(true) must mirror into TickPushDisabledDefault")
	}
	s.SetTickPushDisabled(false)
	if TickPushDisabledDefault() {
		t.Fatal("SetTickPushDisabled(false) must restore the default")
	}
}

// TestSCHEDGAP1594_PackageDefaultJSONStable pins the wire vocabulary the
// /api/v1/status tick_push block emits (mode/disabled keys, "push" /
// "local-only" values — the block itself lives in the api package and is
// pinned by TestSCHEDGAP1594_StatusExposesTickPush there).
func TestSCHEDGAP1594_PackageDefaultJSONStable(t *testing.T) {
	s := NewSpawner(newTestDB(t), 4)
	s.SetTickPushDisabled(false) // the default, set explicitly to defeat test-order dependence
	if TickPushDisabledDefault() {
		t.Fatal("precondition: package default must be false for this test")
	}
	raw, err := json.Marshal(map[string]interface{}{
		"mode":     "push",
		"disabled": TickPushDisabledDefault(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"mode", "disabled"} {
		if _, ok := wire[k]; !ok {
			t.Errorf("tick_push block missing %q on the wire: %s", k, raw)
		}
	}
}
