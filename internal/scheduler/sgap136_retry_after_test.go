package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// SCHED-GAP-136 AC2 end-to-end: a gateway 503 with "Retry-After: N" must
// surface the parsed hint on GatewayStatusError.RetryAfter through the real
// client POST path.
func TestSchedGap136_ClientParsesRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, `{"error":{"message":"Gateway is draining existing work; retry shortly."}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewGatewayClient(srv.URL, "", 5*time.Second)
	_, err := c.SendResponseWithSessionKey(context.Background(), "prompt", "test-model", "test-provider", "", "sess-key")
	if err == nil {
		t.Fatal("expected error from 503")
	}
	var gse *GatewayStatusError
	if !asGatewayStatus(err, &gse) {
		t.Fatalf("error is %T, want *GatewayStatusError", err)
	}
	if gse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want 503", gse.StatusCode)
	}
	if gse.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %v, want 2s", gse.RetryAfter)
	}
	// And the spawn-side sleep honors it.
	if got := gatewayRetrySleep(err, 1); got != 2*time.Second {
		t.Fatalf("gatewayRetrySleep = %v, want 2s (Retry-After floor)", got)
	}
}

// The 503-without-header case: RetryAfter stays 0 — legacy behavior.
func TestSchedGap136_ClientNoHeaderZeroHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewGatewayClient(srv.URL, "", 5*time.Second)
	_, err := c.SendResponseWithSessionKey(context.Background(), "prompt", "test-model", "test-provider", "", "sess-key")
	var gse *GatewayStatusError
	if !asGatewayStatus(err, &gse) {
		t.Fatalf("error is %T, want *GatewayStatusError", err)
	}
	if gse.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 (no header)", gse.RetryAfter)
	}
}

func asGatewayStatus(err error, target **GatewayStatusError) bool {
	g, ok := err.(*GatewayStatusError)
	if ok {
		*target = g
	}
	return ok
}
