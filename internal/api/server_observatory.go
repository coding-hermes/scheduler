package api

// SCHED-GAP-1592 — GET /api/v1/observatory/stream: a Server-Sent Events feed
// of Observatory snapshots (the dashboard's live graphs' data source).
//
// Shape: PUSH, not poll — but unlike /api/v1/events/stream (which pushes one
// frame per committed event from the write-path hub), an Observatory
// snapshot is a COMPUTED aggregate over the tick history, so there is no
// commit to subscribe to. The stream therefore recomputes the snapshot on a
// fixed cadence (observatoryStreamInterval, 10s) and pushes it — a bounded,
// deliberate poll behind one held-open connection, so a dashboard page stays
// live without its own request loop and N viewers cost N cheap windowed
// queries per 10s, not N full-page renders.
//
// Honest degradation (the fleet's publish-real-numbers rule): while the
// compute fails, the stream emits `: error collect: …` SSE comments — never
// a fabricated snapshot, and never a silent gap the client would read as
// "nothing changed".
//
// Heartbeats: a `: heartbeat` comment every 15s of quiet, so idle proxies
// and clients do not tear the connection down (same contract as
// /api/v1/events/stream).
//
// Query params (validated by the dashboard package's own parsing):
//   - window: seconds (or shorthand 1h/6h/24h/7d) — default 6h
//   - namespace: namespace id filter — default all
//
// The dashboard's Generator does the actual collection (one code path for
// the page's inline snapshot, this stream, and the /api/v1/observatory poll
// fallback); the API side owns only the SSE framing and cadence.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// observatoryStreamInterval is the snapshot recompute cadence. A
// package-level var (not a const) so tests can shorten it.
var observatoryStreamInterval = 10 * time.Second

// observatorySnapshotter is the seam the dashboard's collector plugs into.
// The API package cannot import internal/dashboard (the dashboard imports
// api in comments only, but main.go wires them — keeping this an interface
// means the wiring stays in cmd/schedulerd and neither package imports the
// other).
type observatorySnapshotter interface {
	// ObservatorySnapshotJSON must write the snapshot for the given filter
	// as the body of a 200 response WITHOUT the status line tricks — i.e.
	// it is invoked here through a request-shaped contract: collect and
	// encode. To keep the API package free of dashboard types, the
	// implementation returns the encoded JSON bytes and an error.
	CollectObservatory(window, namespace string) (json.RawMessage, error)
}

// observatoryStream serves GET /api/v1/observatory/stream.
func (s *Server) observatoryStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	snap := s.observatorySnapshot
	if snap == nil {
		// Fail closed: without a wired collector there is nothing honest to
		// stream — a 503 that says so beats an empty stream.
		writeError(w, http.StatusServiceUnavailable, "observatory collector not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	q := r.URL.Query()
	window := q.Get("window")
	namespace := q.Get("namespace")

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disable proxy response buffering (nginx and friends).
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	ticker := s.clock().NewTicker(observatoryStreamInterval)
	defer ticker.Stop()

	// First snapshot goes out immediately — a page should not wait 10s for
	// its first live frame.
	emit := func() bool {
		payload, err := snap.CollectObservatory(window, namespace)
		if err != nil {
			// Headers are committed; the only honest channel is an SSE
			// comment. Never fabricate a snapshot to hide a compute failure.
			log.Printf("observatory stream: collect: %v", err)
			if _, werr := fmt.Fprintf(w, ": error collect: %s\n\n", sanitizeComment(err.Error())); werr != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !emit() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !emit() {
				return
			}
		}
	}
}

// observatoryGet serves GET /api/v1/observatory — the non-SSE poll fallback
// that answers one snapshot as JSON.
func (s *Server) observatoryGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.observatorySnapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "observatory collector not configured")
		return
	}
	payload, err := s.observatorySnapshot.CollectObservatory(
		r.URL.Query().Get("window"), r.URL.Query().Get("namespace"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "observatory collect: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
