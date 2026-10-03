package main

// REMOTE-011 test scaffolding: a minimal in-process Crier relay stub so the
// CLI's REAL transport path (bus.Client.Query over POST /relay/publish +
// GET /relay/subscribe/{topic} WebSocket frames) is exercised end to end.
// The upgrade handshake is REAL: the stub computes Sec-WebSocket-Accept
// from the client's own Sec-WebSocket-Key (RFC 6455 §4.2.2), so the bus
// client's dial validation passes honestly. Same shape as the REMOTE-009
// bus package's own queryStubRelay (that one is unexported to package bus;
// the CLI carries its own copy).

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsGUID is the RFC 6455 WebSocket GUID appended to the client key before
// hashing (the accept computation every conforming peer runs).
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// cliStubFrame mirrors the relay's subscribe frame: one text frame per
// published event.
type cliStubFrame struct {
	Topic string          `json:"topic"`
	Event json.RawMessage `json:"event"`
}

// cliStubPublish records one POST /relay/publish body.
type cliStubPublish struct {
	topic string
	event json.RawMessage
	reply string
}

// cliStubRelay is the in-process relay: it records publishes and forwards
// frames to subscribed sockets (exact topic match — the CLI tests address
// literal topics only).
type cliStubRelay struct {
	mu   sync.Mutex
	srv  *httptest.Server
	pubs []cliStubPublish
	subs map[string]net.Conn
}

func newCliStubRelay(t *testing.T) *cliStubRelay {
	t.Helper()
	r := &cliStubRelay{subs: map[string]net.Conn{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/relay/publish", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Topic      string          `json:"topic"`
			Event      json.RawMessage `json:"event"`
			ReplyTopic string          `json:"reply_topic"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		r.mu.Lock()
		r.pubs = append(r.pubs, cliStubPublish{topic: body.Topic, event: body.Event, reply: body.ReplyTopic})
		targets := make([]net.Conn, 0)
		if c, ok := r.subs[body.Topic]; ok {
			targets = append(targets, c)
		}
		r.mu.Unlock()
		payload, _ := json.Marshal(cliStubFrame{Topic: body.Topic, Event: body.Event})
		for _, c := range targets {
			_ = writeServerTextFrame(c, payload)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/relay/subscribe/", func(w http.ResponseWriter, req *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		// Honest handshake: echo the accept hash of the CLIENT's own
		// nonce — exactly what a conforming relay computes.
		key := req.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])
		pattern := strings.TrimPrefix(req.URL.Path, "/relay/subscribe/")
		// REGISTER BEFORE THE 101 (INT-CI-176): the real relay owns the
		// subscription before the upgrade bytes hit the wire, and the
		// stub must too. wsDial returns the moment the 101 is read, and
		// the requester immediately publishes; a registration racing
		// that publish on a loaded runner is a LOST REPLY (the publish
		// forward consults r.subs, finds nobody, and drops silently) —
		// the deterministic arm64/loaded-host red seen since
		// 2026-10-02. With the registration ordered before the 101
		// write, "dial returned" provably implies "subscription
		// visible": total order over one mutex, no clock involved.
		r.mu.Lock()
		r.subs[pattern] = &bufferedBareConn{Conn: conn, r: buf.Reader}
		r.mu.Unlock()
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		r.mu.Lock()
		conns := r.subs
		r.subs = map[string]net.Conn{}
		r.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		r.srv.Close()
	})
	return r
}

// awaitNowSubscribed waits up to d for a REGISTERED subscription on
// pattern. It replaces awaitSubscription (INT-CI-176): the old helper
// proved the same thing but its 2ms sleep cadence was a tuned poll — the
// guarantee it checks is unchanged, the clock arming is just deadline-
// derived now. Callers need this because the subscribe-before-publish
// window is what makes a lost reply impossible: once this returns, a
// publish forward for `pattern` finds the conn in r.subs.
func (r *cliStubRelay) awaitNowSubscribed(t *testing.T, pattern string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		r.mu.Lock()
		_, ok := r.subs[pattern]
		r.mu.Unlock()
		if ok {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no subscriber appeared on %q within %s", pattern, d)
}

// bufferedBareConn keeps the hijacked reader reachable (the conn returned
// by Hijack is the raw socket; the buffered reader may hold pipelined
// bytes — unused by these tests but kept so Close order is honest).
type bufferedBareConn struct {
	net.Conn
	r interface{ Read(p []byte) (int, error) }
}

// writeServerTextFrame writes one unmasked server→client text frame
// (RFC 6455: servers do not mask; FIN+text opcode 0x81).
func writeServerTextFrame(c net.Conn, payload []byte) error {
	hdr := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if _, err := c.Write(hdr); err != nil {
		return err
	}
	_, err := c.Write(payload)
	return err
}
