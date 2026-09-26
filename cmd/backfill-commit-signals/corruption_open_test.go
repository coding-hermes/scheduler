package main

// ============================================================================
// Corruption-aware db open (SCHED-GAP-1613): a chaos-truncated (0-byte)
// scheduler.db must be named as corruption, never reported as a generic
// open/query error. A real SQLite error keeps its class ("open db
// (SQLite error; possible corruption): ..."), and a missing path keeps the
// driver's own create-on-open behavior. Every case exits 1: fail-closed.
// ============================================================================

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun_ZeroByteDBIsNamedCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil { // chaos-truncated: 0 bytes
		t.Fatalf("write empty db: %v", err)
	}

	code, out, errOut := runCLI(t, "--db", path, "--since", "2026-09-19T00:00:00Z", "--until", "2026-09-21T00:00:00Z", "--no-board")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (fail-closed); stdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"corrupt", "0-byte", path} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "no such table") {
		t.Errorf("corruption surfaced as the generic query error instead of the truncation message:\n%s", errOut)
	}
	if out != "" {
		t.Errorf("a corrupt-db run must not print a report to stdout, got:\n%s", out)
	}
}

func TestRun_ValidDBStillOpensAfterCorruptionCheck(t *testing.T) {
	db, dbPath := newTestDB(t)
	now := time.Now()
	since, until := fixture(t, db, now)

	code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--no-board")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, errOut)
	}
	if strings.Contains(errOut, "corrupt") || strings.Contains(out, "corrupt") {
		t.Errorf("a valid database was flagged corrupt:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	const want = "3 affected / 2 back-fillable / 1 unrecoverable"
	if !strings.Contains(out, want) {
		t.Errorf("valid-db run lost its report line %q:\n%s", want, out)
	}
}

func TestRun_MissingDBPathKeepsCreateOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist-yet.db")

	code, _, errOut := runCLI(t, "--db", path, "--since", "2026-09-19T00:00:00Z", "--until", "2026-09-21T00:00:00Z", "--no-board")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "no such table: ticks") {
		t.Errorf("missing-path behavior changed (driver creates the file, the query then names the empty schema); stderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "corrupt") || strings.Contains(errOut, "truncated") {
		t.Errorf("a not-found path must not be reported as corruption:\n%s", errOut)
	}
}

func TestRun_DirectoryAsDBPathNamesPossibleCorruption(t *testing.T) {
	dir := t.TempDir()

	code, _, errOut := runCLI(t, "--db", dir, "--since", "2026-09-19T00:00:00Z", "--until", "2026-09-21T00:00:00Z", "--no-board")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, errOut)
	}
	for _, want := range []string{"open db (SQLite error; possible corruption)", "unable to open database file"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}
