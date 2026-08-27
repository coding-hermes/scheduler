package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrGatewayKeyRejected is the terminal classification for gateway 401/403
// responses (GAP-035). During the 2026-08-04 outage the gateway silently
// rejected per-foreman "fk-*" keys and every spawn kept failing with no
// distinct signal — 8208+ failed ticks fleet-wide. Anything that wraps this
// sentinel is an AUTH rejection, not a transient gateway error: the spawn
// path treats it as terminal (no exec fallback, no retry flood) and emits a
// HIGH event so a key regression is immediately visible.
var ErrGatewayKeyRejected = errors.New("gateway key rejected")

// GatewayClient calls the Hermes gateway API instead of spawning processes.
type GatewayClient struct {
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
		return nil, fmt.Errorf("gateway POST: HTTP %d: %s", resp.StatusCode, authErrorDetail(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
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
