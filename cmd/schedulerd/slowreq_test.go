package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// PERF-001-R3-R2 AC4: a slow request leaves a log line naming path, duration
// and the wait reason. The live failure: the first GET / took 79.685s and
// `grep 'GET / ' scheduler.log` matched NOTHING, so the slow paint was
// unrecoverable after the fact.

// captureLog redirects the standard logger into a buffer for the duration of
// the test and returns the buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestSlowRequestsLogsSlowRequest proves the observer emits exactly one line
// for a request over the threshold, naming method, path, duration, status and
// size, plus an explicit "no instrumentation" wait reason for surfaces the
// dashboard trace does not cover.
func TestSlowRequestsLogsSlowRequest(t *testing.T) {
	t.Setenv(slowRequestThresholdEnv, "10")
	buf := captureLog(t)

	h := slowRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?page=2", nil))

	line := buf.String()
	if !strings.Contains(line, "SLOW-REQ: GET /?page=2") {
		t.Fatalf("no SLOW-REQ line naming the path; log=%q", line)
	}
	if !strings.Contains(line, "status=418") {
		t.Errorf("line must name the response status; log=%q", line)
	}
	if !strings.Contains(line, "bytes=5") {
		t.Errorf("line must name the bytes written; log=%q", line)
	}
	if !strings.Contains(line, "wait=") {
		t.Errorf("line must carry a wait reason; log=%q", line)
	}
	if strings.Count(line, "SLOW-REQ") != 1 {
		t.Errorf("want exactly one line, got %d: %q", strings.Count(line, "SLOW-REQ"), line)
	}
}

// TestSlowRequestsSilentWhenFast: the fleet log is already 47MB — a warm 5ms
// render must not log anything.
func TestSlowRequestsSilentWhenFast(t *testing.T) {
	t.Setenv(slowRequestThresholdEnv, "500")
	buf := captureLog(t)

	h := slowRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := buf.String(); strings.Contains(got, "SLOW-REQ") {
		t.Fatalf("fast request must not log; log=%q", got)
	}
}

// TestSlowRequestsDisabledByZero: SCHEDULER_SLOW_REQUEST_MS=0 turns the
// observer off (the documented escape hatch) without touching the handler.
func TestSlowRequestsDisabledByZero(t *testing.T) {
	t.Setenv(slowRequestThresholdEnv, "0")
	buf := captureLog(t)

	h := slowRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := buf.String(); strings.Contains(got, "SLOW-REQ") {
		t.Fatalf("threshold 0 must disable the line; log=%q", got)
	}
}

// TestSlowRequestThresholdResolution pins the env parsing: unset and
// unparsable fall back to the default (an operator typo must not silently
// disable the observation), <=0 disables.
func TestSlowRequestThresholdResolution(t *testing.T) {
	if got := slowRequestThreshold(); got != slowRequestThresholdDefault {
		t.Errorf("default = %v, want %v", got, slowRequestThresholdDefault)
	}
	t.Setenv(slowRequestThresholdEnv, "bogus")
	if got := slowRequestThreshold(); got != slowRequestThresholdDefault {
		t.Errorf("unparsable must fall back to default, got %v", got)
	}
	t.Setenv(slowRequestThresholdEnv, "250")
	if got := slowRequestThreshold(); got != 250*time.Millisecond {
		t.Errorf("250 = %v, want 250ms", got)
	}
	t.Setenv(slowRequestThresholdEnv, "-5")
	if got := slowRequestThreshold(); got != 0 {
		t.Errorf("negative must disable, got %v", got)
	}
}

// TestSlowRequestsStreamsStayFlushable: the SSE event stream must keep
// flushing through the wrapper (a buffered wrapper would silently break the
// live-event surface).
func TestSlowRequestsStreamsStayFlushable(t *testing.T) {
	t.Setenv(slowRequestThresholdEnv, "1000")
	h := slowRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped ResponseWriter must stay an http.Flusher")
		}
		f.Flush()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil))
}
