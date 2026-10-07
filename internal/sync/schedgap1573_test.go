package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// countingServer counts how many memory POSTs actually reach DuckBrain.
type countingServer struct {
	hits atomic.Int64
	srv  *httptest.Server
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits.Add(1)
		w.WriteHeader(201)
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

// TestPostMemory_RejectsEmptyContent — SCHED-GAP-1573: an empty content value
// must be rejected at the client BEFORE the HTTP POST. It must not reach
// DuckBrain, must not spool, and must not mark DuckBrain unreachable (a
// terminal client-side defect, same policy as a rejected domain).
func TestPostMemory_RejectsEmptyContent(t *testing.T) {
	cs := newCountingServer(t)
	s, db := newTestDuckBrainSync(t, cs.srv.URL)
	ctx := context.Background()

	err := s.postMemory(ctx, "/fleet/summary", "config", "")
	if err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
	if !errors.Is(err, ErrDuckBrainEmptyPayload) {
		t.Fatalf("err = %v, want ErrDuckBrainEmptyPayload", err)
	}
	if hits := cs.hits.Load(); hits != 0 {
		t.Errorf("server hits = %d, want 0 (empty payload must not be POSTed)", hits)
	}
	// Terminal client-side defect: never spooled, health untouched.
	s.flushPending(ctx)
	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 0 {
		t.Errorf("spooled = %d, want 0 (empty payload must not be spooled)", n)
	}
	if h := s.Health(); h.ConsecutiveErr != 0 {
		t.Errorf("ConsecutiveErr = %d, want 0 (not a transport failure)", h.ConsecutiveErr)
	}
}

// TestPostMemory_RejectsWhitespaceOnlyContent — whitespace-only content is
// empty after trim and must be rejected the same way.
func TestPostMemory_RejectsWhitespaceOnlyContent(t *testing.T) {
	cs := newCountingServer(t)
	s, _ := newTestDuckBrainSync(t, cs.srv.URL)
	ctx := context.Background()

	err := s.postMemory(ctx, "/fleet/projects/x/status", "config", " \t\n ")
	if err == nil || !errors.Is(err, ErrDuckBrainEmptyPayload) {
		t.Fatalf("err = %v, want ErrDuckBrainEmptyPayload", err)
	}
	if hits := cs.hits.Load(); hits != 0 {
		t.Errorf("server hits = %d, want 0", hits)
	}
}

// TestPostMemory_QuotedEmptyStringStillAllowed — a legacy spooled
// empty-string content serializes to `""`, which is NOT whitespace: the
// value-shape guard keeps it postable so rows spooled before the guard
// existed can still drain (additive guard, no behavior change for them).
func TestPostMemory_QuotedEmptyStringStillAllowed(t *testing.T) {
	cs := newCountingServer(t)
	s, _ := newTestDuckBrainSync(t, cs.srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/fleet/legacy", "config", `""`); err != nil {
		t.Fatalf("postMemory: %v", err)
	}
	if hits := cs.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
}

// TestReplaySpool_SkipsEmptyContentRows — legacy spool rows with empty or
// whitespace-only content are NOT posted; their attempt counter is bumped so
// the existing 50-strike prune reaps them, and non-empty siblings still drain.
func TestReplaySpool_SkipsEmptyContentRows(t *testing.T) {
	cs := newCountingServer(t)
	s, db := newTestDuckBrainSync(t, cs.srv.URL)
	ctx := context.Background()

	wsID, err := database.SpoolMemory(ctx, db, "/fleet/legacy/blank", "config", "   ")
	if err != nil {
		t.Fatalf("spool whitespace row: %v", err)
	}
	if _, err := database.SpoolMemory(ctx, db, "/fleet/legacy/ok", "config", `{"x":1}`); err != nil {
		t.Fatalf("spool ok row: %v", err)
	}

	replayed, err := s.replaySpool(ctx)
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if replayed != 1 {
		t.Errorf("replayed = %d, want 1 (only the non-empty sibling)", replayed)
	}
	if hits := cs.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
	entries, err := database.ListSpooledMemories(ctx, db, 10)
	if err != nil {
		t.Fatalf("list spool: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("remaining spool rows = %d, want 1", len(entries))
	}
	if e := entries[0]; e.ID != wsID {
		t.Errorf("remaining row id = %d, want %d (the whitespace row)", e.ID, wsID)
	} else if e.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (bumped so the prune reaps it)", e.Attempts)
	}
}

// TestIsEffectivelyEmptyContent pins the predicate's value-shape contract:
// only pre-marshal string values can be empty; the marshaled `""` form and
// non-string payloads are never treated as empty.
func TestIsEffectivelyEmptyContent(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"go empty string", "", true},
		{"whitespace only", " \t\n\r ", true},
		{"quoted empty json", `""`, false},
		{"real json", `{"x":1}`, false},
		{"map payload", map[string]any{"a": 1}, false},
		{"empty map", map[string]any{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEffectivelyEmptyContent(tc.val); got != tc.want {
				t.Fatalf("isEffectivelyEmptyContent(%#v) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
