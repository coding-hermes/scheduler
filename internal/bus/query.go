package bus

// REMOTE-009 (docs/federation-query-spec.md §3, the "Crier bus" row): the
// federation QUERY transport over Crier — the request/reply half of the read
// contract that REMOTE-004 built the visibility half of.
//
//	REQUEST side:  Query(ctx, peerID, q) publishes the spec §2.1 Query
//	               envelope on the peer's query topic `fed.query.<peer>`
//	               (spec §3 bus row), subscribes FIRST to its own correlated
//	               reply topic `fed.reply.<corr_id>`, then waits for THE
//	               reply whose payload `corr_id` matches — never by arrival
//	               order — honouring budget_ms as the caller-side deadline.
//	REPLY side:    a QueryResponder subscribes to `fed.query.<self>`, hands
//	               each envelope to the injected handler (the daemon wires
//	               the SAME internal query entry point the HTTP surface
//	               uses — api.Server.FederationBusHandler, REMOTE-008), and
//	               publishes the §2.2 Response envelope on the requester's
//	               reply topic so it can match by corr_id.
//
// The envelope is the contract and is carried VERBATIM: the wire event on
// both sides is exactly the §2.1/§2.2 JSON object (the topic addressing and
// the requester's reply-topic hint ride OUTSIDE the envelope, in the relay
// publish body, which is transport plumbing — spec §3 "Every adapter: parses
// its surface → builds the envelope → calls the SAME internal query entry
// point"). The default reply topic is `fed.reply.<corr_id>`; because the
// relay only admits topic segments of [A-Za-z0-9_-], a corr_id carrying
// other characters is sanitized FOR THE TOPIC ONLY — the payload corr_id is
// never rewritten, and matching is by payload, never by topic.
//
// THE AUTONOMY LAW (spec §5 degradation model + the §3 bus row): a bus
// failure — relay down, publish refused, no reply inside budget_ms — is a
// NAMED error to the caller (ErrQueryTimeout maps to status="error"
// code="timeout" for the aggregate surface) and NEVER blocks or fails a
// tick. The responder side is log-and-drop like the REMOTE-004 subscriber.
// A reply that arrives AFTER the deadline is structurally undeliverable:
// the wait loop has already torn the reply socket down, and a reply whose
// payload corr_id does not match the outstanding request is dropped with
// one log line — it can never be delivered to the wrong caller.
//
// Replays use the spec §2.5 idempotency key: the answering side (the api
// adapter) keeps the shared replay window keyed (caller, corr_id, op), so a
// bus redelivery returns the FIRST answer byte-identically instead of
// re-reading.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// queryDefaultBudgetMS is the caller-side wait when the envelope carries no
// budget_ms (spec §2.1: "Absent budget_ms means peer default" — the PEER
// clamps its own read; this number is only how long the REQUESTER waits for
// the correlated reply before degrading). It matches the fleet-wide default
// budget the HTTP surface documents for its peers.
const queryDefaultBudgetMS = 10000

// queryCorrCounter disambiguates corr_ids minted within the same nanosecond
// (two queries from one caller in one instant still need distinct keys).
var queryCorrCounter atomic.Uint64

// ErrQueryTimeout is the named degradation (spec §5: a peer that timed out
// is `status="error" code="timeout"`, never an omitted answer): Query
// returns an error wrapping this sentinel when no correlated reply arrived
// inside budget_ms. Callers — today the REMOTE-012 aggregate — branch on it
// with errors.Is; it is never a tick failure.
var ErrQueryTimeout = errors.New("bus: query reply timeout")

// ErrReplyClosed is returned when the reply socket is closed by the peer
// (a relay close frame) before any correlated reply arrived. Distinct from
// ErrQueryTimeout so a caller can tell "the peer hung up" from "nothing
// came back in time".
var ErrReplyClosed = errors.New("bus: reply stream closed by peer")

// QueryEnvelope is the spec §2.1 Query envelope, VERBATIM — the same JSON
// object every transport carries (the HTTP surface's queryEnvelope in
// internal/api is the same contract member-for-member; the type lives twice
// because internal/api cannot be imported from a transport package without
// an import cycle, and the §2.1 field set is frozen by the spec). op and
// corr_id are REQUIRED on the wire; the bus client fills corr_id when the
// caller leaves it empty (minted unique per call), and every other member
// keeps the caller's value untouched — defaults are the PEER's business.
type QueryEnvelope struct {
	Op       string         `json:"op"`
	Args     map[string]any `json:"args,omitempty"`
	CorrID   string         `json:"corr_id"`
	BudgetMS *int           `json:"budget_ms,omitempty"`
	Want     string         `json:"want,omitempty"`
}

// FederationGap is one entry of the Response envelope's gaps array (spec
// §2.2) — what could not be answered and why.
type FederationGap struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// FederationError is the Response envelope's error object (spec §2.2): a
// STABLE machine code plus a human message.
type FederationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResponseEnvelope is the spec §2.2 Response envelope, VERBATIM. The member
// set mirrors the HTTP surface's responseEnvelope exactly (including a null
// data member on error replies, which is what the HTTP surface emits), so a
// reply that travelled the bus re-marshals to the same shape the HTTP
// surface produced — the conformance duty of §6/REMOTE-014. Matching
// against a reply is ALWAYS by the payload CorrID, never by arrival order
// or by topic (spec §2.5: the corr_id is the correlation + idempotency key).
type ResponseEnvelope struct {
	CorrID   string           `json:"corr_id"`
	Op       string           `json:"op"`
	Peer     string           `json:"peer"`
	Status   string           `json:"status"`
	AsOf     string           `json:"as_of"`
	AgeMS    int64            `json:"age_ms"`
	Data     any              `json:"data"`
	Gaps     []FederationGap  `json:"gaps"`
	Error    *FederationError `json:"error,omitempty"`
	Contract string           `json:"contract"`
}

// QueryTopic is the peer's query topic (spec §3 bus row:
// `fed.query.<peer>`): where a requester publishes a query FOR that peer,
// and where that peer's responder listens for its own name.
func QueryTopic(peerID string) string { return "fed.query." + peerID }

// ReplyTopic is the correlated reply topic (spec §3 bus row:
// `fed.reply.<corr_id>`). The relay admits only [A-Za-z0-9_-] topic
// segments, so a corr_id with other characters is sanitized for the TOPIC
// ONLY — the payload corr_id inside the reply frame is never rewritten, and
// requesters match on the payload (never on the topic), so a sanitized
// topic collision loses nothing.
func ReplyTopic(corrID string) string { return "fed.reply." + sanitizeTopicToken(corrID) }

// sanitizeTopicToken maps every byte the relay's topic grammar refuses
// (anything outside [A-Za-z0-9_-], per the relay's validTopicSegment) to
// '-' so the resulting topic is publishable. An empty input yields a
// non-empty placeholder (a topic segment may not be empty).
func sanitizeTopicToken(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			b[i] = '-'
		}
	}
	return string(b)
}

// NextCorrID mints a caller-unique corr_id: scheduler id + wall-nano +
// monotonic counter, so the spec §2.1 law ("unique per (caller, minute)")
// holds across a restart (the counter alone would replay after a restart
// within the same minute and hit the §2.5 replay window as a duplicate).
// The mint reads the client's own clock seam (SCHED-GAP-169 keeps the
// stdlib guard quiet and tests deterministic).
func (c *Client) NextCorrID() string {
	n := queryCorrCounter.Add(1)
	return fmt.Sprintf("q-%s-%d-%016d", sanitizeTopicToken(c.schedID), c.now().UnixNano(), n)
}

// queryFrame is the relay publish body for a bus query: the envelope (the
// contract, verbatim) plus transport plumbing — the query topic and the
// requester's reply-topic hint. A peer that does not read reply_topic
// defaults to fed.reply.<corr_id>, which is what this client subscribes to;
// the hint exists so a requester with a sanitized corr_id still gets its
// replies on the exact topic it listens on.
type queryFrame struct {
	Topic      string        `json:"topic"`
	Event      QueryEnvelope `json:"event"`
	ReplyTopic string        `json:"reply_topic,omitempty"`
}

// replyFrame is the relay publish body for a bus reply: the correlated
// topic plus the §2.2 envelope verbatim as the event.
type replyFrame struct {
	Topic string           `json:"topic"`
	Event ResponseEnvelope `json:"event"`
}

// queryBudgetMS resolves the caller-side wait budget: the envelope's
// budget_ms when positive, else the default. (The peer clamps its own read
// separately — spec §2.1 "the peer clamps to its own cap"; this is only how
// long THIS caller waits for the correlated reply before degrading.)
func (q *QueryEnvelope) queryBudgetMS() int {
	if q.BudgetMS != nil && *q.BudgetMS > 0 {
		return *q.BudgetMS
	}
	return queryDefaultBudgetMS
}

// publishQuery POSTs one pre-marshaled relay publish body and SURFACES the
// error (unlike Publish, which is log-and-drop for the fire-and-forget
// visibility flow): a query caller must see "bus down" as a named error, an
// aggregate as status="error" — silence would be a fabricated answer. The
// relay's opt-in bearer / X-Agent-ID headers follow the client convention.
func (c *Client) publishQuery(ctx context.Context, body []byte) error {
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pubCtx, http.MethodPost, c.baseURL+"/relay/publish", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bus: build query publish: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.agentID != "" {
		req.Header.Set("X-Agent-ID", c.agentID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("bus: query publish (bus unreachable): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("bus: query publish answered %d", resp.StatusCode)
	}
	return nil
}

// Query asks peerID one question and waits for THE correlated reply.
//
// Order of operations (the subscribe-before-publish window): dial the reply
// socket on ReplyTopic(corrID) FIRST, then publish, then read — a reply
// published between our publish and our subscribe would be lost otherwise.
// The reply is matched by the envelope's payload corr_id (spec §2.5), never
// by arrival order; a frame whose corr_id differs (a sanitized-topic
// collision, a stale subscriber) is DROPPED with one log line and can never
// reach the wrong caller. budget_ms (peer clamp rules apply on the peer)
// bounds the whole wait: on expiry the reply socket is torn down and the
// named timeout degradation is returned (ErrQueryTimeout) — a reply that
// arrives after the deadline is structurally undeliverable, not queued.
//
// THE AUTONOMY LAW: every failure — dial, publish, timeout — is returned as
// a named error for the CALLER to degrade on; nothing here blocks a tick
// (queries are issued from read paths, never from the tick terminal path).
// A disabled client returns an error immediately without touching the
// network (there is no silent empty answer).
func (c *Client) Query(ctx context.Context, peerID string, q QueryEnvelope) (ResponseEnvelope, error) {
	if !c.Enabled() {
		return ResponseEnvelope{}, fmt.Errorf("bus: query %s: %w", q.Op, ErrDisabled)
	}
	if peerID == "" {
		return ResponseEnvelope{}, errors.New("bus: query: peer id is required")
	}
	if q.CorrID == "" {
		q.CorrID = c.NextCorrID()
	}
	budget := time.Duration(q.queryBudgetMS()) * time.Millisecond
	qctx, qcancel := context.WithTimeout(ctx, budget)
	defer qcancel()

	// Subscribe FIRST: the reply socket must exist before the request is
	// on the wire (the subscribe-before-publish window).
	dialCtx, dialCancel := context.WithTimeout(qctx, subDialTimeout)
	defer dialCancel()
	conn, err := wsDial(dialCtx, c.baseURL+"/relay/subscribe/"+ReplyTopic(q.CorrID), c.token, c.agentID)
	if err != nil {
		return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, err)
	}
	// Deadline safety net on the socket itself, so a silent peer cannot
	// hold the read past the budget even if the close goroutine below is
	// scheduled late. The seam clock supplies the instant (SCHED-GAP-169).
	_ = conn.conn.SetReadDeadline(c.now().Add(budget))
	// Early-cancel path: a caller ctx done (or the budget expiring) tears
	// the socket down, which unblocks the frame read immediately; qdone
	// releases this watcher the moment Query returns (success included —
	// no parked goroutine outlives the call).
	qdone := make(chan struct{})
	defer func() {
		close(qdone)
		conn.wsClose()
		conn.close()
	}()
	go func() {
		select {
		case <-qctx.Done():
			conn.wsClose()
			conn.close()
		case <-qdone:
		}
	}()

	body, err := json.Marshal(queryFrame{Topic: QueryTopic(peerID), Event: q, ReplyTopic: ReplyTopic(q.CorrID)})
	if err != nil {
		return ResponseEnvelope{}, fmt.Errorf("bus: marshal query: %w", err)
	}
	if err := c.publishQuery(qctx, body); err != nil {
		return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, err)
	}

	for {
		op, payload, err := conn.wsReadFrame()
		if err != nil {
			// Deadline or caller cancellation is the named timeout
			// degradation; the socket is already torn down (the close
			// goroutine), so a reply arriving "now" is undeliverable —
			// exactly the §3 drop law, structural not advisory. The read
			// deadline and the context timer are armed from the same
			// instant with the same duration, so EITHER firing is the
			// budget spent: a net timeout classifies directly (it can
			// win the race by microseconds against ctx.Err()).
			if qerr := qctx.Err(); qerr != nil {
				if errors.Is(qerr, context.Canceled) && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
					return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, ctx.Err())
				}
				return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, ErrQueryTimeout)
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, ErrQueryTimeout)
			}
			return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: reply read: %w", q.Op, q.CorrID, err)
		}
		switch op {
		case wsOpPing:
			_ = conn.wsWriteFrame(wsOpPong, payload) // keep the socket alive on long budgets
		case wsOpClose:
			return ResponseEnvelope{}, fmt.Errorf("bus: query %s corr %s: %w", q.Op, q.CorrID, ErrReplyClosed)
		case wsOpText:
			var fr frame
			if json.Unmarshal(payload, &fr) != nil {
				log.Printf("CRIER: query reply frame malformed dropped (corr %s)", q.CorrID)
				continue
			}
			var resp ResponseEnvelope
			if err := json.Unmarshal(fr.Event, &resp); err != nil {
				log.Printf("CRIER: query reply envelope malformed dropped (corr %s): %v", q.CorrID, err)
				continue
			}
			if resp.CorrID != q.CorrID {
				// THE CORRELATION LAW: never deliver by arrival order. A
				// foreign corr_id on this topic (sanitized collision,
				// stale subscriber) is dropped with one log line.
				log.Printf("CRIER: query reply corr mismatch dropped: got %q want %q", resp.CorrID, q.CorrID)
				continue
			}
			return resp, nil
		default:
			// Pong / binary / anything else — not an answer; keep waiting.
		}
	}
}
