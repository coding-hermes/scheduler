package bus

// SCHED-GAP-1665 — the DISPATCH leg (docs/dispatch-spec.md): the scheduler
// HANDS OUT work.
//
// The bus client already speaks two of the three verbs a fleet needs:
//
//	Publish  — announce (fire-and-forget visibility, REMOTE-004)
//	Query    — ask one peer a question and wait for the correlated reply
//	           (REMOTE-009)
//	Dispatch — hand ONE agent a unit of work                     ← this file
//
// WHY A SEPARATE VERB (and not Publish). Publish rides the relay topic and
// is best-effort by law: a failed publish is dropped with a log line,
// because losing a visibility event costs nothing. A DISPATCHED JOB is the
// opposite: if it evaporates the work simply never happens, and the fleet
// has already paid three times for exactly that silent-fallback failure
// shape. So Dispatch is deliberately NOT best-effort —
//
//	an unreachable, unknown or refusing target is a NAMED error, the
//	attempt carries the agent id + correlation id, and there is never a
//	fallback to another transport (no silent shared-gateway default).
//
// That inversion of the autonomy law is intentional and scoped to this
// path only: the autonomy law protects SCHEDULING from a bus outage; a
// dispatch is what the scheduler decided to schedule, so its failure must
// be loud enough to be recorded and retried by the caller.
//
// TRANSPORT. The relay's durable per-agent inbox
// (`POST /agents/{id}/inbox`, ~/crier/docs/openapi.yaml → inboxDeliver) is
// the agent-addressed, durable, idempotent surface — an agent that is
// offline when the work is handed out still receives it when it comes
// back. The correlation id is carried three ways in one request:
//
//	payload.corr_id  — the job's own identity, readable by the agent
//	request_id       — the relay's correlation passthrough
//	idempotency_key  — the relay's dedupe key, so a retry after an
//	                   ambiguous timeout cannot deliver the job twice
//
// THE PAYLOAD LAW. A dispatched unit carries the LANE the scheduler picked,
// a board/workdir REFERENCE the agent resolves to its own checkout, and the
// correlation id. It NEVER carries a task id: the scheduler picks the LANE,
// the foreman picks the TASK (SCHED-GAP-1665). A payload that named a task
// would move task selection into the scheduler — the exact boundary this
// verb exists to keep.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KindWorkDispatch is the payload `kind` stamped on every dispatched work
// item, so an agent can route on the envelope before reading the lane.
const KindWorkDispatch = "work.dispatch"

// dispatchTimeout bounds one whole deliver request. It is deliberately
// longer than publishTimeout (5s): a dispatch is an ACCEPT, but the relay
// may be talking to a target whose delivery_mode resolves to blocking (the
// relay's own default bound is 30s), and the caller must not tear the
// request down while the relay is still within its documented budget.
const dispatchTimeout = 35 * time.Second

// maxDispatchErrorBody bounds how much of a relay error body is read for
// the error message (the relay answers a small JSON `{"error": "..."}`).
const maxDispatchErrorBody = 8 << 10

// Dispatch failures, each a NAMED sentinel so a caller can branch (and a
// caller MUST branch — never ignore one):
//
//	ErrDispatchTargetUnknown — the relay has no such agent (404): the
//	    target is wrong or unregistered; retrying unchanged is pointless.
//	ErrDispatchRefused       — the relay refused the delivery (4xx: bad
//	    request, guard block, namespace mismatch, over-quota): the job is
//	    NOT queued and must not be reported as handed out.
//	ErrDispatchUnreachable   — the relay could not be reached at all
//	    (dial/timeout) or reported a remote delivery failure (5xx): the
//	    attempt is retryable, and the correlation id makes the retry safe.
//	ErrDispatchFailed        — any other non-accept status: unknown shape,
//	    surfaced rather than guessed at.
var (
	ErrDispatchTargetUnknown = errors.New("bus: dispatch target unknown")
	ErrDispatchRefused       = errors.New("bus: dispatch refused")
	ErrDispatchUnreachable   = errors.New("bus: dispatch unreachable")
	ErrDispatchFailed        = errors.New("bus: dispatch failed")
)

// WorkItem is one unit of work handed to one agent. The zero value is
// invalid by construction — Dispatch validates before it opens a
// connection.
//
// Lane + (Board or Workdir) + correlation id, and nothing else. There is
// deliberately no task-id field: see the payload law at the top of this
// file.
type WorkItem struct {
	// Lane is the scheduler's choice: the lane whose work this is. Required.
	Lane string
	// Board is a reference the agent resolves to the lane's board
	// (path, or path@host for a remote box). One of Board/Workdir is
	// required.
	Board string
	// Workdir is a reference the agent resolves to its own checkout. One of
	// Board/Workdir is required.
	Workdir string
	// CorrID is the correlation id (docs/federation-query-spec.md §2.5).
	// Empty means Dispatch mints one; either way the receipt echoes it and
	// the same value is sent as the request id and the idempotency key.
	CorrID string
}

// validate refuses an incomplete work item by name, before any I/O. A
// half-addressed job is never put on the wire.
func (w WorkItem) validate() error {
	if strings.TrimSpace(w.Lane) == "" {
		return errors.New("bus: dispatch: lane is required")
	}
	if strings.TrimSpace(w.Board) == "" && strings.TrimSpace(w.Workdir) == "" {
		return errors.New("bus: dispatch: a board or workdir reference is required")
	}
	return nil
}

// payload renders the work item as the deliver body's payload object. Keys
// are stable and json.Marshal orders map keys, so the wire shape is
// deterministic (and diffable in tests). No task field exists, here or
// anywhere in the dispatch path.
func (w WorkItem) payload(schedulerID string, now time.Time) map[string]any {
	p := map[string]any{
		"kind":         KindWorkDispatch,
		"lane":         w.Lane,
		"corr_id":      w.CorrID,
		"scheduler_id": schedulerID,
		"issued_at":    now.UTC().Format(time.RFC3339Nano),
	}
	if w.Board != "" {
		p["board"] = w.Board
	}
	if w.Workdir != "" {
		p["workdir"] = w.Workdir
	}
	return p
}

// dispatchRequest is the documented POST /agents/{id}/inbox body for a
// work dispatch. `sender` is the scheduler's bus identity, so the agent can
// see WHO handed it the work; `request_id` is the relay's correlation
// passthrough and `idempotency_key` its dedupe key — both carry the SAME
// correlation id as the payload.
type dispatchRequest struct {
	Payload        map[string]any `json:"payload"`
	Sender         string         `json:"sender"`
	RequestID      string         `json:"request_id"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// dispatchAccept is the relay's accept body (201 inbox / 200 blocking
// webhook / 202 async or held). Not every member is present on every
// shape, so every member is optional here.
type dispatchAccept struct {
	ID               string `json:"id"`
	Transport        string `json:"transport"`
	Status           string `json:"status"`
	IdempotentReplay bool   `json:"idempotent_replay"`
	Error            string `json:"error"`
}

// DispatchReceipt is the evidence that a job was handed out. It is returned
// ONLY on an accept (2xx); a failure returns the zero receipt plus a named
// error, so a caller can never mistake a failed hand-out for a queued job.
type DispatchReceipt struct {
	// AgentID is the target actually addressed.
	AgentID string
	// CorrID is the correlation id this attempt carries (minted when the
	// caller left it empty). It is the key a reply is matched on.
	CorrID string
	// MessageID is the relay's id for the accepted delivery ("" when the
	// accept shape carries none — e.g. a held federation delivery).
	MessageID string
	// Transport is where the relay put it: "inbox" (durable, retrievable by
	// the agent), "webhook" (handed to the target's webhook queue), or
	// "held". Empty means the relay accepted without naming one.
	Transport string
	// IdempotentReplay is true when the relay answered from the recorded
	// accept of an EARLIER delivery under the same idempotency key: nothing
	// was delivered twice, and MessageID names the first (only) message.
	IdempotentReplay bool
	// Status is the relay's own status member when it sent one (e.g.
	// "held").
	Status string
}

// BuildDispatch is the PURE (no-I/O) rendering of one dispatch: the exact
// endpoint and body Dispatch will POST, plus the correlation id the attempt
// carries. It validates the target and the work item, so an invalid
// dispatch can never be rendered — let alone sent.
//
// It is exported for two reasons: an operator tool can show a DRY RUN that
// is byte-identical to the real request (evidence before action), and tests
// can pin the wire shape without a socket.
func BuildDispatch(baseURL, schedulerID, agentID string, item WorkItem, now time.Time) (endpoint string, body []byte, corrID string, err error) {
	target := strings.TrimSpace(agentID)
	if target == "" {
		return "", nil, "", fmt.Errorf("bus: dispatch refused: %w", errors.New("agent id is required"))
	}
	if verr := item.validate(); verr != nil {
		return "", nil, "", verr
	}
	if item.CorrID == "" {
		// A caller that left the correlation id empty gets a unique one;
		// the scheduler id + wall-nano + counter shape survives a restart
		// (docs/federation-query-spec.md §2.5).
		item.CorrID = fmt.Sprintf("q-%s-%d-%016d", sanitizeTopicToken(schedulerID), now.UnixNano(), queryCorrCounter.Add(1))
	}
	raw, merr := json.Marshal(dispatchRequest{
		Payload:        item.payload(schedulerID, now),
		Sender:         "scheduler-" + schedulerID,
		RequestID:      item.CorrID,
		IdempotencyKey: item.CorrID,
	})
	if merr != nil {
		return "", nil, "", fmt.Errorf("bus: marshal dispatch (corr %s): %w", item.CorrID, merr)
	}
	return baseURL + "/agents/" + url.PathEscape(target) + "/inbox", raw, item.CorrID, nil
}

// Dispatch hands ONE unit of work to ONE named agent over the relay's
// durable inbox. See the file comment for why this verb is loud where
// Publish is silent.
//
// A disabled client is an ERROR here, not the no-op it is for Publish: an
// un-dispatched job must never look like a dispatched one. The caller
// degrades deliberately (e.g. local spawn) or not at all.
//
// No status path is retried and no other transport is tried: one call is
// one attempt, and the attempt's identity (agent, correlation id, relay
// status) is in the returned error. The idempotency key makes an
// operator-level retry of the SAME correlation id safe.
func (c *Client) Dispatch(ctx context.Context, agentID string, item WorkItem) (DispatchReceipt, error) {
	target := strings.TrimSpace(agentID)
	if !c.Enabled() {
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch to %q not attempted: %w", target, ErrDisabled)
	}
	endpoint, body, corr, err := BuildDispatch(c.baseURL, c.schedID, target, item, c.now())
	if err != nil {
		return DispatchReceipt{}, err
	}

	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return DispatchReceipt{}, fmt.Errorf("bus: build dispatch (corr %s): %w", corr, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.agentID != "" {
		req.Header.Set("X-Agent-ID", c.agentID)
	}

	// The shared http.Client carries publishTimeout (5s), which would cut a
	// blocking-webhook accept short. Copy it with the wider dispatch bound
	// rather than mutating the client every other verb shares.
	hc := c.httpClient
	if hc == nil {
		hc = &http.Client{}
	}
	if hc.Timeout != 0 && hc.Timeout < dispatchTimeout {
		clone := *hc
		clone.Timeout = dispatchTimeout
		hc = &clone
	}
	resp, err := hc.Do(req)
	if err != nil {
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): %w: %w", target, corr, ErrDispatchUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDispatchErrorBody))
	detail := relayErrorDetail(raw)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var acc dispatchAccept
		if len(bytes.TrimSpace(raw)) > 0 {
			if uerr := json.Unmarshal(raw, &acc); uerr != nil {
				return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): relay accepted %d with an unreadable body: %w", target, corr, resp.StatusCode, ErrDispatchFailed)
			}
		}
		return DispatchReceipt{
			AgentID:          target,
			CorrID:           corr,
			MessageID:        acc.ID,
			Transport:        acc.Transport,
			IdempotentReplay: acc.IdempotentReplay,
			Status:           acc.Status,
		}, nil
	case resp.StatusCode == http.StatusNotFound:
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): %w: %s", target, corr, ErrDispatchTargetUnknown, detail)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): %w (%d): %s", target, corr, ErrDispatchRefused, resp.StatusCode, detail)
	case resp.StatusCode >= 500:
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): %w (%d): %s", target, corr, ErrDispatchUnreachable, resp.StatusCode, detail)
	default:
		return DispatchReceipt{}, fmt.Errorf("bus: dispatch agent %q (corr %s): %w (relay answered %d): %s", target, corr, ErrDispatchFailed, resp.StatusCode, detail)
	}
}

// relayErrorDetail renders a bounded relay error body for an error message:
// the relay's `error` member when the body is the documented JSON shape,
// else the trimmed raw body, else a placeholder. It never invents detail
// and never echoes anything credential-like (only the relay's own answer is
// read).
func relayErrorDetail(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "(no body)"
	}
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch {
		case e.Error != "" && e.Message != "":
			return e.Error + ": " + e.Message
		case e.Error != "":
			return e.Error
		case e.Message != "":
			return e.Message
		}
	}
	s := string(raw)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
