//go:build linux

package scheduler

import (
	"fmt"
	"log"
	"sync"

	"golang.org/x/sys/unix"
)

// ADV-R11 — per-spawn memory rlimits (GAP-048 cure).
//
// The one live incident in the no-host-signal class was a memory-capacity
// earlyoom kill: a spawned foreman session grew until the host-level OOM
// killer took it (and with it the tick, with no gateway/host signal the
// scheduler could see). The incumbent-compatible fix is a per-process
// RLIMIT_AS applied to the spawned process itself — the kernel enforces
// the cap at mmap/brk time, the failure surfaces inside the child (Go
// runtime allocation error / ENOMEM), and the scheduler's normal
// tick-timeout/lifecycle machinery handles the rest.
//
// G7 ruling: this is NOT an admission gate. It does not decide WHETHER a
// project spawns — every selected project still spawns, exactly as before.
// It constrains the spawned process's resources AT spawn time. No project
// is skipped, deferred, or dropped by this file; there is no admission
// logic here by design. (Admission gates live in SlotPool.spawn with a
// declared override; this file is deliberately not one.)
//
// Mechanism note (why prlimit(2) after Start, not SysProcAttr): Go's
// syscall.SysProcAttr on Linux has no pre-exec rlimit hook (no Rlimit
// field — unlike e.g. the cgo posix_spawn attr), so a parent cannot
// setrlimit a child between fork and exec in pure Go. unix.Prlimit is
// applied to the child pid immediately after cmd.Start() returns:
// the pid is exact (Start's pid IS the final process), the limit takes
// effect for the process itself and is INHERITED by everything the
// foreman forks later (workers, git, shells) — which is where the
// runaway growth actually happens. The window between exec and the
// prlimit landing (microseconds; the child is still parsing argv) is
// documented and accepted: earlyoom-class growth develops over minutes,
// not microseconds.
//
// Platform: this file is Linux-only (//go:build linux); non-Linux builds
// get the graceful no-op stub in spawn_mem_limit_other.go — the daemon
// runs, spawns behave exactly as before, and each limited spawn logs one
// WARN instead of failing. That is the documented degradation path
// (acceptance 6: rlimit approach must degrade gracefully on unsupported
// platforms).
var (
	spawnMemLimitMB int64 // 0 = off (default; fleet byte-identical when unset)
	spawnMemLimitMu sync.RWMutex
)

// SetSpawnMemLimitMB installs the global per-spawn RLIMIT_AS cap in
// megabytes (ADV-R11). 0 (or negative — normalized to 0) disables it.
// Called once from the daemon boot wiring after the flag/env/TOML chain
// resolves; last writer wins. Same contract as SetLoadGateThreshold.
func SetSpawnMemLimitMB(mb int64) {
	if mb < 0 {
		mb = 0
	}
	spawnMemLimitMu.Lock()
	spawnMemLimitMB = mb
	spawnMemLimitMu.Unlock()
}

// SpawnMemLimitMB returns the armed per-spawn memory cap in MB (0 = off).
func SpawnMemLimitMB() int64 {
	spawnMemLimitMu.RLock()
	defer spawnMemLimitMu.RUnlock()
	return spawnMemLimitMB
}

// applySpawnMemLimit caps the address space of a freshly-started spawn
// pid to mb MiB via prlimit(RLIMIT_AS), setting BOTH the soft and hard
// limit (soft-only would let the child raise its own cap back; hard ≤ the
// inherited hard is always permitted unprivileged). Errors are returned
// to the caller, which treats the limit as best-effort: a failed cap is
// logged and NEVER fails the spawn — a spawn that runs unlimited is the
// pre-ADV-R11 behavior, which is acceptable; a spawn that refuses to run
// is not.
func applySpawnMemLimit(pid, mb int64) error {
	bytes := mb * 1024 * 1024
	limit := unix.Rlimit{Cur: uint64(bytes), Max: uint64(bytes)}
	if err := unix.Prlimit(int(pid), unix.RLIMIT_AS, &limit, nil); err != nil {
		return fmt.Errorf("prlimit RLIMIT_AS %dMiB on pid %d: %w", mb, pid, err)
	}
	return nil
}

// applySpawnMemLimitIfArmed is the spawn-path hook (called from
// Spawner.Spawn right after cmd.Start() succeeds): when a limit is armed
// it caps the child; when the prlimit fails (or the platform stub says
// unsupported) it WARNs and continues — resource capping must never
// turn into a spawn failure.
func applySpawnMemLimitIfArmed(pid int64, project, tickID string) {
	mb := SpawnMemLimitMB()
	if mb <= 0 {
		return // off (default) — byte-identical pre-ADV-R11 behavior
	}
	if err := applySpawnMemLimit(pid, mb); err != nil {
		log.Printf("WARN: ADV-R11 spawn mem limit %dMiB not applied to %s tick=%s pid=%d: %v (spawn continues unlimited)",
			mb, project, tickID, pid, err)
	}
}
