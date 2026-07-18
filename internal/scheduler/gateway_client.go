package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GatewayClient calls the Hermes gateway API instead of spawning processes.
type GatewayClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewGatewayClient creates a client targeting the Hermes gateway API.
func NewGatewayClient(baseURL, apiKey string, timeout time.Duration) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// ResponseRequest mirrors the Hermes /v1/responses request body.
type ResponseRequest struct {
	Input string `json:"input"`
	Model string `json:"model,omitempty"`
}

// Response mirrors the Hermes /v1/responses response body.
type Response struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Model   string         `json:"model"`
	Output  []OutputItem   `json:"output"`
	Usage   Usage          `json:"usage"`
	Error   *ResponseError `json:"error,omitempty"`
}

// OutputItem is a message or tool call in the response output.
type OutputItem struct {
	Type    string           `json:"type"`
	Role    string           `json:"role,omitempty"`
	Content []ContentBlock   `json:"content,omitempty"`
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

// Ping checks whether the gateway API is reachable and authenticated.
func (g *GatewayClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", g.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	g.setAuth(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("gateway health: HTTP %d", resp.StatusCode)
	}
	return nil
}

// SendResponse sends a prompt to the gateway and returns the text result.
// This replaces exec.Command("hermes", "chat", "-q", prompt, ...)
func (g *GatewayClient) SendResponse(ctx context.Context, prompt, model string) (*Response, error) {
	reqBody := ResponseRequest{
		Input: prompt,
		Model: model,
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
	g.setAuth(req)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway POST: %w", err)
	}
	defer resp.Body.Close()

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

func (g *GatewayClient) setAuth(req *http.Request) {
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
}
