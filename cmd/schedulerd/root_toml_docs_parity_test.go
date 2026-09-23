package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredRootTomlClaims are substrings that assert the root `schedulerd.toml`
// [scheduler] layer is NOT wired into daemon boot. FEAT-005 landed: main.go
// applies that layer at boot (four config.LoadRootConfig call sites) under the
// default-guard pattern (TOML wins only while the corresponding flag sits at its
// default, so CLI > env > TOML). Any doc surface still carrying one of these
// phrases is telling operators the opposite of what the daemon does.
var retiredRootTomlClaims = []string{
	"NOT wired into daemon boot until FEAT-005",
	"exists but NOT wired",
	"NOT loaded from a root `schedulerd.toml`",
	"NOT loaded from a root schedulerd.toml",
	"root TOML wiring arrives in FEAT-005",
	"root TOML loading comes in FEAT-005",
}

// TestRootTomlDocsParity fails the build when a documentation surface
// (README.md, the docs/reference/ reference pages, or any file under docs/)
// re-introduces the retired claim that the root schedulerd.toml [scheduler]
// layer is not loaded at boot.
//
// The guard is written against a CLAIM, not a symbol name: it greps the retired
// phrasings so a rewording that preserves the false statement still has to pass
// through this list deliberately. Sibling precedent for the shape:
// internal/clock/stdlib_guard_test.go.
//
// SCHED-GAP-165's acceptance criterion is that the doc surfaces state the LIVE
// resolution order; this test is what keeps them honest after the row closes.
func TestRootTomlDocsParity(t *testing.T) {
	root := repoRoot(t)

	// The pages the route/flag tables moved to (SCHED-GAP-220) replace AGENTS.md
	// here: they are doc surfaces now, so the retired-claim scan follows the
	// content.
	surfaces := []string{"README.md", "docs/reference/flags.md", "docs/reference/endpoints.md", "docs/reference/clock-modes.md"}
	docsDir := filepath.Join(root, "docs")
	if entries, err := os.ReadDir(docsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			surfaces = append(surfaces, filepath.Join("docs", e.Name()))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("cannot read docs dir: %v", err)
	}

	checked := 0
	for _, rel := range surfaces {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("cannot read %s: %v", rel, err)
		}
		checked++
		text := string(body)
		for _, claim := range retiredRootTomlClaims {
			if strings.Contains(text, claim) {
				idx := strings.Index(text, claim)
				line := 1 + strings.Count(text[:idx], "\n")
				t.Errorf("%s:%d re-introduces the retired root-TOML claim %q — "+
					"FEAT-005 landed and main.go calls config.LoadRootConfig at boot; "+
					"state the live resolution order (TOML [scheduler] < env < flags) instead",
					rel, line, claim)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no documentation surface was readable — the guard checked nothing " +
			"(a parity test that scans zero files is a phantom pass)")
	}
}

// repoRoot walks up from the test's working directory (cmd/schedulerd) until it
// finds go.mod, so the guard works regardless of which directory `go test` ran in.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repoRoot: no go.mod found above the test working directory")
		}
		dir = parent
	}
}
