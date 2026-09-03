// Package version is the single source of truth for the scheduler's build
// identity (KB-GAP-040 / KM-GAP-042 / KM-GAP-049 class fix).
//
// Resolution precedence:
//  1. ldflags-injected value — release workflows pass
//     -X github.com/coding-hermes/scheduler/internal/version.Version=<tag>
//  2. Go's native vcs buildinfo — every `go build` from a git checkout embeds
//     vcs.revision / vcs.time regardless of ldflags, so bare builds report at
//     least dev-<shorthash> instead of a silent stale string
//  3. "dev" / "unknown" fallbacks
//
// The fatal bug this replaces: release workflows injected -X main.Version
// while main declared no such variable — a silent no-op, so every released
// binary served a hardcoded stale "1.0.0" from internal/api and internal/mcp.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the linker-injected version string (empty/dev in builds that
// bypass ldflags). Injected value is authoritative when present.
var Version = "dev"

// Commit and BuildDate are linker-injected build metadata.
var (
	Commit    = "unknown"
	BuildDate = "unknown"
)

// readBuildInfo is a package-level seam so tests can inject a fake.
var readBuildInfo = debug.ReadBuildInfo

// vcsSetting returns the value for a build-info setting key, or "".
func vcsSetting(key string) string {
	info, ok := readBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// Current returns the version to report. Injected ldflags value wins; the
// vcs-stamped module pseudo-version is next; plain "dev" is last.
func Current() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if rev := vcsSetting("vcs.revision"); rev != "" {
		// Mirrors the shape of `git describe --always` output closely
		// enough to match a binary to its commit: dev-<8 char short hash>.
		short := rev
		if len(short) > 8 {
			short = short[:8]
		}
		return "dev-" + strings.ToLower(short)
	}
	return "dev"
}

// CurrentCommit returns the commit identity: injected value wins, then the
// vcs-stamped revision (short form), then "unknown".
func CurrentCommit() string {
	if Commit != "" && Commit != "unknown" {
		return Commit
	}
	if rev := vcsSetting("vcs.revision"); rev != "" {
		short := rev
		if len(short) > 8 {
			short = short[:8]
		}
		return strings.ToLower(short)
	}
	return "unknown"
}

// CurrentBuildDate returns the build timestamp: injected value wins, then the
// vcs-stamped commit time (RFC3339 UTC), then "unknown".
func CurrentBuildDate() string {
	if BuildDate != "" && BuildDate != "unknown" {
		return BuildDate
	}
	if t := vcsSetting("vcs.time"); t != "" {
		return t
	}
	return "unknown"
}
