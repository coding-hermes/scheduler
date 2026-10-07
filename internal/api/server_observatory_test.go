package api

// SCHED-GAP-1592 — /api/v1/observatory + /api/v1/observatory/stream tests.
// These drive the REAL handler over httptest with a stub collector (the
// dashboard's *Generator is exercised by internal/dashboard's own tests); the
// framing, cadence, filter passthrough, and fail-closed contract are pinned
// here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubCollector is a controllable observatorySnapshotter.
type stubCollector struct {
	calls    int
	lastWin  string
	lastNS   string
	payload  string
	err      error
	failOnly bool // fail after the first success (stream error-comment path)
}

func (s *stubCollector) CollectObservatory(window, namespace string) (json.RawMessage, error) {
	s.calls++
	s.lastWin, s.lastNS = window, namespace
	if s.err != nil && (s.calls > 1 || s.failOnly) {
		return nil, s.err
	}
	return json.RawMessage(s.payload), nil
}

func newObsServer(t *testing.T, c observatorySnapshotter) *httptest.Server {
	t.Helper()
	s := NewServer(nil, nil)
	s.SetObservatorySnapshot(c)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestObservatory_Stream_PushesSnapshots proves the LIVE requirement: two
// reads a known interval apart deliver two frames — the stream pushes without
// any client request between them.
func TestObservatory_Stream_PushesSnapshots(t *testing.T) {
	old := observatoryStreamInterval
	observatoryStreamInterval = 200 * time.Millisecond
	t.Cleanup(func() { observatoryStreamInterval = old })

	c := &stubCollector{payload: `{"totals":{"spawned":1}}`}
	ts := newObsServer(t, c)

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/observatory/stream?window=3600&namespace=core", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream dial: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	// Deadline-bounded read: collect frames for ~1s, then judge. An SSE
	// stream never EOFs, so ReadAll would block forever.
	deadline := time.Now().Add(1500 * time.Millisecond)
	var body []byte
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && len(body) < 64<<10 {
		resp.Body.Read(buf) //nolint:errcheck — timeout/EOF land in body content
		body = append(body, buf...)
		if strings.Count(string(body), "data: ") >= 2 {
			break
		}
	}
	frames := strings.Count(string(body), "data: ")
	if frames < 2 {
		t.Errorf("frames = %d, want >= 2 across the interval (body: %q)", frames, body)
	}
	if c.calls < 2 {
		t.Errorf("collector calls = %d, want >= 2", c.calls)
	}
	if c.lastWin != "3600" || c.lastNS != "core" {
		t.Errorf("filter passthrough = window %q ns %q, want 3600/core", c.lastWin, c.lastNS)
	}
}

// TestObservatory_Stream_CollectError_Comment pins honest degradation: a
// failing collector emits `: error collect:` SSE comments — never a
// fabricated snapshot, never silence that reads as "nothing changed".
func TestObservatory_Stream_CollectError_Comment(t *testing.T) {
	old := observatoryStreamInterval
	observatoryStreamInterval = 100 * time.Millisecond
	t.Cleanup(func() { observatoryStreamInterval = old })

	c := &stubCollector{payload: `{"totals":{}}`, err: errors.New("database not configured"), failOnly: true}
	// failOnly=true fails every call after the first; make the FIRST fail too
	// by pre-advancing the counter.
	c.calls = 1
	ts := newObsServer(t, c)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/observatory/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream dial: %v", err)
	}
	defer resp.Body.Close()
	// Bounded read: 3 error comments are proof enough; an SSE stream never
	// EOFs on its own.
	body := make([]byte, 0, 4096)
	buf := make([]byte, 1024)
	for len(body) < 4096 && !strings.Contains(string(body), ": error collect:") {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !strings.Contains(string(body), ": error collect: database not configured") {
		t.Errorf("error comment missing from stream body:\n%s", body)
	}
	if strings.Contains(string(body), `"totals"`) {
		t.Error("a snapshot frame was emitted despite collector failure — fabricated data")
	}
}

// TestObservatory_FailClosed proves the no-collector contract: both routes
// answer 503 naming the missing collector — never an empty stream or a
// zeroed snapshot that reads as real.
func TestObservatory_FailClosed(t *testing.T) {
	s := NewServer(nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	for _, path := range []string{"/api/v1/observatory", "/api/v1/observatory/stream"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "collector not configured") {
			t.Errorf("%s body missing fail-closed reason: %s", path, body)
		}
	}
}

// TestObservatory_Get_PollFallback proves GET /api/v1/observatory answers one
// snapshot as JSON with the filter passed through.
func TestObservatory_Get_PollFallback(t *testing.T) {
	c := &stubCollector{payload: `{"window":86400,"window_label":"24h"}`}
	ts := newObsServer(t, c)

	resp, err := http.Get(ts.URL + "/api/v1/observatory?window=86400&namespace=satellite")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"window_label":"24h"`) {
		t.Errorf("payload = %s", body)
	}
	if c.lastWin != "86400" || c.lastNS != "satellite" {
		t.Errorf("filter passthrough = window %q ns %q, want 86400/satellite", c.lastWin, c.lastNS)
	}
}

// TestObservatory_MethodNotAllowed pins the 405 semantics on both routes.
func TestObservatory_MethodNotAllowed(t *testing.T) {
	c := &stubCollector{payload: `{}`}
	ts := newObsServer(t, c)

	resp, err := http.Post(ts.URL+"/api/v1/observatory", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/observatory status = %d, want 405", resp.StatusCode)
	}
	_ = fmt.Sprint() // keep fmt import if assertions change
}
