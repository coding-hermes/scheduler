package main

// QA-CHS-194 — a failed bind must never look like a successful start.
//
// The defect this pins: main() used to announce readiness inside a goroutine
// that then called ListenAndServe. On an occupied port the log carried
//   HTTP: listening on 127.0.0.1:9090
//     Dashboard: ... API: ... MCP: ... Auth: ...
// followed by "bind: address already in use", while /api/v1/health kept
// answering from the process that already owned the port. A restart that
// silently failed was indistinguishable from one that worked.
//
// Contract now:
//   - net.Listen runs synchronously before ANY announcement line;
//   - a bind failure exits non-zero and names address + errno exactly once,
//     with no readiness/announcement line anywhere in that log;
//   - on the healthy path the ready line only appears once the socket is
//     bound, so the first request issued after it is answered.

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildSchedulerdUnderTest compiles the daemon once per test into a temp dir.
func buildSchedulerdUnderTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "schedulerd-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// readFileString is a tolerant read: the child may be mid-write.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBindFailureEmitsNoReadinessLine(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess boot test (real daemon boot); skipped in -short")
	}

	dir := t.TempDir()
	fakeHome := filepath.Join(dir, "fakehome")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fakehome: %v", err)
	}

	// Occupy the port for the whole test: this IS the defect scenario.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occupied.Close()
	addr := occupied.Addr().String()

	bin := buildSchedulerdUnderTest(t)
	dbPath := filepath.Join(dir, "qa.db")
	outPath := filepath.Join(dir, "bind-fail.log")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	defer outFile.Close()

	cmd := exec.Command(bin, "-db", dbPath, "-listen", addr, "-log-file", filepath.Join(dir, "tick.log"))
	cmd.Env = schedulerdChildEnv(t, fakeHome)
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		if werr == nil {
			t.Fatalf("schedulerd exited 0 although %s was already bound — a bind failure must be non-zero", addr)
		}
		var ee *exec.ExitError
		if ok := errors.As(werr, &ee); ok {
			if ee.ExitCode() == 0 {
				t.Fatalf("exit code 0 on bind failure")
			}
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("schedulerd neither exited nor failed within 60s on an occupied port")
	}

	log := readFileString(t, outPath)

	if strings.Contains(log, "schedulerd ready") {
		t.Errorf("log carries 'schedulerd ready' although the port was occupied — a failed start must not announce readiness\n%s", log)
	}
	if strings.Contains(log, "listening on") {
		t.Errorf("log carries a 'listening on' announcement although net.Listen never bound — announcements must follow a bound socket\n%s", log)
	}
	if strings.Contains(log, "Dashboard:") || strings.Contains(log, "API:") {
		t.Errorf("log advertises the dashboard/API URLs although nothing was bound\n%s", log)
	}
	if n := strings.Count(log, "address already in use"); n != 1 {
		t.Errorf("bind error named %d times, want exactly 1\n%s", n, log)
	}
	if !strings.Contains(log, addr) {
		t.Errorf("bind failure does not name the address %s\n%s", addr, log)
	}
}

func TestReadyLineOnlyAfterSocketAccepts(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess boot test (real daemon boot); skipped in -short")
	}

	dir := t.TempDir()
	fakeHome := filepath.Join(dir, "fakehome")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fakehome: %v", err)
	}

	// Reserve then release: the child must be the one that binds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	bin := buildSchedulerdUnderTest(t)
	dbPath := filepath.Join(dir, "qa.db")
	outPath := filepath.Join(dir, "boot.log")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	defer outFile.Close()

	cmd := exec.Command(bin, "-db", dbPath, "-listen", addr, "-log-file", filepath.Join(dir, "tick.log"))
	cmd.Env = schedulerdChildEnv(t, fakeHome)
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	// Wait for the ready line, then issue ONE request immediately: the ready
	// line is a bound-socket barrier, so it must answer 200 with no retry.
	deadline := time.Now().Add(60 * time.Second)
	var log string
	for {
		log = readFileString(t, outPath)
		if strings.Contains(log, "schedulerd ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("schedulerd never printed its ready line within 60s\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		t.Fatalf("first GET /api/v1/health after the ready line failed: %v — ready must not precede an accepting socket\n%s", err, readFileString(t, outPath))
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/health after ready = %d, want 200 (body %q)", resp.StatusCode, string(body))
	}

	// Announcement ordering: the listening line precedes readiness.
	listenIdx := strings.Index(log, "listening on")
	readyIdx := strings.Index(log, "schedulerd ready")
	if listenIdx < 0 || readyIdx < 0 || listenIdx > readyIdx {
		t.Fatalf("expected 'listening on' before 'schedulerd ready' (listen=%d ready=%d)\n%s", listenIdx, readyIdx, log)
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
		waited = true
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		waited = true
		t.Fatalf("child did not exit after SIGTERM")
	}
}
