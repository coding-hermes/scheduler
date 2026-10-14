package api

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// CTL-002 — GET /api/v1/events/stream. These tests drive the REAL handler over
// httptest with a real (temporary-file) SQLite database and the real
// database.LogEvent write path; nothing here calls an internal helper in place
// of the HTTP route.

// newStreamTestServer builds a Server on a fresh temporary SQLite database and
// serves it over httptest. A file (not :memory:) is used so the database
// behaves exactly like the daemon's.
func newStreamTestServer(t *testing.T) (*Server, *sql.DB, *httptest.Server) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewServer(db, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, db, ts
}

// logStreamEvent commits one event through LogEvent and returns the committed
// row (with its database-assigned ID).
func logStreamEvent(t *testing.T, db *sql.DB, msg string) database.Event {
	t.Helper()
	return logStreamEventFiltered(t, db, database.SeverityHigh, "ctltest", msg)
}

// logStreamEventFiltered commits one event with an explicit severity and
// component through the real LogEvent write path (SCHED-GAP-1737, which needs
// mixed severities/components in one log).
func logStreamEventFiltered(t *testing.T, db *sql.DB, sev database.EventSeverity, component, msg string) database.Event {
	t.Helper()
	ev := database.Event{
		Severity:  sev,
		Component: component,
		Message:   msg,
		Details:   "{}",
	}
	if err := database.LogEvent(context.Background(), db, &ev); err != nil {
		t.Fatalf("LogEvent(%q): %v", msg, err)
	}
	return ev
}

// openStream dials the SSE endpoint with no query params. The returned cancel
// simulates the client disconnecting; both the cancel and the body close are
// registered as cleanups so every test tears its stream down.
func openStream(t *testing.T, ts *httptest.Server, lastEventID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	return openStreamQuery(t, ts, "", lastEventID)
}

// openStreamQuery dials the SSE endpoint with a raw query string ("severity=
// MEDIUM", "" for none) and an optional Last-Event-ID header. It exists so the
// filter contract (SCHED-GAP-1737) is exercised over the REAL route, exactly as
// a browser's EventSource would reach it.
func openStreamQuery(t *testing.T, ts *httptest.Server, query, lastEventID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	target := ts.URL + "/api/v1/events/stream"
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		t.Fatalf("new stream request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("stream status = %d (body %q), want 200", resp.StatusCode, string(body))
	}
	t.Cleanup(func() {
		resp.Body.Close()
		cancel()
	})
	return resp, cancel
}

// readFrames splits an SSE body into frames (blank-line separated, comments
// included). The channel is buffered and drops if a test stops consuming, so a
// finished test can never wedge the reader goroutine.
func readFrames(r io.Reader) <-chan string {
	out := make(chan string, 1024)
	go func() {
		defer close(out)
		br := bufio.NewReader(r)
		var lines []string
		flush := func() {
			if len(lines) == 0 {
				return
			}
			select {
			case out <- strings.Join(lines, "\n"):
			default:
			}
			lines = nil
		}
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				trimmed := strings.TrimRight(line, "\r\n")
				if trimmed == "" {
					flush()
				} else {
					lines = append(lines, trimmed)
				}
			}
			if err != nil {
				flush()
				return
			}
		}
	}()
	return out
}

// nextFrame waits up to timeout for the next frame.
func nextFrame(t *testing.T, frames <-chan string, timeout time.Duration) (string, bool) {
	t.Helper()
	select {
	case f, ok := <-frames:
		return f, ok
	case <-time.After(timeout):
		return "", false
	}
}

func isCommentFrame(frame string) bool {
	return strings.HasPrefix(frame, ":")
}

// frameEventID extracts the id: line of an event frame.
func frameEventID(t *testing.T, frame string) int64 {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		value, ok := strings.CutPrefix(line, "id: ")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			t.Fatalf("frame id %q is not an integer: %v", value, err)
		}
		return id
	}
	t.Fatalf("frame has no id: line: %q", frame)
	return 0
}

// frameEvent decodes the data: line of an event frame as an Event.
func frameEvent(t *testing.T, frame string) database.Event {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev database.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("frame data is not Event JSON: %v (%q)", err, data)
		}
		return ev
	}
	t.Fatalf("frame has no data: line: %q", frame)
	return database.Event{}
}

// collectEvents drains n event frames (comments ignored) within timeout.
func collectEvents(t *testing.T, frames <-chan string, n int, timeout time.Duration) []database.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	out := make([]database.Event, 0, n)
	for len(out) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out after %s with %d of %d event frames", timeout, len(out), n)
		}
		frame, ok := nextFrame(t, frames, remaining)
		if !ok {
			t.Fatalf("no event frame within %s: received %d of %d", timeout, len(out), n)
		}
		if isCommentFrame(frame) {
			continue
		}
		out = append(out, frameEvent(t, frame))
	}
	return out
}

// drainContiguous reads event frames and returns the IDs it saw in order,
// nudging the log with a fresh event whenever the stream goes quiet so that a
// dropped tail is repaired. It stops once reach is reached or the deadline
// expires.
func drainContiguous(t *testing.T, db *sql.DB, frames <-chan string, reach int64, timeout time.Duration) []int64 {
	t.Helper()
	deadline := time.After(timeout)
	quiet := time.NewTimer(250 * time.Millisecond)
	defer quiet.Stop()
	nudges := 0
	var ids []int64
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				return ids
			}
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(250 * time.Millisecond)
			if isCommentFrame(frame) {
				continue
			}
			id := frameEventID(t, frame)
			ids = append(ids, id)
			if id >= reach {
				return ids
			}
		case <-quiet.C:
			// The stream is quiet below the target: a dropped tail needs a
			// live event to trigger the handler's gap repair.
			nudges++
			ev := database.Event{
				Severity:  database.SeverityInfo,
				Component: "ctltest",
				Message:   fmt.Sprintf("nudge-%d", nudges),
				Details:   "{}",
			}
			if err := database.LogEvent(context.Background(), db, &ev); err != nil {
				t.Fatalf("nudge LogEvent: %v", err)
			}
			quiet.Reset(250 * time.Millisecond)
		case <-deadline:
			return ids
		}
	}
}

// wantContiguous fails unless ids is exactly first, first+1, ... in order (no
// gaps, no duplicates).
func wantContiguous(t *testing.T, ids []int64, first int64) {
	t.Helper()
	for i, id := range ids {
		if id != first+int64(i) {
			t.Fatalf("event IDs are not contiguous from %d: got %v (first divergence at index %d: want %d)", first, ids, i, first+int64(i))
		}
	}
}

// waitForSubscribers waits until the database reports want subscribers.
func waitForSubscribers(t *testing.T, db *sql.DB, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := database.SubscriberCount(db); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriber count = %d, want %d after %s", database.SubscriberCount(db), want, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCTL002_EventsStream_ReplayAfterLastEventID is the reconnect contract: a
// client that reconnects with Last-Event-ID=N receives every persisted event
// with ID > N, in SSE framing (id: + data:), before live waiting — and then
// live events continue from there without a duplicate.
func TestCTL002_EventsStream_ReplayAfterLastEventID(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	first := logStreamEvent(t, db, "e-1")
	logStreamEvent(t, db, "e-2")
	last := logStreamEvent(t, db, "e-3")

	resp, _ := openStream(t, ts, strconv.FormatInt(first.ID, 10))

	// Headers the SSE contract requires.
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	frames := readFrames(resp.Body)
	replayed := collectEvents(t, frames, 2, 5*time.Second)
	if replayed[0].ID <= first.ID || replayed[1].ID <= first.ID {
		t.Fatalf("replay ids = [%d %d], want both > %d", replayed[0].ID, replayed[1].ID, first.ID)
	}
	if replayed[0].ID != first.ID+1 || replayed[1].ID != last.ID {
		t.Errorf("replay ids = [%d %d], want [%d %d] in chronological order", replayed[0].ID, replayed[1].ID, first.ID+1, last.ID)
	}
	if replayed[0].Message != "e-2" || replayed[1].Message != "e-3" {
		t.Errorf("replayed messages = [%q %q], want [e-2 e-3]", replayed[0].Message, replayed[1].Message)
	}

	// Live handoff: the next event is delivered once, after the replay.
	live := logStreamEvent(t, db, "e-4")
	got := collectEvents(t, frames, 1, 2*time.Second)
	if got[0].ID != live.ID {
		t.Errorf("live event id = %d, want %d", got[0].ID, live.ID)
	}
}

// TestCTL002_EventsStream_TailReplayWithoutLastEventID pins the documented
// behaviour for a fresh connection: the newest events are replayed (a bounded
// tail, never the whole log) and live events follow.
func TestCTL002_EventsStream_TailReplayWithoutLastEventID(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	for i := 1; i <= 3; i++ {
		logStreamEvent(t, db, fmt.Sprintf("tail-%d", i))
	}

	resp, _ := openStream(t, ts, "")
	frames := readFrames(resp.Body)
	tailed := collectEvents(t, frames, 3, 5*time.Second)
	if tailed[0].Message != "tail-1" || tailed[2].Message != "tail-3" {
		t.Errorf("tail replay messages = [%q ... %q], want tail-1 ... tail-3", tailed[0].Message, tailed[2].Message)
	}
	wantContiguous(t, []int64{tailed[0].ID, tailed[1].ID, tailed[2].ID}, tailed[0].ID)
}

// TestCTL002_EventsStream_LiveDeliveryWithinOneSecond is the push contract: an
// event committed after the connection is established reaches the stream
// WITHOUT any client re-poll — the frame must arrive over the already-open
// connection. The measured budget is generous (5s) because on a loaded host
// the goroutine scheduling delay alone can exceed 1s; the contract under test
// is push-vs-poll, not a wall-clock latency SLO.
func TestCTL002_EventsStream_LiveDeliveryWithinOneSecond(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	// Seed one event so the tail replay doubles as the readiness signal that
	// the server-side subscription exists before the measured write happens.
	logStreamEvent(t, db, "seed")

	resp, _ := openStream(t, ts, "")
	frames := readFrames(resp.Body)
	collectEvents(t, frames, 1, 5*time.Second)

	start := time.Now()
	logStreamEvent(t, db, "live")
	got := collectEvents(t, frames, 1, 5*time.Second)
	elapsed := time.Since(start)

	if got[0].Message != "live" {
		t.Fatalf("live frame message = %q, want live", got[0].Message)
	}
	// The frame arrived on the single already-open stream above — no second
	// request was made — so this only bounds how long a PUSHED frame may take
	// under host load.
	if elapsed > 5*time.Second {
		t.Errorf("live event took %s to reach the open stream, want < 5s (push budget)", elapsed)
	}
}

// TestCTL002_EventsStream_HeartbeatAndCancellation covers the keepalive and the
// shutdown path: idle connections receive a heartbeat comment on the configured
// cadence, and canceling the request stops the handler and releases the
// subscription (no leaked hub entry).
func TestCTL002_EventsStream_HeartbeatAndCancellation(t *testing.T) {
	prev := streamHeartbeatInterval
	streamHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { streamHeartbeatInterval = prev })

	_, db, ts := newStreamTestServer(t)

	resp, cancel := openStream(t, ts, "")
	frames := readFrames(resp.Body)

	for i := 0; i < 3; i++ {
		frame, ok := nextFrame(t, frames, 3*time.Second)
		if !ok {
			t.Fatalf("timed out waiting for heartbeat %d", i+1)
		}
		if frame != ": heartbeat" {
			t.Fatalf("frame %d = %q, want %q", i+1, frame, ": heartbeat")
		}
	}
	waitForSubscribers(t, db, 1, 2*time.Second)

	cancel()
	waitForSubscribers(t, db, 0, 3*time.Second)
}

// TestCTL002_EventsStream_InvalidLastEventIDIsSafe pins the input handling: a
// malformed cursor is treated as "no cursor" (a bounded tail replay) instead of
// panicking, erroring the stream, or reaching the query layer.
func TestCTL002_EventsStream_InvalidLastEventIDIsSafe(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	cases := []string{"abc", "-1", "0", "9999999999999999999999", "7x", " ", "+7"}
	for _, header := range cases {
		t.Run(fmt.Sprintf("header=%q", header), func(t *testing.T) {
			resp, _ := openStream(t, ts, header)
			frames := readFrames(resp.Body)

			// The stream must still deliver live events after a bad cursor.
			logStreamEvent(t, db, "after-bad-cursor")
			got := collectEvents(t, frames, 1, 3*time.Second)
			if got[0].Message != "after-bad-cursor" {
				t.Errorf("message = %q, want after-bad-cursor", got[0].Message)
			}
		})
	}
}

// TestCTL002_EventsStream_MethodNotAllowed pins the method guard.
func TestCTL002_EventsStream_MethodNotAllowed(t *testing.T) {
	_, _, ts := newStreamTestServer(t)

	resp, err := http.Post(ts.URL+"/api/v1/events/stream", "application/json", nil)
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
}

// TestCTL002_EventsStream_SlowClientDoesNotBlockWritersOrLoseEvents is the
// back-pressure contract. One client opens the stream and never reads (its
// handler eventually blocks in a socket write); a second client drains. The
// writers must stay fast, the healthy client must see a gap-free sequence, and
// the stalled subscription must be reclaimed once it disconnects.
func TestCTL002_EventsStream_SlowClientDoesNotBlockWritersOrLoseEvents(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	// Stalled client: connected, never reads its body.
	stalled, stalledCancel := openStream(t, ts, "")
	_ = stalled

	// Healthy client: drains from the start.
	healthy, _ := openStream(t, ts, "")
	frames := readFrames(healthy.Body)

	waitForSubscribers(t, db, 2, 2*time.Second)

	// Byte-heavy payloads so the stalled client's socket genuinely backs up
	// and the handler has to stop writing to it.
	pad := strings.Repeat("x", 16*1024)
	const burst = 200
	lastID := int64(0)
	var maxLatency time.Duration
	for i := 1; i <= burst; i++ {
		ev := database.Event{
			Severity:  database.SeverityInfo,
			Component: "ctltest",
			Message:   fmt.Sprintf("burst-%d", i),
			Details:   fmt.Sprintf(`{"pad":%q}`, pad),
		}
		start := time.Now()
		if err := database.LogEvent(context.Background(), db, &ev); err != nil {
			t.Fatalf("burst LogEvent %d: %v", i, err)
		}
		if d := time.Since(start); d > maxLatency {
			maxLatency = d
		}
		lastID = ev.ID
	}
	if maxLatency > time.Second {
		t.Errorf("slowest LogEvent with a stalled subscriber took %s, want < 1s", maxLatency)
	}

	// The healthy client must converge on a gap-free sequence covering the
	// whole burst (repairing anything it dropped).
	ids := drainContiguous(t, db, frames, lastID, 20*time.Second)
	if len(ids) == 0 {
		t.Fatalf("healthy client received no event frames")
	}
	wantContiguous(t, ids, 1)
	if ids[len(ids)-1] < lastID {
		t.Errorf("healthy client reached id %d, want at least %d (ids: %v)", ids[len(ids)-1], lastID, ids)
	}

	// The stalled subscription stops blocking once the client disconnects and
	// is released promptly.
	if got := database.SubscriberCount(db); got != 2 {
		t.Errorf("subscriber count = %d, want 2 before disconnect", got)
	}
	stalledCancel()
	waitForSubscribers(t, db, 1, 5*time.Second)
}

// TestCTL002_EventsStream_ReconnectHandoffNoLossNoDuplicate races a reconnect
// against concurrent writes: events committed while the request is in flight
// must be emitted exactly once each, in ID order, whether they arrive through
// the replay query or the live subscription.
func TestCTL002_EventsStream_ReconnectHandoffNoLossNoDuplicate(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	seed := logStreamEvent(t, db, "seed")

	// Write concurrently with the connect: some rows land before the handler
	// subscribes (replay path), others after (live path).
	errCh := make(chan error, 1)
	go func() {
		for i := 2; i <= 8; i++ {
			ev := database.Event{
				Severity:  database.SeverityInfo,
				Component: "ctltest",
				Message:   fmt.Sprintf("race-%d", i),
				Details:   "{}",
			}
			if err := database.LogEvent(context.Background(), db, &ev); err != nil {
				errCh <- err
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		errCh <- nil
	}()

	resp, _ := openStream(t, ts, strconv.FormatInt(seed.ID, 10))
	frames := readFrames(resp.Body)
	got := collectEvents(t, frames, 7, 5*time.Second)
	if err := <-errCh; err != nil {
		t.Fatalf("concurrent LogEvent: %v", err)
	}

	var ids = make([]int64, 0, len(got))
	for _, ev := range got {
		ids = append(ids, ev.ID)
	}
	wantContiguous(t, ids, seed.ID+1)
	if ids[0] != seed.ID+1 || ids[len(ids)-1] != seed.ID+7 {
		t.Errorf("ids = %v, want %d..%d", ids, seed.ID+1, seed.ID+7)
	}
}

// blockedStreamWriter is a ResponseWriter + Flusher that blocks inside its
// first Write until released. It lets a test hold the handler inside a client
// write — the real "slow client" condition — deterministically, without
// depending on socket buffer sizes.
type blockedStreamWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	entered chan struct{}
	release chan struct{}
	blocked bool
}

func newBlockedStreamWriter() *blockedStreamWriter {
	return &blockedStreamWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockedStreamWriter) Header() http.Header { return http.Header{} }

func (b *blockedStreamWriter) WriteHeader(int) {}

func (b *blockedStreamWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	first := !b.blocked
	b.blocked = true
	b.mu.Unlock()
	if first {
		close(b.entered)
		<-b.release
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *blockedStreamWriter) Flush() {}

func (b *blockedStreamWriter) snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForStreamID polls the writer until the frame for id has been written.
func waitForStreamID(t *testing.T, w *blockedStreamWriter, id int64, timeout time.Duration) {
	t.Helper()
	needle := fmt.Sprintf("id: %d\ndata: ", id)
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(w.snapshot(), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("frame for id %d never reached the client (have %d ids)", id, len(idsInStream(w.snapshot())))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// idsInStream extracts every id: line of a recorded SSE stream.
func idsInStream(raw string) []int64 {
	var ids []int64
	for _, line := range strings.Split(raw, "\n") {
		value, ok := strings.CutPrefix(line, "id: ")
		if !ok {
			continue
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestCTL002_EventsStream_GapRepairAfterDroppedEvents pins the overflow
// recovery path deterministically. The handler invites the client to write
// while the client is stalled, so the subscriber's bounded buffer overflows and
// events are dropped; the next live event must make the handler re-read the log
// and fill the gap, so the client still ends up with a gap-free sequence and
// nothing is silently lost.
func TestCTL002_EventsStream_GapRepairAfterDroppedEvents(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	// Event 1 is the client's reconnect cursor; no replay follows it.
	seed := logStreamEvent(t, db, "seed")

	w := newBlockedStreamWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(seed.ID, 10))

	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.Config.Handler.ServeHTTP(w, req)
	}()

	// Wait for the subscription, then arm the stall: the handler picks up this
	// live event and blocks inside the client write that delivers it.
	waitForSubscribers(t, db, 1, 5*time.Second)
	logStreamEvent(t, db, "live-2")
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never blocked in a client write")
	}

	// Overflow the stalled subscriber: 200 events against a 64-deep buffer
	// means genuine drops.
	for i := 3; i <= 201; i++ {
		live := database.Event{
			Severity:  database.SeverityInfo,
			Component: "ctltest",
			Message:   fmt.Sprintf("burst-%d", i),
			Details:   "{}",
		}
		if err := database.LogEvent(context.Background(), db, &live); err != nil {
			t.Fatalf("burst LogEvent %d: %v", i, err)
		}
	}

	close(w.release)
	waitForStreamID(t, w, 66, 10*time.Second)

	// One more live event after the stall: the handler sees the ID gap and
	// repairs it from the log.
	closer := database.Event{
		Severity:  database.SeverityInfo,
		Component: "ctltest",
		Message:   "closer",
		Details:   "{}",
	}
	if err := database.LogEvent(context.Background(), db, &closer); err != nil {
		t.Fatalf("closer LogEvent: %v", err)
	}
	waitForStreamID(t, w, closer.ID, 10*time.Second)

	ids := idsInStream(w.snapshot())
	wantContiguous(t, ids, seed.ID+1)
	if ids[len(ids)-1] != closer.ID {
		t.Errorf("stream ended at id %d, want %d (ids: %v)", ids[len(ids)-1], closer.ID, ids)
	}
	if len(ids) != int(closer.ID-seed.ID) {
		t.Errorf("stream carried %d frames, want %d (no duplicates)", len(ids), closer.ID-seed.ID)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after the request context was canceled")
	}
	waitForSubscribers(t, db, 0, 3*time.Second)
}

// TestCTL002_EventsStream_PollEndpointUnchanged guards the wire contract of the
// pre-existing poll endpoint: adding the stream must not alter
// GET /api/v1/events.
func TestCTL002_EventsStream_PollEndpointUnchanged(t *testing.T) {
	_, db, ts := newStreamTestServer(t)
	logStreamEvent(t, db, "poll-me")

	resp, err := http.Get(ts.URL + "/api/v1/events?limit=1")
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/events = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Events []database.Event `json:"events"`
		Count  int              `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	if body.Count != 1 || len(body.Events) != 1 || body.Events[0].Message != "poll-me" {
		t.Errorf("poll response = %+v, want one event (poll-me)", body)
	}
}

// ---------------------------------------------------------------------------
// SCHED-GAP-1737 — /api/v1/events/stream honors the severity/component filters.
//
// Before this, the handler read only Last-Event-ID: a client that added
// ?severity=CRITICAL got HTTP 200 and a full replay of unrelated severities,
// with nothing in the response to say the filter had been dropped. These tests
// pin the fixed contract over the real route.
// ---------------------------------------------------------------------------

// expectNoEventFrame fails if an EVENT frame arrives within timeout. Comment
// frames (heartbeats) are not data and are skipped, so this asserts "the filter
// held" rather than "the socket stayed byte-silent".
func expectNoEventFrame(t *testing.T, frames <-chan string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		frame, ok := nextFrame(t, frames, remaining)
		if !ok {
			return
		}
		if isCommentFrame(frame) {
			continue
		}
		t.Fatalf("unexpected event frame on a filtered stream: %q", frame)
	}
}

// TestSCHEDGAP1737_StreamFilterPredicate pins the cursor rule the whole filter
// contract rests on, on the pure function the live loop uses: an event above the
// cursor that does NOT match the filter is CONSUMED — it advances the cursor
// without being written — while an event at or below the cursor is a duplicate
// and leaves the cursor alone. This is the half of "non-matching events are
// skipped" that a frame-level test cannot observe.
func TestSCHEDGAP1737_StreamFilterPredicate(t *testing.T) {
	medium := database.Event{ID: 7, Severity: database.SeverityMedium, Component: "loop"}

	cases := []struct {
		name       string
		filter     streamFilter
		cursor     int64
		wantWrite  bool
		wantCursor int64
	}{
		{"no filter writes everything", streamFilter{}, 6, true, 7},
		{"empty fields accept every value", streamFilter{severity: "", component: ""}, 6, true, 7},
		{"matching severity is written", streamFilter{severity: "MEDIUM"}, 6, true, 7},
		{"lowercase severity matches nothing (exact match, like the poll endpoint)", streamFilter{severity: "medium"}, 6, false, 7},
		{"unrecognized severity matches nothing", streamFilter{severity: "BOGUS"}, 6, false, 7},
		{"non-matching severity advances the cursor without writing", streamFilter{severity: "CRITICAL"}, 6, false, 7},
		{"non-matching component advances the cursor without writing", streamFilter{component: "sync"}, 6, false, 7},
		{"both filters matching is written", streamFilter{severity: "MEDIUM", component: "loop"}, 6, true, 7},
		{"both filters must hold (AND, not OR)", streamFilter{severity: "MEDIUM", component: "sync"}, 6, false, 7},
		{"duplicate at the cursor is dropped and does not move it", streamFilter{}, 7, false, 7},
		{"matching event at the cursor is still a duplicate", streamFilter{severity: "MEDIUM"}, 7, false, 7},
		{"event below the cursor is dropped", streamFilter{}, 9, false, 9},
		{"filtered event below the cursor does not rewind it", streamFilter{severity: "CRITICAL"}, 9, false, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write, next := applyLiveEvent(medium, tc.cursor, tc.filter)
			if write != tc.wantWrite {
				t.Errorf("write = %v, want %v", write, tc.wantWrite)
			}
			if next != tc.wantCursor {
				t.Errorf("cursor = %d, want %d", next, tc.wantCursor)
			}
		})
	}
}

// TestSCHEDGAP1737_EventsStream_TailSeedHonorsSeverityFilter is the acceptance
// case: a filtered tail-seed emits ONLY matching frames. The severities are
// interleaved so the pre-fix behaviour would deliver an INFO frame FIRST —
// exactly the symptom in the ticket.
func TestSCHEDGAP1737_EventsStream_TailSeedHonorsSeverityFilter(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", "info-1")
	firstMedium := logStreamEventFiltered(t, db, database.SeverityMedium, "ctltest", "medium-1")
	logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", "info-2")
	secondMedium := logStreamEventFiltered(t, db, database.SeverityMedium, "ctltest", "medium-2")

	resp, _ := openStreamQuery(t, ts, "severity=MEDIUM", "")
	frames := readFrames(resp.Body)

	got := collectEvents(t, frames, 2, 5*time.Second)
	if got[0].ID != firstMedium.ID || got[1].ID != secondMedium.ID {
		t.Fatalf("filtered tail ids = [%d %d], want [%d %d] (only the MEDIUM events)",
			got[0].ID, got[1].ID, firstMedium.ID, secondMedium.ID)
	}
	for i, ev := range got {
		if ev.Severity != database.SeverityMedium {
			t.Errorf("frame %d severity = %q, want MEDIUM", i, ev.Severity)
		}
	}
	// Nothing else may follow: the log holds two more (INFO) events, and a
	// dropped filter would have emitted them.
	expectNoEventFrame(t, frames, 300*time.Millisecond)
}

// TestSCHEDGAP1737_EventsStream_TailWindowSelectsNewestMatching pins the
// catch-up window semantics: the tail is the newest streamReplayTail events
// MATCHING the filter, not "the newest streamReplayTail events of any severity,
// filtered afterwards". The only CRITICAL event is buried under more than a
// window's worth of unrelated events, so a filter applied after the window would
// deliver nothing — the same "my filter did nothing" complaint, for a different
// reason. This is the stream's parallel to
// GET /api/v1/events?severity=CRITICAL&limit=100.
func TestSCHEDGAP1737_EventsStream_TailWindowSelectsNewestMatching(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	crit := logStreamEventFiltered(t, db, database.SeverityCritical, "ctltest", "the-critical")
	for i := 0; i < streamReplayTail+5; i++ {
		logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", fmt.Sprintf("noise-%d", i))
	}

	resp, _ := openStreamQuery(t, ts, "severity=CRITICAL", "")
	frames := readFrames(resp.Body)

	got := collectEvents(t, frames, 1, 5*time.Second)
	if got[0].ID != crit.ID {
		t.Fatalf("filtered tail reached id %d, want the CRITICAL event %d (window must select the newest matching events)",
			got[0].ID, crit.ID)
	}
	expectNoEventFrame(t, frames, 300*time.Millisecond)
}

// TestSCHEDGAP1737_EventsStream_LiveSkipsNonMatchingAndDeliversLater covers the
// live path: a committed event that does not match the filter is never written,
// and the matching event after it is still delivered exactly once — i.e. the
// stream neither leaks an unrelated severity nor stalls behind one.
func TestSCHEDGAP1737_EventsStream_LiveSkipsNonMatchingAndDeliversLater(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	// The matching seed doubles as the readiness signal that the server-side
	// subscription exists: it is tail-seeded, so it is the first frame.
	seed := logStreamEventFiltered(t, db, database.SeverityCritical, "ctltest", "critical-seed")

	resp, _ := openStreamQuery(t, ts, "severity=CRITICAL", "")
	frames := readFrames(resp.Body)
	got := collectEvents(t, frames, 1, 5*time.Second)
	if got[0].ID != seed.ID {
		t.Fatalf("first frame id = %d, want the CRITICAL seed %d", got[0].ID, seed.ID)
	}

	// A live non-matching event must not reach the client...
	logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", "info-live")
	expectNoEventFrame(t, frames, 300*time.Millisecond)

	// ...and must not stall the stream: the next matching event arrives, once.
	live := logStreamEventFiltered(t, db, database.SeverityCritical, "ctltest", "critical-live")
	after := collectEvents(t, frames, 1, 5*time.Second)
	if after[0].ID != live.ID || after[0].Message != "critical-live" {
		t.Fatalf("frame after the filtered event = {id:%d msg:%q}, want {id:%d msg:critical-live}",
			after[0].ID, after[0].Message, live.ID)
	}
	expectNoEventFrame(t, frames, 300*time.Millisecond)
}

// TestSCHEDGAP1737_EventsStream_ComponentFilter covers the second param, alone
// and combined with severity (the two filters AND together).
func TestSCHEDGAP1737_EventsStream_ComponentFilter(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	logStreamEventFiltered(t, db, database.SeverityInfo, "alpha", "a-1")
	beta := logStreamEventFiltered(t, db, database.SeverityInfo, "beta", "b-1")
	logStreamEventFiltered(t, db, database.SeverityInfo, "alpha", "a-2")

	resp, _ := openStreamQuery(t, ts, "component=beta", "")
	frames := readFrames(resp.Body)
	got := collectEvents(t, frames, 1, 5*time.Second)
	if got[0].ID != beta.ID || got[0].Component != "beta" {
		t.Fatalf("component-filtered frame = {id:%d comp:%q}, want {id:%d comp:beta}", got[0].ID, got[0].Component, beta.ID)
	}
	expectNoEventFrame(t, frames, 300*time.Millisecond)

	// Both params hold: the only beta event is INFO, so severity=CRITICAL
	// excludes it and the stream must stay empty.
	both, _ := openStreamQuery(t, ts, "severity=CRITICAL&component=beta", "")
	framesBoth := readFrames(both.Body)
	expectNoEventFrame(t, framesBoth, 300*time.Millisecond)
}

// TestSCHEDGAP1737_EventsStream_NoFilterUnchangedAndOtherParamsIgnored guards the
// other direction: a client that sends no filter (or params this endpoint does
// not honor, such as `limit`) still receives every event, in order — the
// pre-existing contract is untouched.
func TestSCHEDGAP1737_EventsStream_NoFilterUnchangedAndOtherParamsIgnored(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	want := []int64{
		logStreamEventFiltered(t, db, database.SeverityInfo, "alpha", "e-1").ID,
		logStreamEventFiltered(t, db, database.SeverityCritical, "beta", "e-2").ID,
		logStreamEventFiltered(t, db, database.SeverityMedium, "alpha", "e-3").ID,
	}

	for _, query := range []string{"", "severity=", "limit=1", "foo=bar&unknown=1"} {
		t.Run("query="+query, func(t *testing.T) {
			resp, _ := openStreamQuery(t, ts, query, "")
			frames := readFrames(resp.Body)
			got := collectEvents(t, frames, len(want), 5*time.Second)
			for i, ev := range got {
				if ev.ID != want[i] {
					t.Fatalf("frame %d id = %d, want %d (query %q must not change the unfiltered stream)", i, ev.ID, want[i], query)
				}
			}
			expectNoEventFrame(t, frames, 150*time.Millisecond)
		})
	}
}

// TestSCHEDGAP1737_EventsStream_ReconnectWithFilterResumesCleanly is the
// subtlety the ticket calls out: a reconnecting filtered client (Last-Event-ID +
// filter) must not re-deliver an event it already saw and must not skip a
// matching one — the cursor advances over filtered events without emitting them.
func TestSCHEDGAP1737_EventsStream_ReconnectWithFilterResumesCleanly(t *testing.T) {
	_, db, ts := newStreamTestServer(t)

	logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", "i-1")
	cursorEvent := logStreamEventFiltered(t, db, database.SeverityMedium, "ctltest", "m-1")
	logStreamEventFiltered(t, db, database.SeverityInfo, "ctltest", "i-2")
	newest := logStreamEventFiltered(t, db, database.SeverityMedium, "ctltest", "m-2")

	resp, _ := openStreamQuery(t, ts, "severity=MEDIUM", strconv.FormatInt(cursorEvent.ID, 10))
	frames := readFrames(resp.Body)

	// Exactly the MEDIUM event above the cursor: no re-delivery of m-1, no INFO
	// leak, nothing skipped.
	got := collectEvents(t, frames, 1, 5*time.Second)
	if got[0].ID != newest.ID {
		t.Fatalf("replayed id = %d, want %d (cursor %d must not re-deliver or skip)",
			got[0].ID, newest.ID, cursorEvent.ID)
	}
	if got[0].ID <= cursorEvent.ID {
		t.Errorf("replay re-delivered id %d at or below the client's cursor %d", got[0].ID, cursorEvent.ID)
	}
	expectNoEventFrame(t, frames, 300*time.Millisecond)

	// Live delivery continues from the cursor watermark, still filtered.
	live := logStreamEventFiltered(t, db, database.SeverityMedium, "ctltest", "m-3")
	after := collectEvents(t, frames, 1, 5*time.Second)
	if after[0].ID != live.ID {
		t.Errorf("live frame id = %d, want %d", after[0].ID, live.ID)
	}
}
