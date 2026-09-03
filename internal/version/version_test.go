package version

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// fakeBI installs a fake debug.BuildInfo and returns a restore func.
func fakeBI(t *testing.T, rev, commitTime string) func() {
	t.Helper()
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		settings := []debug.BuildSetting{}
		if rev != "" {
			settings = append(settings, debug.BuildSetting{Key: "vcs.revision", Value: rev})
		}
		if commitTime != "" {
			settings = append(settings, debug.BuildSetting{Key: "vcs.time", Value: commitTime})
		}
		return &debug.BuildInfo{Settings: settings}, true
	}
	return func() { readBuildInfo = old }
}

func TestResolveInjectedLdflagsWins(t *testing.T) {
	defer fakeBI(t, "deadbeef00c0ffee1234567890abcdef12345678", "2026-01-01T00:00:00Z")()
	oldV, oldC, oldB := Version, Commit, BuildDate
	Version, Commit, BuildDate = "v1.2.3", "abc12345", "2026-02-02T00:00:00Z"
	defer func() { Version, Commit, BuildDate = oldV, oldC, oldB }()

	if got := Current(); got != "v1.2.3" {
		t.Errorf("Current() = %q, want injected v1.2.3", got)
	}
	if got := CurrentCommit(); got != "abc12345" {
		t.Errorf("CurrentCommit() = %q, want injected abc12345", got)
	}
	if got := CurrentBuildDate(); got != "2026-02-02T00:00:00Z" {
		t.Errorf("CurrentBuildDate() = %q, want injected value", got)
	}
}

func TestResolveVCSFallback(t *testing.T) {
	// Simulate bare `go build` (no ldflags): zero-value vars, vcs stamped.
	oldV, oldC, oldB := Version, Commit, BuildDate
	Version, Commit, BuildDate = "dev", "unknown", "unknown"
	defer func() { Version, Commit, BuildDate = oldV, oldC, oldB }()
	defer fakeBI(t, "ABCDEF12ab34ef5678901234567890abcdef1234", "2026-08-22T05:51:12Z")()

	// 8-char lowercase short hash — matches `git rev-parse --short` shape.
	if got := Current(); got != "dev-abcdef12" {
		t.Errorf("Current() = %q, want dev-abcdef12", got)
	}
	if got := CurrentCommit(); got != "abcdef12" {
		t.Errorf("CurrentCommit() = %q, want abcdef12", got)
	}
	if got := CurrentBuildDate(); got != "2026-08-22T05:51:12Z" {
		t.Errorf("CurrentBuildDate() = %q, want vcs.time", got)
	}
}

func TestResolveNoBuildinfoAtAll(t *testing.T) {
	// Neither ldflags nor buildinfo (e.g. -buildvcs=false): honest fallbacks.
	oldV, oldC, oldB := Version, Commit, BuildDate
	Version, Commit, BuildDate = "dev", "unknown", "unknown"
	defer func() { Version, Commit, BuildDate = oldV, oldC, oldB }()
	defer fakeBI(t, "", "")()

	if got := Current(); got != "dev" {
		t.Errorf("Current() = %q, want dev", got)
	}
	if got := CurrentCommit(); got != "unknown" {
		t.Errorf("CurrentCommit() = %q, want unknown", got)
	}
	if got := CurrentBuildDate(); got != "unknown" {
		t.Errorf("CurrentBuildDate() = %q, want unknown", got)
	}
}

// TestInjectedVersionMatchesGitTag proves an ldflags-built test binary reports
// the release tag — the exact shape the release workflow relies on.
func TestInjectedVersionMatchesGitTag(t *testing.T) {
	if os.Getenv("SCHEDULER_VERSION_TEST_TAG") == "" {
		t.Skip("set SCHEDULER_VERSION_TEST_TAG=<tag> to run")
	}
	tag := os.Getenv("SCHEDULER_VERSION_TEST_TAG")
	if Current() != tag {
		t.Errorf("Current() = %q, want release tag %q", Current(), tag)
	}
	if !strings.HasPrefix(filepath.Base(os.Args[0]), "schedulerd.test") &&
		!strings.HasSuffix(os.Args[0], ".test") {
		t.Logf("note: running in binary %s", os.Args[0])
	}
}
