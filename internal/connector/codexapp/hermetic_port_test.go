package codexapp

// This file exercises the ported connector end to end against a real child
// process acting as the Codex app-server sidecar. The adapter itself never
// launches a process (Options.Transport is injected), so the only way to prove
// the port kept lifecycle ownership and left no orphan is to hand it the stdio
// of an OS process and check that process is gone after Dispose.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/connector"
	"github.com/coding-hermes/scheduler/internal/execution"
)

// TestCodexAppServerChildHelper is the fake native sidecar. It is inert unless
// the parent test re-executes the binary with CODEXAPP_CHILD=1, so in a normal
// suite run it is a no-op. It speaks newline-delimited JSON-RPC 2.0 on
// stdin/stdout and appends every inbound request to CODEXAPP_CHILD_LOG.
func TestCodexAppServerChildHelper(t *testing.T) {
	if os.Getenv("CODEXAPP_CHILD") != "1" {
		t.Skip("inert helper: only runs as a re-executed child process")
	}
	logPath := os.Getenv("CODEXAPP_CHILD_LOG")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(3)
	}
	logw := bufio.NewWriter(logFile)
	out := bufio.NewWriter(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		logw.Write(line)
		logw.WriteByte('\n')
		logw.Flush()
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			os.Exit(4)
		}
		id, hasID := m["id"]
		if !hasID {
			// Notifications (e.g. "initialized") are acknowledged only by ordering.
			continue
		}
		var result any
		switch m["method"] {
		case "initialize":
			result = map[string]any{"userAgent": "codex/0.152.0", "codexHome": "/fake", "platformFamily": "unix", "platformOs": "linux"}
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"model": "requested"}}, "nextCursor": nil}
		case "thread/start":
			p, _ := m["params"].(map[string]any)
			result = map[string]any{
				"model":             p["model"],
				"modelProvider":     p["modelProvider"],
				"cwd":               p["cwd"],
				"approvalPolicy":    "never",
				"approvalsReviewer": "user",
				// networkAccess deliberately omitted: the generated schema defaults it to false.
				"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{}},
				"thread":  map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}},
			}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}}
		case "thread/delete":
			result = map[string]any{}
		case "turn/interrupt":
			result = map[string]any{}
		default:
			result = map[string]any{}
		}
		reply, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		if err != nil {
			os.Exit(5)
		}
		out.Write(reply)
		out.WriteByte('\n')
		if err := out.Flush(); err != nil {
			os.Exit(6)
		}
	}
	logw.Flush()
	logFile.Close()
	// Exit without letting the test framework write "PASS"/"ok" onto the RPC stream.
	os.Exit(0)
}

// childTransport adapts a child process's stdio to io.ReadWriteCloser.
type childTransport struct {
	toChild   io.WriteCloser
	fromChild io.ReadCloser
	closeOnce func() error
}

func (c *childTransport) Read(p []byte) (int, error)  { return c.fromChild.Read(p) }
func (c *childTransport) Write(p []byte) (int, error) { return c.toChild.Write(p) }
func (c *childTransport) Close() error                { return c.closeOnce() }

// sidecar is a real fake app-server process plus its stdio transport.
type sidecar struct {
	cmd     *exec.Cmd
	log     string
	done    chan struct{}
	waitErr error
	trans   *childTransport
}

func startSidecar(t *testing.T) *sidecar {
	t.Helper()
	return startSidecarWith(t, "^TestCodexAppServerChildHelper$", nil)
}

// startSidecarWith launches the re-executed child with an explicit test filter
// and extra environment, so adversarial children can share the same wiring.
func startSidecarWith(t *testing.T, runFilter string, extraEnv []string) *sidecar {
	t.Helper()
	logPath := t.TempDir() + "/child-requests.jsonl"
	cmd := exec.Command(os.Args[0], "-test.run="+runFilter, "-test.timeout=60s")
	cmd.Env = append(append(os.Environ(), "CODEXAPP_CHILD=1", "CODEXAPP_CHILD_LOG="+logPath), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	s := &sidecar{cmd: cmd, log: logPath, done: make(chan struct{})}
	var once bool
	s.trans = &childTransport{
		toChild:   stdin,
		fromChild: stdout,
		closeOnce: func() error {
			if once {
				return nil
			}
			once = true
			err := stdin.Close()
			if cerr := stdout.Close(); err == nil {
				err = cerr
			}
			return err
		},
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		select {
		case <-s.done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Errorf("child %d survived kill for 10s (stderr: %s)", cmd.Process.Pid, stderr.String())
		}
	})
	return s
}

// lockedBuffer is a small concurrent-safe stderr sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// exited reports whether the child has been reaped.
func (s *sidecar) exited(timeout time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// alive reports whether the OS still has the child's pid.
func (s *sidecar) alive() bool {
	return syscall.Kill(s.cmd.Process.Pid, 0) == nil
}

// requests replays every inbound RPC request the child logged.
func (s *sidecar) requests(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(s.log)
	if errors.Is(err, os.ErrNotExist) {
		// The child creates the log on start; no file yet means no request yet.
		return nil
	}
	if err != nil {
		t.Fatalf("open child request log: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("child log line: %v", err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan child log: %v", err)
	}
	return out
}

// mutableCollector returns a bundle the test can rewrite between calls.
type mutableCollector struct{ bundle connector.ArtifactBundle }

func (c *mutableCollector) Collect(context.Context, connector.CollectRequest) (connector.ArtifactBundle, error) {
	return c.bundle, nil
}

// fixtureBrief loads the canonical execution brief the slice ships and verifies
// its digest, so the run is bound to a real authored brief rather than a literal.
func fixtureBrief(t *testing.T) execution.ExecutionBrief {
	t.Helper()
	raw, err := os.ReadFile("testdata/execution-brief-v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		ExecutionBrief json.RawMessage `json:"execution_brief"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if err := execution.ValidateBriefDigest(fixture.ExecutionBrief); err != nil {
		t.Fatalf("fixture brief digest: %v", err)
	}
	brief, err := execution.DecodeBrief(fixture.ExecutionBrief)
	if err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	return *brief
}

// TestPortHermeticBoundBriefDigestsAndOwnedLifecycle is the port's end-to-end
// exercise: one run over a real child process, with a digest-verified brief and
// a bound connector profile, that records artifact digests, keeps lifecycle
// ownership (the adapter is what ends the sidecar) and leaves no orphan.
func TestPortHermeticBoundBriefDigestsAndOwnedLifecycle(t *testing.T) {
	sc := startSidecar(t)
	collector := &mutableCollector{}
	a, err := New(Options{
		Transport:              sc.trans,
		Collector:              collector,
		Clock:                  &fakeClock{t: time.Unix(100, 0)},
		Capabilities:           connector.Capabilities{Events: true, Steer: true, Cancel: true, Quiescence: true},
		EventBuffer:            16,
		ConnectorProfileID:     "codex-app-0.152.0",
		ConnectorProfileDigest: "sha256:" + fmt.Sprintf("%064d", 7),
		PreflightTimeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if !sc.alive() {
		t.Fatal("sidecar did not start")
	}

	bound := corr()
	bound.ConnectorProfileID = "codex-app-0.152.0"
	bound.ConnectorProfileDigest = "sha256:" + fmt.Sprintf("%064d", 7)

	// A correlation bound to another profile must fail before any wire I/O.
	wrong := bound
	wrong.ConnectorProfileDigest = "sha256:" + fmt.Sprintf("%064d", 8)
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: wrong}); err == nil {
		t.Fatal("preflight accepted a correlation bound to a different connector profile")
	}
	if got := sc.requests(t); len(got) != 0 {
		t.Fatalf("profile mismatch reached the wire: %v", got)
	}

	pf, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: bound})
	if err != nil || !pf.Ready || !pf.Authenticated || !pf.ModelAvailable || pf.ModelsChecked != 1 {
		t.Fatalf("preflight=%+v err=%v", pf, err)
	}

	brief := fixtureBrief(t)
	res, err := a.Start(context.Background(), connector.StartRequest{Correlation: bound, WorkspacePath: "/lease", Brief: brief})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Correlation.NativeSessionID != "thread-1" || res.Correlation.NativeTurnID != "turn-1" {
		t.Fatalf("native identity not recorded: %+v", res.Correlation)
	}
	if res.Correlation.ConnectorProfileID != bound.ConnectorProfileID || res.Correlation.ConnectorProfileDigest != bound.ConnectorProfileDigest {
		t.Fatalf("profile binding lost across start: %+v", res.Correlation)
	}
	if res.Actual.Model != "requested" || res.Actual.Provider != "requested-provider" || res.Actual.EvidenceSource != "thread/start response" {
		t.Fatalf("actual identity=%+v", res.Actual)
	}

	// The brief must have travelled as the rendered instruction payload, in the
	// R2 wire shape (no top-level networkAccess, workspace-write sandbox).
	var startParams map[string]any
	for _, req := range sc.requests(t) {
		if req["method"] == "thread/start" {
			startParams, _ = req["params"].(map[string]any)
		}
	}
	if startParams == nil {
		t.Fatal("no thread/start reached the sidecar")
	}
	if _, present := startParams["networkAccess"]; present {
		t.Errorf("thread/start leaked unsupported top-level networkAccess: %#v", startParams)
	}
	if startParams["cwd"] != "/lease" || startParams["sandbox"] != "workspace-write" || startParams["approvalPolicy"] != "never" {
		t.Errorf("thread/start policy params: %#v", startParams)
	}
	instructions, _ := startParams["baseInstructions"].(string)
	for _, want := range []string{brief.TaskID, brief.BriefID, "workspace_policy", "closeout_policy", "provenance"} {
		if want == "" || !strings.Contains(instructions, want) {
			t.Errorf("rendered brief missing %q", want)
		}
	}

	// Artifact digests recorded verbatim: prefixed manifest digest plus
	// per-reference bare sha256.
	ref := connector.ArtifactReference{URI: "cas://bundle/1", MediaType: "application/json", SHA256: fmt.Sprintf("%064x", 11), Size: 42}
	collector.bundle = connector.ArtifactBundle{
		Correlation:    res.Correlation,
		References:     []connector.ArtifactReference{ref},
		ManifestSHA256: "sha256:" + fmt.Sprintf("%064x", 12),
	}
	got, err := a.Collect(context.Background(), connector.CollectRequest{Correlation: res.Correlation})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got.ManifestSHA256 != collector.bundle.ManifestSHA256 || len(got.References) != 1 || got.References[0].SHA256 != ref.SHA256 || got.References[0].URI != ref.URI {
		t.Fatalf("artifact digests not recorded: %+v", got)
	}
	// The R2 alignment also means a bare-hex manifest digest is refused.
	collector.bundle.ManifestSHA256 = fmt.Sprintf("%064x", 12)
	if _, err := a.Collect(context.Background(), connector.CollectRequest{Correlation: res.Correlation}); err == nil {
		t.Fatal("collect accepted an unprefixed manifest digest")
	}

	// Lifecycle ownership: the adapter is what ends the native process.
	if !sc.alive() {
		t.Fatal("sidecar exited before Dispose")
	}
	if err := a.Dispose(context.Background(), connector.DisposeRequest{Correlation: res.Correlation}); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if !sc.exited(15 * time.Second) {
		t.Fatalf("sidecar pid %d still running after Dispose (orphan)", sc.cmd.Process.Pid)
	}
	if sc.alive() {
		t.Fatalf("sidecar pid %d still visible to the OS after Dispose (orphan)", sc.cmd.Process.Pid)
	}
	if err := a.Dispose(context.Background(), connector.DisposeRequest{Correlation: res.Correlation}); err != nil {
		t.Fatalf("second dispose not idempotent: %v", err)
	}
	// A disposed adapter refuses new work instead of silently reusing the transport.
	if _, err := a.Start(context.Background(), connector.StartRequest{Correlation: bound, WorkspacePath: "/lease", Brief: brief}); err == nil {
		t.Fatal("start succeeded after dispose")
	}
}
