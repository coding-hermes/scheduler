package bus

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsServerWriteFrame writes one UNMASKED server frame (RFC 6455 §5.1).
func wsServerWriteFrame(conn net.Conn, op wsOpcode, payload []byte) error {
	hdr := []byte{0x80 | byte(op)}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	_, err := conn.Write(append(hdr, payload...))
	return err
}

// wsServerReadFrame reads one client frame and unmasks it.
func wsServerReadFrame(conn net.Conn) (wsOpcode, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := readFull(conn, hdr); err != nil {
		return 0, nil, err
	}
	op := wsOpcode(hdr[0] & 0x0F)
	masked := hdr[1]&0x80 != 0
	n := int(hdr[1] & 0x7F)
	if n == 126 {
		ext := make([]byte, 2)
		if _, err := readFull(conn, ext); err != nil {
			return 0, nil, err
		}
		n = int(ext[0])<<8 | int(ext[1])
	}
	var mask [4]byte
	if masked {
		if _, err := readFull(conn, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := readFull(conn, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// wsTestServer hijacks the upgrade like a real relay: validates the
// handshake, answers 101, then runs the test's frame script.
type wsTestServer struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	gotPath    string
	gotUpgrade string
	gotKey     string
	gotAuth    string
	gotAgent   string
	frame      func(conn net.Conn) // the server-side frame script
}

// capture records the request headers from the server goroutine. The
// fields are written inside the HTTP handler and read later from the
// test goroutine, so every access must hold ws.mu (race detector
// verified, INT-CI-172).
func (ws *wsTestServer) capture(r *http.Request) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.gotPath = r.URL.EscapedPath()
	ws.gotUpgrade = r.Header.Get("Upgrade")
	ws.gotKey = r.Header.Get("Sec-WebSocket-Key")
	ws.gotAuth = r.Header.Get("Authorization")
	ws.gotAgent = r.Header.Get("X-Agent-ID")
}

// headerKeyLocked returns the captured Sec-WebSocket-Key. Caller must
// hold ws.mu.
func (ws *wsTestServer) headerKeyLocked() string {
	return ws.gotKey
}

// snapshot returns a consistent copy of the captured handshake data,
// safe to call from the test goroutine.
func (ws *wsTestServer) snapshot() (path, upgrade, key, auth, agent string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.gotPath, ws.gotUpgrade, ws.gotKey, ws.gotAuth, ws.gotAgent
}

func newWSTestServer(t *testing.T, script func(conn net.Conn)) *wsTestServer {
	ws := &wsTestServer{t: t, frame: script}
	ready := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.capture(r)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		h := sha1.New()
		h.Write([]byte(ws.headerKeyLocked() + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		// Drain client frames (pong/close) in the background so the
		// conn buffers never block the script.
		go func() {
			br := bufio.NewReader(buf)
			for {
				if _, err := br.ReadByte(); err != nil {
					return
				}
			}
		}()
		close(ready)
		ws.frame(conn)
	})
	ws.srv = httptest.NewServer(handler)
	t.Cleanup(ws.srv.Close)
	return ws
}

// counterID builds a counter-minted event id the way the client does.
func counterID(scheduler string, n uint64) string {
	return fmt.Sprintf("%s-%020d", scheduler, n)
}

func TestSubscribe_ReceivesDocumentedFramesEndToEnd(t *testing.T) {
	evt := Envelope{
		SchedulerID: "beta", EventID: counterID("beta", 1),
		Kind: KindTickTerminal, Project: "p", TickID: "t-1", Status: "completed",
	}
	frameBody, _ := json.Marshal(frame{
		Topic: "sched.tick.beta",
		Event: mustJSON(t, evt),
	})
	ws := newWSTestServer(t, func(conn net.Conn) {
		_ = wsServerWriteFrame(conn, wsOpText, frameBody) // the event
		_ = wsServerWriteFrame(conn, wsOpText, frameBody) // duplicate replay
		// Leave the conn open; the test closes the client side.
		time.Sleep(2 * time.Second)
	})

	c := NewClient(true, ws.srv.URL, "", "alpha")
	s := NewSubscriber(c)
	events := s.Events()
	if events == nil {
		t.Fatal("Events() nil on an enabled client")
	}
	subDone := make(chan error, 1)
	go func() { subDone <- s.Subscribe(context.Background(), "sched.tick.>") }()

	var got []IngestedEvent
	deadline := time.After(3 * time.Second)
	for len(got) < 1 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before the first event")
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out; got %d events", len(got))
		}
	}
	s.Close() // tears the conn down; Subscribe returns ErrClosed
	if err := <-subDone; !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe err = %v, want ErrClosed", err)
	}
	if len(got) != 1 {
		t.Fatalf("ingested %d events, want exactly 1 — the duplicate replay must be dropped", len(got))
	}
	e := got[0]
	if e.Envelope != evt {
		t.Errorf("envelope mismatch: %+v", e.Envelope)
	}
	if e.Topic != "sched.tick.beta" {
		t.Errorf("Topic = %q, want the literal published topic from the frame", e.Topic)
	}
	if e.Pattern != "sched.tick.>" {
		t.Errorf("Pattern = %q", e.Pattern)
	}
	if e.Self || e.KnownScheduler {
		t.Errorf("a beta event must not read as self/known on alpha: %+v", e)
	}
	// The handshake was the documented upgrade.
	gotPath, gotUpgrade, gotKey, _, _ := ws.snapshot()
	if !strings.HasSuffix(gotPath, "/relay/subscribe/sched.tick.%3E") {
		t.Errorf("upgrade path = %q", gotPath)
	}
	if gotUpgrade != "websocket" || gotKey == "" {
		t.Errorf("upgrade headers missing: upgrade=%q key=%q", gotUpgrade, gotKey)
	}
}

func TestSubscribe_AuthHeadersPassThrough(t *testing.T) {
	block := make(chan struct{})
	ws := newWSTestServer(t, func(conn net.Conn) {
		<-block // hold the conn; the test asserts then closes
	})
	c := NewClient(true, ws.srv.URL, "relay-token", "alpha")
	s := NewSubscriber(c)
	done := make(chan error, 1)
	go func() { done <- s.Subscribe(context.Background(), "sched.tick.>") }()
	time.Sleep(100 * time.Millisecond)
	_, _, _, auth, agent := ws.snapshot()
	if auth != "Bearer relay-token" {
		t.Errorf("Authorization = %q, want Bearer relay-token", auth)
	}
	if agent != "scheduler-alpha" {
		t.Errorf("X-Agent-ID = %q", agent)
	}
	s.Close()
	<-done
}

func TestSubscribe_MalformedAndMismatchedFramesDropped(t *testing.T) {
	bad := []byte(`{not json`)
	mismatch := mustJSON(t, frame{Topic: "sched.tick.beta", Event: mustJSON(t, Envelope{
		SchedulerID: "beta", EventID: counterID("gamma", 5), // id owner ≠ scheduler_id
		Kind: KindTickTerminal, Project: "p", TickID: "t", Status: "failed",
	})})
	incomplete := mustJSON(t, frame{Topic: "sched.tick.beta", Event: mustJSON(t, Envelope{
		SchedulerID: "beta", EventID: counterID("beta", 2), // missing kind/project/tick/status
	})})
	ws := newWSTestServer(t, func(conn net.Conn) {
		_ = wsServerWriteFrame(conn, wsOpText, bad)
		_ = wsServerWriteFrame(conn, wsOpText, mismatch)
		_ = wsServerWriteFrame(conn, wsOpText, incomplete)
		_ = wsServerWriteFrame(conn, wsOpBinary, []byte{0xde, 0xad})
		time.Sleep(1500 * time.Millisecond)
	})
	c := NewClient(true, ws.srv.URL, "", "alpha")
	s := NewSubscriber(c)
	events := s.Events()
	done := make(chan error, 1)
	go func() { done <- s.Subscribe(context.Background(), "sched.tick.>") }()

	select {
	case ev := <-events:
		t.Fatalf("ingested event %+v from malformed traffic — all four frames must drop", ev)
	case <-time.After(700 * time.Millisecond):
	}
	s.Close()
	<-done
}

func TestSubscriber_DisabledAndNilAreNoOps(t *testing.T) {
	disabled := NewSubscriber(NewClient(false, "http://127.0.0.1:1", "", "alpha"))
	if err := disabled.Subscribe(context.Background(), "sched.tick.>"); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled Subscribe = %v, want ErrDisabled", err)
	}
	if disabled.Events() != nil {
		t.Error("disabled Events() must be nil")
	}
	disabled.Run(context.Background()) // returns immediately
	disabled.Close()                   // safe twice
	disabled.Close()
	var nilSub *Subscriber
	nilSub.Close() // nil-safe
}

func TestSeenSet_DropsDuplicateKeysAndPrunes(t *testing.T) {
	ss := newSeenSet()
	env := Envelope{SchedulerID: "beta", EventID: counterID("beta", 1)}
	if !ss.Allow(env) {
		t.Fatal("first observation refused")
	}
	if ss.Allow(env) {
		t.Fatal("duplicate (scheduler_id, event_id) allowed — replay would double-apply")
	}
	// Same event id, different scheduler: a DIFFERENT key.
	other := env
	other.SchedulerID = "gamma"
	if !ss.Allow(other) {
		t.Error("distinct scheduler with the same event id refused — the key is the PAIR")
	}
	// Non-counter ids are remembered verbatim too.
	weird := Envelope{SchedulerID: "beta", EventID: "not-a-counter"}
	if !ss.Allow(weird) || ss.Allow(weird) {
		t.Error("verbatim id dedupe broken")
	}
	// The window prunes without losing the newest entries.
	for i := uint64(2); i <= windowPerPeer+10; i++ {
		ss.Allow(Envelope{SchedulerID: "beta", EventID: counterID("beta", i)})
	}
	if ss.Allow(Envelope{SchedulerID: "beta", EventID: counterID("beta", windowPerPeer+10)}) {
		t.Error("newest entry forgotten after prune")
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
