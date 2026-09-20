//go:build linux

package scheduler

// ADV-R13 — Linux-only regression tests for the /proc-backed sampler: a
// missing or unreadable /proc file must degrade to an honest "no reading"
// (ok=false) — never a panic, never a fabricated zero.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestADVR13_MissingProcFilesDoNotPanic points the /proc path vars at
// missing files and proves the sampler reports ok=false without panicking.
func TestADVR13_MissingProcFilesDoNotPanic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc-path test is Linux-only")
	}
	origLoad, origMem := procLoadavgPath, procMeminfoPath
	t.Cleanup(func() { procLoadavgPath, procMeminfoPath = origLoad, origMem })

	dir := t.TempDir()
	procLoadavgPath = filepath.Join(dir, "missing-loadavg")
	procMeminfoPath = filepath.Join(dir, "missing-meminfo")

	s, ok := sampleHostLoad()
	if ok {
		t.Fatalf("sampler reported ok=true with missing /proc files: %+v", s)
	}
	if s.Source != "" {
		t.Errorf("gap sample carries source=%q, want empty", s.Source)
	}
}

// TestADVR13_UnreadableMeminfoIsAGap proves the partial-failure arm: a
// readable loadavg with an unreadable/empty meminfo still reports ok=false
// (both halves are required for a complete reading).
func TestADVR13_UnreadableMeminfoIsAGap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc-path test is Linux-only")
	}
	origLoad, origMem := procLoadavgPath, procMeminfoPath
	t.Cleanup(func() { procLoadavgPath, procMeminfoPath = origLoad, origMem })

	dir := t.TempDir()
	procLoadavgPath = filepath.Join(dir, "loadavg")
	if err := os.WriteFile(procLoadavgPath, []byte("0.52 0.58 0.59 1/469 12345\n"), 0o644); err != nil {
		t.Fatalf("write loadavg fixture: %v", err)
	}
	procMeminfoPath = filepath.Join(dir, "missing-meminfo")

	if _, ok := sampleHostLoad(); ok {
		t.Fatal("sampler reported ok=true with a missing meminfo — a partial reading must be a gap")
	}
}

// TestADVR13_MalformedProcContentIsAGap proves malformed content (truncated
// loadavg, meminfo without MemAvailable) degrades to ok=false, never a
// panic.
func TestADVR13_MalformedProcContentIsAGap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc-path test is Linux-only")
	}
	origLoad, origMem := procLoadavgPath, procMeminfoPath
	t.Cleanup(func() { procLoadavgPath, procMeminfoPath = origLoad, origMem })

	dir := t.TempDir()
	procLoadavgPath = filepath.Join(dir, "loadavg")
	if err := os.WriteFile(procLoadavgPath, []byte("only-one-field\n"), 0o644); err != nil {
		t.Fatalf("write loadavg fixture: %v", err)
	}
	procMeminfoPath = filepath.Join(dir, "meminfo")
	if err := os.WriteFile(procMeminfoPath, []byte("MemTotal:  16308280 kB\n"), 0o644); err != nil {
		t.Fatalf("write meminfo fixture: %v", err)
	}

	if _, ok := sampleHostLoad(); ok {
		t.Fatal("sampler reported ok=true with malformed /proc content — must be a gap")
	}
}

// TestADVR13_SamplerReadsRealProc proves the real path parses THIS host's
// /proc into a complete reading (the deployment platform is always Linux
// here; the values are environment-dependent, so only shape is asserted).
func TestADVR13_SamplerReadsRealProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc test is Linux-only")
	}
	s, ok := sampleHostLoad()
	if !ok {
		t.Skip("host provides no /proc reading right now")
	}
	if s.Source != "proc" {
		t.Errorf("source = %q, want proc", s.Source)
	}
	if s.MemTotalBytes <= 0 || s.MemAvailableBytes <= 0 {
		t.Errorf("memory bytes not positive: total=%d avail=%d", s.MemTotalBytes, s.MemAvailableBytes)
	}
	if s.MemAvailableBytes > s.MemTotalBytes {
		t.Errorf("available (%d) exceeds total (%d)", s.MemAvailableBytes, s.MemTotalBytes)
	}
}
