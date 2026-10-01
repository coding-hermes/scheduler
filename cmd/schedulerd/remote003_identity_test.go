package main

// REMOTE-003 §1 — the boot-level identity proof.
//
// The unit tests pin the pieces (ResolveDefaultSchedulerID, the config
// layering, the store-level stamp + backfill); this test proves the CHAIN
// main() wires: env > TOML > hostname resolution happens ONCE at boot, the
// `SCHEDULER: id=<id>` line reaches the log file, and the first boot
// backfills pre-existing rows ('' scheduler_id) with the local id.
//
// main() is not unit-testable (flags + listener + signals), so — same shape
// as TestInstanceIdentityReachesLogFile — the honest test builds the binary,
// seeds a scratch DB with an identity-less row through the real write paths
// (in-process, before the child boots: the test process never installs an
// identity, so the row reads ''), boots the child with SCHEDULER_ID set and
// a fake HOME, waits for "schedulerd ready", shuts it down, and reopens the
// DB to assert the backfill claimed the row. Skipped in -short (subprocess
// boot + warm-cache build), exactly like its sibling test.

import (
	"bytes"
	"context"
	"database/sql"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestSchedulerIdentityBootChain(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess boot test (real daemon boot + graceful shutdown); skipped in -short")
	}

	dir := t.TempDir()
	fakeHome := filepath.Join(dir, "fakehome")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir fakehome: %v", err)
	}
	dbPath := filepath.Join(dir, "remote3.db")
	logPath := dbPath + ".log"

	// Seed a pre-identity row through the real write path (the test process
	// installs no scheduler identity, so the row carries '').
	seedDB, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("seed InitDB: %v", err)
	}
	if err := database.CreateProject(context.Background(), seedDB, &database.Project{
		Name: "legacy-lane", RepoURL: "https://example.com/legacy", Workdir: "/tmp/legacy-lane",
		Weight: 10, Priority: 5, CooldownS: 900,
	}); err != nil {
		t.Fatalf("seed CreateProject: %v", err)
	}
	var before string
	if err := seedDB.QueryRow(`SELECT scheduler_id FROM projects WHERE name='legacy-lane'`).Scan(&before); err != nil {
		t.Fatalf("seed readback: %v", err)
	}
	if before != "" {
		t.Fatalf("seed row scheduler_id = %q, want \"\" (the identity-less writer shape)", before)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	bin := filepath.Join(dir, "schedulerd-under-test")
	if out, berr := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); berr != nil {
		t.Fatalf("go build: %v\n%s", berr, out)
	}

	cmd := exec.Command(bin, "-db", dbPath, "-listen", addr)
	cmd.Env = append(schedulerdChildEnv(t, fakeHome), "SCHEDULER_ID=box-remote3")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	const wantID = "SCHEDULER: id=box-remote3"
	ready := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitCh:
			t.Fatalf("child exited before becoming ready: %v\nstdout/stderr:\n%s", err, stdout.String())
		default:
		}
		if b, rerr := os.ReadFile(logPath); rerr == nil &&
			bytes.Contains(b, []byte(wantID)) && bytes.Contains(b, []byte("schedulerd ready")) {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatalf("log file %s never carried %q within 20s\nstdout/stderr:\n%s", logPath, wantID, stdout.String())
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-waitCh:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatalf("child did not exit within 10s of SIGTERM\nstdout/stderr:\n%s", stdout.String())
	}

	// The log FILE carries the boot identity line (deliverable: "logged
	// SCHEDULER: id=<id>").
	fileBody, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read log file: %v", rerr)
	}
	if !bytes.Contains(fileBody, []byte(wantID)) {
		t.Fatalf("identity line %q missing from log file\nfile:\n%s", wantID, fileBody)
	}

	// The backfill claimed the pre-identity row for the booting scheduler.
	after, aerr := sql.Open("sqlite", dbPath)
	if aerr != nil {
		t.Fatalf("reopen db: %v", aerr)
	}
	defer after.Close()
	var gotID string
	if err := after.QueryRow(`SELECT scheduler_id FROM projects WHERE name='legacy-lane'`).Scan(&gotID); err != nil {
		t.Fatalf("backfill readback: %v", err)
	}
	if gotID != "box-remote3" {
		t.Errorf("pre-existing row scheduler_id = %q after boot, want box-remote3 (first boot backfills with the local id)", gotID)
	}
}

// The hostname-default arm of acceptance #3: with SCHEDULER_ID unset the
// resolved id is the SHORT hostname. Proven as a unit against the exact
// derivation main() falls back to (a second subprocess boot for one grep is
// not worth the cost; the precedence chain env>toml>hostname is linear code
// and the other two layers are proven above/at the config layer).
func TestSchedulerIDDefaultIsShortHostname(t *testing.T) {
	got := database.ResolveDefaultSchedulerID()
	want, herr := os.Hostname()
	if herr != nil || want == "" {
		t.Skip("no hostname available on this host")
	}
	if i := strings.IndexByte(want, '.'); i >= 0 {
		want = want[:i]
	}
	if got != want {
		t.Errorf("default scheduler id = %q, want the short hostname %q", got, want)
	}
}
