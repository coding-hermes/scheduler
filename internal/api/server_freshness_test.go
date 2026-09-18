package api_test

// SCHED-GAP-148 — daemon-freshness guard.
//
// The defect this pins: an operator could read /api/v1/health and see a
// version string ("v1.3.0-44-g8afac21-dirty" at 3h15m uptime) but could not
// correlate that string to a commit on main, so "did this restart actually
// pick up the fix?" was answered by diffing binary timestamps by hand. That
// failed twice — a 23:51 restart that loaded the pre-fix build while the fix
// landed at 23:59 (2026-09-17), and b57c80f (SCHED-GAP-155 ADMIT log +
// admission counters) being pushed while the live daemon kept serving a
// pre-fix dirty build hours later.
//
// Three surfaces are pinned here, in one place:
//
//	1. /api/v1/status (and its sibling /api/v1/health) carry version +
//	   build_sha + build_time sourced from internal/version. Proven by
//	   driving the ldflags seam (the exact path the release workflow uses,
//	   release.yaml -X ...version.Commit=...) and reading the values back
//	   off the wire — a hardcoded literal cannot satisfy it.
//	2. A REAL `go build` of cmd/schedulerd reports a stamped commit +
//	   build date. The freshness guard's input is only meaningful if the
//	   binary carries a sha, so that claim is proven on the built artifact,
//	   not asserted from inside the test process.
//	3. ops/check-daemon-freshness.sh's exit-code contract — 0 FRESH /
//	   1 STALE with both shas / 2 UNKNOWN — against a scratch git repo and
//	   a fake status endpoint (equal, descendant, ancestor, diverged,
//	   unknown, missing-key, unreachable-daemon cases).
//
// Measured evidence, not assumption: `go test` binaries are NOT
// vcs-stamped. A probe module in a git repo printed
// `vcs.revision=04ef24ae...` + `vcs.time=2026-09-18T08:19:09Z` from
// `go build` of its main package and NO vcs.* settings at all from the test
// binary of the same package. So `build_sha` is legitimately "unknown" in
// this test process, and an assertion demanding non-"unknown" HERE would be
// wrong. The non-"unknown" guarantee is asserted where it actually holds:
// on the built daemon (test 2) and on the live endpoint (checked by the ops
// script). The in-process test asserts non-empty + backed-by-the-seam.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/coding-hermes/scheduler/internal/version"
)

// injectBuildIdentity drives the ldflags seam the release workflow uses and
// returns a restore func. version.Commit is returned verbatim by
// CurrentCommit (lowercasing only applies to the vcs fallback), so the test
// can assert an exact sha-shaped value.
func injectBuildIdentity(t *testing.T, ver, commit, built string) func() {
	t.Helper()
	prevVer, prevCommit, prevBuilt := version.Version, version.Commit, version.BuildDate
	version.Version, version.Commit, version.BuildDate = ver, commit, built
	return func() { version.Version, version.Commit, version.BuildDate = prevVer, prevCommit, prevBuilt }
}

// TestStatusIncludesBuildIdentity pins acceptance (1)/(4): the status and
// health endpoints expose version + build_sha + build_time as non-empty
// strings backed by internal/version.Current / CurrentCommit /
// CurrentBuildDate.
func TestStatusIncludesBuildIdentity(t *testing.T) {
	a := newAPITestServer(t)

	const (
		wantVer   = "v9.9.9-schedgap148"
		wantSha   = "1ed5d31a"
		wantBuilt = "2026-09-18T07:56:43Z"
	)
	restore := injectBuildIdentity(t, wantVer, wantSha, wantBuilt)
	defer restore()

	for _, path := range []string{"/api/v1/status", "/api/v1/health"} {
		code, body := a.do(t, "GET", path, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
		for key, want := range map[string]string{
			"version":    wantVer,
			"build_sha":  wantSha,
			"build_time": wantBuilt,
		} {
			raw, present := body[key]
			if !present {
				t.Errorf("%s: key %q missing from response", path, key)
				continue
			}
			got, isStr := raw.(string)
			if !isStr {
				t.Errorf("%s: %s is %T, want string", path, key, raw)
				continue
			}
			if strings.TrimSpace(got) == "" {
				t.Errorf("%s: %s is empty", path, key)
			}
			if got != want {
				t.Errorf("%s: %s = %q, want %q — the endpoint must read the internal/version seam, not a literal",
					path, key, got, want)
			}
		}
	}

	// RED-mutation guard (in-process half): with the injected identity
	// cleared, the endpoints must report the fallbacks, NOT the values
	// above. A handler that hardcoded the sha (or cached it at package
	// init) keeps reporting wantSha and fails here.
	version.Version, version.Commit, version.BuildDate = "dev", "", ""
	for _, path := range []string{"/api/v1/status", "/api/v1/health"} {
		code, body := a.do(t, "GET", path, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
		if got, _ := body["build_sha"].(string); got == wantSha {
			t.Errorf("%s: build_sha still %q after clearing the seam — the value is hardcoded/cached, not resolved per request",
				path, got)
		}
		// Non-empty is the floor in every build: "dev"/"unknown" are the
		// documented fallbacks, never an omitted field.
		if got, _ := body["build_sha"].(string); got == "" {
			t.Errorf("%s: build_sha empty with the seam cleared — want a fallback string (unknown/dev)", path)
		}
		if got, _ := body["build_time"].(string); got == "" {
			t.Errorf("%s: build_time empty with the seam cleared — want a fallback string (unknown)", path)
		}
		if got, _ := body["version"].(string); got != version.Current() {
			t.Errorf("%s: version = %q, want %q (version.Current())", path, got, version.Current())
		}
	}
}

// TestBuiltDaemonReportsStampedBuildIdentity proves the guard's input exists
// on the artifact operators actually run: a plain `go build` of
// cmd/schedulerd (no ldflags — the path this host's bin/schedulerd takes)
// stamps vcs.revision + vcs.time, so --version prints a real sha and an
// RFC3339 build time rather than "unknown".
func TestBuiltDaemonReportsStampedBuildIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("builds ./cmd/schedulerd — skipped in -short mode")
	}
	bin := filepath.Join(t.TempDir(), "schedulerd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/schedulerd")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/schedulerd: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	// schedulerd <ver> (commit: <sha>, built: <rfc3339>)
	re := regexp.MustCompile(`^schedulerd (\S+) \(commit: ([0-9a-f]{8,40}), built: (\d{4}-\d\d-\d\dT[^)]+)\)\s*$`)
	m := re.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		t.Fatalf("--version output %q does not carry a stamped build identity (want "+
			"`schedulerd <ver> (commit: <8-40 hex>, built: <rfc3339>)`)", strings.TrimSpace(string(out)))
	}
	if m[2] == "unknown" || m[2] == "" {
		t.Errorf("stamped commit = %q, want a vcs revision", m[2])
	}
	if m[3] == "unknown" || m[3] == "" {
		t.Errorf("stamped build time = %q, want an RFC3339 vcs.time", m[3])
	}
	t.Logf("built daemon reports version=%s commit=%s built=%s", m[1], m[2], m[3])
}

// fakeStatus serves a mutable scheduler-status body to the ops script.
type fakeStatus struct {
	mu   sync.Mutex
	body string
}

func (f *fakeStatus) set(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

func (f *fakeStatus) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		body := f.body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

// TestFreshnessScript_ExitCodes pins acceptance (3): the guard's exit-code
// contract against a scratch git repo where the ancestor/descendant/diverged
// relationships are all known, driven end-to-end through bash + curl + jq.
func TestFreshnessScript_ExitCodes(t *testing.T) {
	for _, tool := range []string{"git", "bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "ops", "check-daemon-freshness.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("ops/check-daemon-freshness.sh missing: %v", err)
	}

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", ".")
	// r1 — first commit on the admission surface.
	r1 := commitFile(t, repo, "internal/scheduler/alpha.go", "package scheduler\n")
	// r2 — docs only: does NOT move the admission reference.
	r2 := commitFile(t, repo, "docs/notes.md", "notes\n")
	// r3 — newest admission-touching commit reachable from HEAD.
	r3 := commitFile(t, repo, "internal/scheduler/beta.go", "package scheduler\n")
	// r4 — docs-only descendant of r3.
	r4 := commitFile(t, repo, "docs/notes2.md", "notes2\n")
	// r5 — sibling of r3: built from r1, so r3 and r5 have diverged.
	branch := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	gitRun(t, repo, "checkout", "-q", "-b", "side", r1)
	r5 := commitFile(t, repo, "internal/scheduler/side.go", "package scheduler\n")
	gitRun(t, repo, "checkout", "-q", branch)

	sh := func(sha string) string { return gitRun(t, repo, "rev-parse", "--short=8", sha) }

	status := &fakeStatus{}
	ts := httptest.NewServer(status.handler())
	defer ts.Close()

	cases := []struct {
		name      string
		liveBody  string
		against   string
		statusURL string
		wantCode  int
		wantOut   string // exact stdout (verdict line) or a prefix when annotated
		prefix    bool
	}{
		{
			name:     "live equals reference",
			liveBody: statusBody(sh(r3)),
			against:  "HEAD",
			wantCode: 0,
			wantOut:  fmt.Sprintf("FRESH: live=%s reference=%s", sh(r3), sh(r3)),
		},
		{
			name:     "live is a descendant of reference",
			liveBody: statusBody(sh(r4)),
			against:  "HEAD",
			wantCode: 0,
			wantOut:  fmt.Sprintf("FRESH: live=%s reference=%s", sh(r4), sh(r3)),
		},
		{
			name:     "live is an ancestor of reference (stale daemon)",
			liveBody: statusBody(sh(r2)),
			against:  "HEAD",
			wantCode: 1,
			wantOut:  fmt.Sprintf("STALE: live=%s reference=%s", sh(r2), sh(r3)),
		},
		{
			name:     "live and reference diverged",
			liveBody: statusBody(sh(r5)),
			against:  "HEAD",
			wantCode: 1,
			wantOut:  fmt.Sprintf("STALE: live=%s reference=%s", sh(r5), sh(r3)),
		},
		{
			name:     "explicit --against REV resolves that revision's surface",
			liveBody: statusBody(sh(r1)),
			against:  sh(r1),
			wantCode: 0,
			wantOut:  fmt.Sprintf("FRESH: live=%s reference=%s", sh(r1), sh(r1)),
		},
		{
			name:     "daemon reports unknown build_sha",
			liveBody: statusBody("unknown"),
			against:  "HEAD",
			wantCode: 2,
			wantOut:  "UNKNOWN:",
			prefix:   true,
		},
		{
			name:     "endpoint predates build identity (no build_sha key)",
			liveBody: `{"version":"v1.3.0-44-g8afac21-dirty","status":"ok"}`,
			against:  "HEAD",
			wantCode: 2,
			wantOut:  "UNKNOWN:",
			prefix:   true,
		},
		{
			name:      "daemon unreachable",
			liveBody:  statusBody(sh(r3)),
			against:   "HEAD",
			statusURL: "http://127.0.0.1:1/api/v1/status",
			wantCode:  2,
			wantOut:   "UNKNOWN:",
			prefix:    true,
		},
		{
			name:     "reported sha is not a commit in this checkout",
			liveBody: statusBody("deadbeef"),
			against:  "HEAD",
			wantCode: 2,
			wantOut:  "UNKNOWN:",
			prefix:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status.set(tc.liveBody)
			url := ts.URL + "/api/v1/status"
			if tc.statusURL != "" {
				url = tc.statusURL
			}
			code, stdout, stderr := runFreshness(t, repo, script, url, tc.against)

			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, stdout, stderr)
			}
			got := strings.TrimSpace(stdout)
			if tc.prefix {
				if !strings.HasPrefix(got, tc.wantOut) {
					t.Errorf("stdout = %q, want prefix %q", got, tc.wantOut)
				}
			} else if got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			// The verdict is exactly one line on stdout — operators and
			// restart scripts parse it; notes belong on stderr.
			if lines := strings.Split(got, "\n"); len(lines) != 1 {
				t.Errorf("stdout carries %d lines, want exactly 1 verdict line: %q", len(lines), got)
			}
			if strings.Contains(stderr, "STALE:") || strings.Contains(stderr, "FRESH:") {
				t.Errorf("verdict leaked to stderr: %q", stderr)
			}
		})
	}
}

// runFreshness executes the guard with an explicit repo as its working tree.
func runFreshness(t *testing.T, repo, script, statusURL, against string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("bash", script, "--status-url", statusURL, "--against", against)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v\nstderr: %s", script, err, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// statusBody builds a minimal status payload carrying build_sha.
func statusBody(sha string) string {
	return fmt.Sprintf(`{"version":"dev-00000000","build_sha":%q,"build_time":"2026-09-18T07:56:43Z"}`, sha)
}

// commitFile writes path/content under repo and commits it, returning the
// full commit sha. Git identity comes from the environment so no global
// config is required (or trusted) in the sandbox.
func commitFile(t *testing.T, repo, path, content string) string {
	t.Helper()
	full := filepath.Join(repo, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	gitRun(t, repo, "add", "--", path)
	gitRun(t, repo, "commit", "-q", "-m", "add "+path)
	return gitRun(t, repo, "rev-parse", "HEAD")
}

// gitRun runs git in dir with an isolated config (no user/system config,
// explicit identity) so the scratch repo behaves identically on any host.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=schedgap148-test",
		"GIT_AUTHOR_EMAIL=schedgap148@example.invalid",
		"GIT_COMMITTER_NAME=schedgap148-test",
		"GIT_COMMITTER_EMAIL=schedgap148@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
