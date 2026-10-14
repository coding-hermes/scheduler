package bus

// SCHED-GAP-1710 — the INBOX receive leg: the scheduler's own agent-addressed
// mailbox on the relay.
//
// Dispatch (internal/bus/dispatch.go) hands a unit of work to an agent's
// DURABLE inbox (`POST /agents/{id}/inbox`). The reply has to come back the
// same way: the proven fleet dispatcher answers with `deliver(reply_to, …)`,
// i.e. it posts into the reply address's inbox, and the relay's inbox routes
// are AGENT-SCOPED — retrieving or acknowledging requires the owner's ed25519
// signature (X-Agent-ID / X-Agent-Ts / X-Agent-Sig over
// "METHOD\n<path>\n<unix-seconds>", query string excluded).
//
// This file implements exactly that surface, and nothing else:
//
//	GET  /agents/{id}/inbox?limit=&lease_seconds=   → {"lease_id", "messages":[…]}
//	POST /agents/{id}/inbox/ack                     → 204, body {"lease_id","message_ids":[…]}
//
// THE LEASE LAW. A retrieve LEASES the messages it returns for lease_seconds:
// another consumer cannot see them until the lease is released (ack) or
// expires. A consumer that reads an inbox it does not own can therefore hide
// foreign traffic, so the dispatch leg leases SHORT and acks ONLY the messages
// it can correlate to its own dispatch — everything else is left for its
// rightful owner to see again when the lease lapses. This file itself never
// acks anything it was not asked to ack.
//
// The identity is optional by construction: a client with no agent key simply
// cannot use these routes (ErrAgentIdentityMissing), and every caller treats
// that as "no receive leg configured", never as a bus failure.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Errors for the inbox leg. Each is NAMED so a caller can branch: a missing
// identity is a configuration fact, an unreachable relay is retryable, and a
// refused ack means the message stays leased (the operator sees why).
var (
	// ErrAgentIdentityMissing — no inbox identity is configured on this
	// client, so no agent-scoped request may be attempted.
	ErrAgentIdentityMissing = errors.New("bus: no agent inbox identity configured")
	// ErrInboxUnreachable — the relay could not be reached, or answered 5xx.
	ErrInboxUnreachable = errors.New("bus: inbox unreachable")
	// ErrInboxRefused — the relay refused an agent-scoped request (4xx: bad
	// signature, wrong owner, unknown agent).
	ErrInboxRefused = errors.New("bus: inbox refused")
)

// inboxRequestTimeout bounds one inbox retrieve/ack. Short on purpose: the
// receive leg polls, so a wedged relay must not park a poller for long.
const inboxRequestTimeout = 10 * time.Second

// maxInboxResponseBody bounds how much of an inbox response is read.
const maxInboxResponseBody = 4 << 20

// InboxPayloadKindField is the payload member a dispatched unit carries its
// envelope kind in (bus.KindWorkDispatch for work).
const InboxPayloadKindField = "kind"

// InboxMessage is one message read out of an agent inbox. The relay answers
// its payload base64-encoded, so the raw string is preserved and Decoded()
// returns the JSON bytes the sender wrote.
type InboxMessage struct {
	ID            string `json:"id"`
	AgentID       string `json:"agent_id"`
	Sender        string `json:"sender"`
	PayloadBase64 string `json:"payload"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	Acked         bool   `json:"acked"`
}

// Decoded returns the sender's payload bytes (the relay stores them base64).
func (m InboxMessage) Decoded() ([]byte, error) {
	if strings.TrimSpace(m.PayloadBase64) == "" {
		return nil, errors.New("bus: inbox message has an empty payload")
	}
	raw, err := base64.StdEncoding.DecodeString(m.PayloadBase64)
	if err != nil {
		return nil, fmt.Errorf("bus: inbox message %s payload is not base64: %w", m.ID, err)
	}
	return raw, nil
}

// Payload decodes the message payload into a generic map. A payload that is
// not a JSON object is an error — a reply the scheduler cannot read is not a
// reply it may correlate.
func (m InboxMessage) Payload() (map[string]any, error) {
	raw, err := m.Decoded()
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("bus: inbox message %s payload is not a JSON object: %w", m.ID, err)
	}
	return obj, nil
}

// InboxLease is one retrieve: the messages read and the lease that covers
// them. Ack requires the lease id — a message can only be released by the
// lease that read it.
type InboxLease struct {
	LeaseID  string         `json:"lease_id"`
	Messages []InboxMessage `json:"messages"`
}

// LoadAgentKeyFile reads the ed25519 private key `crier keygen` writes: a
// PKCS#8 PEM block (the same file the fleet's Python client consumes).
func LoadAgentKeyFile(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bus: read agent key %s: %w", path, err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, fmt.Errorf("bus: agent key %s is not PEM", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("bus: parse agent key %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("bus: agent key %s is %T, want ed25519", path, k)
	}
	return priv, nil
}

// SetAgentIdentity arms the agent-scoped inbox routes with an agent id and its
// ed25519 key. An empty id or a nil key leaves the client without an identity
// (the pure "no receive leg" state).
func (c *Client) SetAgentIdentity(agentID string, key ed25519.PrivateKey) {
	if c == nil {
		return
	}
	if strings.TrimSpace(agentID) == "" || len(key) != ed25519.PrivateKeySize {
		c.inboxAgentID = ""
		c.agentKey = nil
		return
	}
	c.inboxAgentID = strings.TrimSpace(agentID)
	c.agentKey = key
}

// InboxAgentID is the agent whose inbox this client reads ("" when the
// receive leg is not configured).
func (c *Client) InboxAgentID() string {
	if c == nil {
		return ""
	}
	return c.inboxAgentID
}

// InboxEnabled reports whether the agent-scoped inbox routes can be used.
func (c *Client) InboxEnabled() bool {
	return c != nil && c.enabled && c.inboxAgentID != "" && len(c.agentKey) == ed25519.PrivateKeySize
}

// signedAgentRequest builds (but does not send) an agent-scoped request with
// the documented signature trio. The signed path EXCLUDES the query string —
// the relay verifies the path it routes on, and the query is transport
// plumbing.
func (c *Client) signedAgentRequest(method, path string, body []byte, query url.Values) (*http.Request, error) {
	signedPath := path
	full := path
	if len(query) > 0 {
		full = path + "?" + query.Encode()
	}
	ts := strconv.FormatInt(c.now().Unix(), 10)
	sig := ed25519.Sign(c.agentKey, []byte(method+"\n"+signedPath+"\n"+ts))
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.baseURL+full, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("X-Agent-ID", c.inboxAgentID)
	req.Header.Set("X-Agent-Ts", ts)
	req.Header.Set("X-Agent-Sig", hex.EncodeToString(sig))
	return req, nil
}

// InboxRetrieve leases up to limit messages from the client's own inbox for
// leaseSeconds. Every returned message is invisible to other consumers until
// it is acked or the lease expires — callers must therefore lease briefly and
// ack only what they own.
//
// A client without an identity returns ErrAgentIdentityMissing WITHOUT any
// I/O: an unconfigured receive leg must never look like an empty inbox.
func (c *Client) InboxRetrieve(ctx context.Context, limit, leaseSeconds int) (InboxLease, error) {
	var lease InboxLease
	if !c.InboxEnabled() {
		return lease, fmt.Errorf("bus: inbox retrieve: %w", ErrAgentIdentityMissing)
	}
	if limit <= 0 {
		limit = 20
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 30
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("lease_seconds", strconv.Itoa(leaseSeconds))
	path := "/agents/" + url.PathEscape(c.inboxAgentID) + "/inbox"

	rctx, cancel := context.WithTimeout(ctx, inboxRequestTimeout)
	defer cancel()
	req, err := c.signedAgentRequest(http.MethodGet, path, nil, q)
	if err != nil {
		return lease, fmt.Errorf("bus: build inbox retrieve: %w", err)
	}
	resp, err := c.httpDo(rctx, req)
	if err != nil {
		return lease, fmt.Errorf("bus: inbox retrieve: %w: %w", ErrInboxUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxInboxResponseBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if len(bytes.TrimSpace(raw)) == 0 {
			return lease, nil
		}
		if err := json.Unmarshal(raw, &lease); err != nil {
			return lease, fmt.Errorf("bus: inbox retrieve: relay answered %d with an unreadable body: %w", resp.StatusCode, err)
		}
		return lease, nil
	case resp.StatusCode >= 500:
		return lease, fmt.Errorf("bus: inbox retrieve: %w (%d): %s", ErrInboxUnreachable, resp.StatusCode, relayErrorDetail(raw))
	default:
		return lease, fmt.Errorf("bus: inbox retrieve: %w (%d): %s", ErrInboxRefused, resp.StatusCode, relayErrorDetail(raw))
	}
}

// InboxAck releases a lease over the named messages. It is deliberately a
// separate, explicit call: a consumer must ack ONLY what it acted on, so an
// un-owned message returns to its owner when the lease lapses.
func (c *Client) InboxAck(ctx context.Context, leaseID string, messageIDs ...string) error {
	if !c.InboxEnabled() {
		return fmt.Errorf("bus: inbox ack: %w", ErrAgentIdentityMissing)
	}
	if strings.TrimSpace(leaseID) == "" || len(messageIDs) == 0 {
		return errors.New("bus: inbox ack: a lease id and at least one message id are required")
	}
	body, err := json.Marshal(map[string]any{"lease_id": leaseID, "message_ids": messageIDs})
	if err != nil {
		return fmt.Errorf("bus: marshal inbox ack: %w", err)
	}
	path := "/agents/" + url.PathEscape(c.inboxAgentID) + "/inbox/ack"
	actx, cancel := context.WithTimeout(ctx, inboxRequestTimeout)
	defer cancel()
	req, err := c.signedAgentRequest(http.MethodPost, path, body, nil)
	if err != nil {
		return fmt.Errorf("bus: build inbox ack: %w", err)
	}
	resp, err := c.httpDo(actx, req)
	if err != nil {
		return fmt.Errorf("bus: inbox ack: %w: %w", ErrInboxUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxInboxResponseBody))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("bus: inbox ack: %w (%d): %s", ErrInboxUnreachable, resp.StatusCode, relayErrorDetail(raw))
	}
	return fmt.Errorf("bus: inbox ack: %w (%d): %s", ErrInboxRefused, resp.StatusCode, relayErrorDetail(raw))
}

// httpDo issues a request through the client's shared HTTP client, widening
// its timeout when the shared one is shorter than this call's bound (the same
// courtesy Dispatch extends to blocking accepts).
func (c *Client) httpDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	hc := c.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: inboxRequestTimeout}
	}
	if hc.Timeout != 0 && hc.Timeout < inboxRequestTimeout {
		clone := *hc
		clone.Timeout = inboxRequestTimeout
		hc = &clone
	}
	return hc.Do(req.WithContext(ctx))
}
