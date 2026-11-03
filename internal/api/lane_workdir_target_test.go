package api

import (
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
