//go:build !linux

package scheduler

// ADV-R13 — host load/memory telemetry, non-Linux stub.
//
// sampleHostLoad's real reading is Linux-only: /proc/loadavg and
// /proc/meminfo have no portable equivalent. On non-Linux platforms this
// stub reports a MISSING reading (ok=false) — never a fabricated zero: the
// gap is visible as a log line in the evaluation cycle and no row is
// persisted, so telemetry consumers can distinguish "no data recorded"
// from "host idle". The threshold/admission question is out of scope for
// this row entirely (measurement only — see load_telemetry.go).
//
// Deliberately platform-neutral: no OS-specific imports.

// sampleHostLoad is the non-Linux stub: no platform load/memory telemetry is
// read, so the sampler reports "no reading" (ok=false). Var (not func) to
// mirror the Linux shape so tests can inject a deterministic sampler.
var sampleHostLoad = func() (HostLoadSample, bool) {
	return HostLoadSample{}, false
}
