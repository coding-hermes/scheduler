//go:build windows

package lanehook

import (
	"os"
	"syscall"
)

// INT-CI-4184 — windows stubs for the hook-runner process-group
// primitives (see runner_unix.go). Windows has no POSIX process groups:
// syscall.SysProcAttr has no Setpgid field and there is no kill(2), so
// the windows build degrades gracefully instead of failing to compile.

// processGroupAttrs returns nil on windows: no SysProcAttr is set on the
// hook child, so it stays in the runner's own process group.
func processGroupAttrs() *syscall.SysProcAttr {
	return nil
}

// killProcessGroup is a DOCUMENTED NO-OP on windows, returning nil
// (chosen semantics: no-op/nil, not a sentinel error). A timeout
// therefore does not group-kill on windows: the direct hook process is
// still terminated by Go's WaitDelay backstop (exec.Cmd.WaitDelay
// terminates a child that failed to exit after its Context was canceled
// with os.Process.Kill), while grandchildren spawned by a hook may
// survive until their own exit. This is the same documented degradation
// as internal/scheduler/spawn_helpers_other.go, minus the FindProcess
// fallback: the Cancel closure in runner.go already holds the *os.Process
// that Go would kill at WaitDelay, so re-resolving it here adds nothing.
func killProcessGroup(p *os.Process, sig os.Signal) error {
	return nil
}
