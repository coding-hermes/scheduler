package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/connector"
	"github.com/coding-hermes/scheduler/internal/execution"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(time.Millisecond)
	return f.t
}

type fakeSupervisor struct {
	zero bool
	err  error
}

func (f fakeSupervisor) WaitZeroDescendants(context.Context, connector.Correlation) (bool, error) {
	return f.zero, f.err
}

type fakeCollector struct{ bundle connector.ArtifactBundle }

func (f fakeCollector) Collect(context.Context, connector.CollectRequest) (connector.ArtifactBundle, error) {
	return f.bundle, nil
}

type harness struct {
	t        *testing.T
	conn     net.Conn
	mu       sync.Mutex
	methods  []string
	requests []map[string]any
}

func newHarness(t *testing.T, handler func(*harness, map[string]any)) (*Adapter, *harness) {
	t.Helper()
	client, server := net.Pipe()
	h := &harness{t: t, conn: server}
	go func() {
		s := bufio.NewScanner(server)
		for s.Scan() {
			var m map[string]any
			if json.Unmarshal(s.Bytes(), &m) != nil {
				return
			}
			h.mu.Lock()
			h.methods = append(h.methods, m["method"].(string))
			h.requests = append(h.requests, m)
			h.mu.Unlock()
			if _, ok := m["id"]; ok {
				handler(h, m)
			}
		}
	}()
	a, err := New(Options{Transport: client, Supervisor: fakeSupervisor{zero: true}, Clock: &fakeClock{t: time.Unix(100, 0)}, Capabilities: connector.Capabilities{Events: true, Steer: true, Cancel: true, Quiescence: true}, EventBuffer: 16, ConnectorProfileID: "profile", ConnectorProfileDigest: "profile-digest"})
	if err != nil {
		t.Fatal(err)
	}
	return a, h
}
func (h *harness) response(req map[string]any, result any) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
	b = append(b, '\n')
	if _, err := h.conn.Write(b); err != nil {
		h.t.Errorf("response: %v", err)
	}
}
func (h *harness) notification(method string, p any) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": p})
	_, err := h.conn.Write(append(b, '\n'))
	if err != nil {
		h.t.Errorf("notification: %v", err)
	}
}
func initResult() any {
	return map[string]any{"userAgent": "codex/0.152.0", "codexHome": "/fake", "platformFamily": "unix", "platformOs": "linux"}
}
func threadResult(model, provider, status string) any {
	// networkAccess is legally omitted by Codex: its generated schema defaults
	// the field to false. The happy path pins that wire-compatible omission.
	return map[string]any{"model": model, "modelProvider": provider, "cwd": "/lease", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, "thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": status}}}
}
func baseHandler(model, provider, readStatus string) func(*harness, map[string]any) {
	return func(h *harness, m map[string]any) {
		switch m["method"] {
		case "initialize":
			h.response(m, initResult())
		case "account/read":
			h.response(m, map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true})
		case "model/list":
			h.response(m, map[string]any{"data": []any{map[string]any{"model": "requested"}}, "nextCursor": nil})
		case "thread/start":
			h.response(m, threadResult(model, provider, "idle"))
		case "turn/start":
			h.response(m, map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}})
		case "thread/read":
			h.response(m, map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": readStatus}}})
		case "turn/interrupt":
			h.response(m, map[string]any{})
		case "turn/steer":
			h.response(m, map[string]any{"turnId": "turn-1"})
		case "thread/delete":
			h.response(m, map[string]any{})
		}
	}
}
func corr() connector.Correlation {
	return connector.Correlation{RunID: "run", Generation: 1, AttemptKey: "key", AttemptID: "attempt", RouteDigest: "route", WorkspaceLeaseID: "lease", WorkspaceLeaseEpoch: 2, ConnectorProfileID: "profile", ConnectorProfileDigest: "profile-digest", Requested: connector.Candidate{Model: "requested", Provider: "requested-provider"}}
}
func start(t *testing.T, a *Adapter) connector.StartResult {
	t.Helper()
	a.mu.Lock()
	ready := a.state == statePreflightReady
	a.mu.Unlock()
	if !ready {
		p, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()})
		if err != nil || !p.Ready {
			t.Fatalf("preflight=%+v err=%v", p, err)
		}
	}
	r, err := a.Start(context.Background(), connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: validBrief(t)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func validBrief(t *testing.T) execution.ExecutionBrief {
	t.Helper()
	b := execution.ExecutionBrief{SchemaVersion: "1.0", BriefID: "brief", RevisionID: "rev", IssuedAt: "2026-10-16T00:00:00Z", IssuedBy: "foreman", Project: "scheduler", Lane: "main", TaskID: "task", TickID: "tick", Mission: "implement", RationaleSummary: "needed", Source: execution.Source{RepositoryURI: "repo", SourceRef: "main", BaseRevision: "abc", DiffBaseRevision: "abc"}, ContextRefs: []execution.ContextRef{{Namespace: "board", KeyOrID: "task", RevisionOrDigest: "rev", RetrievedAt: "2026-10-16T00:00:00Z", Authority: "board"}}, Scope: execution.Scope{AllowedPaths: []string{"internal/"}, ForbiddenPaths: []string{"config"}}, Constraints: []execution.Constraint{{ID: "c1", Level: "must", Statement: "stay dark"}}, NonGoals: []string{"deploy"}, CapabilityRequirements: execution.CapabilityRequirements{Required: []string{"events"}, Optional: []string{"steer"}}, WorkspacePolicy: execution.WorkspacePolicy{Kind: "worktree", Isolation: "exclusive", Network: "disabled", RequiredTools: []string{"go"}}, PermissionPolicy: execution.PermissionPolicy{Profile: "unattended", Unattended: true}, ResourcePolicy: execution.ResourcePolicy{CPU: 1, MemoryMB: 512, PIDs: 64, DiskMB: 1024, WallTimeoutS: 600, IdleTimeoutS: 60}, AcceptanceCriteria: []execution.AcceptanceCriterion{{ID: "a1", Statement: "tests pass", Required: true, ProofRequirements: []execution.ProofRequirement{{ID: "p1", Kind: "command", Argv: []string{"go", "test", "./..."}, CWD: ".", TimeoutS: 600, ExpectExit: 0}}}}, RequiredDeliverables: []string{"commit"}, CloseoutPolicy: execution.CloseoutPolicy{RequiresCommit: true, RequiresSourceOrTestDiff: true, TargetRemote: "origin", TargetBranch: "main"}, Provenance: execution.Provenance{ForemanRunID: "foreman-run", BoardRevision: "board-rev"}}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	b.BriefDigest, err = execution.BriefDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHappyPathAndNativeActualIdentity(t *testing.T) {
	a, h := newHarness(t, baseHandler("served", "served-provider", "idle"))
	p, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()})
	if err != nil || !p.Ready || p.ModelsChecked != 1 {
		t.Fatalf("preflight=%+v err=%v", p, err)
	}
	r := start(t, a)
	if r.Actual.Model != "served" || r.Actual.Provider != "served-provider" || r.Correlation.NativeSessionID != "thread-1" {
		t.Fatalf("start=%+v", r)
	}
	h.notification("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}})
	receipt, err := a.WaitSettled(context.Background(), r.Correlation)
	if err != nil || receipt.Terminal != connector.TerminalCompleted || receipt.ThreadStatus != "idle" {
		t.Fatalf("settlement=%+v err=%v", receipt, err)
	}
	h.mu.Lock()
	got := strings.Join(h.methods, ",")
	reqs := append([]map[string]any(nil), h.requests...)
	h.mu.Unlock()
	if got != "initialize,initialized,account/read,model/list,thread/start,turn/start,thread/read" {
		t.Fatalf("methods %s", got)
	}
	for _, q := range reqs {
		if q["method"] == "thread/start" {
			p := q["params"].(map[string]any)
			if p["cwd"] != "/lease" || p["sandbox"] != "workspace-write" || p["approvalPolicy"] != "never" || p["model"] != "requested" || p["modelProvider"] != "requested-provider" {
				t.Fatalf("thread params %#v", p)
			}
			if strings.Contains(fmt.Sprint(p), "chain-of-thought") {
				t.Fatal("hidden reasoning field rendered")
			}
			if _, present := p["networkAccess"]; present {
				t.Fatalf("thread/start sent unsupported top-level networkAccess: %#v", p)
			}
		}
	}
}

func TestCanonicalExecutionFixtureAcceptedAndFullyRendered(t *testing.T) {
	raw, err := os.ReadFile("testdata/execution-brief-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ExecutionBrief json.RawMessage `json:"execution_brief"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	brief, err := execution.DecodeBrief(fixture.ExecutionBrief)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.ValidateBriefDigest(fixture.ExecutionBrief); err != nil {
		t.Fatal(err)
	}
	a, h := newHarness(t, baseHandler("served", "provider", "idle"))
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(context.Background(), connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: *brief}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	requests := append([]map[string]any(nil), h.requests...)
	h.mu.Unlock()
	var rendered string
	for _, request := range requests {
		if request["method"] == "thread/start" {
			rendered = request["params"].(map[string]any)["baseInstructions"].(string)
		}
	}
	for _, want := range []string{"mission", "rationale_summary", "source", "context_refs", "allowed_paths", "forbidden_paths", "constraints", "non_goals", "capability_requirements", "workspace_policy", "permission_policy", "resource_policy", "proof_requirements", "required_deliverables", "closeout_policy", "provenance"} {
		if !strings.Contains(rendered, `"`+want+`"`) {
			t.Errorf("rendered brief missing %s", want)
		}
	}
	if strings.Contains(strings.ToLower(rendered), "chain-of-thought") {
		t.Fatal("hidden reasoning rendered")
	}
}

func TestRerouteUpdatesIdentityAndOrderedEvents(t *testing.T) {
	a, h := newHarness(t, baseHandler("served", "provider", "idle"))
	r := start(t, a)
	es, err := a.Events(context.Background(), r.Correlation)
	if err != nil {
		t.Fatal(err)
	}
	h.notification("model/rerouted", map[string]any{"fromModel": "served", "toModel": "safer", "reason": "highRiskCyberActivity", "threadId": "thread-1", "turnId": "turn-1"})
	first, _ := es.Next(context.Background())
	second, _ := es.Next(context.Background())
	if first.Sequence != 1 || second.Sequence != 2 || second.Kind != connector.EventRerouted || second.Actual.Model != "safer" {
		t.Fatalf("events %#v %#v", first, second)
	}
	s, _ := a.Get(context.Background(), r.Correlation)
	if s.Actual.Model != "safer" || !strings.Contains(s.Actual.EvidenceSource, "model/rerouted") {
		t.Fatalf("state %+v", s)
	}
}

func TestCancellationProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		zero bool
		want connector.ErrorKind
	}{{"success", true, ""}, {"descendants remain", false, connector.ErrorDescendantsRemain}} {
		t.Run(tc.name, func(t *testing.T) {
			a, h := newHarness(t, baseHandler("served", "provider", "idle"))
			a.supervisor = fakeSupervisor{zero: tc.zero}
			r := start(t, a)
			go func() {
				time.Sleep(time.Millisecond)
				h.notification("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted", "items": []any{}}})
			}()
			got, err := a.Cancel(context.Background(), connector.CancelRequest{Correlation: r.Correlation})
			if tc.want == "" {
				if err != nil || !got.ZeroDescendants || got.Terminal != connector.TerminalInterrupted {
					t.Fatalf("got=%+v err=%v", got, err)
				}
			} else if !connector.IsKind(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestTerminalWithoutIdleRefused(t *testing.T) {
	a, h := newHarness(t, baseHandler("served", "provider", "active"))
	r := start(t, a)
	h.notification("turn/completed", map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}})
	_, err := a.WaitSettled(context.Background(), r.Correlation)
	if !connector.IsKind(err, connector.ErrorNotSettled) {
		t.Fatalf("err=%v", err)
	}
}

func TestWrongTerminalCorrelationKillsTransport(t *testing.T) {
	a, h := newHarness(t, baseHandler("served", "provider", "idle"))
	r := start(t, a)
	h.notification("turn/completed", map[string]any{"threadId": "wrong", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := a.WaitSettled(ctx, r.Correlation)
	if !connector.IsKind(err, connector.ErrorTransport) {
		t.Fatalf("err=%v", err)
	}
}

func TestUnsupportedFailsClosed(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	a, _ := New(Options{Transport: client, ConnectorProfileID: "profile", ConnectorProfileDigest: "profile-digest"})
	c := corr()
	c.NativeSessionID = "x"
	c.NativeTurnID = "y"
	for name, fn := range map[string]func() error{"events": func() error { _, e := a.Events(context.Background(), c); return e }, "steer": func() error { _, e := a.Steer(context.Background(), connector.SteerRequest{Correlation: c}); return e }, "cancel": func() error {
		_, e := a.Cancel(context.Background(), connector.CancelRequest{Correlation: c})
		return e
	}, "settle": func() error { _, e := a.WaitSettled(context.Background(), c); return e }} {
		t.Run(name, func(t *testing.T) {
			if e := fn(); !connector.IsKind(e, connector.ErrorUnsupported) {
				t.Fatalf("err=%v", e)
			}
		})
	}
}

func TestCollectReferencesOnly(t *testing.T) {
	a, _ := newHarness(t, baseHandler("served", "provider", "idle"))
	r := start(t, a)
	ref := connector.ArtifactReference{URI: "cas://abc", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), Size: 12}
	a.collector = fakeCollector{bundle: connector.ArtifactBundle{Correlation: r.Correlation, References: []connector.ArtifactReference{ref}, ManifestSHA256: "sha256:" + strings.Repeat("b", 64)}}
	b, err := a.Collect(context.Background(), connector.CollectRequest{Correlation: r.Correlation})
	if err != nil || len(b.References) != 1 || b.References[0].URI != "cas://abc" {
		t.Fatalf("bundle=%+v err=%v", b, err)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "artifact bytes") {
		t.Fatal("artifact bytes leaked")
	}
}

func TestErrorsDoNotLeakSecretPayloads(t *testing.T) {
	secret := "sk-secret-token"
	handler := func(h *harness, m map[string]any) {
		if m["method"] == "initialize" {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -1, "message": secret}})
			h.conn.Write(append(b, '\n'))
		}
	}
	a, _ := newHarness(t, handler)
	_, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()})
	if strings.Contains(fmt.Sprintf("%v", err), secret) || strings.Contains(fmt.Sprintf("%+v", err), secret) {
		t.Fatalf("secret leaked: %v", err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("upstream error was unwrap-exported: %v", errors.Unwrap(err))
	}
	var injected interface{ Secret() string }
	if errors.As(err, &injected) {
		t.Fatal("injected upstream error remained reachable")
	}
}

func TestStartAuthorityValidationBeforeWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*connector.StartRequest)
	}{
		{"tampered brief", func(r *connector.StartRequest) { r.Brief.Mission = "tampered" }},
		{"profile id", func(r *connector.StartRequest) { r.Correlation.ConnectorProfileID = "other" }},
		{"profile digest", func(r *connector.StartRequest) { r.Correlation.ConnectorProfileDigest = "other" }},
		{"route", func(r *connector.StartRequest) { r.Correlation.RouteDigest = "other" }},
		{"lease", func(r *connector.StartRequest) { r.Correlation.WorkspaceLeaseEpoch++ }},
		{"candidate", func(r *connector.StartRequest) { r.Correlation.Requested.Model = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h := newHarness(t, baseHandler("served", "provider", "idle"))
			if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()}); err != nil {
				t.Fatal(err)
			}
			r := connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: validBrief(t)}
			tc.edit(&r)
			if _, err := a.Start(context.Background(), r); err == nil {
				t.Fatal("start accepted")
			}
			h.mu.Lock()
			methods := strings.Join(h.methods, ",")
			h.mu.Unlock()
			if strings.Contains(methods, "thread/start") {
				t.Fatalf("wire start occurred: %s", methods)
			}
		})
	}
}

func TestConcurrentInitializationAndStartAreSingleShot(t *testing.T) {
	a, h := newHarness(t, baseHandler("served", "provider", "idle"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()})
		}()
	}
	wg.Wait()
	h.mu.Lock()
	initCount := countMethod(h.methods, "initialize")
	initializedCount := countMethod(h.methods, "initialized")
	h.mu.Unlock()
	if initCount != 1 || initializedCount != 1 {
		t.Fatalf("initialize=%d initialized=%d", initCount, initializedCount)
	}
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()}); err != nil {
		t.Fatal(err)
	}
	req := connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: validBrief(t)}
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := a.Start(context.Background(), req); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	h.mu.Lock()
	threadStarts := countMethod(h.methods, "thread/start")
	turnStarts := countMethod(h.methods, "turn/start")
	h.mu.Unlock()
	if success != 1 || threadStarts != 1 || turnStarts != 1 {
		t.Fatalf("success=%d thread/start=%d turn/start=%d", success, threadStarts, turnStarts)
	}
}

func countMethod(methods []string, want string) int {
	n := 0
	for _, method := range methods {
		if method == want {
			n++
		}
	}
	return n
}

func TestAppliedPolicyMismatchDeletesThread(t *testing.T) {
	for _, field := range []string{"cwd", "approval", "sandbox", "network"} {
		t.Run(field, func(t *testing.T) {
			handler := baseHandler("served", "provider", "idle")
			a, h := newHarness(t, func(h *harness, m map[string]any) {
				if m["method"] != "thread/start" {
					handler(h, m)
					return
				}
				result := threadResult("served", "provider", "idle").(map[string]any)
				switch field {
				case "cwd":
					result["cwd"] = "/wrong"
				case "approval":
					result["approvalPolicy"] = "on-request"
				case "sandbox":
					result["sandbox"].(map[string]any)["type"] = "dangerFullAccess"
				case "network":
					result["sandbox"].(map[string]any)["networkAccess"] = true
				}
				h.response(m, result)
			})
			_, err := startResult(t, a)
			if !connector.IsKind(err, connector.ErrorPartialStart) {
				t.Fatalf("err=%v", err)
			}
			h.mu.Lock()
			deletes := countMethod(h.methods, "thread/delete")
			h.mu.Unlock()
			if deletes != 1 {
				t.Fatalf("deletes=%d", deletes)
			}
		})
	}
}

func startResult(t *testing.T, a *Adapter) (connector.StartResult, error) {
	t.Helper()
	if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()}); err != nil {
		t.Fatal(err)
	}
	return a.Start(context.Background(), connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: validBrief(t)})
}

func TestTurnFailureCleanupAndPoison(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		t.Run(fmt.Sprint("delete-fails-", deleteFails), func(t *testing.T) {
			base := baseHandler("served", "provider", "idle")
			a, h := newHarness(t, func(h *harness, m map[string]any) {
				switch m["method"] {
				case "turn/start":
					h.response(m, map[string]any{"turn": map[string]any{}})
				case "thread/delete":
					if deleteFails {
						b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -1, "message": "secret"}})
						_, _ = h.conn.Write(append(b, '\n'))
					} else {
						h.response(m, map[string]any{})
					}
				default:
					base(h, m)
				}
			})
			_, err := startResult(t, a)
			want := connector.ErrorPartialStart
			if deleteFails {
				want = connector.ErrorStartUnknown
			}
			if !connector.IsKind(err, want) {
				t.Fatalf("err=%v", err)
			}
			_, retryErr := a.Start(context.Background(), connector.StartRequest{Correlation: corr(), WorkspacePath: "/lease", Brief: validBrief(t)})
			if retryErr == nil {
				t.Fatal("unsafe retry accepted")
			}
			h.mu.Lock()
			starts := countMethod(h.methods, "thread/start")
			h.mu.Unlock()
			if starts != 1 {
				t.Fatalf("thread starts=%d", starts)
			}
		})
	}
}

func TestPreflightModelCaps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pages, models int
		data          []any
		next          any
	}{{"pages", 1, 100, []any{}, "next"}, {"models", 10, 1, []any{map[string]any{"model": "x"}, map[string]any{"model": "requested"}}, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			base := baseHandler("served", "provider", "idle")
			a, _ := newHarness(t, func(h *harness, m map[string]any) {
				if m["method"] == "model/list" {
					h.response(m, map[string]any{"data": tc.data, "nextCursor": tc.next})
					return
				}
				base(h, m)
			})
			a.maxModelPages, a.maxModels = tc.pages, tc.models
			if _, err := a.Preflight(context.Background(), connector.PreflightRequest{Correlation: corr()}); err == nil {
				t.Fatal("cap accepted")
			}
		})
	}
}

func TestSecondEventSubscriberRejected(t *testing.T) {
	a, _ := newHarness(t, baseHandler("served", "provider", "idle"))
	r := start(t, a)
	if _, err := a.Events(context.Background(), r.Correlation); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Events(context.Background(), r.Correlation); err == nil {
		t.Fatal("second subscriber accepted")
	}
}

func TestCollectRejectsInvalidBundles(t *testing.T) {
	a, _ := newHarness(t, baseHandler("served", "provider", "idle"))
	r := start(t, a)
	good := connector.ArtifactReference{URI: "cas://a", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), Size: 1}
	for _, tc := range []struct {
		name   string
		mutate func(*connector.ArtifactBundle)
	}{
		{"correlation", func(b *connector.ArtifactBundle) { b.Correlation.RouteDigest = "bad" }},
		{"manifest", func(b *connector.ArtifactBundle) { b.ManifestSHA256 = "ABC" }},
		{"digest", func(b *connector.ArtifactBundle) { b.References[0].SHA256 = strings.Repeat("A", 64) }},
		{"media", func(b *connector.ArtifactBundle) { b.References[0].MediaType = "" }},
		{"duplicate", func(b *connector.ArtifactBundle) { b.References = append(b.References, b.References[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := connector.ArtifactBundle{Correlation: r.Correlation, References: []connector.ArtifactReference{good}, ManifestSHA256: "sha256:" + strings.Repeat("b", 64)}
			tc.mutate(&b)
			a.collector = fakeCollector{bundle: b}
			if _, err := a.Collect(context.Background(), connector.CollectRequest{Correlation: r.Correlation}); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}

func TestRPCFailuresAndLimits(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		c, s := net.Pipe()
		r := newRPC(c, 128, 1, func(string, json.RawMessage) error { return nil })
		go s.Write([]byte("not-json\n"))
		<-r.done
		if r.fatal == nil {
			t.Fatal("missing fatal")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		c, s := net.Pipe()
		r := newRPC(c, 32, 1, func(string, json.RawMessage) error { return nil })
		go s.Write([]byte(strings.Repeat("x", 64) + "\n"))
		<-r.done
		if r.fatal == nil {
			t.Fatal("missing fatal")
		}
	})
	t.Run("unknown response", func(t *testing.T) {
		c, s := net.Pipe()
		r := newRPC(c, 128, 1, func(string, json.RawMessage) error { return nil })
		go s.Write([]byte(`{"jsonrpc":"2.0","id":9,"result":{}}` + "\n"))
		<-r.done
		if r.fatal == nil {
			t.Fatal("missing fatal")
		}
	})
	t.Run("eof", func(t *testing.T) {
		c, s := net.Pipe()
		r := newRPC(c, 128, 1, func(string, json.RawMessage) error { return nil })
		s.Close()
		<-r.done
		if !errors.Is(r.fatal, io.EOF) {
			t.Fatalf("fatal %v", r.fatal)
		}
	})
	t.Run("timeout and pending", func(t *testing.T) {
		c, s := net.Pipe()
		defer s.Close()
		r := newRPC(c, 1024, 1, func(string, json.RawMessage) error { return nil })
		go io.Copy(io.Discard, s)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.call(ctx, "one", map[string]any{}, nil) }()
		for i := 0; i < 100; i++ {
			r.mu.Lock()
			n := len(r.pending)
			r.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err := r.call(context.Background(), "two", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("pending err %v", err)
		}
		cancel()
		if !errors.Is(<-done, context.Canceled) {
			t.Fatal("expected cancellation")
		}
	})
}
