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
//
// INT-CI-4190 — what the healthy path does NOT guarantee, and therefore what
// this test must not assert. The announcement block ("HTTP: listening on ...",
// Dashboard/API/MCP/Auth) is printed by the goroutine that then calls
// serveDashboard (main.go: `go func(){ log ...; serveDashboard }`). The ready
// line is printed by main. Two goroutines, no synchronization between them:
// main can reach the ready line while the announce/serve goroutine is still in
// the run queue, so "listening on" may be logged AFTER "schedulerd ready".
// Hosted CI run 43008084586 (head 5f46506e) failed a perfectly healthy boot on
// exactly that interleaving — ready at log offset 2076, "HTTP: listening on" at
// 2912 — and the health request that followed returned 200, which is the
// contract holding. So this test now pins, in this order:
//   - the socket is bound and accepting at the moment the ready line appears
//     (probed directly; a refused connection is fatal and never retried);
//   - the serve goroutine's announcement block is internally ordered (its own
//     program order — deterministic, unlike ready-vs-announcement);
//   - the first request after the ready line is answered within a bounded
//     deadline rather than on the very first syscall, because scheduling the
//     accept loop is the runner's business, not the daemon's.

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
	// ONE wait for the child's whole lifetime: every exit path (ready poll,
	// shutdown, cleanup) drains this channel instead of racing a second
	// cmd.Wait(). A child that dies during boot is reported with its own log
	// immediately, so a failed start is never disguised as a timeout.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitCh:
		case <-time.After(45 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	})

	// Ready line: bounded poll. A boot that outruns the deadline on a loaded
	// runner is a real failure, not something to paper over by polling forever.
	const readyDeadline = 120 * time.Second
	readyUntil := time.Now().Add(readyDeadline)
	var log string
	for {
		log = readFileString(t, outPath)
		if strings.Contains(log, "schedulerd ready") {
			break
		}
		select {
		case werr := <-waitCh:
			waited = true
			t.Fatalf("schedulerd exited (%v) before printing its ready line\n%s", werr, log)
		default:
		}
		if time.Now().After(readyUntil) {
			t.Fatalf("schedulerd never printed its ready line within %s\n%s", readyDeadline, log)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// TEETH — readiness means a bound, ACCEPTING socket. Probed against the
	// live socket at the moment readiness is observed, and never retried: on
	// the QA-CHS-194 defect (readiness announced before net.Listen) this dial
	// is refused, and retrying it would let a boot that announces too early
	// pass as soon as it eventually binds. A completed TCP handshake is the
	// kernel's own statement that the socket accepts.
	probe, perr := net.DialTimeout("tcp", addr, 5*time.Second)
	if perr != nil {
		t.Fatalf("ready line observed but %s does not accept connections: %v — readiness must not precede a bound socket\n%s",
			addr, perr, readFileString(t, outPath))
	}
	_ = probe.Close()

	// First request: bounded poll rather than a single attempt. main and the
	// announce/serve goroutine are not ordered (see the INT-CI-4190 note at the
	// top of this file), so under runner contention the accept loop can still
	// be in the run queue when the ready line lands. A REFUSED connection stays
	// fatal — that is the contract violation; a slow accept loop is the
	// runner's business and is retried within the deadline.
	const (
		healthDeadline  = 60 * time.Second
		healthAttemptTO = 5 * time.Second
	)
	client := &http.Client{Timeout: healthAttemptTO}
	healthURL := "http://" + addr + "/api/v1/health"
	healthUntil := time.Now().Add(healthDeadline)
	var (
		attempts int
		lastErr  error
		status   int
		body     []byte
	)
	for {
		attempts++
		resp, gerr := client.Get(healthURL)
		if gerr != nil {
			lastErr = gerr
			if errors.Is(gerr, syscall.ECONNREFUSED) {
				t.Fatalf("GET %s refused after the ready line (%v) — ready must not precede an accepting socket\n%s",
					healthURL, gerr, readFileString(t, outPath))
			}
			if time.Now().After(healthUntil) {
				t.Fatalf("GET %s still failing after %d attempts over %s: %v\n%s",
					healthURL, attempts, healthDeadline, lastErr, readFileString(t, outPath))
			}
			time.Sleep(25 * time.Millisecond)
			continue
		}
		status = resp.StatusCode
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		break // any HTTP answer is an answer; asserted immediately below
	}
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/health after ready = %d, want 200 (body %q)", status, string(body))
	}

	// Announcement block: order INSIDE the block only. One goroutine writes
	// these lines sequentially, so the order is program order and stays
	// deterministic on a loaded runner. Its position relative to the ready line
	// is deliberately NOT asserted (INT-CI-4190 note); the socket probe above
	// is what covers "announcements follow a bound socket".
	announceUntil := time.Now().Add(30 * time.Second)
	for !strings.Contains(log, "listening on") {
		if time.Now().After(announceUntil) {
			t.Fatalf("health answered 200 but no 'listening on' announcement appeared within 30s — announcements must follow a bound socket\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
		log = readFileString(t, outPath)
	}
	listenIdx := strings.Index(log, "listening on")
	rest := log[listenIdx:]
	dashRel := strings.Index(rest, "Dashboard:")
	apiRel := strings.Index(rest, "API:")
	if dashRel < 0 || apiRel < 0 {
		t.Fatalf("announcement block incomplete after 'listening on' (Dashboard=%d API=%d)\n%s", dashRel, apiRel, rest)
	}
	if dashRel > apiRel {
		t.Fatalf("announcement block out of order (Dashboard=%d after API=%d) — one goroutine writes this block sequentially\n%s", dashRel, apiRel, rest)
	}

	// Graceful shutdown: SIGTERM must be honoured within the shutdown budget.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-waitCh:
		waited = true
	case <-time.After(45 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		waited = true
		t.Fatalf("child did not exit after SIGTERM")
	}
}
