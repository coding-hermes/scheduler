//go:build !linux

package scheduler

// SCHED-GAP-201 — load-average gate, non-Linux stub.
//
// currentLoad1m's real reading is Linux-only: unix.Sysinfo(2) and
// SI_LOAD_SHIFT (the fixed-point math /proc/loadavg uses) have no
// portable equivalent. On non-Linux platforms this stub returns
// (0, false) — a MISSING reading, never a fabricated zero: the gate
// fails open on missing telemetry (loadGateActive treats ok=false as
// "gate inactive", the documented behavior at the top of load_gate.go),
// so a non-Linux build keeps the gate PERMANENTLY INACTIVE. The
// threshold is still read from config and stored, but with no load
// signal available it can never trip — an operator who enables the gate
// on darwin/windows gets a silent no-op by design, not a deadlock.
//
// Deliberately does NOT import golang.org/x/sys/unix (Linux-only API
// surface; SCHED-GAP-201 cross-compile fix).

// currentLoad1m is the non-Linux stub: no platform load telemetry is
// read, so the sampler reports "no signal" (ok=false). Var (not func)
// to mirror the Linux shape so tests can inject a deterministic sampler.
var currentLoad1m = func() (float64, bool) {
	return 0, false
}
