//go:build linux

package scheduler

import "syscall"

// SCHED-GAP-201 — Linux-only process-group primitives for the spawn
// path, split out of spawn.go so the cross-compile targets
// (darwin/windows) build. Each scheduler-owned worker runs in its own
// process group (SysProcAttr.Setpgid at Start) and is killed as a
// GROUP on timeout (negative-pid kill), so shells, Hermes workers and
// test runners cannot survive a timed-out tick. Non-Linux builds get
// the graceful stubs in spawn_helpers_other.go.

// processGroupAttr returns the SysProcAttr that puts the spawned child
// in its own process group (Setpgid). Linux-only: the Setpgid field
// does not exist on darwin/windows SysProcAttr.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the whole process group led by pid
// (negative pid = group kill, matching the Setpgid above).
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
