package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A remote workdir names its own node and path, so the local naming convention
// must not veto it (CF-053 / "the scheduler must understand the bunker path").
func TestValidateLaneWorkdirAcceptsRemoteTargets(t *testing.T) {
	remote := []string{
		"bunker://cf-node/home/bunker-cf-node/repo",
		"builder@node-01:/srv/code-factory",
		"ssh://builder@node-01:2222/srv/code-factory",
		"sftp://deploy@mirror/srv/code-factory",
	}
	for _, wd := range remote {
		if err := validateLaneWorkdir("code-factory-sync", wd); err != nil {
			t.Errorf("remote workdir %q refused: %v", wd, err)
		}
	}
}

func TestValidateLaneWorkdirStillEnforcesLocalConvention(t *testing.T) {
	// A local path outside the lane's convention is still refused: the relaxation
	// is for targets that name a node, not a licence to point a satellite at $HOME.
	err := validateLaneWorkdir("code-factory-sync", "/home/kara")
	if err == nil {
		t.Fatal("off-convention local workdir should still be refused")
	}
	if !strings.Contains(err.Error(), "off-convention") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A satellite may live in the project's own checkout: docs, qa and pm lanes
// belong in the repository they read.
func TestValidateLaneWorkdirAcceptsProjectCheckout(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "code-factory")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{
		"code-factory", "code-factory-docs", "code-factory-duckbrain-pm", "code-factory-sync",
	} {
		if err := validateLaneWorkdir(lane, repo); err != nil {
			t.Errorf("lane %q in its project checkout refused: %v", lane, err)
		}
	}

	// A different project's checkout is not this lane's home.
	if err := validateLaneWorkdir("trouble-docs", repo); err == nil {
		t.Error("a lane must not adopt another project's checkout")
	}
	// A subdirectory of a checkout is not a checkout root.
	sub := filepath.Join(repo, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateLaneWorkdir("code-factory-docs", sub); err == nil {
		t.Error("a subdirectory of a checkout must not pass as the project checkout")
	}
	// A plain directory named like the project is not a checkout.
	plain := filepath.Join(root, "code-factory-plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateLaneWorkdir("code-factory-plain-docs", plain); err == nil {
		t.Error("a directory with no .git must not pass as the project checkout")
	}
}
