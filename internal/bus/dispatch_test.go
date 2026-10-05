package bus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dispatchRecorder is a stub relay that records every deliver request it
// receives and answers with the configured status/body.
type dispatchRecorder struct {
	mu       sync.Mutex
	requests []recordedDispatch
	status   int
	body     string
}

type recordedDispatch struct {
	method      string
	path        string
	escapedPath string
	auth        string
	agentHeader string
	contentType string
	body        []byte
}

func (r *dispatchRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := readAllBounded(req)
	r.mu.Lock()
	r.requests = append(r.requests, recordedDispatch{
		method:      req.Method,
		path:        req.URL.Path,
		escapedPath: req.URL.EscapedPath(),
		auth:        req.Header.Get("Authorization"),
		agentHeader: req.Header.Get("X-Agent-ID"),
		contentType: req.Header.Get("Content-Type"),
		body:        raw,
	})
	status, body := r.status, r.body
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusCreated
	}
	if body == "" {
		body = `{"id":"msg-1","transport":"inbox","expires_at":null}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (r *dispatchRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *dispatchRecorder) first(t *testing.T) recordedDispatch {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("no dispatch request reached the stub relay")
	}
	return r.requests[0]
}

func readAllBounded(req *http.Request) ([]byte, error) {
	defer func() { _ = req.Body.Close() }()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := req.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// assertNoTaskMember walks a decoded JSON body and fails on any OBJECT KEY
// that addresses a task (task / task_id / task-id). The dispatch payload
// must never name one: the scheduler picks the LANE, the foreman picks the
// TASK.
func assertNoTaskMember(t *testing.T, body []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("deliver body is not JSON: %v (%s)", err, body)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				norm := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
				if norm == "task" || norm == "task_id" || strings.HasPrefix(norm, "task_") {
					t.Errorf("deliver body member %s.%s addresses a task: %s", path, k, body)
				}
				walk(v, path+"."+k)
			}
		case []any:
			for i, v := range n {
				walk(v, path)
				_ = i
			}
		}
	}
	walk(decoded, "")
}

func newDispatchClient(t *testing.T, url string, opts ...Option) *Client {
	fixed := time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC)
	opts = append([]Option{WithClock(func() time.Time { return fixed })}, opts...)
	return NewClient(true, url, "relay-token", "sched-a", opts...)
}

func TestDispatchPayloadNeverContainsTaskID(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	receipt, err := c.Dispatch(context.Background(), "agent-7", WorkItem{
		Lane:    "auger",
		Board:   "/home/kara/auger/.coding-hermes/board/tasks.jsonl",
		Workdir: "/home/kara/auger",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if receipt.AgentID != "agent-7" || receipt.CorrID == "" || receipt.MessageID != "msg-1" || receipt.Transport != "inbox" {
		t.Fatalf("receipt = %+v, want agent-7 / minted corr / msg-1 / inbox", receipt)
	}
	if receipt.IdempotentReplay {
		t.Fatalf("receipt marked idempotent_replay on a fresh accept: %+v", receipt)
	}

	got := rec.first(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/agents/agent-7/inbox" {
		t.Errorf("path = %s, want /agents/agent-7/inbox", got.path)
	}
	if got.auth != "Bearer relay-token" {
		t.Errorf("Authorization = %q, want the configured bearer", got.auth)
	}
	if got.agentHeader != "scheduler-sched-a" {
		t.Errorf("X-Agent-ID = %q, want scheduler-sched-a", got.agentHeader)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q", got.contentType)
	}

	var req dispatchRequest
	if err := json.Unmarshal(got.body, &req); err != nil {
		t.Fatalf("unmarshal deliver body: %v (%s)", err, got.body)
	}
	if req.Sender != "scheduler-sched-a" {
		t.Errorf("sender = %q, want scheduler-sched-a", req.Sender)
	}
	if req.RequestID != receipt.CorrID {
		t.Errorf("request_id = %q, want the receipt corr id %q", req.RequestID, receipt.CorrID)
	}
	if req.IdempotencyKey != receipt.CorrID {
		t.Errorf("idempotency_key = %q, want the receipt corr id %q (retry-safe)", req.IdempotencyKey, receipt.CorrID)
	}

	// THE PAYLOAD LAW: lane + board/workdir ref + corr id, and NOT a task id.
	wantKeys := map[string]bool{
		"kind": true, "lane": true, "board": true, "workdir": true,
		"corr_id": true, "scheduler_id": true, "issued_at": true,
	}
	for k, v := range req.Payload {
		if !wantKeys[k] {
			t.Errorf("payload carries unexpected key %q (value %v)", k, v)
		}
		if strings.Contains(strings.ToLower(k), "task") {
			t.Errorf("payload names a task (%q): the scheduler picks the LANE, the foreman picks the TASK", k)
		}
	}
	for k := range wantKeys {
		if _, ok := req.Payload[k]; !ok {
			t.Errorf("payload missing key %q", k)
		}
	}
	if req.Payload["kind"] != KindWorkDispatch {
		t.Errorf("payload.kind = %v, want %s", req.Payload["kind"], KindWorkDispatch)
	}
	if req.Payload["lane"] != "auger" {
		t.Errorf("payload.lane = %v, want auger", req.Payload["lane"])
	}
	if req.Payload["board"] != "/home/kara/auger/.coding-hermes/board/tasks.jsonl" {
		t.Errorf("payload.board = %v", req.Payload["board"])
	}
	if req.Payload["workdir"] != "/home/kara/auger" {
		t.Errorf("payload.workdir = %v", req.Payload["workdir"])
	}
	if req.Payload["corr_id"] != receipt.CorrID {
		t.Errorf("payload.corr_id = %v, want %q", req.Payload["corr_id"], receipt.CorrID)
	}
	if req.Payload["scheduler_id"] != "sched-a" {
		t.Errorf("payload.scheduler_id = %v, want sched-a", req.Payload["scheduler_id"])
	}
	if req.Payload["issued_at"] != "2026-10-02T15:30:00Z" {
		t.Errorf("payload.issued_at = %v, want the injected clock stamp", req.Payload["issued_at"])
	}

	// The raw wire body must contain no task-shaped MEMBER at all, at any
	// nesting depth (the board path is named tasks.jsonl and that is fine —
	// the law is about addressing a task, not about the word).
	assertNoTaskMember(t, got.body)
}

func TestDispatchCarriesCallerCorrID(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	receipt, err := c.Dispatch(context.Background(), "agent-7", WorkItem{
		Lane: "helix", Workdir: "/home/kara/helix", CorrID: "q-caller-42",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if receipt.CorrID != "q-caller-42" {
		t.Fatalf("receipt corr = %q, want the caller's q-caller-42", receipt.CorrID)
	}
	var req dispatchRequest
	if err := json.Unmarshal(rec.first(t).body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, got := range []string{req.RequestID, req.IdempotencyKey, req.Payload["corr_id"].(string)} {
		if got != "q-caller-42" {
			t.Errorf("correlation member = %q, want q-caller-42 on all three carriers", got)
		}
	}
}

func TestDispatchMintsCorrIDWhenEmpty(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	first, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	second, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if first.CorrID == "" || second.CorrID == "" {
		t.Fatalf("minted corr ids must be non-empty: %q / %q", first.CorrID, second.CorrID)
	}
	if first.CorrID == second.CorrID {
		t.Fatalf("two dispatches share a minted corr id: %q", first.CorrID)
	}
}

func TestDispatchRefusesIncompleteWorkItemWithoutIOWrite(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	cases := []struct {
		name string
		item WorkItem
		want string
	}{
		{"no lane", WorkItem{Workdir: "/w"}, "lane is required"},
		{"no board or workdir", WorkItem{Lane: "helix"}, "board or workdir reference is required"},
		{"blank lane", WorkItem{Lane: "   ", Board: "b"}, "lane is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Dispatch(context.Background(), "agent-7", tc.item)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
	if called.Load() {
		t.Fatal("a refused work item reached the relay: validation must run before any I/O")
	}
}

func TestDispatchRefusesEmptyAgentID(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	}))
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	_, err := c.Dispatch(context.Background(), "  ", WorkItem{Lane: "helix", Workdir: "/w"})
	if err == nil || !strings.Contains(err.Error(), "agent id is required") {
		t.Fatalf("err = %v, want an agent-id refusal", err)
	}
	if called.Load() {
		t.Fatal("an empty agent id reached the relay")
	}
}

func TestDispatchDisabledClientIsAnErrorNotANoOp(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	}))
	defer srv.Close()

	c := NewClient(false, srv.URL, "", "sched-a")
	receipt, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled (a disabled dispatcher must never look like a successful hand-out)", err)
	}
	if receipt != (DispatchReceipt{}) {
		t.Fatalf("receipt = %+v, want the zero receipt on failure", receipt)
	}
	if called.Load() {
		t.Fatal("a disabled client opened a connection")
	}
}

func TestDispatchUnknownAgentIsLoudAndNotRetried(t *testing.T) {
	rec := &dispatchRecorder{status: 404, body: `{"error":"agent not found: \"ghost\""}`}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	receipt, err := c.Dispatch(context.Background(), "ghost", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDispatchTargetUnknown) {
		t.Fatalf("err = %v, want ErrDispatchTargetUnknown", err)
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "agent not found") {
		t.Fatalf("err = %v, want the target id and the relay's reason", err)
	}
	if receipt != (DispatchReceipt{}) {
		t.Fatalf("receipt = %+v, want zero on failure", receipt)
	}
	if rec.count() != 1 {
		t.Fatalf("relay saw %d requests, want exactly 1 (no fallback transport, no retry on a definitive refusal)", rec.count())
	}
}

func TestDispatchRefusalIsLoudAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"payload is required"}`},
		{403, `{"error":"GUARD_BLOCKED"}`},
		{429, `{"error":"ingest budget"}`},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			rec := &dispatchRecorder{status: tc.status, body: tc.body}
			srv := httptest.NewServer(rec)
			defer srv.Close()

			c := newDispatchClient(t, srv.URL)
			_, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
			if !errors.Is(err, ErrDispatchRefused) {
				t.Fatalf("err = %v, want ErrDispatchRefused", err)
			}
			if rec.count() != 1 {
				t.Fatalf("relay saw %d requests, want exactly 1", rec.count())
			}
			if !strings.Contains(err.Error(), "agent-7") {
				t.Errorf("err = %v, want the target id in the attempt record", err)
			}
		})
	}
}

func TestDispatchUnreachableIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := newDispatchClient(t, url)
	_, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDispatchUnreachable) {
		t.Fatalf("err = %v, want ErrDispatchUnreachable", err)
	}
	if !strings.Contains(err.Error(), "agent-7") {
		t.Errorf("err = %v, want the target id in the attempt record", err)
	}
}

func TestDispatchServerErrorIsUnreachable(t *testing.T) {
	rec := &dispatchRecorder{status: 502, body: `{"error":"FEDERATION_FAILED","detail":"no link reachable"}`}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	_, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDispatchUnreachable) {
		t.Fatalf("err = %v, want ErrDispatchUnreachable for a 502", err)
	}
	if !strings.Contains(err.Error(), "FEDERATION_FAILED") {
		t.Errorf("err = %v, want the relay's error member", err)
	}
}

func TestDispatchParsesIdempotentReplay(t *testing.T) {
	rec := &dispatchRecorder{body: `{"id":"msg-9","transport":"inbox","idempotent_replay":true}`}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	receipt, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w", CorrID: "q-retry"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !receipt.IdempotentReplay || receipt.MessageID != "msg-9" {
		t.Fatalf("receipt = %+v, want an idempotent replay of msg-9", receipt)
	}
}

func TestDispatchParsesHeldAccept(t *testing.T) {
	rec := &dispatchRecorder{status: 202, body: `{"status":"held"}`}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	receipt, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if receipt.Status != "held" || receipt.MessageID != "" {
		t.Fatalf("receipt = %+v, want status=held with no message id", receipt)
	}
}

func TestDispatchUnreadableAcceptBodyIsFailed(t *testing.T) {
	rec := &dispatchRecorder{status: 201, body: `not json`}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	_, err := c.Dispatch(context.Background(), "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDispatchFailed) {
		t.Fatalf("err = %v, want ErrDispatchFailed (an accept we cannot read is not a receipt)", err)
	}
}

func TestDispatchEscapesAgentIDInPath(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := newDispatchClient(t, srv.URL)
	if _, err := c.Dispatch(context.Background(), "box 01/agent", WorkItem{Lane: "helix", Workdir: "/w"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := rec.first(t).escapedPath; got != "/agents/box%2001%2Fagent/inbox" {
		t.Fatalf("escaped path = %q, want the agent id percent-escaped as one segment", got)
	}
}

func TestDispatchHonoursContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-block
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer close(block)

	c := newDispatchClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Dispatch(ctx, "agent-7", WorkItem{Lane: "helix", Workdir: "/w"})
	if !errors.Is(err, ErrDispatchUnreachable) {
		t.Fatalf("err = %v, want ErrDispatchUnreachable on a cancelled caller context", err)
	}
}
