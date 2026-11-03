package target

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseClasses(t *testing.T) {
	cases := []struct {
		raw    string
		kind   Kind
		scheme string
		path   string
		agent  string
		host   string
		user   string
		port   string
	}{
		{"/home/kara/code-factory", KindLocal, "local", "/home/kara/code-factory", "", "", "", ""},
		{"local:/srv/cf", KindLocal, "local", "/srv/cf", "", "", "", ""},
		{"file:///srv/cf", KindLocal, "local", "/srv/cf", "", "", "", ""},
		{"bunker://d274d7cc/home/bunker-d274d7cc/repo", KindBunker, "bunker", "/home/bunker-d274d7cc/repo", "d274d7cc", "", "", ""},
		{"builder@node-01:/srv/cf", KindSSH, "ssh", "/srv/cf", "", "node-01", "builder", ""},
		{"ssh://builder@node-01:2222/srv/cf", KindSSH, "ssh", "/srv/cf", "", "node-01", "builder", "2222"},
		{"sftp://deploy@mirror/srv/cf", KindSFTP, "sftp", "/srv/cf", "", "mirror", "deploy", ""},
		{"ftp://deploy@mirror.example/pub/cf", KindFTP, "ftp", "/pub/cf", "", "mirror.example", "deploy", ""},
	}
	for _, c := range cases {
		got, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.raw, err)
		}
		if got.Kind != c.kind || got.Scheme != c.scheme || got.Path != c.path ||
			got.Agent != c.agent || got.Host != c.host || got.User != c.user || got.Port != c.port {
			t.Errorf("Parse(%q) = %+v, want kind=%s scheme=%s path=%s agent=%s host=%s user=%s port=%s",
				c.raw, got, c.kind, c.scheme, c.path, c.agent, c.host, c.user, c.port)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "work/cf", "bunker://agent", "bunker:///path",
		"ssh://host", "gopher://host/path", "/srv/cf\nrm -rf /",
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) should have failed", raw)
		}
	}
}

func TestResolveLocal(t *testing.T) {
	root := t.TempDir()
	agent := "abc"
	agentHome := filepath.Join(root, agent)
	repo := filepath.Join(agentHome, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := ResolveLocal("bunker://abc/home/bunker-abc/repo", root)
	if !ok || got != repo {
		t.Fatalf("bunker resolve (home-absolute form) = %q,%v want %q,true", got, ok, repo)
	}

	// The mount maps the agent's HOME, so a path written relative to the home
	// must resolve to the same place.
	got, ok = ResolveLocal("bunker://abc/repo", root)
	if !ok || got != repo {
		t.Fatalf("bunker resolve (home-relative form) = %q,%v want %q,true", got, ok, repo)
	}

	// A path the mount does not carry must NOT resolve: a phantom board path is
	// worse than an honest "not here".
	if _, ok := ResolveLocal("bunker://abc/absent", root); ok {
		t.Fatal("resolve of a missing path should be false")
	}

	// A stale agent id must not resolve either.
	if _, ok := ResolveLocal("bunker://ghost/repo", root); ok {
		t.Fatal("resolve of an unknown agent should be false")
	}

	// Local targets pass through regardless of existence: callers keep their own
	// stat semantics.
	if got, ok := ResolveLocal("/srv/cf", root); !ok || got != "/srv/cf" {
		t.Fatalf("local resolve = %q,%v", got, ok)
	}

	// Remote-without-mount never resolves locally.
	if _, ok := ResolveLocal("builder@node-01:/srv/cf", root); ok {
		t.Fatal("ssh target should not resolve locally")
	}
}

func TestDescribeRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"/srv/cf",
		"bunker://abc/home/bunker-abc/repo",
		"ssh://builder@node-01:2222/srv/cf",
	} {
		got, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got.Describe() != raw {
			t.Errorf("Describe(%q) = %q", raw, got.Describe())
		}
	}
}
