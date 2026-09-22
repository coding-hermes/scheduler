package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ErrGatewayKeyRejected is the terminal classification for gateway 401/403
// responses (GAP-035). During the 2026-08-04 outage the gateway silently
// rejected per-foreman "fk-*" keys and every spawn kept failing with no
// distinct signal — 8208+ failed ticks fleet-wide. Anything that wraps this
// sentinel is an AUTH rejection, not a transient gateway error: the spawn
// path treats it as terminal (no exec fallback, no retry flood) and emits a
// HIGH event so a key regression is immediately visible.
var ErrGatewayKeyRejected = errors.New("gateway key rejected")

// ErrGatewayTransient is the classification for transport-level gateway
// failures that are worth a bounded retry (SCHED-GAP-080): response-body
// read errors and unmarshal errors. HTTP 5xx responses are classified via
// *GatewayStatusError instead, and network/timeout failures via *url.Error;
// IsTransientGatewayErr recognizes all three. It is never used for 401/403 —
// those stay ErrGatewayKeyRejected (terminal, GAP-035, no retry flood).
var ErrGatewayTransient = errors.New("gateway transient error")

// GatewayStatusError carries the HTTP status code of a non-2xx, non-auth
// gateway response (SCHED-GAP-080). The message is byte-identical to the
// legacy plain-error text ("gateway POST: HTTP <code>: <detail>") so
// existing assertions on error text keep passing; the StatusCode field lets
// the spawn path classify 5xx as transient and retry.
//
// SCHED-GAP-136: retryAfter carries the server's Retry-After hint (parsed
// seconds-form only; the gateway's 503 drain response sends
// "Retry-After: 1"). 0 = absent/unparseable. The spawn retry loop uses it
// as a FLOOR on its backoff sleep — never below what the server asked for.
type GatewayStatusError struct {
	StatusCode int
	RetryAfter time.Duration
	msg        string
}

// Error returns the legacy gateway error text (unchanged from pre-GAP-080).
func (e *GatewayStatusError) Error() string {
	return e.msg
}

// IsTransientGatewayErr reports whether err is a transient gateway failure
// worth a bounded same-pair retry (SCHED-GAP-080): HTTP 5xx responses,
// network/timeout errors from client.Do (*url.Error), and read/unmarshal
// failures (ErrGatewayTransient). 401/403 are NEVER transient — they classify
// as ErrGatewayKeyRejected (terminal, GAP-035) and the retry path must not
// touch them. nil is never transient.
func IsTransientGatewayErr(err error) bool {
	if err == nil {
		return false
	}
	// Auth rejections stay terminal — the 2026-08-04 flood guard (GAP-035).
	if errors.Is(err, ErrGatewayKeyRejected) {
		return false
	}
	var gse *GatewayStatusError
	if errors.As(err, &gse) {
		return gse.StatusCode >= http.StatusInternalServerError
	}
	// client.Do wraps network failures, timeouts and context cancellation in
	// *url.Error — all transient (the tick context bounds the retry window).
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return errors.Is(err, ErrGatewayTransient)
}

// gatewayTransientBlip reports whether a TERMINAL gateway error — one that has
// already exhausted the SCHED-GAP-080 bounded same-pair retry — is a transient
// BLIP the harness should DEFER (SCHED-GAP-203) rather than charge to the lane.
//
// This is deliberately NOT the same predicate as IsTransientGatewayErr, and the
// difference is the whole point of the row. "Worth retrying" and "never the
// lane's fault" are different questions, and two sub-classes of the retryable
// set answer the second one NO:
//
//   - context.DeadlineExceeded / context.Canceled: our OWN deadline expired
//     (the tick ctx, or a cancelled drain). The tick consumed the wall time it
//     was given — that is TickTimeout semantics with its own, already-shipped
//     contract ("no timeout backoff"), not a gateway blip. Deferring it would
//     silently convert a runaway tick into an invisible one.
//
//   - *GatewayStatusError (HTTP 5xx, the drain 503 included): a gateway
//     REFUSAL that already has its own failure class (SCHED-GAP-143's
//     ticks.failure_reason gateway_drain / gateway_transport, SCHED-GAP-136's
//     Retry-After pacing, and the auto-disable exclusion both surfaces share).
//     Those rows stay failures WITH a marker — the design that SCHED-GAP-134/143
//     landed deliberately — so the deferral path must not swallow them.
//
// What IS a blip, and what the live evidence (SCHED-GAP-203) actually measured:
//
//   - ErrGatewayTransient: the response body could not be read, could not be
//     unmarshalled, or an SSE stream ended without a terminal event — the
//     mid-run drop shape.
//   - *url.Error: the dial/handshake never completed. NOTE this shape is NOT
//     wrapped in ErrGatewayTransient (gateway_client.go returns
//     fmt.Errorf("gateway POST: %w", err) around the *url.Error), which is why
//     a predicate built on errors.Is(err, ErrGatewayTransient) alone would fix
//     only half the measured sample — the refused connect
//     (`gateway POST: Post "http://127.0.0.1:8642/v1/responses": dial tcp …:
//     connect: connection refused`, 5 of the 9 ticks) would keep failing.
func gatewayTransientBlip(err error) bool {
	if !IsTransientGatewayErr(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// Everything left is a blip EXCEPT *GatewayStatusError (HTTP 5xx / drain),
	// which keeps its own failure class — see the doc above.
	var gse *GatewayStatusError
	return !errors.As(err, &gse)
}

// GatewayClient calls the Hermes gateway API instead of spawning processes.
type GatewayClient struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk        clockSeam
	baseURL    string
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
}

// NewGatewayClient creates a client targeting the Hermes gateway API.
func NewGatewayClient(baseURL, apiKey string, timeout time.Duration) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// ResponseRequest mirrors the Hermes /v1/responses request body.
type ResponseRequest struct {
	Input           string `json:"input"`
	Model           string `json:"model,omitempty"`
	Provider        string `json:"provider,omitempty"`         // empty = gateway default (was silently defaulting fleet spawns to the main key)
	RequireApproval *bool  `json:"require_approval,omitempty"` // nil = use gateway default, false = disable approvals
	// Stream (SCHED-GAP-119): true asks the gateway for the SSE surface so
	// the scheduler can observe turn activity and apply an IDLE deadline
	// instead of GAP-117's wall clock. Omitted (nil) on the legacy path.
	Stream *bool `json:"stream,omitempty"`
}

// Response mirrors the Hermes /v1/responses response body.
type Response struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Model  string         `json:"model"`
	Output []OutputItem   `json:"output"`
	Usage  Usage          `json:"usage"`
	Error  *ResponseError `json:"error,omitempty"`
}

// OutputItem is a message or tool call in the response output.
type OutputItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []ContentBlock `json:"content,omitempty"`
}

// ContentBlock is a block of content (text, tool_use, etc.)
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Usage holds token usage info.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponseError is an error from the API.
type ResponseError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// authErrorDetail extracts a human-readable detail from a gateway error
// body. The envelope {"error": {"type", "message"}} is preferred (keeps the
// gateway's own classification, e.g. "auth_error: Invalid gateway API key");
// anything unparseable falls back to the raw trimmed body.
func authErrorDetail(body []byte) string {
	var envelope struct {
		Error ResponseError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		switch {
		case envelope.Error.Type != "" && envelope.Error.Message != "":
			return envelope.Error.Type + ": " + envelope.Error.Message
		case envelope.Error.Message != "":
			return envelope.Error.Message
		}
	}
	return strings.TrimSpace(string(body))
}

// ExtractText returns the first output_text block content, or empty string.
func (r *Response) ExtractText() string {
	for _, item := range r.Output {
		if item.Type == "message" {
			for _, block := range item.Content {
				if block.Type == "output_text" {
					return block.Text
				}
			}
		}
	}
	return ""
}

// HasAssistantMessage reports whether the response carries any assistant
// output item (role="assistant").
//
// SCHED-GAP-205: a status="completed" response with NO assistant output item
// but non-zero token usage is a provider that billed for tokens and never
// produced a real assistant message — the same zero-assistant shape as the
// 102 instant-death (0/0 tokens), only with real billing behind it. Empty
// Output with 0/0 usage stays 102's domain; this helper feeds the gate arm
// that catches the non-zero-token zero-assistant completion.
func (r *Response) HasAssistantMessage() bool {
	for _, item := range r.Output {
		if item.Role == "assistant" {
			return true
		}
	}
	return false
}

// Ping checks whether the gateway API is reachable and authenticated with
// the client's shared daemon key.
func (g *GatewayClient) Ping(ctx context.Context) error {
	return g.health(ctx, "")
}

// ValidateKey probes the gateway with the given key (GAP-035). A per-project
// key is validated BEFORE dispatch so a rejected key fails the tick fast with
// a clear classification instead of burning a full SendResponse cycle (and
// potentially thousands of them fleet-wide). Returns ErrGatewayKeyRejected
// (wrapped) on 401/403, a plain error on other failures. A gateway whose
// /health does not authenticate keys returns nil — the SendResponse status
// check is the dispatch-time backstop.
func (g *GatewayClient) ValidateKey(ctx context.Context, key string) error {
	return g.health(ctx, key)
}

// health performs the authenticated /health probe shared by Ping (daemon
// key) and ValidateKey (per-project key). 401/403 are classified as
// ErrGatewayKeyRejected so callers can distinguish a key regression from a
// transient gateway outage.
func (g *GatewayClient) health(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", g.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	g.setAuth(req, key)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w (HTTP %d): %s", ErrGatewayKeyRejected, resp.StatusCode, authErrorDetail(body))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// SendResponse sends a prompt to the gateway and returns the text result.
// This replaces exec.Command("hermes", "chat", "-q", prompt, ...)
//
// key overrides the client's default API key for this one request. Pass ""
// to use the daemon-level shared key (--gateway-key). Foreman spawns pass
// project.GatewayKey when set, so each foreman authenticates with its own
// key (Bane 2026-07-31).
func (g *GatewayClient) SendResponse(ctx context.Context, prompt, model, provider, key string) (*Response, error) {
	return g.SendResponseWithSessionKey(ctx, prompt, model, provider, key, "")
}

// SendResponseWithSessionKey is SendResponse plus the optional
// X-Hermes-Session-Key header. A non-empty sessionKey scopes the run under
// that stable identifier (SCHED-GAP-074): scheduler ticks pass their tick id
// so every spawned session carries a durable, self-describing handle for
// fleet-quality review linkage, independent of the per-request resp_* id
// that lands in ticks.session_id. An empty sessionKey sends no header,
// matching legacy behavior exactly.
func (g *GatewayClient) SendResponseWithSessionKey(ctx context.Context, prompt, model, provider, key, sessionKey string) (*Response, error) {
	noApproval := false
	reqBody := ResponseRequest{
		Input:           prompt,
		Model:           model,
		Provider:        provider,
		RequireApproval: &noApproval, // scheduler agents never need approval
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", g.baseURL+"/v1/responses",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionKey != "" {
		req.Header.Set("X-Hermes-Session-Key", sessionKey)
	}
	g.setAuth(req, key)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// GAP-035: the gateway response STATUS is authoritative. Before this
	// guard, a 401 whose body happened to be valid JSON without an "error"
	// field unmarshalled into an empty Response and the spawn "succeeded"
	// silently — the exact failure mode of the 2026-08-04 outage. 401/403
	// are classified as ErrGatewayKeyRejected (terminal, no retry flood);
	// any other non-2xx fails the request instead of pretending success.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w (HTTP %d): %s", ErrGatewayKeyRejected, resp.StatusCode, authErrorDetail(body))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// SCHED-GAP-080: carry the status code so the spawn path can
		// classify 5xx as transient. Error() text is byte-identical to the
		// legacy plain error ("gateway POST: HTTP <code>: <detail>").
		// SCHED-GAP-136: parse Retry-After (seconds-form) so the retry
		// loop can honor the server's pacing hint.
		retryAfter := time.Duration(0)
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, &GatewayStatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter,
			msg:        fmt.Sprintf("gateway POST: HTTP %d: %s", resp.StatusCode, authErrorDetail(body)),
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// SCHED-GAP-080: a body-read failure is transport-level transient —
		// the response was accepted but never fully received.
		return nil, fmt.Errorf("read response: %w: %w", ErrGatewayTransient, err)
	}

	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		// SCHED-GAP-080: an unparseable 2xx body is a gateway-side transient
		// (e.g. a 500 HTML error page behind a 200, or mid-restart garbage).
		return nil, fmt.Errorf("unmarshal response: %w: %w", ErrGatewayTransient, err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("gateway error: %s — %s", result.Error.Type, result.Error.Message)
	}

	return &result, nil
}

// setAuth sets the Authorization header. A non-empty key overrides the
// client default (per-foreman key); empty falls back to the shared daemon key.
func (g *GatewayClient) setAuth(req *http.Request, key string) {
	effective := key
	if effective == "" {
		effective = g.apiKey
	}
	if effective != "" {
		req.Header.Set("Authorization", "Bearer "+effective)
	}
}

// ResetHttpClient replaces the internal http.Client with a fresh one,
// avoiding stale connection pools after a gateway restart.
func (g *GatewayClient) ResetHttpClient() {
	g.httpClient = &http.Client{Timeout: g.timeout}
}

// SetClock installs the clock this gateway client reads and waits on
// (SCHED-GAP-169). nil keeps the wall clock.
func (g *GatewayClient) SetClock(c clock.Clock) { g.clk.Set(c) }

// clock returns the client's clock, never nil.
func (g *GatewayClient) clock() clock.Clock { return g.clk.Get() }
