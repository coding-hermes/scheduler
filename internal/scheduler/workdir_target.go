package scheduler

import (
	"log"
	"os"
	"sync"

	"github.com/coding-hermes/scheduler/internal/target"
)

// A lane workdir may be a remote target (bunker://<agent>/<path>, user@host:/path,
// ssh://…, sftp://…). Everything the scheduler does with a workdir — locate the
// board, decide ownership, read pending counts — is filesystem work, so every
// such site must resolve the target to a LOCAL path first and treat "cannot
// resolve here" as its own condition.
//
// The distinction matters: an unmounted remote workdir that is mistaken for an
// empty board silently defers a lane forever (tasks_no_work), which looks like a
// healthy idle fleet. resolveWorkdir therefore returns ok=false and the caller
// keeps its existing fail-closed behaviour, while resolveWorkdirReason records
// why so the refusal is attributable instead of mysterious.

// bunkerMountRoot is where agent homes are mounted (/mnt/bunker/<agent>).
// Overridable so a host that mounts elsewhere — or a test — can point it.
func bunkerMountRoot() string {
	if v := os.Getenv("BUNKER_MOUNT_ROOT"); v != "" {
		return v
	}
	return target.DefaultMountRoot
}

// workdirTargetEnv is the environment variable that overrides the bunker mount
// root, re-exported for the config/status surfaces that document it.
const workdirTargetEnv = "BUNKER_MOUNT_ROOT"

// resolveWorkdir maps a lane workdir to a local path. Local targets pass
// through unchanged (the caller still stats them); bunker targets resolve
// through the agent mount only when the path really exists there.
func resolveWorkdir(workdir string) (string, bool) {
	return target.ResolveLocal(workdir, bunkerMountRoot())
}

var (
	unresolvedWorkdirMu   sync.Mutex
	unresolvedWorkdirSeen = map[string]string{}
)

// noteUnresolvedWorkdir logs one line per workdir per distinct reason, so a
// lane whose remote node is not mounted is visible instead of silently idle.
func noteUnresolvedWorkdir(workdir, reason string) {
	if workdir == "" {
		return
	}
	unresolvedWorkdirMu.Lock()
	prev, seen := unresolvedWorkdirSeen[workdir]
	if seen && prev == reason {
		unresolvedWorkdirMu.Unlock()
		return
	}
	unresolvedWorkdirSeen[workdir] = reason
	unresolvedWorkdirMu.Unlock()
	log.Printf("WORKDIR-TARGET: %q is not resolvable on this host (%s); mount root %s — lane will be treated as having no board here",
		workdir, reason, bunkerMountRoot())
}
