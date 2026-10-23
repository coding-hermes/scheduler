package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/dashboard"
)

// PERF-001-R3-R2 — slow-request observation.
//
// The row's fourth acceptance criterion, stated from the live failure: the
// first GET / after an idle window took 79.685s and the daemon logs NO request
// record for it at all (`grep 'GET / ' scheduler.log` → 0 matches), so a slow
// first paint was unrecoverable after the fact. Two things were missing:
//
//  1. a request record naming path + duration + status, and
//  2. the WAIT REASON — a duration alone re-opens the investigation, because
//     the overview render is a pipeline (DB → lanes → CI warm → enrichment).
//
// This middleware supplies both. It owns the per-request dashboard.RenderTrace
// (put into the request context, marked by collect()'s stages) and prints one
// line for every request that exceeds the threshold:
//
//	SLOW-REQ: GET / 79.685342s status=200 bytes=135376 wait=ci-warm[78.1s,in-flight] stages=db-projects[1.2s],ci-warm[78.1s],... served=stale[84s]
//
// The threshold comes from SCHEDULER_SLOW_REQUEST_MS (default 1000 ms; 0 or
// negative disables the logging entirely). Requests under the threshold log
// nothing — the fleet log is already 47MB and a per-request INFO line would
// bury the tick records operators actually read.
//
// Clock discipline: instants come from clock.Real() (the daemon's stdlib guard
// forbids a bare time.Now outside internal/clock).

const slowRequestThresholdEnv = "SCHEDULER_SLOW_REQUEST_MS"

// slowRequestThresholdDefault is the "this request is a problem" bar: above
// the warm render (5-6ms) and above a normal page (tens of ms) by two orders
// of magnitude, below the multi-second waits this row is about.
const slowRequestThresholdDefault = 1000 * time.Millisecond

// slowRequestThreshold resolves the env knob once per call site; it is called
// from the middleware constructor at boot, not per request.
func slowRequestThreshold() time.Duration {
	raw := strings.TrimSpace(os.Getenv(slowRequestThresholdEnv))
	if raw == "" {
		return slowRequestThresholdDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return slowRequestThresholdDefault
	}
	if n <= 0 {
		return 0 // disabled
	}
	return time.Duration(n) * time.Millisecond
}

// statusRecorder captures the status code and body size a handler wrote so the
// slow line states what the operator actually received.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK // net/http implicit-200 rule
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush keeps streaming handlers (the SSE event stream) working through the
// wrapper — without it a wrapped ResponseWriter silently buffers.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// slowRequests wraps next with the per-request render trace and the slow-line
// logger. It is mounted OUTSIDE the pprof mux so pprof and API requests are
// observed too (the trace simply has no stages for them).
func slowRequests(next http.Handler) http.Handler {
	threshold := slowRequestThreshold()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clk := clock.Real()
		start := clk.Now()
		tr := dashboard.NewRenderTrace(clk)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(dashboard.WithRenderTrace(r.Context(), tr)))
		if threshold <= 0 {
			return
		}
		dur := clk.Since(start)
		if dur < threshold {
			return
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		summary := tr.Summary()
		if summary == "" {
			// No stage instrumentation for this surface (API, pprof, static):
			// say so rather than implying the path was instrumented and clean.
			summary = "wait=not-instrumented"
		}
		log.Printf("SLOW-REQ: %s %s %s status=%d bytes=%d %s",
			r.Method, r.URL.RequestURI(), dur.Round(time.Millisecond), status, rec.bytes, summary)
	})
}
