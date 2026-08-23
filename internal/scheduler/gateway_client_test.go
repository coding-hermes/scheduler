package scheduler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func TestGatewayClient_SendResponse_DisablesApprovals(t *testing.T) {
	// Capture the request body sent to the gateway and verify
	// require_approval=false is always present.
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read and capture the full body.
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		capturedBody = buf[:n]

		// Return a minimal valid response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "test output"},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  100,
				"output_tokens": 50,
				"total_tokens":  150,
			},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	resp, err := client.SendResponse(t.Context(), "test prompt", "test-model", "", "")
	if err != nil {
		t.Fatalf("SendResponse: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
		return
	}

	// Parse captured body and verify require_approval field.
	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("unmarshal captured body: %v (raw: %s)", err, string(capturedBody))
	}

	ra, ok := body["require_approval"]
	if !ok {
		t.Fatal("require_approval field MISSING from request body — regression!")
	}
	raBool, ok := ra.(bool)
	if !ok {
		t.Fatalf("require_approval is not a bool: %T = %v", ra, ra)
	}
	if raBool != false {
		t.Fatalf("require_approval = %v, want false — approvals would be ENABLED!", raBool)
	}

	t.Logf("OK: require_approval=false confirmed in gateway request body")
}

func TestGatewayClient_SendResponse_IncludesModel(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		capturedBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	_, err := client.SendResponse(t.Context(), "test prompt", "deepseek-v4-pro", "", "")
	if err != nil {
		t.Fatalf("SendResponse: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body["model"] != "deepseek-v4-pro" {
		t.Errorf("model = %v, want deepseek-v4-pro", body["model"])
	}
	if body["input"] != "test prompt" {
		t.Errorf("input = %v, want 'test prompt'", body["input"])
	}
}

// Regression test for the 2026-08-23 cost-audit bug: the gateway spawn
// dropped the provider entirely, so fleet ticks silently defaulted to the
// main key instead of the foreman key pinned in fleet.toml.
func TestGatewayClient_SendResponse_IncludesProvider(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		capturedBody = buf[:n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	if _, err := client.SendResponse(t.Context(), "prompt", "deepseek-v4-flash", "deepseek-foreman", ""); err != nil {
		t.Fatalf("SendResponse: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["provider"] != "deepseek-foreman" {
		t.Errorf("provider = %v, want deepseek-foreman", body["provider"])
	}
}

func TestGatewayClient_SendResponse_AuthHeader(t *testing.T) {
	var capturedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "sk-test-key-123", 30*time.Second)
	_, err := client.SendResponse(t.Context(), "hello", "test-model", "", "")
	if err != nil {
		t.Fatalf("SendResponse: %v", err)
	}

	if capturedAuth != "Bearer sk-test-key-123" {
		t.Errorf("Authorization header = %q, want 'Bearer sk-test-key-123'", capturedAuth)
	}
}

// TestGatewayClient_SendResponse_PerForemanKey — a non-empty key passed to
// SendResponse overrides the client default, so each foreman authenticates
// with its own key (Bane 2026-07-31).
func TestGatewayClient_SendResponse_PerForemanKey(t *testing.T) {
	var capturedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "sk-shared-key", 30*time.Second)
	if _, err := client.SendResponse(t.Context(), "hello", "test-model", "", "sk-foreman-abc"); err != nil {
		t.Fatalf("SendResponse: %v", err)
	}
	if capturedAuth != "Bearer sk-foreman-abc" {
		t.Errorf("Authorization = %q, want per-foreman 'Bearer sk-foreman-abc'", capturedAuth)
	}

	// Empty key falls back to the shared daemon key.
	if _, err := client.SendResponse(t.Context(), "hello", "test-model", "", ""); err != nil {
		t.Fatalf("SendResponse (empty key): %v", err)
	}
	if capturedAuth != "Bearer sk-shared-key" {
		t.Errorf("Authorization = %q, want shared 'Bearer sk-shared-key'", capturedAuth)
	}
}

func TestGatewayClient_SendResponse_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"status": "failed",
			"error": map[string]string{
				"type":    "api_error",
				"message": "model not found",
			},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	_, err := client.SendResponse(t.Context(), "hello", "nonexistent-model", "", "")
	if err == nil {
		t.Fatal("expected error for gateway error response, got nil")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error should mention 'model not found', got: %v", err)
	}
}
