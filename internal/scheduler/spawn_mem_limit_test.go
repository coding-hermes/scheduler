//go:build linux

package scheduler

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ADV-R11 — per-spawn memory rlimits (GAP-048 cure), Linux tests.
//
// The one live incident in the no-host-signal class was a memory-capacity
// earlyoom kill. The cure arms an RLIMIT_AS cap on each spawned foreman
// process. These tests drive the REAL exec spawn path (a bash child, no
// hermes binary needed — same trick as the GAP-048 exec-fallback test)
// and assert the limit through /proc/<pid>/limits, the kernel's own view.

// readProcASLimit reads the "Max address space" soft limit (bytes) from
// /proc/<pid>/limits — the acceptance surface (ADV-R11 acceptance 1:
// "verifiable in /proc/<pid>/limits"). Returns 0 for "unlimited".
func readProcASLimit(t *testing.T, pid int) uint64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/limits: %v", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Max address space") {
			continue
		}
		fields := strings.Fields(line)
		// Format: "Max address space <soft> <hard> <units>" — the limit
		// NAME itself is three words, so soft is fields[3].
		if len(fields) < 5 {
			t.Fatalf("unexpected limits line format: %q", line)
		}
		if fields[3] == "unlimited" {
			return 0
		}
		var v uint64
		if _, err := fmt.Sscanf(fields[3], "%d", &v); err != nil {
			t.Fatalf("parse soft limit %q: %v", fields[3], err)
		}
		return v
	}
	t.Fatalf("no 'Max address space' line in /proc/%d/limits", pid)
	return 0
}

// TestSpawn_MemLimitAppliedToRealChild proves acceptance 1: with a limit
// armed, the spawned process carries it, read straight from the kernel
// (/proc/<pid>/limits). RED-provable: delete the applySpawnMemLimitIfArmed
// call in spawn.go and this test FAILS (the child runs unlimited).
func TestSpawn_MemLimitAppliedToRealChild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-child rlimit test — skip in short mode")
	}
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "advr11-limited")

	spawner := NewSpawner(db, 4)
	spawner.SetNoExecFallback(false) // nil gateway → exec path

	SetSpawnMemLimitMB(256)
	t.Cleanup(func() { SetSpawnMemLimitMB(0) })

	project := PackedProject{
		Name:    "advr11-limited",
		Workdir: t.TempDir(),
		// Long sleep: the child stays alive while we read its limits.
		Command: "bash -c 'sleep 5'",
	}
	tick, err := spawner.Spawn(project, "advr11-limited-2026-09-16-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer tick.Wait()

	wantBytes := uint64(256 * 1024 * 1024)
	deadline := time.Now().Add(3 * time.Second)
	var got uint64
	for time.Now().Before(deadline) {
		got = readProcASLimit(t, tick.PID)
		if got == wantBytes {
			return // cap landed, kernel-verified
		}
		// The prlimit lands microseconds after Start; retry briefly
		// (also covers procfs read racing process startup).
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d never carried the RLIMIT_AS cap; soft limit = %d bytes, want %d", tick.PID, got, wantBytes)
}

// parentASSoftLimit reads the CALLING process's own RLIMIT_AS soft limit
// in bytes via getrlimit(2) (Go stdlib, no new dependency). Returns 0 for
// RLIM_INFINITY (uncapped) — the same sentinel meaning readProcASLimit
// uses for "unlimited" in /proc/<pid>/limits, so the two surfaces compare
// directly.
func parentASSoftLimit() uint64 {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &rl); err != nil {
		// Cannot happen on Linux; if it ever did, treat the parent as
		// uncapped and let the real assertion run rather than skipping.
		return 0
	}
	// kernel RLIM_INFINITY (all bits set) — syscall.RLIM_INFINITY is an
	// untyped -1 constant that cannot compare against the uint64 field.
	const rlimInfinity = ^uint64(0)
	if rl.Cur == rlimInfinity {
		return 0
	}
	return rl.Cur
}

// TestSpawn_MemLimitOffByDefault proves the off half of acceptance 3/5:
// with the limit unset (0 — the default), a spawned process carries NO
// address-space cap. This pins the byte-identical-when-off contract.
//
// The OFF path makes no prlimit call at all (spawn.go: "0 = off — no
// prlimit call at all, byte-identical spawn path"), so the child simply
// INHERITS whatever RLIMIT_AS the parent carries — Unix fork semantics,
// not a spawn-path defect. Under an uncapped parent (dev box) "no cap"
// and "no prlimit call" are the same observation; under a capped parent
// (e.g. a clean-machine battery capping its harness at 3GiB RLIMIT_AS)
// the child legitimately inherits the cap and this assertion is
// meaningless. Skip in that case — an explicit SKIP, not a false FAIL.
// The byte-identical-when-off production contract stays pinned by
// TestSpawn_MemLimitConfigAccessors and the spawn.go comment regardless.
func TestSpawn_MemLimitOffByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("real-child rlimit test — skip in short mode")
	}
	if soft := parentASSoftLimit(); soft != 0 {
		t.Skipf("parent harness carries RLIMIT_AS soft=%d; OFF-path inheritance test is meaningless under a capped parent — run uncapped", soft)
	}
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "advr11-unlimited")

	spawner := NewSpawner(db, 4)
	spawner.SetNoExecFallback(false)

	// Explicitly the default state: no SetSpawnMemLimitMB call at all
	// (spawnMemLimitMB is package-zero unless armed). Guard against test
	// pollution anyway.
	SetSpawnMemLimitMB(0)
	t.Cleanup(func() { SetSpawnMemLimitMB(0) })

	project := PackedProject{
		Name:    "advr11-unlimited",
		Workdir: t.TempDir(),
		Command: "bash -c 'sleep 5'",
	}
	tick, err := spawner.Spawn(project, "advr11-unlimited-2026-09-16-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer tick.Wait()

	// Give a hypothetical (buggy) prlimit every chance to land, then read.
	time.Sleep(50 * time.Millisecond)
	if got := readProcASLimit(t, tick.PID); got != 0 {
		t.Errorf("child carried an address-space cap while the limit is OFF: soft = %d bytes — off must mean NO cap (byte-identical spawns)", got)
	}
}

// TestSpawn_MemLimitConfigAccessors pins the config-accessor contract:
// default off, SetSpawnMemLimitMB arms, negative normalizes to 0.
func TestSpawn_MemLimitConfigAccessors(t *testing.T) {
	SetSpawnMemLimitMB(0)
	t.Cleanup(func() { SetSpawnMemLimitMB(0) })
	if got := SpawnMemLimitMB(); got != 0 {
		t.Errorf("SpawnMemLimitMB() = %d after reset, want 0 (off by default)", got)
	}
	SetSpawnMemLimitMB(512)
	if got := SpawnMemLimitMB(); got != 512 {
		t.Errorf("SpawnMemLimitMB() = %d after SetSpawnMemLimitMB(512), want 512", got)
	}
	// Negative input is normalized to 0 (off), never a negative cap.
	SetSpawnMemLimitMB(-8)
	if got := SpawnMemLimitMB(); got != 0 {
		t.Errorf("SpawnMemLimitMB() = %d after SetSpawnMemLimitMB(-8), want 0 (normalized off)", got)
	}
}

// TestSpawn_MemLimitUnderLimitSpawnRunsUnchanged proves acceptance 2: a
// spawn UNDER the armed limit runs byte-identically — normal completion,
// correct output, normal tick path. The limit only exists to stop runaway
// growth, never to change what a well-behaved tick does.
func TestSpawn_MemLimitUnderLimitSpawnRunsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("real-child rlimit test — skip in short mode")
	}
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "advr11-normal")

	spawner := NewSpawner(db, 4)
	spawner.SetNoExecFallback(false)

	// A generous cap (bash + echo fit trivially) — the spawn must run to
	// completion and produce its output exactly as it would unlimited.
	SetSpawnMemLimitMB(1024)
	t.Cleanup(func() { SetSpawnMemLimitMB(0) })

	project := PackedProject{
		Name:    "advr11-normal",
		Workdir: t.TempDir(),
		Command: "bash -c 'echo advr11-ok'",
	}
	tick, err := spawner.Spawn(project, "advr11-normal-2026-09-16-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()

	if outcome.Status != TickCompleted {
		t.Errorf("outcome.Status = %q, want %q — a spawn under the limit must run unchanged", outcome.Status, TickCompleted)
	}
	// The full stdout is captured on the tick (st.Output), not the outcome.
	if !strings.Contains(tick.Output.String(), "advr11-ok") {
		t.Errorf("tick.Output = %q, want it to contain the child's echo output (normal tick path untouched)", tick.Output.String())
	}
}
