package bus

// REMOTE-009, reply side: the responder that ANSWERS peer queries.
//
// A QueryResponder subscribes to `fed.query.<self>` and hands every valid
// §2.1 envelope to the injected Handler — the daemon wires
// api.Server.FederationBusHandler, i.e. the SAME internal query entry point
// (the federationOps dispatch + replay window) the HTTP surface serves
// REMOTE-008 from. The handler returns the §2.2 Response envelope; the
// responder publishes it on the requester's reply topic (`reply_topic` when
// the query carried the hint, else the spec default `fed.reply.<corr_id>`),
// so the requester matches it BY PAYLOAD corr_id. THE AUTONOMY LAW: every
// failure here is log-and-drop (a malformed frame, a failed publish, a
// dropped socket) — the responder can never fail the scheduler; it is the
// REMOTE-004 subscriber discipline applied to the answer path.

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// QueryHandler answers one query envelope. It returns the §2.2 Response
// envelope to publish back — including for refusals (status="error" is a
// named answer, never silence; spec §2.2 + §4).
type QueryHandler func(q QueryEnvelope) ResponseEnvelope

// QueryResponder answers peer queries for one scheduler. Built from the
// process's single bus client (same construction convention as Subscriber:
// unexported struct, exported constructor, nil / disabled client = no-op).
type QueryResponder struct {
	pub *Client
	// handler is the injected internal query entry point (the api
	// adapter). nil = no-op responder (Run returns immediately).
	handler QueryHandler

	mu     sync.Mutex
	closed bool
	conns  map[string]*wsConn
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewQueryResponder builds the responder. A nil or disabled client, or a
// nil handler, yields a no-op: Run returns immediately, nothing dials.
func NewQueryResponder(c *Client, h QueryHandler) *QueryResponder {
	r := &QueryResponder{conns: map[string]*wsConn{}, done: make(chan struct{})}
	if c == nil || !c.Enabled() || h == nil {
		// Mark the no-op closed up front: Close stays safe, Run is a
		// pure return, nothing ever dials.
		r.closed = true
		return r
	}
	r.pub = c
	r.handler = h
	return r
}

// Enabled reports whether the responder will actually answer.
func (r *QueryResponder) Enabled() bool {
	return r != nil && r.pub != nil && r.handler != nil
}

// Topic is the query topic this responder consumes (`fed.query.<self>`).
func (r *QueryResponder) Topic() string {
	if r == nil || r.pub == nil {
		return QueryTopic("")
	}
	return QueryTopic(r.pub.SchedulerID())
}

// Run consumes the self query topic until ctx is done or Close. The
// reconnect ladder mirrors Subscriber.runOne: dial, consume, back off,
// repeat — a down relay is a slow retry loop, never an error surfaced.
func (r *QueryResponder) Run(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	r.wg.Add(1)
	defer r.wg.Done()
	backoff := time.Second
	for ctx.Err() == nil {
		err := r.serve(ctx)
		if r.isClosed() || ctx.Err() != nil {
			return
		}
		log.Printf("CRIER: query responder %s dropped: %v (retrying)", r.Topic(), err)
		if !r.wait(ctx, backoff) {
			return
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// wait sleeps d, interrupted by ctx or Close. False = interrupted.
func (r *QueryResponder) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.done:
		return false
	case <-t.C:
		return true
	}
}

// serve is one connection life over the self query topic: dial, consume
// frames, answer each, until an error or shutdown.
func (r *QueryResponder) serve(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, subDialTimeout)
	defer cancel()
	conn, err := wsDial(dialCtx, r.pub.baseURL+"/relay/subscribe/"+r.Topic(), r.pub.token, r.pub.agentID)
	if err != nil {
		return err
	}
	topic := r.Topic()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		conn.close()
		return ErrClosed
	}
	r.conns[topic] = conn
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, topic)
		r.mu.Unlock()
		conn.wsClose()
		conn.close()
	}()
	for {
		op, payload, err := conn.wsReadFrame()
		if err != nil {
			if r.isClosed() {
				return ErrClosed
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch op {
		case wsOpPing:
			_ = conn.wsWriteFrame(wsOpPong, payload)
		case wsOpClose:
			_ = conn.wsWriteFrame(wsOpClose, nil)
			return ErrClosed
		case wsOpText:
			r.handleQueryFrame(payload)
		default:
			// Binary/pong — not a query; ignore.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// handleQueryFrame decodes one relay frame, validates the envelope, runs
// the internal entry point, and publishes the correlated reply. Every
// failure mode is log-and-drop (the autonomy law).
func (r *QueryResponder) handleQueryFrame(payload []byte) {
	var fr frame
	if err := json.Unmarshal(payload, &fr); err != nil {
		log.Printf("CRIER: query frame malformed dropped: %v", err)
		return
	}
	var q QueryEnvelope
	if err := json.Unmarshal(fr.Event, &q); err != nil {
		log.Printf("CRIER: query envelope malformed dropped: %v", err)
		return
	}
	if q.CorrID == "" {
		// Spec §2.1: corr_id is REQUIRED — it IS the correlation key. A
		// query without one cannot be answered on a correlated topic;
		// refuse on the requester's explicit hint when it sent one
		// (a named refusal beats silence), else drop.
		if fr.ReplyTopic != "" {
			r.publishReply(fr.ReplyTopic, ResponseEnvelope{
				Op:       q.Op,
				Peer:     r.pub.SchedulerID(),
				Status:   "error",
				Gaps:     []FederationGap{},
				Error:    &FederationError{Code: "missing_corr_id", Message: "corr_id is required (the idempotency key, spec §2.5)"},
				Contract: "1.0.0",
			})
			return
		}
		log.Printf("CRIER: query without corr_id dropped (op=%q)", q.Op)
		return
	}
	resp := r.handler(q)
	// Prefer the requester's explicit reply-topic hint; default to the
	// spec's correlated topic for peers that did not send one.
	topic := fr.ReplyTopic
	if topic == "" {
		topic = ReplyTopic(q.CorrID)
	}
	r.publishReply(topic, resp)
}

// publishReply POSTs one §2.2 envelope on topic. Log-and-drop on failure
// (the requester's own budget will surface the timeout — the autonomy law).
func (r *QueryResponder) publishReply(topic string, resp ResponseEnvelope) {
	body, err := json.Marshal(replyFrame{Topic: topic, Event: resp})
	if err != nil {
		log.Printf("CRIER: marshal reply dropped (corr %s): %v", resp.CorrID, err)
		return
	}
	if err := r.pub.publishQuery(context.Background(), body); err != nil {
		log.Printf("CRIER: reply publish dropped (corr %s topic %s): %v", resp.CorrID, topic, err)
	}
}

// Close tears the responder down. Safe to call twice; no-op when disabled.
func (r *QueryResponder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	conns := r.conns
	r.conns = map[string]*wsConn{}
	r.mu.Unlock()
	for _, c := range conns {
		c.wsClose()
		c.close()
	}
	r.wg.Wait()
}

func (r *QueryResponder) isClosed() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}
