//go:build !linux

package scheduler

import (
	"errors"
	"log"
	"sync"
)

// ADV-R11 — per-spawn memory rlimits (GAP-048 cure), non-Linux stub.
//
// RLIMIT_AS via prlimit(2) is a Linux kernel facility. On platforms
// without it (darwin/windows/…), the daemon must still build and run
// with per-spawn behavior byte-identical to pre-ADV-R11: the config
// chain still parses and reports the value, SetSpawnMemLimitMB still
// stores it, but applySpawnMemLimit reports ENOTSUP-equivalent so every
// limited spawn logs exactly one WARN and runs unlimited — the same
// graceful degradation as a prlimit(2) failure on Linux (documented in
// spawn_mem_limit.go; acceptance 6).
var (
	spawnMemLimitMB int64 // 0 = off
	spawnMemLimitMu sync.RWMutex
)

// SetSpawnMemLimitMB installs the global per-spawn cap in MB (0/neg = off).
func SetSpawnMemLimitMB(mb int64) {
	if mb < 0 {
		mb = 0
	}
	spawnMemLimitMu.Lock()
	spawnMemLimitMB = mb
	spawnMemLimitMu.Unlock()
}

// SpawnMemLimitMB returns the armed cap in MB (0 = off).
func SpawnMemLimitMB() int64 {
	spawnMemLimitMu.RLock()
	defer spawnMemLimitMu.RUnlock()
	return spawnMemLimitMB
}

var errSpawnMemLimitUnsupported = errors.New("spawn mem limit unsupported on this platform (prlimit/RLIMIT_AS is Linux-only; ADV-R11)")

// applySpawnMemLimit is the no-op stub: never caps, always reports
// unsupported so the shared hook WARNs once per limited spawn.
func applySpawnMemLimit(pid, mb int64) error {
	_ = pid
	_ = mb
	return errSpawnMemLimitUnsupported
}

// applySpawnMemLimitIfArmed mirrors the Linux hook byte-for-byte.
func applySpawnMemLimitIfArmed(pid int64, project, tickID string) {
	mb := SpawnMemLimitMB()
	if mb <= 0 {
		return
	}
	if err := applySpawnMemLimit(pid, mb); err != nil {
		log.Printf("WARN: ADV-R11 spawn mem limit %dMiB not applied to %s tick=%s pid=%d: %v (spawn continues unlimited)",
			mb, project, tickID, pid, err)
	}
}
