package bus

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// subDialTimeout bounds the subscription handshake (GET + 101). The
// subscribe loop itself is LONG-LIVED: the deadline below covers only the
// dial, never the open stream.
const subDialTimeout = 10 * time.Second

// maxBackoff is the reconnect backoff ceiling for the subscribe loop.
const maxBackoff = 30 * time.Second

// frame is the documented subscribe frame: one text frame per published
// event, `{"topic": "<literal>", "event": {...}}` (openapi 101 response).
// reply_topic is the REMOTE-009 bus-query transport hint: the requester's
// correlated reply topic carried OUTSIDE the envelope on the request
// publish (the relay itself ignores unknown body members; a visibility
// frame simply never sets it).
type frame struct {
	Topic      string          `json:"topic"`
	Event      json.RawMessage `json:"event"`
	ReplyTopic string          `json:"reply_topic,omitempty"`
}

// Publisher is the minimal client surface the subscriber needs: mint
// monotonic ids, resolve a frame against the known scheduler ids, and read
// the topic list. Satisfied by *Client.
type Publisher interface {
	NextEventID() string
	SchedulerID() string
	IsSchedulerID(id string) bool
	KnownSchedulerIDs() []string
	Topics() []string
}

// ErrDisabled is returned by Subscribe on a disabled client. Callers treat
// it as "nothing to do", never as a failure.
var ErrDisabled = errors.New("bus: crier disabled")

// ErrClosed is returned by Subscribe when the connection was torn down by
// Close (a requested shutdown, not a bus failure).
var ErrClosed = errors.New("bus: subscriber closed")

// ── Minimal RFC 6455 client ────────────────────────────────────────────────
// Dependency-free on purpose (brief constraint: net/http +
// encoding/json). Only what Crier's relay needs: client → masked text
// frames, server → unmasked text frames, ping/pong, close handshake.

type wsOpcode byte

const (
	wsOpText   wsOpcode = 0x1
	wsOpBinary wsOpcode = 0x2
	wsOpClose  wsOpcode = 0x8
	wsOpPing   wsOpcode = 0x9
	wsOpPong   wsOpcode = 0xA
)

// wsMaskCounter feeds the client frame masking keys (RFC 6455 §5.3). The
// mask exists to defeat intermediary cache poisoning, not to keep secrets,
// so a counter+time mix through a hasher is sufficient variance.
var wsMaskCounter atomic.Uint64

func wsNextMaskKey() [4]byte {
	var key [4]byte
	// MASK ENTROPY only (defeats cache poisoning) — not scheduling time;
	// routed through the clock seam per SCHED-GAP-169.
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", wsMaskCounter.Add(1), clock.Real().Now().UnixNano())))
	copy(key[:], h[:4])
	return key
}

// wsConn is one WebSocket connection: the (buffered) net.Conn plus the
// client nonce used at handshake.
type wsConn struct {
	conn      net.Conn
	clientKey string
	writeMu   sync.Mutex
}

// wsDial performs the opening handshake (RFC 6455 §4.1): GET upgrade with
// the Sec-WebSocket-Key nonce and opt-in auth headers, then validates the
// 101, the Sec-WebSocket-Accept hash, and that the server invented neither
// extensions nor a subprotocol (the client requests neither).
func wsDial(ctx context.Context, rawURL, token, agentID string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bus: bad subscribe url %q: %w", rawURL, err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" || u.Scheme == "https" {
			host = u.Host + ":443"
		} else {
			host = u.Host + ":80"
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("bus: dial %s: %w", host, err)
	}
	// The nonce is exactly 16 base64 bytes (RFC 6455 §4.1).
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("bus: handshake nonce: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce)

	req := strings.Join([]string{
		"GET " + u.RequestURI() + " HTTP/1.1",
		"Host: " + u.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + key,
		"Sec-WebSocket-Version: 13",
	}, "\r\n")
	if token != "" {
		// Opt-in auth (Crier CR_AUTH_TOKEN), same rule as publish.
		req += "\r\nAuthorization: Bearer " + token
	}
	if agentID != "" {
		req += "\r\nX-Agent-ID: " + agentID
	}
	req += "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("bus: send upgrade: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("bus: read upgrade response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("bus: subscribe handshake answered %d", resp.StatusCode)
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		// RFC 6455 §4.1: a client MUST fail the connection if the response
		// contains an extension it did not ask for. The client asks for
		// none, so ANY extension answer is a protocol failure.
		_ = conn.Close()
		return nil, errors.New("bus: server offered unexpected Sec-WebSocket-Extensions")
	}
	if resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		_ = conn.Close()
		return nil, errors.New("bus: server chose a subprotocol the client never listed")
	}
	accept := base64.StdEncoding.EncodeToString(wsAcceptHash(key))
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != accept {
		_ = conn.Close()
		return nil, errors.New("bus: bad Sec-WebSocket-Accept")
	}

	// Bytes the HTTP response parse left buffered belong to the WebSocket
	// stream — wrap the conn so the first frame is never lost.
	return &wsConn{conn: &wsBufferedConn{br: br, Conn: conn}, clientKey: key}, nil
}

// wsAcceptHash is RFC 6455 §1.3: SHA1(key + GUID), raw (the caller base64s).
func wsAcceptHash(key string) []byte {
	h := sha1.New()
	_, _ = h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return h.Sum(nil)
}

// wsBufferedConn chains the handshake's bufio.Reader in front of the raw
// conn so already-read bytes stay in the frame stream.
type wsBufferedConn struct {
	br *bufio.Reader
	net.Conn
}

func (w *wsBufferedConn) Read(p []byte) (int, error) { return w.br.Read(p) }

// wsWriteFrame writes one masked client frame (RFC 6455 §5.1: a client
// MUST mask every frame it sends).
func (c *wsConn) wsWriteFrame(op wsOpcode, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	hdr := []byte{0x80 | byte(op)} // FIN + opcode; no fragmentation
	n := len(payload)
	const maskBit = byte(0x80)
	switch {
	case n < 126:
		hdr = append(hdr, maskBit|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, maskBit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, maskBit|127,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	key := wsNextMaskKey()
	hdr = append(hdr, key[:]...)
	masked := make([]byte, n)
	for i, b := range payload {
		masked[i] = b ^ key[i%4]
	}
	_, err := c.conn.Write(append(hdr, masked...))
	return err
}

// wsReadFrame reads one complete server frame. Server frames are unmasked
// (RFC 6455 §5.1); a masked server frame is a protocol violation.
func (c *wsConn) wsReadFrame() (wsOpcode, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[0]&0x70 != 0 {
		return 0, nil, fmt.Errorf("bus: reserved RSV bits set (0x%02x)", hdr[0])
	}
	op := wsOpcode(hdr[0] & 0x0F)
	if hdr[0]&0x80 == 0 {
		return 0, nil, errors.New("bus: fragmented server frame (FIN=0) unsupported")
	}
	if hdr[1]&0x80 != 0 {
		return 0, nil, errors.New("bus: masked server frame (protocol violation)")
	}
	n := int(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.conn, ext[:]); err != nil {
			return 0, nil, err
		}
		n = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.conn, ext[:]); err != nil {
			return 0, nil, err
		}
		if ext[0]&0x80 != 0 {
			return 0, nil, errors.New("bus: negative frame length")
		}
		n = int(uint64(ext[0])<<56 | uint64(ext[1])<<48 | uint64(ext[2])<<40 |
			uint64(ext[3])<<32 | uint64(ext[4])<<24 | uint64(ext[5])<<16 |
			uint64(ext[6])<<8 | uint64(ext[7]))
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// wsClose starts the closing handshake (RFC 6455 §5.5.1): a masked close
// frame with status 1000 (normal closure). The peer's close echo is not
// awaited — the caller closes the TCP conn right after.
func (c *wsConn) wsClose() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	payload := []byte{0x03, 0xE8} // 1000 normal closure
	hdr := make([]byte, 0, 2+4+len(payload))
	hdr = append(hdr, 0x88, 0x80|byte(len(payload)))
	key := wsNextMaskKey()
	hdr = append(hdr, key[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ key[i%4]
	}
	_, _ = c.conn.Write(append(hdr, masked...))
}

// Close tears down the underlying connection.
func (c *wsConn) close() { _ = c.conn.Close() }

// Subscriber consumes peer events from the relay. Construction follows the
// spooled-outbox seam convention (internal/sync): an unexported struct,
// exported constructor, nil client = no-op. It runs one long-lived
// connection per configured topic pattern; every failure mode is
// LOG AND DROP — the visibility flow can never fail the scheduler
// (autonomy law, §3).
type Subscriber struct {
	mu     sync.Mutex
	closed bool
	conns  map[string]*wsConn

	pub     Publisher
	baseURL string
	token   string
	agentID string

	seen   *seenSet
	events chan IngestedEvent
	now    func() time.Time

	wg   sync.WaitGroup
	done chan struct{}
}

// NewSubscriber builds the subscriber for a client. A nil client (or one
// whose Enabled() is false) yields a no-op subscriber: Run returns
// immediately, Events returns nil.
func NewSubscriber(c *Client) *Subscriber {
	s := &Subscriber{
		now:  time.Now,
		done: make(chan struct{}),
	}
	if c == nil || !c.Enabled() {
		return s
	}
	s.pub = c
	s.baseURL = c.baseURL
	s.token = c.token
	s.agentID = c.agentID
	s.seen = newSeenSet()
	s.events = make(chan IngestedEvent, 64)
	return s
}

// Events returns the ingest channel: one IngestedEvent per valid,
// non-duplicate peer frame. Nil on a disabled client ("nothing to
// subscribe"). The channel is closed by Close after all connections stop.
func (s *Subscriber) Events() <-chan IngestedEvent {
	if s == nil {
		return nil
	}
	return s.events
}

// isClosed reports whether Close has been requested.
func (s *Subscriber) isClosed() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Subscribe blocks, consuming frames for one topic pattern until the
// connection drops, ctx is done, or Close runs. A disabled client returns
// ErrDisabled immediately. Reconnect + redial is runOne's job.
func (s *Subscriber) Subscribe(ctx context.Context, pattern string) error {
	if s == nil || s.pub == nil {
		return ErrDisabled
	}
	dialCtx, cancel := context.WithTimeout(ctx, subDialTimeout)
	conn, err := s.dial(dialCtx, pattern)
	cancel()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.close()
		return ErrClosed
	}
	if s.conns == nil {
		s.conns = map[string]*wsConn{}
	}
	s.conns[pattern] = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, pattern)
		s.mu.Unlock()
		conn.wsClose()
		conn.close()
	}()

	for {
		op, payload, err := conn.wsReadFrame()
		if err != nil {
			// A locally-requested Close tears the conn down under the
			// reader — that is a shutdown, not a bus failure: surface
			// ErrClosed so the caller does not retry. A REMOTE drop
			// (relay restart, network cut) keeps the raw error; runOne
			// logs it and redials.
			if s.isClosed() {
				return ErrClosed
			}
			return err // read failure — runOne logs and redials
		}
		switch op {
		case wsOpPing:
			_ = conn.wsWriteFrame(wsOpPong, payload)
		case wsOpClose:
			// RFC 6455 §5.5.1: answer with a close frame, then tear down.
			_ = conn.wsWriteFrame(wsOpClose, nil)
			return ErrClosed
		case wsOpText:
			s.handleFrame(pattern, payload)
		case wsOpBinary:
			// Documented frames are JSON text; a binary frame carries no
			// event. Log and drop (autonomy law).
			log.Printf("CRIER: binary frame on %s dropped", pattern)
		default:
			// Pong or unexpected opcode — nothing to ingest.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// dial opens the subscribe WebSocket for one topic pattern.
func (s *Subscriber) dial(ctx context.Context, pattern string) (*wsConn, error) {
	u := strings.TrimSuffix(s.baseURL, "/") + "/relay/subscribe/" + pattern
	return wsDial(ctx, u, s.token, s.agentID)
}

// handleFrame decodes one text frame and enqueues the ingest event, or
// logs-and-drops. THE ONLY CONTENT GATE IS DEDUPE (spec §5: "the ingest
// side drops a duplicate key"): an id the local process cannot resolve to a
// known scheduler is still ingested — REMOTE-006's merged view may know
// schedulers this process does not. The envelope's scheduler_id is the
// PUBLISHER's truth; the wildcard topic the frame arrived on is the
// transport label and is never trusted for identity.
func (s *Subscriber) handleFrame(pattern string, payload []byte) {
	var fr frame
	if err := json.Unmarshal(payload, &fr); err != nil {
		log.Printf("CRIER: malformed frame dropped: %v", err)
		return
	}
	var env Envelope
	if err := json.Unmarshal(fr.Event, &env); err != nil {
		log.Printf("CRIER: malformed event dropped: %v", err)
		return
	}
	if err := env.Validate(); err != nil {
		log.Printf("CRIER: invalid envelope dropped: %v", err)
		return
	}
	// A counter-minted id whose embedded owner disagrees with the
	// envelope's scheduler_id is malformed at the mint site.
	if id, ok := ParseEventID(env.EventID); ok && id != env.SchedulerID {
		log.Printf("CRIER: id/scheduler mismatch dropped: %q vs %q", env.EventID, env.SchedulerID)
		return
	}
	if !s.seen.Allow(env) {
		log.Printf("CRIER: duplicate dropped: %s/%s", env.SchedulerID, env.EventID)
		return
	}
	ing := IngestedEvent{
		Envelope:       env,
		Topic:          fr.Topic,
		Pattern:        pattern,
		IngestedAt:     s.now().UTC(),
		Self:           s.pub.IsSchedulerID(env.SchedulerID),
		KnownScheduler: s.pub.IsSchedulerID(env.SchedulerID),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ing:
	default:
		log.Printf("CRIER: ingest buffer full — event dropped: %s/%s", env.SchedulerID, env.EventID)
	}
}

// Run starts one consuming loop per configured topic pattern and blocks
// until every loop has exited (ctx done or Close). The autonomy law holds
// here too: a permanently-down relay is a slow log-and-retry loop, never an
// error surfaced anywhere.
func (s *Subscriber) Run(ctx context.Context) {
	if s == nil || s.pub == nil {
		return
	}
	for _, pattern := range s.pub.Topics() {
		if pattern == "" {
			continue
		}
		s.wg.Add(1)
		go s.runOne(ctx, pattern)
	}
	s.wg.Wait()
}

// runOne is one pattern's reconnect loop: dial, consume, back off, repeat,
// until ctx is done or Close ran.
func (s *Subscriber) runOne(ctx context.Context, pattern string) {
	defer s.wg.Done()
	backoff := time.Second
	now := s.now
	for ctx.Err() == nil {
		started := now()
		err := s.Subscribe(ctx, pattern)
		if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		log.Printf("CRIER: subscribe %s dropped: %v (retrying)", pattern, err)
		if now().Sub(started) > maxBackoff {
			backoff = time.Second // the session was healthy — reset the ladder
		}
		if !s.wait(ctx, backoff) {
			return
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// wait sleeps d, interrupted by ctx or Close. False = interrupted.
func (s *Subscriber) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.done:
		return false
	case <-t.C:
		return true
	}
}

// Close tears down every connection and stops the subscriber. Blocks until
// all per-topic loops have exited, then closes the Events channel. Safe to
// call twice; safe to call on a disabled subscriber.
func (s *Subscriber) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	close(s.done)
	for _, c := range conns {
		c.wsClose()
		c.close()
	}
	s.wg.Wait()
	if s.events != nil {
		close(s.events)
	}
}

// SetIngestClock installs the clock the ingest stamps read (SCHED-GAP-169
// convention). nil keeps the wall clock.
func (s *Subscriber) SetIngestClock(c clock.Clock) {
	if c == nil {
		return
	}
	s.mu.Lock()
	s.now = c.Now
	s.mu.Unlock()
}
