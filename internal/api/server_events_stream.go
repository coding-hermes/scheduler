package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// GET /api/v1/events/stream is a Server-Sent Events feed — push, not poll: the
// handler subscribes to the database package's event hub
// (database.SubscribeEvents) and writes each committed event as it arrives, so
// a connected client sees a new event as soon as the write that produced it
// commits. The only queries it issues are the bounded replay/catch-up pages
// described below; there is no polling loop. These constants tune that feed.
const (
	// streamBufferSize bounds one subscriber's pending-event channel. It is
	// the client's burst allowance: a client that keeps up over any 64-event
	// window never sees a drop.
	streamBufferSize = 64

	// streamReplayPageSize bounds ONE replay query (rows per SELECT).
	streamReplayPageSize = 500

	// streamMaxReplayPages bounds the total work of a single replay/catch-up
	// (100 pages = 50k events). Each query is bounded by streamReplayPageSize;
	// this caps the loop so an absurd Last-Event-ID cannot pin the scheduler's
	// single database connection under a multi-minute replay. When it trips,
	// the stream says so explicitly (`: replay truncated`) rather than
	// silently skipping the remainder.
	streamMaxReplayPages = 100

	// streamReplayTail is how many of the newest events are replayed to a
	// client that connects without a usable Last-Event-ID. Replaying the whole
	// log for a fresh connection would be an unbounded read; the tail matches
	// the default page size of the poll endpoint (GET /api/v1/events?limit=100)
	// so a fresh SSE client and a fresh poll client see the same window.
	streamReplayTail = 100
)

// streamHeartbeatInterval is the SSE keepalive cadence: a comment line every
// 15s keeps idle proxies and clients from tearing down a quiet connection. A
// package-level var (not a const) so tests can shorten it instead of sleeping
// 15 seconds.
var streamHeartbeatInterval = 15 * time.Second

// eventsStream serves GET /api/v1/events/stream as a Server-Sent Events feed
// (CTL-002).
//
// Reconnect contract: the client sends Last-Event-ID (set automatically by
// EventSource from the last `id:` it saw) and this handler replays every
// persisted event with a greater ID before it waits for new ones. The
// subscription is taken BEFORE the replay query, so an event committed between
// the two is buffered here and cannot be lost; duplicates between the buffer
// and the replay are dropped by the ID comparison.
//
// An absent, empty or malformed Last-Event-ID (anything that is not a plain
// decimal integer) is treated as "no cursor" and replays the newest
// streamReplayTail events. Malformed input can never reach the query layer and
// can never panic.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// A ResponseWriter without Flush cannot stream; fail loudly instead of
		// buffering the whole feed behind the client's back.
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ctx := r.Context()
	cursor := parseLastEventID(r.Header.Get("Last-Event-ID"))

	// Subscribe first, replay second (see the reconnect contract above).
	events, unsubscribe := database.SubscribeEvents(s.db, streamBufferSize)
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disable proxy response buffering (nginx and friends) so events reach the
	// client immediately instead of in one flushed block.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay (or tail-seed) before waiting for live events.
	var err error
	if cursor > 0 {
		cursor, err = s.streamReplay(ctx, w, flusher, cursor, 0)
	} else {
		cursor, err = s.streamTail(ctx, w, flusher, streamReplayTail)
	}
	if err != nil {
		s.streamError(w, flusher, "replay", err)
		return
	}

	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client went away (or the server shut down): return promptly so
			// the deferred unsubscribe runs and no goroutine outlives the
			// request.
			return
		case ev, ok := <-events:
			if !ok {
				// The hub never closes subscriber channels; a closed channel
				// would otherwise spin this loop on zero-value events.
				return
			}
			// An event at or below the cursor is a duplicate of one the
			// replay already sent (or a late arrival from a previous gap
			// repair).
			if ev.ID <= cursor {
				continue
			}
			// A skip means the subscriber dropped events while this client was
			// slow: repair from the log before emitting the live event.
			if ev.ID > cursor+1 {
				repaired, rerr := s.streamReplay(ctx, w, flusher, cursor, ev.ID)
				cursor = repaired
				if rerr != nil {
					s.streamError(w, flusher, "catch-up", rerr)
					return
				}
				if ev.ID <= cursor {
					continue
				}
			}
			if err := writeSSEEvent(w, flusher, ev); err != nil {
				return
			}
			cursor = ev.ID
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// streamTail emits the newest n events (chronological) and returns the ID of
// the last one written (0 when the log is empty). Used for a client that
// connects without a usable Last-Event-ID — a bounded read, never the whole
// log.
func (s *Server) streamTail(ctx context.Context, w io.Writer, flusher http.Flusher, n int) (int64, error) {
	events, err := database.ListEventsAfterID(ctx, s.db, 0, n)
	if err != nil {
		return 0, err
	}
	var cursor int64
	for _, e := range events {
		if err := writeSSEEvent(w, flusher, e); err != nil {
			return cursor, err
		}
		cursor = e.ID
	}
	return cursor, nil
}

// streamReplay pages events with id > afterID and emits them in ID order,
// stopping at upTo (0 = no upper bound) or when the log is exhausted. It
// returns the ID of the last event written plus an error. The loop terminates
// because every page strictly advances the cursor; each query is capped at
// streamReplayPageSize rows and the whole loop at streamMaxReplayPages pages.
// A tripped page cap emits a `: replay truncated` comment so the client can
// tell the difference between "quiet" and "incomplete".
func (s *Server) streamReplay(ctx context.Context, w io.Writer, flusher http.Flusher, afterID, upTo int64) (int64, error) {
	cursor := afterID
	for page := 0; page < streamMaxReplayPages; page++ {
		events, err := database.ListEventsAfterID(ctx, s.db, cursor, streamReplayPageSize)
		if err != nil {
			return cursor, err
		}
		if len(events) == 0 {
			return cursor, nil
		}
		for _, e := range events {
			if upTo > 0 && e.ID > upTo {
				return cursor, nil
			}
			if err := writeSSEEvent(w, flusher, e); err != nil {
				return cursor, err
			}
			cursor = e.ID
		}
		if len(events) < streamReplayPageSize {
			return cursor, nil
		}
		if upTo > 0 && cursor >= upTo {
			return cursor, nil
		}
		if page == streamMaxReplayPages-1 {
			if _, err := io.WriteString(w, ": replay truncated\n\n"); err != nil {
				return cursor, err
			}
			flusher.Flush()
		}
	}
	return cursor, nil
}

// streamError reports a mid-stream failure. The response headers are already
// committed (200 + text/event-stream), so the only honest channel left is an
// SSE comment: it is not parsed as data by an EventSource client and it keeps
// `curl` output self-documenting. The failure is also logged for operators.
func (s *Server) streamError(w io.Writer, flusher http.Flusher, stage string, err error) {
	log.Printf("events stream: %s: %v", stage, err)
	if _, werr := fmt.Fprintf(w, ": error %s: %s\n\n", stage, sanitizeComment(err.Error())); werr != nil {
		return
	}
	flusher.Flush()
}

// sanitizeComment collapses newlines so an error message can never break the
// single-line SSE comment it is embedded in.
func sanitizeComment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// writeSSEEvent writes one frame — an id line (the value a reconnecting client
// echoes back as Last-Event-ID) and a single-line JSON data payload — and
// flushes it. It returns any write error so the caller can end the stream.
func writeSSEEvent(w io.Writer, flusher http.Flusher, e database.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event %d: %w", e.ID, err)
	}
	if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// parseLastEventID parses the Last-Event-ID request header into a cursor. Any
// value that is not a plain non-negative decimal integer — absent, empty,
// whitespace, "abc", "-1", "+7", "7x", an overflowing number — is reported as
// 0 ("no cursor") instead of an error: an SSE reconnect cannot act on a 400,
// and an unparsed value must never reach the query layer.
//
// Digits-only (rather than strconv.ParseInt's grammar) is deliberate: the
// client is supposed to echo an id this server emitted, so a sign or any other
// decoration means it is not echoing one of our ids, and normalizing it would
// silently suppress a real event that happens to be assigned that id.
func parseLastEventID(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// Out of int64 range: not an id this server could have emitted.
		return 0
	}
	return id
}
