package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1573: DuckBrain sync wrote 60,925 empty payloads (33.9% of the
// scheduler namespace). These tests pin the guard that rejects empty
// content (after trim) before anything reaches the DuckBrain HTTP API.

func TestValidatePayloadContent(t *testing.T) {
	cases := []struct {
		name      string
		payload   []byte
		wantEmpty bool
	}{
		{"nil payload", nil, true},
		{"empty bytes", []byte{}, true},
		{"empty string", []byte(""), true},
		{"whitespace only", []byte("   \t\n  "), true},
		{"empty JSON object is content", []byte(`{}`), false},
		{"empty JSON string is empty content", []byte(`""`), true},
		{"real payload", []byte(`{"total_projects":1}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePayloadContent("/test/key", tc.payload)
			if tc.wantEmpty {
				if err == nil {
					t.Fatal("expected empty-payload rejection, got nil")
				}
				if !errors.Is(err, ErrDuckBrainEmptyPayload) {
					t.Fatalf("error must wrap ErrDuckBrainEmptyPayload, got: %v", err)
				}
				if !strings.Contains(err.Error(), "/test/key") {
					t.Errorf("error must name the key, got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("non-empty payload must be accepted, got: %v", err)
			}
		})
	}
}

// postMemory must reject an empty payload BEFORE any HTTP traffic and
// WITHOUT spooling it (replaying an empty payload just re-writes junk).
func TestPostMemory_RejectsEmptyPayload(t *testing.T) {
	for _, name := range []string{"nil", "empty-string", "whitespace"} {
		t.Run(name, func(t *testing.T) {
			db, err := database.InitDB(":memory:")
			if err != nil {
				t.Fatalf("InitDB: %v", err)
			}
			defer db.Close()

			var posts, receives atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receives.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			s := NewDuckBrainSync(db, "test-ns", srv.URL)
			ctx := context.Background()

			var content any
			switch name {
			case "nil":
				content = nil
			case "empty-string":
				content = ""
			case "whitespace":
				content = "   \n\t "
			}

			err = s.postMemory(ctx, "/fleet/empty", "config", content)
			if !errors.Is(err, ErrDuckBrainEmptyPayload) {
				t.Fatalf("want ErrDuckBrainEmptyPayload, got: %v", err)
			}
			if receives.Load() != 0 {
				t.Errorf("empty payload reached the DuckBrain server (%d posts)", receives.Load())
			}
			if posts.Load() != 0 {
				t.Errorf("unexpected spool buffer length: %d", posts.Load())
			}
			// The write must never have been recorded as successful.
			if h := s.Health(); h.Reachable {
				t.Error("empty-payload rejection must not mark DuckBrain reachable")
			}
		})
	}
}

// A REAL payload must still go through — the guard must not eat data.
func TestPostMemory_NonEmptyPayloadStillPosts(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	receives := 0
	var gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receives++
		var env postMemoryBody
		_ = json.NewDecoder(r.Body).Decode(&env)
		gotContent = env.Content
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	err = s.postMemory(context.Background(), "/test/ok", "config",
		map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("postMemory: %v", err)
	}
	if receives != 1 {
		t.Fatalf("posts = %d, want 1", receives)
	}
	if !strings.Contains(gotContent, "hello") {
		t.Errorf("content = %q", gotContent)
	}
}
