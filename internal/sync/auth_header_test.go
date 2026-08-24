package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestPostMemory_APIKeyHeader_Set proves that when DUCKBRAIN_API_KEY is set,
// postMemoryBody sends the X-API-Key header with the token (DB-GAP-039 —
// required once DuckBrain runs with --auth=apikey).
func TestPostMemory_APIKeyHeader_Set(t *testing.T) {
	// t.Setenv forbids t.Parallel — keep sequential.
	t.Setenv("DUCKBRAIN_API_KEY", "test-token-123")

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		var receivedBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/test/auth-key", "config", map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("postMemory: %v", err)
	}

	if gotAPIKey != "test-token-123" {
		t.Errorf("X-API-Key header = %q, want %q", gotAPIKey, "test-token-123")
	}
}

// TestPostMemory_APIKeyHeader_Unset proves backwards compatibility: with
// DUCKBRAIN_API_KEY unset/empty, no X-API-Key header is sent and the write
// still succeeds against an unauthenticated daemon (pre-auth-flip behavior).
func TestPostMemory_APIKeyHeader_Unset(t *testing.T) {
	// Explicitly empty — same code path as a fully unset env var.
	t.Setenv("DUCKBRAIN_API_KEY", "")

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	headerSeen := false
	var gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Api-Key"]; ok {
			headerSeen = true
		}
		gotAPIKey = r.Header.Get("X-API-Key")
		var receivedBody map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/test/auth-key-unset", "config", map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("postMemory: %v", err)
	}

	if headerSeen {
		t.Error("X-API-Key header must not be sent when DUCKBRAIN_API_KEY is empty")
	}
	if gotAPIKey != "" {
		t.Errorf("X-API-Key header = %q, want empty", gotAPIKey)
	}
}
