package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeGatewayJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestNewGatewayClient(t *testing.T) {
	const (
		baseURL = "http://gateway.example"
		apiKey  = "test-api-key"
	)
	timeout := 17 * time.Second

	client := NewGatewayClient(baseURL, apiKey, timeout)

	if client == nil {
		t.Fatal("NewGatewayClient returned nil")
	}
	if client.baseURL != baseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, baseURL)
	}
	if client.apiKey != apiKey {
		t.Errorf("apiKey = %q, want %q", client.apiKey, apiKey)
	}
	if client.timeout != timeout {
		t.Errorf("timeout = %v, want %v", client.timeout, timeout)
	}
	if client.httpClient == nil {
		t.Fatal("httpClient is nil")
	}
	if client.httpClient.Timeout != timeout {
		t.Errorf("httpClient.Timeout = %v, want %v", client.httpClient.Timeout, timeout)
	}
}

func TestResponseExtractText(t *testing.T) {
	tests := []struct {
		name     string
		response Response
		want     string
	}{
		{
			name: "empty output",
		},
		{
			name: "message without output text",
			response: Response{Output: []OutputItem{
				{Type: "message", Content: []ContentBlock{{Type: "input_text", Text: "ignored"}}},
			}},
		},
		{
			name: "single output text block",
			response: Response{Output: []OutputItem{
				{Type: "message", Content: []ContentBlock{{Type: "output_text", Text: "hello"}}},
			}},
			want: "hello",
		},
		{
			name: "first message output text wins",
			response: Response{Output: []OutputItem{
				{Type: "message", Content: []ContentBlock{{Type: "output_text", Text: "first"}}},
				{Type: "message", Content: []ContentBlock{{Type: "output_text", Text: "second"}}},
			}},
			want: "first",
		},
		{
			name: "non-message item is skipped",
			response: Response{Output: []OutputItem{
				{Type: "tool_call", Content: []ContentBlock{{Type: "output_text", Text: "ignored"}}},
				{Type: "message", Content: []ContentBlock{{Type: "output_text", Text: "answer"}}},
			}},
			want: "answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.response.ExtractText(); got != tt.want {
				t.Errorf("ExtractText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGatewayClientPing(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "200 OK", statusCode: http.StatusOK},
		{name: "403 Forbidden", statusCode: http.StatusForbidden, wantErr: true},
		{name: "500 Internal Server Error", statusCode: http.StatusInternalServerError, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %q, want %q", r.Method, http.MethodGet)
				}
				if r.URL.Path != "/health" {
					t.Errorf("path = %q, want %q", r.URL.Path, "/health")
				}
				if got := r.Header.Get("Authorization"); got != "Bearer ping-key" {
					t.Errorf("Authorization = %q, want %q", got, "Bearer ping-key")
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			client := NewGatewayClient(server.URL, "ping-key", time.Second)
			err := client.Ping(context.Background())
			if tt.wantErr && err == nil {
				t.Fatal("Ping() error = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Ping() error = %v, want nil", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(tt.statusCode)) {
				t.Errorf("Ping() error = %q, want HTTP status %d", err, tt.statusCode)
			}
		})
	}
}

func TestGatewayClientPingTransportErrors(t *testing.T) {
	t.Run("invalid URL", func(t *testing.T) {
		client := NewGatewayClient("http://[::1", "", time.Second)
		if err := client.Ping(context.Background()); err == nil {
			t.Fatal("Ping() error = nil, want invalid URL error")
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := server.URL
		server.Close()

		client := NewGatewayClient(baseURL, "", time.Second)
		if err := client.Ping(context.Background()); err == nil {
			t.Fatal("Ping() error = nil, want connection error")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := NewGatewayClient(server.URL, "", time.Second)
		if err := client.Ping(ctx); err == nil {
			t.Fatal("Ping() error = nil, want context cancellation error")
		}
	})
}

func TestGatewayClientSendResponseSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/v1/responses")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer response-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer response-key")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}

		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Input != "test prompt" {
			t.Errorf("request input = %q, want %q", request.Input, "test prompt")
		}
		if request.Model != "test-model" {
			t.Errorf("request model = %q, want %q", request.Model, "test-model")
		}

		writeGatewayJSON(t, w, http.StatusOK, `{
			"id":"resp-1",
			"status":"completed",
			"model":"test-model",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"gateway answer"}]}],
			"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}
		}`)
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "response-key", time.Second)
	response, err := client.SendResponse(context.Background(), "test prompt", "test-model")
	if err != nil {
		t.Fatalf("SendResponse() error = %v, want nil", err)
	}
	if response.ID != "resp-1" {
		t.Errorf("response ID = %q, want %q", response.ID, "resp-1")
	}
	if got := response.ExtractText(); got != "gateway answer" {
		t.Errorf("ExtractText() = %q, want %q", got, "gateway answer")
	}
	if response.Usage.TotalTokens != 5 {
		t.Errorf("total tokens = %d, want 5", response.Usage.TotalTokens)
	}
}

func TestGatewayClientSendResponseErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantError  string
	}{
		{
			name:       "response error field",
			statusCode: http.StatusOK,
			body:       `{"error":{"message":"bad request","type":"invalid_request"}}`,
			wantError:  "gateway error: invalid_request — bad request",
		},
		{
			name:       "non-200 status code",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":{"message":"try later","type":"unavailable"}}`,
			wantError:  "gateway error: unavailable — try later",
		},
		{
			name:       "invalid JSON response",
			statusCode: http.StatusOK,
			body:       `not-json`,
			wantError:  "unmarshal response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeGatewayJSON(t, w, tt.statusCode, tt.body)
			}))
			defer server.Close()

			client := NewGatewayClient(server.URL, "", time.Second)
			response, err := client.SendResponse(context.Background(), "prompt", "model")
			if err == nil {
				t.Fatal("SendResponse() error = nil, want an error")
			}
			if response != nil {
				t.Errorf("SendResponse() response = %#v, want nil", response)
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("SendResponse() error = %q, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestGatewayClientSendResponseContextTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := NewGatewayClient(server.URL, "", time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	response, err := client.SendResponse(ctx, "prompt", "model")
	if err == nil {
		t.Fatal("SendResponse() error = nil, want context timeout error")
	}
	if response != nil {
		t.Errorf("SendResponse() response = %#v, want nil", response)
	}
	if !strings.Contains(err.Error(), "gateway POST") {
		t.Errorf("SendResponse() error = %q, want gateway POST context", err)
	}
}

func TestGatewayClientSendResponseEmptyPrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty header", got)
		}

		var request ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Input != "" {
			t.Errorf("request input = %q, want empty string", request.Input)
		}
		if request.Model != "" {
			t.Errorf("request model = %q, want empty string", request.Model)
		}

		writeGatewayJSON(t, w, http.StatusOK, `{"id":"empty","status":"completed","output":[]}`)
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", time.Second)
	response, err := client.SendResponse(context.Background(), "", "")
	if err != nil {
		t.Fatalf("SendResponse() error = %v, want nil", err)
	}
	if response.ID != "empty" {
		t.Errorf("response ID = %q, want %q", response.ID, "empty")
	}
	if got := response.ExtractText(); got != "" {
		t.Errorf("ExtractText() = %q, want empty string", got)
	}
}

func TestGatewayClientResetHTTPClient(t *testing.T) {
	timeout := 23 * time.Second
	client := NewGatewayClient("http://gateway.example", "key", timeout)
	original := client.httpClient

	client.ResetHttpClient()

	if client.httpClient == nil {
		t.Fatal("httpClient is nil after ResetHttpClient()")
	}
	if client.httpClient == original {
		t.Fatal("ResetHttpClient() did not replace the http client")
	}
	if client.httpClient.Timeout != timeout {
		t.Errorf("httpClient.Timeout = %v, want %v", client.httpClient.Timeout, timeout)
	}
}
