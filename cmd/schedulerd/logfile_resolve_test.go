package main

// SCHED-GAP-1647 — log-path resolution tests.
//
// The bug this pins: cmd/schedulerd/main.go used to default --log-file to
// the production path, so EVERY instance — including scratch/test instances
// started with their own -db and --listen — appended its boot/shutdown lines
// (including "Shutdown complete") into the production log. A report derived
// from that log then counted nine shutdowns for 2026-09-26 where the systemd
// unit restarted exactly twice, and a scratch SIGTERM could be read as a
// production outage. resolveLogFile now derives <db>.log for any non-default
// db, and the identity line in main() stamps every boot with db + listen.
//
// The helper is pure except for defaultDBPath/defaultLogPath, which expand
// $HOME at call time — so every test pins HOME via t.Setenv and asserts
// against expanded literals instead of the host's real home.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestResolveLogFileExplicitFlagWins(t *testing.T) {
	t.Setenv("HOME", "/home/fakehome")
	prodLog := defaultLogPath()

	cases := []struct {
		name   string
		dbPath string
		flag   string
		want   string
	}{
		{"explicit path over scratch db", "/tmp/scratch/qa.db", "/tmp/scratch/custom.log", "/tmp/scratch/custom.log"},
		{"explicit path over production db", defaultDBPath(), prodLog + ".2", prodLog + ".2"},
		{"explicit empty disables the file log", "/tmp/scratch/qa.db", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLogFile(tc.dbPath, tc.flag, true)
			if got != tc.want {
				t.Fatalf("resolveLogFile(%q, %q, explicit) = %q, want %q", tc.dbPath, tc.flag, got, tc.want)
			}
		})
	}
}

func TestResolveLogFileScratchDBDerivesOwnLog(t *testing.T) {
	t.Setenv("HOME", "/home/fakehome")

	// The shapes from the false-restart report: scratch instances on their
	// own db must derive <db>.log — never the production path.
	for _, db := range []string{
		"/tmp/schedgap1623/scheduler-perf.db",
		"/tmp/sched177-test.db",
		"/tmp/qa-sched-ui-bin/qa.db",
		"scratch/dev.db",
	} {
		got := resolveLogFile(db, defaultLogPath(), false)
		want := db + ".log"
		if got != want {
			t.Fatalf("resolveLogFile(%q, _, false) = %q, want %q", db, got, want)
		}
		if got == defaultLogPath() {
			t.Fatalf("scratch db %q derived the production log path %q", db, got)
		}
		if !strings.HasSuffix(got, ".log") {
			t.Fatalf("derived path %q must end in .log", got)
		}
	}
}

func TestResolveLogFileProductionInvocationUnchanged(t *testing.T) {
	t.Setenv("HOME", "/home/fakehome")
	prodDB, prodLog := defaultDBPath(), defaultLogPath()

	// The production invocation (defaults, no --log-file) must keep the
	// documented production path exactly.
	if got := resolveLogFile(prodDB, prodLog, false); got != prodLog {
		t.Fatalf("production invocation resolved log = %q, want %q", got, prodLog)
	}
	want := "/home/fakehome/.hermes/coding-hermes/scheduler.log"
	if prodLog != want {
		t.Fatalf("defaultLogPath() = %q, want %q", prodLog, want)
	}

	// An equivalent spelling of the production db (cleaned/relative noise)
	// must still resolve to the production log, not derive a sibling.
	for _, variant := range []string{
		filepath.Join(prodDB, "..", filepath.Base(prodDB)),
		prodDB + "/./",
	} {
		if got := resolveLogFile(variant, prodLog, false); got != prodLog {
			t.Fatalf("production db variant %q resolved log = %q, want %q", variant, got, prodLog)
		}
	}
}

// TestFlagDefaultsMatchProductionPair pins the source end of the production
// guarantee: main()'s --db default is the production db literal and the
// --log-file default is defaultLogPath(), so a plain production boot (which
// passes -db <production db> and no --log-file) is resolved by the same
// production pair these tests assert against. If someone changes either
// default in main.go or the canonical paths above, this fails until the
// pair is reconciled.
func TestFlagDefaultsMatchProductionPair(t *testing.T) {
	t.Setenv("HOME", "/home/fakehome")

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	dbRe := regexp.MustCompile(`flag\.String\("db", os\.ExpandEnv\("(\$HOME/[^"]+)"\)`)
	m := dbRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no --db os.ExpandEnv default found in main.go")
	}
	if got, want := os.ExpandEnv(m[1]), defaultDBPath(); got != want {
		t.Fatalf("main.go --db default %q expands to %q, want the canonical %q", m[1], got, want)
	}

	logRe := regexp.MustCompile(`flag\.String\("log-file", defaultLogPath\(\)`)
	if !logRe.MatchString(body) {
		t.Fatalf("--log-file default no longer uses defaultLogPath() — the derived-vs-flag resolution in main() no longer matches the source default; reconcile with resolveLogFile")
	}
}
