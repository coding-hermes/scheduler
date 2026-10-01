// Package bus provides the REMOTE-004 visibility flow over Crier: a small
// HTTP client that publishes tick-terminal and lane-state events to the
// scheduler's topic (docs/remote-spec.md §3) and a subscriber that receives
// peer events for idempotent ingestion.
//
// THE AUTONOMY LAW (§3): a Crier failure — down, timeout, refused — NEVER
// blocks or fails a tick. Every failure mode is logged and dropped; publish
// is best-effort by construction, and a disabled client is a pure no-op that
// never opens a connection. The transport speaks only what
// ~/crier/docs/openapi.yaml documents as shipped:
//
//	POST /relay/publish   body {"topic": "...", "event": {...}} → 202
//	GET  /relay/subscribe/{topic}  WebSocket upgrade, frames
//	{"topic": "<literal>", "event": {...}}
//
// Auth is opt-in (CR_AUTH_TOKEN): when a token is configured the client
// sends `Authorization: Bearer <token>`; without one it sends nothing, which
// is what a default relay accepts. The X-Agent-ID header is sent when the
// scheduler knows its own id, because a rate-limit-enabled relay keys the
// publish budget on it; it is never required.
package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// DefaultURL is the [crier] url default — a local relay.
const DefaultURL = "http://127.0.0.1:8767"

// DefaultTopics is the subscriber's default wildcard topic list: every
// scheduler's tick topic (a `>` matches one or more trailing dot-separated
// segments, so sched.tick.<scheduler_id> matches for any id).
var DefaultTopics = []string{"sched.tick.>"}

// publishTimeout bounds a single POST /relay/publish. A publish is
// best-effort: the deadline exists so a wedged relay cannot stall a tick's
// terminal path beyond this bound.
const publishTimeout = 5 * time.Second

// Envelope is the visibility event carried as the publish body's `event`
// (docs/remote-spec.md §3 + §5). The pair (SchedulerID, EventID) is the
// idempotency key: EventID is a MONOTONIC per-scheduler counter value
// ("counter + scheduler_id", never a random uuid — §8), so a replayed event
// can be dropped by key on ingest (REMOTE-006's replay is safe).
type Envelope struct {
	SchedulerID string `json:"scheduler_id"`
	EventID     string `json:"event_id"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	TickID      string `json:"tick_id"`
	Status      string `json:"status"`
	TS          string `json:"ts"`
}

// KindTickTerminal is the event kind for a tick reaching a terminal state
// (completed / failed / deferred / timeout).
const KindTickTerminal = "tick.terminal"

// KindLaneState is the event kind for a lane state change (pause/resume/
// auto-disable).
const KindLaneState = "lane.state"

// Lane-state vocabulary for the envelope's status field on kind=lane.state
// events (REMOTE-004 §3): the enabled flag's direction plus who moved it.
const (
	// LaneStatePaused — the lane was paused (POST /projects/{name}/pause).
	LaneStatePaused = "paused"
	// LaneStateResumed — the lane was resumed (POST /projects/{name}/resume).
	LaneStateResumed = "resumed"
	// LaneStateDisabled — the lane was auto-disabled (failure-rate policy).
	LaneStateDisabled = "disabled"
	// LaneStateEnabled — the lane was enabled via the PUT/POST create path.
	LaneStateEnabled = "enabled"
)

// publishRequest is the documented POST /relay/publish body shape. The
// namespace realm header is deliberately not sent: the scheduler publishes
// in the default namespace, and a namespace MEMBER in the body is refused
// with 400 (openapi: "a `namespace` MEMBER in the body is refused with 400").
type publishRequest struct {
	Topic string   `json:"topic"`
	Event Envelope `json:"event"`
}

// Client is the Crier bus client. The zero value is safe but inert; use
// NewClient. A disabled client (Enabled() == false) is a PURE no-op:
// Publish returns nil without any I/O and Subscribe returns ErrDisabled,
// which callers treat as "nothing to do", never as an error.
type Client struct {
	baseURL   string
	topic     string
	token     string
	schedID   string
	agentID   string
	enabled   bool
	seq       func() uint64 // monotonic per-scheduler counter
	subTopics []string

	httpClient *http.Client
	// now is the timestamp source, overridable in tests. It is NOT the
	// process clock seam (SCHED-GAP-169): the bus never schedules, it only
	// stamps outgoing envelopes, and the stdlib guard scopes in-repo time
	// reads — an injectable func here keeps the client free of both.
	now func() time.Time
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithHTTPClient installs a custom *http.Client (tests: stub servers with
// injected latency or failures).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithClock installs the envelope timestamp source (tests: fixed time).
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}

// NewClient builds a bus client. enabled=false yields the documented pure
// no-op: Publish never touches the network. baseURL/token default when
// empty (DefaultURL; no token = no Authorization header, the opt-in auth
// shape). schedulerID participates in every event_id it mints, so callers
// MUST pass the stable REMOTE-003 identity.
func NewClient(enabled bool, baseURL, token, schedulerID string, opts ...Option) *Client {
	c := &Client{
		enabled:    enabled,
		baseURL:    baseURL,
		token:      token,
		schedID:    schedulerID,
		httpClient: &http.Client{Timeout: publishTimeout},
		now:        time.Now,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultURL
	}
	c.topic = TickTopic(c.schedID)
	c.agentID = "scheduler-" + c.schedID
	c.subTopics = DefaultTopics
	var seq uint64
	c.seq = func() uint64 { seq++; return seq }
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Enabled reports whether the client will actually publish. Disabled and
// misconfigured clients (no scheduler id) report false and callers skip
// their work entirely.
func (c *Client) Enabled() bool { return c != nil && c.enabled && c.schedID != "" }

// Topics returns the subscriber's topic list (never nil — the default when
// unconfigured, empty when the caller disabled subscription).
func (c *Client) Topics() []string { return append([]string(nil), c.subTopics...) }

// Topic returns the publish topic: sched.tick.<scheduler_id>.
func (c *Client) Topic() string { return c.topic }

// SetSubscribeTopics replaces the subscriber topic list. Empty disables
// subscription entirely (publishing stays armed).
func (c *Client) SetSubscribeTopics(topics []string) {
	c.subTopics = append([]string(nil), topics...)
}

// TickTopic is the scheduler's publish topic: sched.tick.<scheduler_id>
// (docs/remote-spec.md §3, "one publisher per scheduler").
func TickTopic(schedulerID string) string { return "sched.tick." + schedulerID }

// NextEventID mints the next monotonic per-scheduler event id: a plain
// counter joined with the scheduler id (docs/remote-spec.md §8 — NOT a
// random uuid, so replay ordering within a peer is stable and the id is
// unique across schedulers without coordination).
func (c *Client) NextEventID() string {
	return fmt.Sprintf("%s-%020d", c.schedID, c.seq())
}

// Publish sends one envelope to this scheduler's topic. THE AUTONOMY LAW:
// every failure mode — disabled client, empty id, marshal error, dial
// error, timeout, non-202 — logs and returns NIL. A Crier outage can never
// fail or block a tick's terminal path; the publish is simply lost.
func (c *Client) Publish(ctx context.Context, env Envelope) error {
	if !c.Enabled() {
		return nil // pure no-op: no connection, no error
	}
	// The publisher owns the identity pair and the timestamp: a caller
	// that leaves EventID empty gets the monotonic id, and a stale caller
	// stamp is overwritten. (scheduler_id is NOT overwritten — a test may
	// deliberately publish a foreign id to exercise ingest dedupe.)
	if env.EventID == "" {
		env.EventID = c.NextEventID()
	}
	if env.SchedulerID == "" {
		env.SchedulerID = c.schedID
	}
	env.TS = c.now().UTC().Format(time.RFC3339Nano)

	body, err := json.Marshal(publishRequest{Topic: c.topic, Event: env})
	if err != nil {
		log.Printf("CRIER: marshal publish dropped: %v", err)
		return nil
	}

	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pubCtx, http.MethodPost, c.baseURL+"/relay/publish", bytes.NewReader(body))
	if err != nil {
		log.Printf("CRIER: build publish dropped: %v", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		// Opt-in auth (Crier CR_AUTH_TOKEN): Bearer ONLY when configured.
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.agentID != "" {
		// Rate-limited relays key the publish budget on X-Agent-ID.
		req.Header.Set("X-Agent-ID", c.agentID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Down, refused, timed out — log and drop (§3 failure behaviour).
		log.Printf("CRIER: publish %s dropped (bus unreachable): %v", env.Kind, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusAccepted {
		log.Printf("CRIER: publish %s dropped (relay answered %d)", env.Kind, resp.StatusCode)
	}
	return nil
}

// PublishTickTerminal builds and publishes the tick-terminal envelope for a
// tick that just reached a terminal status. Same autonomy law as Publish.
func (c *Client) PublishTickTerminal(ctx context.Context, project, tickID, status string) error {
	return c.Publish(ctx, Envelope{
		Kind:    KindTickTerminal,
		Project: project,
		TickID:  tickID,
		Status:  status,
	})
}
