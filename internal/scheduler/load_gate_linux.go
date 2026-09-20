//go:build linux

package scheduler

import "golang.org/x/sys/unix"

// SCHED-GAP-201 — Linux sampler for the load-average admission gate
// (SCHED-GAP-125). Split out of load_gate.go so darwin/windows builds
// compile: unix.Sysinfo(2) and SI_LOAD_SHIFT are Linux-only symbols.
// The non-Linux stub (always-missing reading, gate permanently
// inactive) lives in load_gate_other.go; the gate/fail-open semantics
// are documented at the top of load_gate.go.

// currentLoad1m returns the 1-minute load average; ok=false when the
// platform provides no reading (gate fails open on missing telemetry).
// Var (not func) so tests can inject a deterministic sampler.
var currentLoad1m = func() (float64, bool) {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0, false
	}
	// Linux: fixed-point, SI_LOAD_SHIFT=16 (same math /proc/loadavg uses).
	l1 := float64(si.Loads[0]) / (1 << unix.SI_LOAD_SHIFT)
	if l1 <= 0 {
		// 0 on a genuinely idle box reads 0 too — but 0 never trips the
		// gate either way, so treat as no-signal without ambiguity.
		return 0, l1 > 0
	}
	return l1, true
}
