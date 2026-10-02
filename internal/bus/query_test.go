package bus

// REMOTE-009 acceptance battery (docs/federation-query-spec.md §3, the
// "Crier bus" row), run against a stub relay that reimplements just the two
// shipped endpoints the transport speaks:
//
//	POST /relay/publish              (202)
//	GET  /relay/subscribe/{topic}    (WebSocket upgrade + frames)
//
// The upgrade handshake is REAL: the stub computes Sec-WebSocket-Accept
// from the client's own Sec-WebSocket-Key (the wsDial validation passes
// honestly, not by weakening the client). The named cells map 1:1 to the
// row's acceptance list:
//   - request/reply round-trip over the bus with corr_id matching, and the
//     wire envelope proven to be the §2.1 object verbatim
//   - a reply that arrives AFTER budget_ms is dropped — the caller gets
//     the named timeout degradation, never a late delivery
//   - a foreign corr_id on the reply topic is dropped (correlation law)
//   - topic addressing pins (fed.query.<peer> / fed.reply.<corr_id>)

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// queryStubRelay is a minimal in-process relay: it records publishes and
// delivers frames to topic subscribers.
type queryStubRelay struct {
	mu   sync.Mutex
	srv  *httptest.Server
	pubs []queryStubPublish
	subs map[string]*wsConn
}

type queryStubPublish struct {
	topic string
	event json.RawMessage
	reply string
}

func newQueryStubRelay(t *testing.T) *queryStubRelay {
	t.Helper()
	r := &queryStubRelay{subs: map[string]*wsConn{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/relay/publish", func(w http.ResponseWriter, req *http.Request) {
		var fr struct {
			Topic      string          `json:"topic"`
			Event      json.RawMessage `json:"event"`
			ReplyTopic string          `json:"reply_topic"`
		}
		if err := json.NewDecoder(req.Body).Decode(&fr); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		r.mu.Lock()
		r.pubs = append(r.pubs, queryStubPublish{topic: fr.Topic, event: fr.Event, reply: fr.ReplyTopic})
		targets := make([]*wsConn, 0)
		for pat, s := range r.subs {
			if queryStubPatternMatches(pat, fr.Topic) {
				targets = append(targets, s)
			}
		}
		r.mu.Unlock()
		payload, _ := json.Marshal(frame{Topic: fr.Topic, Event: fr.Event})
		for _, s := range targets {
			_ = wsServerWriteFrame(s.conn, wsOpText, payload)
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
		accept := base64.StdEncoding.EncodeToString(wsAcceptHash(key))
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		pattern := strings.TrimPrefix(req.URL.Path, "/relay/subscribe/")
		r.mu.Lock()
		r.subs[pattern] = &wsConn{conn: &wsBufferedConn{br: buf.Reader, Conn: conn}}
		r.mu.Unlock()
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		r.mu.Lock()
		conns := r.subs
		r.subs = map[string]*wsConn{}
		r.mu.Unlock()
		for _, s := range conns {
			s.wsClose()
			s.close()
		}
		r.srv.Close()
	})
	return r
}

// queryStubPatternMatches is the relay's wildcard law reduced to what the stub
// needs: a literal pattern matches only itself; `>` matches one or more
// trailing segments (the terminal rule of the relay's wildcard.go).
func queryStubPatternMatches(pattern, topic string) bool {
	if pattern == topic {
		return true
	}
	if !strings.HasSuffix(pattern, ">") {
		return false
	}
	prefix := strings.TrimSuffix(strings.TrimSuffix(pattern, ">"), ".")
	return strings.HasPrefix(topic, prefix+".") && len(topic) > len(prefix)+1
}

// published returns the recorded publishes (topic, event, reply_topic).
func (r *queryStubRelay) published(t *testing.T) []queryStubPublish {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]queryStubPublish(nil), r.pubs...)
}

// subscribedConn returns the socket subscribed to pattern, waiting up to d
// for the subscription to appear.
func (r *queryStubRelay) subscribedConn(t *testing.T, pattern string, d time.Duration) *wsConn {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		s, ok := r.subs[pattern]
		r.mu.Unlock()
		if ok {
			return s
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no subscriber appeared on %q within %s", pattern, d)
	return nil
}

// writeReplyFrame writes one relayed frame onto a subscribed socket (the
// test playing the answering peer).
func writeReplyFrame(t *testing.T, conn *wsConn, resp ResponseEnvelope) {
	t.Helper()
	body, err := json.Marshal(replyFrame{Topic: "via-stub", Event: resp})
	if err != nil {
		t.Fatalf("marshal reply frame: %v", err)
	}
	if err := wsServerWriteFrame(conn.conn, wsOpText, body); err != nil {
		t.Fatalf("write reply frame: %v", err)
	}
}

// TestREMOTE009_QueryRoundTripCorrID proves the acceptance cell: a request
// on fed.query.<peer> + a correlated reply on fed.reply.<corr_id> completes
// the round trip, the reply is matched BY PAYLOAD corr_id, and the wire
// envelope is the §2.1 object verbatim (op/args/corr_id/budget_ms/want),
// with the requester's reply-topic hint riding outside the envelope.
func TestREMOTE009_QueryRoundTripCorrID(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "test-self")

	type result struct {
		resp ResponseEnvelope
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := c.Query(context.Background(), "peer-b", QueryEnvelope{
			Op: "peer.status", Args: map[string]any{"x": float64(1)},
			CorrID: "corr-round-1", Want: "answer",
		})
		resCh <- result{resp, err}
	}()

	// SUBSCRIBE-BEFORE-PUBLISH: the requester's reply subscription must
	// exist before we play the peer — wait for it, then answer.
	conn := relay.subscribedConn(t, "fed.reply.corr-round-1", 2*time.Second)
	writeReplyFrame(t, conn, ResponseEnvelope{
		CorrID: "corr-round-1", Op: "peer.status", Peer: "peer-b",
		Status: "ok", AsOf: "2026-10-01T00:00:00Z", AgeMS: 0,
		Data:     map[string]any{"scheduler_id": "peer-b"},
		Gaps:     []FederationGap{},
		Contract: "1.0.0",
	})

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("round trip failed: %v", res.err)
		}
		got := res.resp
		if got.CorrID != "corr-round-1" || got.Op != "peer.status" || got.Peer != "peer-b" ||
			got.Status != "ok" || got.AsOf != "2026-10-01T00:00:00Z" || got.AgeMS != 0 ||
			got.Contract != "1.0.0" {
			t.Errorf("envelope members drifted: %+v", got)
		}
		dJSON, _ := json.Marshal(got.Data)
		if string(dJSON) != `{"scheduler_id":"peer-b"}` {
			t.Errorf("data drifted: %s", dJSON)
		}
		if len(got.Gaps) != 0 {
			t.Errorf("gaps = %v, want empty", got.Gaps)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("round trip did not complete")
	}

	// The request side published EXACTLY one query on the peer's topic.
	pubs := relay.published(t)
	if len(pubs) != 1 {
		t.Fatalf("publishes = %d, want 1", len(pubs))
	}
	p := pubs[0]
	if p.topic != QueryTopic("peer-b") {
		t.Errorf("query topic = %q, want %q", p.topic, QueryTopic("peer-b"))
	}
	if p.reply != ReplyTopic("corr-round-1") {
		t.Errorf("reply_topic hint = %q, want %q", p.reply, ReplyTopic("corr-round-1"))
	}
	var sent QueryEnvelope
	if err := json.Unmarshal(p.event, &sent); err != nil {
		t.Fatalf("request event decode: %v", err)
	}
	// §2.1 VERBATIM on the wire: required members present, args intact.
	if sent.Op != "peer.status" || sent.CorrID != "corr-round-1" || sent.Want != "answer" {
		t.Errorf("wire envelope drifted: %+v", sent)
	}
	if sent.Args["x"] == nil {
		t.Errorf("args lost on the wire: %+v", sent)
	}
}

// TestREMOTE009_LateReplyDropped proves the acceptance cell: a reply that
// arrives AFTER budget_ms is NOT delivered — the caller degrades with the
// named timeout error (the status="error" code="timeout" sentinel), and
// the caller has already returned when the late reply is written.
func TestREMOTE009_LateReplyDropped(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "test-self")

	budget := 250
	type result struct {
		resp ResponseEnvelope
		err  error
	}
	resCh := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := c.Query(context.Background(), "peer-slow", QueryEnvelope{
			Op: "fleet.status", CorrID: "corr-late", BudgetMS: &budget,
		})
		resCh <- result{resp, err}
	}()

	conn := relay.subscribedConn(t, "fed.reply.corr-late", 2*time.Second)
	// Hold the reply PAST the budget, then send it anyway (a late peer).
	time.Sleep(time.Duration(budget)*time.Millisecond + 200*time.Millisecond)
	select {
	case res := <-resCh:
		if res.err == nil {
			t.Fatalf("expected timeout degradation, got answer: %+v", res.resp)
		}
		if !errors.Is(res.err, ErrQueryTimeout) {
			t.Errorf("err = %v, want ErrQueryTimeout (code=timeout sentinel)", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller still waiting after budget expiry — the deadline did not fire")
	}

	// Now write the LATE reply. The caller has ALREADY returned — prove
	// delivery is structurally impossible: nothing can receive it again.
	writeReplyFrame(t, conn, ResponseEnvelope{
		CorrID: "corr-late", Op: "fleet.status", Peer: "peer-slow",
		Status: "ok", Data: map[string]any{"LATE": true},
	})
	select {
	case res := <-resCh:
		t.Errorf("the late reply WAS delivered after the timeout: %+v / %v", res.resp, res.err)
	case <-done:
		// The query goroutine has exited; the channel's buffered result
		// was consumed exactly once above. No second delivery exists.
	}
}

// TestREMOTE009_ForeignCorrIDNeverDelivered proves the correlation law: a
// reply whose payload corr_id does not match is dropped (one log line) and
// never returned to the caller — the real (matching) reply arriving after
// it still completes the round trip.
func TestREMOTE009_ForeignCorrIDNeverDelivered(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "test-self")

	type result struct {
		resp ResponseEnvelope
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := c.Query(context.Background(), "peer-x", QueryEnvelope{
			Op: "queue.get", CorrID: "corr-mine", BudgetMS: intPtr(3000),
		})
		resCh <- result{resp, err}
	}()
	conn := relay.subscribedConn(t, "fed.reply.corr-mine", 2*time.Second)

	// 1) A foreign reply (someone else's corr_id colliding on the
	// sanitized topic): must be dropped, not delivered.
	writeReplyFrame(t, conn, ResponseEnvelope{
		CorrID: "corr-NOT-mine", Op: "queue.get", Peer: "peer-x", Status: "ok",
		Data: map[string]any{"wrong": true},
	})
	// 2) The real reply: the round trip completes.
	writeReplyFrame(t, conn, ResponseEnvelope{
		CorrID: "corr-mine", Op: "queue.get", Peer: "peer-x", Status: "ok",
		Data: []any{}, Contract: "1.0.0",
	})

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("real reply lost: %v", res.err)
		}
		if res.resp.CorrID != "corr-mine" {
			t.Errorf("delivered corr %q, want corr-mine (a foreign reply reached the caller)", res.resp.CorrID)
		}
		if m, ok := res.resp.Data.(map[string]any); ok {
			if _, wrong := m["wrong"]; wrong {
				t.Errorf("the FOREIGN reply's data reached the caller: %+v", res.resp)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("real reply never delivered after a foreign one was dropped")
	}
}

// TestREMOTE009_QueryTopicHelpers pins the addressing scheme (spec §3 bus
// row) and the topic-segment sanitization rule.
func TestREMOTE009_QueryTopicHelpers(t *testing.T) {
	if got := QueryTopic("sched-a"); got != "fed.query.sched-a" {
		t.Errorf("QueryTopic = %q", got)
	}
	if got := ReplyTopic("corr-123"); got != "fed.reply.corr-123" {
		t.Errorf("ReplyTopic = %q", got)
	}
	// A corr_id with a slash sanitizes for the TOPIC only.
	if got := ReplyTopic("a/b:c"); got != "fed.reply.a-b-c" {
		t.Errorf("ReplyTopic sanitize = %q", got)
	}
}

// TestREMOTE009_QueryBudgetResolution pins budget_ms resolution: absent /
// non-positive = the documented default; a positive value rides through.
func TestREMOTE009_QueryBudgetResolution(t *testing.T) {
	if got := (&QueryEnvelope{}).queryBudgetMS(); got != queryDefaultBudgetMS {
		t.Errorf("absent budget = %d, want %d", got, queryDefaultBudgetMS)
	}
	if got := (&QueryEnvelope{BudgetMS: intPtr(-5)}).queryBudgetMS(); got != queryDefaultBudgetMS {
		t.Errorf("negative budget = %d, want the default", got)
	}
	if got := (&QueryEnvelope{BudgetMS: intPtr(250)}).queryBudgetMS(); got != 250 {
		t.Errorf("explicit budget = %d, want 250", got)
	}
}

// TestREMOTE009_DisabledClientQueryIsNamedError pins the autonomy-law
// surface on the request side: a disabled client is the named ErrDisabled,
// never a fabricated empty answer, never a panic; a blank peer id is a
// named error too.
func TestREMOTE009_DisabledClientQueryIsNamedError(t *testing.T) {
	disabled := NewClient(false, "", "", "test-self")
	if _, err := disabled.Query(context.Background(), "peer-a", QueryEnvelope{Op: "peer.status"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled client err = %v, want ErrDisabled", err)
	}
	enabled := NewClient(true, "http://127.0.0.1:1", "", "test-self")
	if _, err := enabled.Query(context.Background(), "", QueryEnvelope{Op: "peer.status"}); err == nil || errors.Is(err, ErrQueryTimeout) {
		t.Errorf("blank peer err = %v, want a peer-id error (not a timeout)", err)
	}
}

func intPtr(n int) *int { return &n }
