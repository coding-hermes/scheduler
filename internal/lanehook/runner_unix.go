//go:build unix

package lanehook

import (
	"fmt"
	"os"
	"syscall"
)

// INT-CI-4184 — unix process-group primitives for the hook runner, split
// out of runner.go so the windows cross-compile (GOOS=windows amd64/arm64)
// builds. Mirrors internal/scheduler/spawn_helpers.go: each hook step runs
// in its own process group (SysProcAttr.Setpgid at Start) and a timeout
// kills the GROUP (negative-pid SIGKILL), so grandchildren spawned by a
// hook cannot survive the WaitDelay deadline (S13 §2.3.2). Windows builds
// get the documented stubs in runner_windows.go.

// processGroupAttrs returns the SysProcAttr that puts the hook child in
// its own process group (Setpgid). Unix-only: the Setpgid field does not
// exist on the windows SysProcAttr.
func processGroupAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole process group led by p (negative
// pid = group kill, matching the Setpgid above). The runner only ever
// passes os.Kill (SIGKILL); a different signal is rejected instead of
// being silently mis-signalled.
func killProcessGroup(p *os.Process, sig os.Signal) error {
	if p == nil {
		return nil
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("killProcessGroup: unsupported signal %v", sig)
	}
	return syscall.Kill(-p.Pid, s)
}
