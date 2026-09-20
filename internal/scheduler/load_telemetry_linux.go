//go:build linux

package scheduler

import (
	"os"
	"strconv"
	"strings"
)

// ADV-R13 — host load/memory telemetry, MEASUREMENT ONLY.
//
// This sampler reads /proc/loadavg (1/5/15-minute load averages) and
// /proc/meminfo (MemTotal/MemAvailable, reported by the kernel in kB and
// converted to bytes here). It is consumed by exactly one caller — the
// evaluation cycle's recordHostSample (tick_process.go), which persists one
// row per evaluation pass into host_samples. It is NOT an admission input:
// the load-average admission gate (SCHED-GAP-125) reads its own live
// unix.Sysinfo sample at admission time (load_gate_linux.go currentLoad1m)
// and MUST stay byte-for-byte unchanged. Reviving any load threshold on
// this data requires neighbor attribution analysis, an
// escalation-amplification answer, a fail-open ruling and owner
// re-ratification — all later rows (ADV-R13 scope boundary).
//
// Split out per-platform exactly like the load gate pair so darwin/windows
// builds compile: /proc is a Linux-only filesystem.

// sampleHostLoad reads one load/memory sample from /proc. ok=false when the
// platform provides no reading (a MISSING reading is a reported gap, never a
// fabricated zero row). Var (not func) so tests can inject a deterministic
// sampler — the same convention currentLoad1m uses in load_gate_linux.go.
var sampleHostLoad = func() (s HostLoadSample, ok bool) {
	load1, load5, load15, lok := readProcLoadavg()
	total, avail, mok := readProcMeminfo()
	if !lok || !mok {
		return HostLoadSample{}, false
	}
	return HostLoadSample{
		Load1:             load1,
		Load5:             load5,
		Load15:            load15,
		MemTotalBytes:     total,
		MemAvailableBytes: avail,
		Source:            "proc",
	}, true
}

// procLoadavgPath / procMeminfoPath are vars (not constants) so the
// missing-file test can point them at an unreadable location and prove the
// sampler reports ok=false without panicking (ADV-R13). Non-test code never
// reassigns them.
var (
	procLoadavgPath = "/proc/loadavg"
	procMeminfoPath = "/proc/meminfo"
)

// readProcLoadavg parses the 1/5/15-minute load averages from
// /proc/loadavg ("0.52 0.58 0.59 1/469 12345" — the first three fields).
func readProcLoadavg() (load1, load5, load15 float64, ok bool) {
	raw, err := os.ReadFile(procLoadavgPath)
	if err != nil {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err2 := strconv.ParseFloat(fields[1], 64)
	l15, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return l1, l5, l15, true
}

// readProcMeminfo parses MemTotal and MemAvailable (kB) from /proc/meminfo
// and converts them to bytes.
func readProcMeminfo() (total, avail int64, ok bool) {
	raw, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		return 0, 0, false
	}
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			if v, perr := parseProcMeminfoKb(line); perr == nil {
				total, haveTotal = v, true
			}
		case strings.HasPrefix(line, "MemAvailable:"):
			if v, perr := parseProcMeminfoKb(line); perr == nil {
				avail, haveAvail = v, true
			}
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, false
	}
	return total, avail, true
}

// parseProcMeminfoKb converts one "<Key>:  <value> kB" /proc/meminfo line
// to bytes.
func parseProcMeminfoKb(line string) (int64, error) {
	_, valuePart, found := strings.Cut(line, ":")
	if !found {
		return 0, strconv.ErrSyntax
	}
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(valuePart), "kB"))
	kb, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}
