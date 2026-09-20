//go:build !linux

package scheduler

import (
	"log"
	"os"
	"syscall"
)

// SCHED-GAP-201 — non-Linux stubs for the spawn-path process-group
// primitives (see spawn_helpers.go). Process groups are a POSIX facility:
// windows SysProcAttr has no Setpgid field and there is no kill(2), so
// non-Linux builds degrade gracefully instead of failing to compile.
//
// Degradation is documented and best-effort: the child is killed
// individually (os.Process.Kill), NOT as a group — on darwin, orphaned
// descendants of a timed-out tick may survive where the Linux group kill
// would have reaped them. Spawning itself behaves identically (no
// process-group attr is set).

// processGroupAttr returns nil on non-Linux: no SysProcAttr is set on
// the child, so it stays in the daemon's own process group.
func processGroupAttr() *syscall.SysProcAttr {
	return nil
}

// killProcessGroup is the best-effort non-Linux fallback: kill the
// process itself. A group kill does not exist here, so descendants are
// out of scope (documented degradation, mirrors the mem-limit stub
// contract in spawn_mem_limit_other.go).
func killProcessGroup(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		log.Printf("WARN: SCHED-GAP-201 process-group kill fallback: FindProcess(%d): %v (best-effort, spawn timeout proceeds)", pid, err)
		return
	}
	if err := p.Kill(); err != nil {
		log.Printf("WARN: SCHED-GAP-201 process-group kill fallback: Kill(%d): %v (best-effort)", pid, err)
	}
}
