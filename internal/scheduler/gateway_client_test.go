package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// Regression tests for SCHED-GAP-074 (2026-08-27): every scheduler tick
// spawn must carry the tick id as a durable session handle on the gateway
// POST (X-Hermes-Session-Key header) so fleet-quality review can link each
// /v1/responses run to its state.db session deterministically — independent
// of the unreliable time-window + prompt-marker heuristic used for
// historical ticks.

func TestGatewayClient_SendResponseWithSessionKey_SetsHeader(t *testing.T) {
	var gotHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hermes-Session-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_sesskey",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	const wantKey = "coding-hermes-scheduler-2026-08-27-09-36-40"
	resp, err := client.SendResponseWithSessionKey(t.Context(), "test prompt", "test-model", "", "", wantKey)
	if err != nil {
		t.Fatalf("SendResponseWithSessionKey: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
		return
	}
	if gotHeader != wantKey {
		t.Fatalf("X-Hermes-Session-Key header = %q, want exactly %q — tick sessions would stay unlinkable!", gotHeader, wantKey)
	}

	t.Logf("OK: X-Hermes-Session-Key=%q sent verbatim on /v1/responses POST", gotHeader)
}

func TestGatewayClient_SendResponse_OmitsSessionKeyHeader(t *testing.T) {
	var gotHeader string
	var hadHeader bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hermes-Session-Key")
		hadHeader = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_nosesskey",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	if _, err := client.SendResponse(t.Context(), "hello", "test-model", "", ""); err != nil {
		t.Fatalf("SendResponse: %v", err)
	}
	if !hadHeader {
		t.Fatal("handler never ran")
	}
	if gotHeader != "" {
		t.Fatalf("legacy SendResponse must send NO X-Hermes-Session-Key header, got %q", gotHeader)
	}

	t.Log("OK: legacy SendResponse sends no X-Hermes-Session-Key header")
}

func TestGatewayClient_SendResponseWithSessionKey_EmptyKeyOmitsHeader(t *testing.T) {
	var gotHeader string
	var hadHeader bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hermes-Session-Key")
		hadHeader = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_emptykey",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	if _, err := client.SendResponseWithSessionKey(t.Context(), "hello", "test-model", "", "", ""); err != nil {
		t.Fatalf("SendResponseWithSessionKey(empty): %v", err)
	}
	if !hadHeader {
		t.Fatal("handler never ran")
	}
	if gotHeader != "" {
		t.Fatalf("empty sessionKey must omit the header entirely, got %q (gateway _parse_session_key_header treats empty as absent, but the wire contract should not rely on it)", gotHeader)
	}

	t.Log("OK: empty sessionKey sends no X-Hermes-Session-Key header")
}

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

// --- SCHED-GAP-080 transient classification (2026-08-29) ---

// TestGatewayClient_IsTransient_HTTP500 — a gateway 5xx is transient: the
// classifier must report true, the status code must be reachable via
// errors.As, and the error text must keep the legacy shape ('HTTP 500' +
// the gateway's detail) so downstream error-column persistence is unchanged.
func TestGatewayClient_IsTransient_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "server_error",
				"message": "Internal server error: database disk image is malformed",
			},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	_, err := client.SendResponseWithSessionKey(t.Context(), "hello", "test-model", "", "", "tick-1")
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if !scheduler.IsTransientGatewayErr(err) {
		t.Errorf("IsTransientGatewayErr = false for HTTP 500, want true")
	}
	var gse *scheduler.GatewayStatusError
	if !errors.As(err, &gse) {
		t.Fatalf("error is %T, want *scheduler.GatewayStatusError", err)
	}
	if gse.StatusCode != http.StatusInternalServerError {
		t.Errorf("GatewayStatusError.StatusCode = %d, want 500", gse.StatusCode)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to carry 'HTTP 500'", err.Error())
	}
	if !strings.Contains(err.Error(), "database disk image is malformed") {
		t.Errorf("error = %q, want the gateway detail 'database disk image is malformed'", err.Error())
	}
}

// TestGatewayClient_IsTransient_401NotTransient — 401 stays terminal: it
// classifies as ErrGatewayKeyRejected (GAP-035) and the transient classifier
// MUST reject it, so the SCHED-GAP-080 retry path can never burn attempts on
// auth rejections.
func TestGatewayClient_IsTransient_401NotTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "auth_error",
				"message": "Invalid gateway API key",
			},
		})
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	_, err := client.SendResponseWithSessionKey(t.Context(), "hello", "test-model", "", "", "tick-1")
	if err == nil {
		t.Fatal("expected error on HTTP 401, got nil")
	}
	if !errors.Is(err, scheduler.ErrGatewayKeyRejected) {
		t.Errorf("errors.Is(err, ErrGatewayKeyRejected) = false, want true")
	}
	if scheduler.IsTransientGatewayErr(err) {
		t.Errorf("IsTransientGatewayErr = true for HTTP 401, want false — auth rejection must stay terminal")
	}
}

// TestGatewayClient_IsTransient_Timeout — a client-side timeout surfaces as a
// *url.Error from client.Do and must be classified transient.
func TestGatewayClient_IsTransient_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 100*time.Millisecond)
	_, err := client.SendResponseWithSessionKey(t.Context(), "hello", "test-model", "", "", "tick-1")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("error is %T, want *url.Error (client.Do timeout)", err)
	}
	if !scheduler.IsTransientGatewayErr(err) {
		t.Errorf("IsTransientGatewayErr = false for timeout, want true")
	}
}

// TestGatewayClient_IsTransient_Unmarshal — a 200 with a garbage body is a
// transient unmarshal failure (ErrGatewayTransient wrapped).
func TestGatewayClient_IsTransient_Unmarshal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json {{{"))
	}))
	defer srv.Close()

	client := scheduler.NewGatewayClient(srv.URL, "test-key", 30*time.Second)
	_, err := client.SendResponseWithSessionKey(t.Context(), "hello", "test-model", "", "", "tick-1")
	if err == nil {
		t.Fatal("expected unmarshal error, got nil")
	}
	if !errors.Is(err, scheduler.ErrGatewayTransient) {
		t.Errorf("errors.Is(err, ErrGatewayTransient) = false, want true")
	}
	if !scheduler.IsTransientGatewayErr(err) {
		t.Errorf("IsTransientGatewayErr = false for unmarshal failure, want true")
	}
}

// SCHED-GAP-1654 — gateway availability classification, driven against REAL
// mock HTTP responses. The distinction between "the gateway refused to serve
// this" (503 drain, 429 rate limit, refused dial) and every other failure is
// what the spawn path's per-tick event and /api/v1/gateway-errors count, so
// the mapping is pinned here at the client boundary where the status code
// actually exists.

// TestSCHEDGAP1654_ClassifyGatewayError_StatusClasses maps the served status
// to a class: only 503 and 429 are availability refusals. 401/403 stay the
// terminal auth classification (GAP-035), and any other 5xx keeps its own
// failure class rather than being laundered into "the gateway was down".
func TestSCHEDGAP1654_ClassifyGatewayError_StatusClasses(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		want       string
		wantReject bool
	}{
		{name: "503 service unavailable", status: http.StatusServiceUnavailable, want: scheduler.GatewayErrClassUnavailable503},
		{name: "429 rate limited", status: http.StatusTooManyRequests, want: scheduler.GatewayErrClassRateLimited429},
		{name: "500 internal", status: http.StatusInternalServerError, want: ""},
		{name: "502 bad gateway", status: http.StatusBadGateway, want: ""},
		{name: "401 auth rejected", status: http.StatusUnauthorized, want: "", wantReject: true},
		{name: "403 auth forbidden", status: http.StatusForbidden, want: "", wantReject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"type": "gateway_error", "message": "mock refusal"},
				})
			}))
			defer srv.Close()

			client := scheduler.NewGatewayClient(srv.URL, "test-key", 10*time.Second)
			_, err := client.SendResponseWithSessionKey(t.Context(), "hi", "m", "", "", "tick-1654")
			if err == nil {
				t.Fatalf("HTTP %d: SendResponseWithSessionKey returned nil error", tc.status)
			}
			if got := scheduler.ClassifyGatewayError(err); got != tc.want {
				t.Errorf("ClassifyGatewayError(HTTP %d) = %q, want %q (err: %v)", tc.status, got, tc.want, err)
			}
			if got := errors.Is(err, scheduler.ErrGatewayKeyRejected); got != tc.wantReject {
				t.Errorf("errors.Is(err, ErrGatewayKeyRejected) = %t, want %t", got, tc.wantReject)
			}
			// The class must survive a wrap — the spawn path hands the
			// classification a *wrapped* error ("gateway POST: %w"), so a
			// classifier that only inspects the outermost error would report
			// nothing for every real tick.
			wrapped := fmt.Errorf("gateway unreachable and exec fallback disabled: %w", err)
			if got := scheduler.ClassifyGatewayError(wrapped); got != tc.want {
				t.Errorf("ClassifyGatewayError(wrapped HTTP %d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestSCHEDGAP1654_ClassifyGatewayError_ConnectionRefused — no HTTP response
// at all (dead listener) is the third class. It is derived from the error
// chain via client.Do's *url.Error → *net.OpError → syscall.ECONNREFUSED,
// with the phrase as the text-level fallback for a proxied/wrapped shape.
func TestSCHEDGAP1654_ClassifyGatewayError_ConnectionRefused(t *testing.T) {
	// Port 1 has no listener: the dial is refused by the kernel.
	client := scheduler.NewGatewayClient("http://127.0.0.1:1", "test-key", 5*time.Second)
	_, err := client.SendResponseWithSessionKey(t.Context(), "hi", "m", "", "", "tick-1654-refused")
	if err == nil {
		t.Fatal("SendResponseWithSessionKey against a dead port returned nil error")
	}
	if got := scheduler.ClassifyGatewayError(err); got != scheduler.GatewayErrClassConnRefused {
		t.Errorf("ClassifyGatewayError(refused dial) = %q, want %q (err: %v)",
			got, scheduler.GatewayErrClassConnRefused, err)
	}
	if !scheduler.IsTransientGatewayErr(err) {
		t.Error("IsTransientGatewayErr = false for a refused dial, want true (SCHED-GAP-080 retry invariant)")
	}
	if got := scheduler.ClassifyGatewayError(fmt.Errorf("gateway POST: %w", err)); got != scheduler.GatewayErrClassConnRefused {
		t.Errorf("ClassifyGatewayError(wrapped refused dial) = %q, want %q", got, scheduler.GatewayErrClassConnRefused)
	}
	// Text-level fallback: a shape that reached us as prose only.
	if got := scheduler.ClassifyGatewayError(errors.New(`Post "http://127.0.0.1:8642/v1/responses": dial tcp 127.0.0.1:8642: connect: Connection refused`)); got != scheduler.GatewayErrClassConnRefused {
		t.Errorf("ClassifyGatewayError(prose refused dial) = %q, want %q", got, scheduler.GatewayErrClassConnRefused)
	}
}

// TestSCHEDGAP1654_ClassifyGatewayError_NonAvailability — nil and ordinary
// errors classify to "" (no class). This is what keeps the availability
// counter honest: every failure class that is NOT a gateway refusal must fall
// through the spawn path's emitter without writing an event.
func TestSCHEDGAP1654_ClassifyGatewayError_NonAvailability(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("boom")},
		{name: "context deadline", err: fmt.Errorf("gateway POST: %w", context.DeadlineExceeded)},
		{name: "transient body read", err: fmt.Errorf("read response: %w: %w", scheduler.ErrGatewayTransient, errors.New("unexpected EOF"))},
		{name: "key rejected", err: fmt.Errorf("%w (HTTP 401): auth_error", scheduler.ErrGatewayKeyRejected)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduler.ClassifyGatewayError(tc.err); got != "" {
				t.Errorf("ClassifyGatewayError(%v) = %q, want \"\" (not an availability class)", tc.err, got)
			}
		})
	}
}

// TestSCHEDGAP1654_ClassOrderIsClosed pins the reporting vocabulary: the
// endpoint seeds its by_class map from this order, so a class dropped from it
// would silently vanish from the API output instead of reading 0.
func TestSCHEDGAP1654_ClassOrderIsClosed(t *testing.T) {
	want := map[string]bool{
		scheduler.GatewayErrClassUnavailable503: true,
		scheduler.GatewayErrClassRateLimited429: true,
		scheduler.GatewayErrClassConnRefused:    true,
	}
	got := map[string]bool{}
	for _, class := range scheduler.GatewayErrorClassOrder {
		if !want[class] {
			t.Errorf("GatewayErrorClassOrder carries unexpected class %q", class)
		}
		got[class] = true
	}
	for class := range want {
		if !got[class] {
			t.Errorf("GatewayErrorClassOrder is missing %q — the endpoint would report it as absent, not 0", class)
		}
	}
	if len(scheduler.GatewayErrorClassOrder) != len(want) {
		t.Errorf("GatewayErrorClassOrder length = %d, want %d", len(scheduler.GatewayErrorClassOrder), len(want))
	}
}
