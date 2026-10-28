package codexapp

// Hardening for the ported connector: the failure paths, exercised against a
// real sidecar process rather than a pipe double. The unit tests in
// adapter_test.go already pin the error classes; what they cannot see is whether
// a failed attempt leaves the native process behind. Every case below asserts
// both halves: fail-closed classification AND the child is reaped.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/connector"
)

// TestCodexAppServerAdversarialChild is the misbehaving native sidecar. Like the
// happy-path helper it is inert unless re-executed with CODEXAPP_CHILD=1, and it
// exits via os.Exit so the test framework never writes onto the RPC stream.
func TestCodexAppServerAdversarialChild(t *testing.T) {
	if os.Getenv("CODEXAPP_CHILD") != "1" {
		t.Skip("inert helper: only runs as a re-executed child process")
	}
	mode := os.Getenv("CODEXAPP_ADV_MODE")
	out := bufio.NewWriter(os.Stdout)
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			os.Exit(4)
		}
		out.Write(b)
		out.WriteByte('\n')
		if err := out.Flush(); err != nil {
			os.Exit(6)
		}
	}
	resp := func(id any, result any) { write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	notif := func(method string, params any) {
		write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	threadResult := func(params map[string]any, netAccess any) any {
		sandbox := map[string]any{"type": "workspaceWrite", "writableRoots": []any{}}
		if netAccess != nil {
			sandbox["networkAccess"] = netAccess
		}
		return map[string]any{
			"model":             params["model"],
			"modelProvider":     params["modelProvider"],
			"cwd":               params["cwd"],
			"approvalPolicy":    "never",
			"approvalsReviewer": "user",
			"sandbox":           sandbox,
			"thread":            map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}},
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			os.Exit(5)
		}
		id, hasID := m["id"]
		if !hasID {
			continue
		}
		params, _ := m["params"].(map[string]any)
		switch m["method"] {
		case "initialize":
			resp(id, map[string]any{"userAgent": "codex/0.152.0", "codexHome": "/fake", "platformFamily": "unix", "platformOs": "linux"})
		case "account/read":
			resp(id, map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true})
		case "model/list":
			resp(id, map[string]any{"data": []any{map[string]any{"model": "requested"}}, "nextCursor": nil})
		case "thread/start":
			switch mode {
			case "mismatch":
				// Applied policy differs from what was requested: the adapter must
				// delete this thread and close, not proceed.
				resp(id, threadResult(params, true))
			case "garbage":
				// A non-JSON frame must fail the transport, never be skipped.
				out.WriteString("this is not json\n")
				out.Flush()
			case "oversize":
				out.WriteString(strings.Repeat("x", 2<<20))
				out.WriteString("\n")
				out.Flush()
			case "silent-exit":
				os.Exit(0)
			default:
				resp(id, threadResult(params, nil))
			}
		case "turn/start":
			resp(id, map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}})
		case "turn/interrupt":
			// Native interrupt evidence arrives before the RPC reply.
			notif("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted", "items": []any{}}})
			resp(id, map[string]any{})
		default:
			resp(id, map[string]any{})
		}
	}
	os.Exit(0)
}

// adversarialAdapter starts a misbehaving sidecar and returns the adapter, the
// sidecar, and the correlation it is bound to.
func adversarialAdapter(t *testing.T, mode string, supervisor Supervisor, maxLine int) (*Adapter, *sidecar, connector.Correlation) {
	t.Helper()
	sc := startSidecarWith(t, "^TestCodexAppServerAdversarialChild$", []string{"CODEXAPP_ADV_MODE=" + mode})
	c := corr()
	a, err := New(Options{
		Transport:              sc.trans,
		Supervisor:             supervisor,
		Collector:              &mutableCollector{},
		Clock:                  &fakeClock{t: time.Unix(100, 0)},
		MaxLineBytes:           maxLine,
		Capabilities:           connector.Capabilities{Events: true, Steer: true, Cancel: true, Quiescence: true},
		EventBuffer:            16,
		ConnectorProfileID:     c.ConnectorProfileID,
		ConnectorProfileDigest: c.ConnectorProfileDigest,
		PreflightTimeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a, sc, c
}

// TestHardeningStartFailurePathsLeaveNoOrphan drives four native misbehaviours
// through Start and requires, for each: the documented error class, a refusal
// afterwards, and a reaped child process.
func TestHardeningStartFailurePathsLeaveNoOrphan(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		maxLine int
		want    connector.ErrorKind
	}{
		{"applied policy mismatch", "mismatch", 0, connector.ErrorPartialStart},
		{"malformed native frame", "garbage", 0, connector.ErrorStartUnknown},
		{"native exit mid-start", "silent-exit", 0, connector.ErrorStartUnknown},
		{"oversize native frame", "oversize", 4096, connector.ErrorStartUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, sc, c := adversarialAdapter(t, tc.mode, fakeSupervisor{zero: true}, tc.maxLine)
			if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: c}); err != nil {
				t.Fatalf("preflight: %v", err)
			}
			_, err := a.Start(context.Background(), connector.StartRequest{Correlation: c, WorkspacePath: "/lease", Brief: validBrief(t)})
			if err == nil {
				t.Fatal("start succeeded against a misbehaving sidecar")
			}
			if !connector.IsKind(err, tc.want) {
				t.Fatalf("error class = %v, want %v (err: %v)", err, tc.want, err)
			}
			if !sc.exited(15 * time.Second) {
				t.Fatalf("sidecar pid %d outlived a failed start (orphan)", sc.cmd.Process.Pid)
			}
			if sc.alive() {
				t.Fatalf("sidecar pid %d still visible to the OS after a failed start", sc.cmd.Process.Pid)
			}
			// A failed attempt must not be resurrected by a second Start.
			if _, err := a.Start(context.Background(), connector.StartRequest{Correlation: c, WorkspacePath: "/lease", Brief: validBrief(t)}); err == nil {
				t.Fatal("second start succeeded after a failed start")
			}
		})
	}
}

// TestHardeningCancelRefusalPreservesSidecar pins the semantic line between
// cancel and dispose: a cancel that cannot prove zero descendants must be
// refused, and it must NOT tear the native process down. Disposal is the only
// operation allowed to end the sidecar.
func TestHardeningCancelRefusalPreservesSidecar(t *testing.T) {
	a, sc, c := adversarialAdapter(t, "normal", fakeSupervisor{zero: false}, 0)
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: c}); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res, err := a.Start(context.Background(), connector.StartRequest{Correlation: c, WorkspacePath: "/lease", Brief: validBrief(t)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	receipt, err := a.Cancel(context.Background(), connector.CancelRequest{Correlation: res.Correlation})
	if err == nil {
		t.Fatalf("cancel returned a receipt without a zero-descendant proof: %+v", receipt)
	}
	if !connector.IsKind(err, connector.ErrorDescendantsRemain) {
		t.Fatalf("cancel error class = %v, want %v", err, connector.ErrorDescendantsRemain)
	}
	if !sc.alive() {
		t.Fatal("cancel killed the sidecar without a zero-descendant proof")
	}
	if err := a.Dispose(context.Background(), connector.DisposeRequest{Correlation: res.Correlation}); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if !sc.exited(15 * time.Second) {
		t.Fatalf("sidecar pid %d survived dispose (orphan)", sc.cmd.Process.Pid)
	}
}

// TestHardeningCollectRefusesForeignCorrelation proves a bundle collected for a
// different attempt cannot be laundered into this one, even when the collector
// itself is buggy or malicious.
func TestHardeningCollectRefusesForeignCorrelation(t *testing.T) {
	a, sc, c := adversarialAdapter(t, "normal", fakeSupervisor{zero: true}, 0)
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: c}); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res, err := a.Start(context.Background(), connector.StartRequest{Correlation: c, WorkspacePath: "/lease", Brief: validBrief(t)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	foreign := res.Correlation
	foreign.AttemptID = "someone-else"
	collector := a.collector.(*mutableCollector)
	collector.bundle = connector.ArtifactBundle{
		Correlation:    foreign,
		References:     []connector.ArtifactReference{{URI: "cas://x", MediaType: "text/plain", SHA256: fmt.Sprintf("%064x", 3), Size: 1}},
		ManifestSHA256: "sha256:" + fmt.Sprintf("%064x", 4),
	}
	if _, err := a.Collect(context.Background(), connector.CollectRequest{Correlation: res.Correlation}); err == nil {
		t.Fatal("collect accepted a bundle bound to a foreign attempt")
	} else if !connector.IsKind(err, connector.ErrorProtocol) {
		t.Fatalf("collect error class = %v, want protocol", err)
	}
	if err := a.Dispose(context.Background(), connector.DisposeRequest{Correlation: res.Correlation}); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if !sc.exited(15 * time.Second) {
		t.Fatalf("sidecar pid %d survived dispose", sc.cmd.Process.Pid)
	}
}
