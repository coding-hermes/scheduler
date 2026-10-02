package bus

// REMOTE-009 responder-side battery: the reply side over the stub relay —
// a full requester↔responder round trip through ONE relay, reply-topic
// resolution (default vs hint), malformed-frame log-and-drop, and the
// disabled/nil no-op law.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestREMOTE009_ResponderFullRoundTrip proves the reply side end to end:
// a real requester (bus.Client.Query) and a real responder
// (QueryResponder.Run) over one stub relay — the requester's correlated
// reply comes back through the responder's handler-driven publish.
func TestREMOTE009_ResponderFullRoundTrip(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "self-1")

	mu := sync.Mutex{}
	var seen []QueryEnvelope
	r := NewQueryResponder(c, func(q QueryEnvelope) ResponseEnvelope {
		mu.Lock()
		seen = append(seen, q)
		mu.Unlock()
		out := ResponseEnvelope{
			CorrID: q.CorrID, Op: q.Op, Peer: "self-1",
			Status: "ok", AsOf: "2026-10-01T12:00:00Z", AgeMS: 0,
			Data:     map[string]any{"answer": true},
			Gaps:     []FederationGap{},
			Contract: "1.0.0",
		}
		return out
	})
	if !r.Enabled() {
		t.Fatal("responder not enabled")
	}
	if r.Topic() != QueryTopic("self-1") {
		t.Errorf("responder topic = %q, want %q", r.Topic(), QueryTopic("self-1"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	// Wait for the responder's subscription on fed.query.self-1.
	relay.subscribedConn(t, QueryTopic("self-1"), 2*time.Second)

	// A real requester asks self-1. The responder must answer on the
	// correlated reply topic and the requester must match by corr_id.
	got, err := c.Query(context.Background(), "self-1", QueryEnvelope{
		Op: "queue.get", CorrID: "corr-rt-9", BudgetMS: intPtr(2000),
	})
	if err != nil {
		t.Fatalf("round trip through responder: %v", err)
	}
	if got.CorrID != "corr-rt-9" || got.Op != "queue.get" || got.Status != "ok" {
		t.Errorf("reply drifted: %+v", got)
	}
	dJSON, _ := json.Marshal(got.Data)
	if string(dJSON) != `{"answer":true}` {
		t.Errorf("reply data drifted: %s", dJSON)
	}
	mu.Lock()
	if len(seen) != 1 || seen[0].CorrID != "corr-rt-9" || seen[0].Op != "queue.get" {
		t.Errorf("handler saw %+v, want exactly the corr-rt-9 query", seen)
	}
	mu.Unlock()

	// The reply landed on the DEFAULT correlated topic: the requester's
	// own subscription on fed.reply.corr-rt-9 answered first, which is
	// the spec's addressing law (the hint only matters for a peer that
	// does not read the default). Prove BOTH publishes happened: the
	// query (with the hint) and the responder's reply (on the default
	// topic — the hint is what the responder published onto, but the
	// requester's own correlated subscription received it first).
	pubs := relay.published(t)
	var sawQueryWithHint, sawReply bool
	for _, p := range pubs {
		if p.topic == QueryTopic("self-1") && p.reply == ReplyTopic("corr-rt-9") {
			sawQueryWithHint = true
		}
		if p.topic == ReplyTopic("corr-rt-9") {
			sawReply = true
		}
	}
	if !sawQueryWithHint {
		t.Errorf("query without the reply-topic hint on %s; publishes: %+v", QueryTopic("self-1"), pubs)
	}
	if !sawReply {
		t.Errorf("no reply published on %s; publishes: %+v", ReplyTopic("corr-rt-9"), pubs)
	}
}

// TestREMOTE009_ResponderDefaultTopicWithoutHint proves the correlated
// default: a query with NO reply_topic hint is answered on
// fed.reply.<corr_id> (the spec's addressing law, not the hint).
func TestREMOTE009_ResponderDefaultTopicWithoutHint(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "self-2")
	r := NewQueryResponder(c, func(q QueryEnvelope) ResponseEnvelope {
		return ResponseEnvelope{CorrID: q.CorrID, Op: q.Op, Peer: "self-2", Status: "ok", Gaps: []FederationGap{}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	relay.subscribedConn(t, QueryTopic("self-2"), 2*time.Second)

	// Publish a query DIRECTLY (no hint) as a foreign requester would.
	qenv := QueryEnvelope{Op: "peer.status", CorrID: "corr-no-hint"}
	body, _ := json.Marshal(queryFrame{Topic: QueryTopic("self-2"), Event: qenv})
	pubResp, err := http.Post(relay.srv.URL+"/relay/publish", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("publish query: %v", err)
	}
	pubResp.Body.Close()

	// The reply must land on the DEFAULT topic.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range relay.published(t) {
			if p.topic == ReplyTopic("corr-no-hint") {
				var env ResponseEnvelope
				if err := json.Unmarshal(p.event, &env); err != nil {
					t.Fatalf("reply decode: %v", err)
				}
				if env.CorrID != "corr-no-hint" || env.Status != "ok" {
					t.Errorf("reply drifted: %+v", env)
				}
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no reply on default topic %s within 2s", ReplyTopic("corr-no-hint"))
}

// TestREMOTE009_ResponderMalformedFramesDropped proves the autonomy law on
// the reply side: malformed frames and envelope-less garbage are logged and
// dropped — never a publish, never a crash.
func TestREMOTE009_ResponderMalformedFramesDropped(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "self-3")
	r := NewQueryResponder(c, func(q QueryEnvelope) ResponseEnvelope {
		t.Errorf("handler invoked on malformed input: %+v", q)
		return ResponseEnvelope{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	conn := relay.subscribedConn(t, QueryTopic("self-3"), 2*time.Second)

	// Not JSON at all.
	if err := wsServerWriteFrame(conn.conn, wsOpText, []byte("this is not json")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	// JSON but not a query envelope.
	if err := wsServerWriteFrame(conn.conn, wsOpText, []byte(`{"unrelated":true}`)); err != nil {
		t.Fatalf("write envelope-less: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if pubs := relay.published(t); len(pubs) != 0 {
		t.Errorf("malformed input produced %d publishes, want 0", len(pubs))
	}
}

// TestREMOTE009_ResponderOplessQueryIsNamedRefusal pins the §2.2 law on the
// boundary: an op-less query WITH a corr_id is a valid TRANSPORT frame —
// the handler (the internal entry point) answers the unknown_op refusal,
// which IS published (a named refusal, never silence).
func TestREMOTE009_ResponderOplessQueryIsNamedRefusal(t *testing.T) {
	relay := newQueryStubRelay(t)
	c := NewClient(true, relay.srv.URL, "", "self-3b")
	r := NewQueryResponder(c, func(q QueryEnvelope) ResponseEnvelope {
		// Stand-in for the api adapter's unknown-op arm: a named error.
		return ResponseEnvelope{
			CorrID: q.CorrID, Op: q.Op, Peer: "self-3b", Status: "error",
			Gaps:     []FederationGap{},
			Error:    &FederationError{Code: "unknown_op", Message: "unknown op — supported: peer.status, ..."},
			Contract: "1.0.0",
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	conn := relay.subscribedConn(t, QueryTopic("self-3b"), 2*time.Second)

	good, _ := json.Marshal(queryFrame{
		Topic:      QueryTopic("self-3b"),
		Event:      QueryEnvelope{CorrID: "corr-opless"},
		ReplyTopic: "fed.reply.corr-opless",
	})
	if err := wsServerWriteFrame(conn.conn, wsOpText, good); err != nil {
		t.Fatalf("write op-less: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range relay.published(t) {
			if p.topic == "fed.reply.corr-opless" {
				var env ResponseEnvelope
				if err := json.Unmarshal(p.event, &env); err != nil {
					t.Fatalf("refusal decode: %v", err)
				}
				if env.Status != "error" || env.Error == nil || env.Error.Code != "unknown_op" {
					t.Errorf("refusal drifted: %+v", env)
				}
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("op-less query never got its named refusal")
}

// TestREMOTE009_ResponderDisabledAndNilAreNoOps pins the no-op law: a nil
// responder, a disabled client, and a nil handler all yield a responder
// that reports disabled and whose Run returns immediately (nothing dials).
func TestREMOTE009_ResponderDisabledAndNilAreNoOps(t *testing.T) {
	disabledClient := NewClient(false, "", "", "self-4")
	if r := NewQueryResponder(disabledClient, func(QueryEnvelope) ResponseEnvelope { return ResponseEnvelope{} }); r.Enabled() {
		t.Error("disabled client produced an enabled responder")
	}
	if r := NewQueryResponder(nil, nil); r.Enabled() {
		t.Error("nil client produced an enabled responder")
	}
	enabled := NewClient(true, "http://127.0.0.1:1", "", "self-4")
	if r := NewQueryResponder(enabled, nil); r.Enabled() {
		t.Error("nil handler produced an enabled responder")
	}
	// Run on a disabled responder returns at once (nothing to consume).
	var rnil *QueryResponder
	rnil.Run(context.Background()) // must not panic
	disabled := NewQueryResponder(disabledClient, func(QueryEnvelope) ResponseEnvelope { return ResponseEnvelope{} })
	done := make(chan struct{})
	go func() { disabled.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled responder's Run did not return")
	}
	// Close is safe twice and on disabled responders.
	disabled.Close()
	disabled.Close()
}
