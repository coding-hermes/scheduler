// Package target classifies a lane's workdir.
//
// A lane workdir is not always a local directory. With remote nodes it can be
// addressed the way people address remote paths:
//
//	/home/agent/repo                      local
//	user@host:/srv/repo                   ssh/scp style
//	ssh://user@host:2222/srv/repo         URL style
//	sftp://user@host/srv/repo             sftp
//	bunker://<agent>/home/bunker-<id>/repo bunker agent + path inside it
//
// The scheduler's board reading, ownership check and dispatch all take a
// workdir and then hit the filesystem. They must therefore resolve a target to
// a LOCAL path first, and say so plainly when they cannot: a workdir that is
// remote but not mounted is "not resolvable here", which is different from
// "no board" and must never be silently treated as an empty board.
package target

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is the class of a lane target.
type Kind string

const (
	KindLocal  Kind = "local"
	KindBunker Kind = "bunker"
	KindSSH    Kind = "ssh"
	KindSFTP   Kind = "sftp"
	KindFTP    Kind = "ftp"
)

// DefaultMountRoot is where bunker agent homes are mounted:
// /mnt/bunker/<agent> -> /home/bunker-<agent>. Overridable for tests and for a
// host that mounts elsewhere.
const DefaultMountRoot = "/mnt/bunker"

// Target is a parsed lane workdir.
type Target struct {
	Raw    string
	Kind   Kind
	Scheme string // local, ssh, sftp, ftp, bunker
	Path   string // absolute path on the owning host/agent
	User   string
	Host   string
	Port   string
	Agent  string // bunker agent id
}

// Remote reports whether the target lives on another host.
func (t Target) Remote() bool { return t.Kind != KindLocal }

var (
	scpRe   = regexp.MustCompile(`^(?P<user>[^@/\s]+)@(?P<host>[^:/\s]+):(?P<path>.+)$`)
	localRe = regexp.MustCompile(`^(?:local|file)://(?P<path>.*)$`)
)

// Parse classifies raw. It never touches the filesystem.
func Parse(raw string) (Target, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Target{}, fmt.Errorf("target is empty")
	}
	if strings.ContainsAny(text, "\n\r\x00") {
		return Target{}, fmt.Errorf("target contains a control character")
	}

	if strings.Contains(text, "://") {
		scheme, rest, _ := strings.Cut(text, "://")
		scheme = strings.ToLower(scheme)
		switch scheme {
		case "local", "file":
			p := rest
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			return Target{Raw: raw, Kind: KindLocal, Scheme: "local", Path: filepath.Clean(p)}, nil
		case "bunker":
			agent, path, ok := strings.Cut(rest, "/")
			if !ok || agent == "" || path == "" {
				return Target{}, fmt.Errorf("bunker target needs bunker://<agent>/<path>: %q", raw)
			}
			return Target{
				Raw: raw, Kind: KindBunker, Scheme: "bunker", Agent: agent,
				Path: filepath.Clean("/" + path),
			}, nil
		case "ssh", "sftp", "ftp", "ftps", "rsync":
			creds, path, ok := strings.Cut(rest, "/")
			if !ok || path == "" {
				return Target{}, fmt.Errorf("%s target needs %s://[user@]host[:port]/<path>: %q", scheme, scheme, raw)
			}
			t := Target{Raw: raw, Scheme: scheme, Path: filepath.Clean("/" + path)}
			if scheme == "sftp" {
				t.Kind = KindSFTP
			} else if scheme == "ftp" || scheme == "ftps" {
				t.Kind = KindFTP
			} else {
				t.Kind = KindSSH
			}
			if user, hostPort, hasUser := strings.Cut(creds, "@"); hasUser {
				t.User, creds = user, hostPort
			}
			if host, port, hasPort := strings.Cut(creds, ":"); hasPort {
				t.Host, t.Port = host, port
			} else {
				t.Host = creds
			}
			if t.Host == "" {
				return Target{}, fmt.Errorf("%s target has no host: %q", scheme, raw)
			}
			return t, nil
		default:
			return Target{}, fmt.Errorf("unsupported scheme %q in %q", scheme, raw)
		}
	}

	if m := localRe.FindStringSubmatch(text); m != nil {
		p := m[1]
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return Target{Raw: raw, Kind: KindLocal, Scheme: "local", Path: filepath.Clean(p)}, nil
	}

	if m := scpRe.FindStringSubmatch(text); m != nil {
		user, host, path := m[1], m[2], m[3]
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		return Target{
			Raw: raw, Kind: KindSSH, Scheme: "ssh", User: user, Host: host,
			Path: filepath.Clean(path),
		}, nil
	}

	if strings.HasPrefix(text, "local:") {
		return Target{Raw: raw, Kind: KindLocal, Scheme: "local", Path: filepath.Clean(text[len("local:"):])}, nil
	}
	if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "~") {
		return Target{Raw: raw, Kind: KindLocal, Scheme: "local", Path: filepath.Clean(text)}, nil
	}

	return Target{}, fmt.Errorf(
		"cannot classify target %q: use an absolute path, user@host:/path, <scheme>://host/path, or bunker://<agent>/path",
		raw)
}

// MountPath returns the local path a bunker agent's directory is mounted at:
// <mountRoot>/<agent>. The in-agent path is NOT appended — the mount root IS
// the agent's home, so filepath.Join(mount, rel) where rel is relative to home.
func (t Target) MountPath(mountRoot string) (string, bool) {
	if t.Kind != KindBunker || t.Agent == "" {
		return "", false
	}
	if mountRoot == "" {
		mountRoot = DefaultMountRoot
	}
	return filepath.Join(mountRoot, t.Agent), true
}

// ResolveLocal maps a target to a local filesystem path when one exists:
//
//	local   -> its own path (returned whether or not it exists; callers stat it)
//	bunker  -> <mountRoot>/<agent><path-inside-agent>, only when that directory exists
//	ssh/sftp/ftp -> never (no local mount is assumed)
//
// The bool is "resolved", not "exists": for local targets the caller keeps its
// existing stat/board logic, for bunker targets resolution is only true when
// the mount actually carries the directory, so a stale agent id degrades to
// "not resolvable here" instead of a phantom board path.
func ResolveLocal(raw, mountRoot string) (string, bool) {
	t, err := Parse(raw)
	if err != nil {
		return "", false
	}
	switch t.Kind {
	case KindLocal:
		return t.Path, true
	case KindBunker:
		mount, ok := t.MountPath(mountRoot)
		if !ok {
			return "", false
		}
		candidate := filepath.Join(mount, strings.TrimPrefix(t.Path, "/"))
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, true
		}
		return "", false
	default:
		return "", false
	}
}

// Describe renders a target for logs: "bunker://d274d7cc/home/... " etc.
func (t Target) Describe() string {
	switch t.Kind {
	case KindLocal:
		return t.Path
	case KindBunker:
		return "bunker://" + t.Agent + t.Path
	default:
		cred := ""
		if t.User != "" {
			cred = t.User + "@"
		}
		port := ""
		if t.Port != "" {
			port = ":" + t.Port
		}
		return t.Scheme + "://" + cred + t.Host + port + t.Path
	}
}
