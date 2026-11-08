package lanehook

import "strings"

// SCHED-GAP-16987: the armed repo-fetch hook runs `git -C {repo} fetch` and
// {repo} used to expand to the lane's repo_url verbatim. On lanes whose
// repo_url is a remote URL (https://github.com/…) the hook therefore exec'd
// `git -C https://github.com/…`, which dies with "fatal: cannot change to
// …" — recorded as state=failed while nothing was ever fetched. The local
// path the scheduler already knows for such a lane is its workdir, so {repo}
// resolves to the workdir when the declared repo is remote-shaped; when no
// local path exists at all the runner refuses to exec and records a named
// skip (state=skipped reason=repo-not-local) instead of state=failed.

// RepoNotLocalReason is the named skip reason recorded when a {repo} step has
// no local path to run against.
const RepoNotLocalReason = "repo-not-local"

// isRemoteRepoShape reports whether s has the shape of a remote repository
// URL rather than a path on this machine: a scheme and authority
// (`https://`, `ssh://`, `git://`, also `file://` — git accepts it as a URL
// but `git -C` cannot change to it) or the scp-like syntax
// (`git@host:path`).
func isRemoteRepoShape(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	// scp-like: user@host:path — no slash before the '@' (that would make it
	// a path segment) and no slash between the '@' and the first ':' (the
	// host part), with something after the colon.
	if at := strings.IndexByte(s, '@'); at > 0 && !strings.Contains(s[:at], "/") {
		rest := s[at+1:]
		if colon := strings.IndexByte(rest, ':'); colon > 0 && !strings.Contains(rest[:colon], "/") {
			return true
		}
	}
	return false
}

// isLocalRepoPath reports whether s can serve as the directory argument of
// `git -C`: non-empty and not remote-shaped.
func isLocalRepoPath(s string) bool {
	return s != "" && !isRemoteRepoShape(s)
}

// hasLocalRepoPath reports whether the env carries any usable local repo
// path: the declared repo itself or the lane's workdir.
func (env Env) hasLocalRepoPath() bool {
	return isLocalRepoPath(env.Repo) || isLocalRepoPath(env.Workdir)
}

// RepoArg returns the value the {repo} placeholder expands to for this env.
// A local repo path passes through unchanged; a remote repo_url falls back
// to the lane's workdir (its local checkout); with no local path available
// the raw value is returned and the runner records the repo-not-local skip
// instead of exec'ing a URL.
func (env Env) RepoArg() string {
	if isLocalRepoPath(env.Repo) {
		return env.Repo
	}
	if isLocalRepoPath(env.Workdir) {
		return env.Workdir
	}
	return env.Repo
}

// consumesRepoArg reports whether a step's arguments use the {repo}
// placeholder. Only such steps are subject to the repo-not-local skip; a
// step built on {workdir} alone always has a local argument available and
// runs regardless of what repo_url looks like.
func consumesRepoArg(step Step) bool {
	for _, a := range step.Args {
		if strings.Contains(a, "{repo}") {
			return true
		}
	}
	return false
}
