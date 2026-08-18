package sync

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// newTestDuckBrainSync builds a syncer against an in-memory DB.
func newTestDuckBrainSync(t *testing.T, url string) (*DuckBrainSync, *sql.DB) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewDuckBrainSync(db, "test-ns", url)
	return s, db
}

// TestPostMemory_SpoolsOnFailure — when DuckBrain is unreachable the write
// must NOT vanish: it lands in sync_spool for replay (Bane 2026-08-01).
func TestPostMemory_SpoolsOnFailure(t *testing.T) {
	s, db := newTestDuckBrainSync(t, "http://127.0.0.1:1") // nothing listens
	ctx := context.Background()

	if err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"x": 1}); err == nil {
		t.Fatal("expected error posting to dead endpoint")
	}
	// Buffered writes persist to sync_spool at flush (end of sync cycle).
	s.flushPending(ctx)
	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 1 {
		t.Fatalf("spooled = %d, want 1 (write must be spooled, not dropped)", n)
	}

	h := s.Health()
	if h.Reachable {
		t.Error("health.Reachable = true after failure, want false")
	}
	if h.ConsecutiveErr != 1 {
		t.Errorf("ConsecutiveErr = %d, want 1", h.ConsecutiveErr)
	}
}

// TestPostMemory_NoSpoolOnSuccess — healthy posts never spool.
func TestPostMemory_NoSpoolOnSuccess(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(201)
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"x": 1}); err != nil {
		t.Fatalf("postMemory: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1", hits.Load())
	}
	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 0 {
		t.Errorf("spooled = %d, want 0", n)
	}
	h := s.Health()
	if !h.Reachable {
		t.Error("health.Reachable = false after success, want true")
	}
	if h.ConsecutiveErr != 0 {
		t.Errorf("ConsecutiveErr = %d, want 0", h.ConsecutiveErr)
	}
	if h.LastOKAt == "" {
		t.Error("LastOKAt empty after success")
	}
}

// TestReplaySpool_ReplaysAndDeletes — spooled writes are replayed once
// DuckBrain comes back, then removed from the spool.
func TestReplaySpool_ReplaysAndDeletes(t *testing.T) {
	// 1. Dead endpoint: spool two writes.
	s, db := newTestDuckBrainSync(t, "http://127.0.0.1:1")
	ctx := context.Background()
	_ = s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1})
	_ = s.postMemory(ctx, "/fleet/events/9", "event", map[string]any{"b": 2})
	s.flushPending(ctx)

	// 2. DuckBrain comes back: replay.
	var replayed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replayed = append(replayed, r.URL.Query().Get("namespace")+":"+r.URL.Path)
		w.WriteHeader(201)
	}))
	defer srv.Close()
	s.baseURL = srv.URL

	n, err := s.replaySpool(ctx)
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if n != 2 {
		t.Errorf("replayed = %d, want 2", n)
	}
	left, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if left != 0 {
		t.Errorf("spool after replay = %d, want 0", left)
	}
}

// TestReplaySpool_KeepsFailingWrites — if DuckBrain stays down, spooled
// writes remain queued (with attempt count bumped) and are NOT deleted.
func TestReplaySpool_KeepsFailingWrites(t *testing.T) {
	s, db := newTestDuckBrainSync(t, "http://127.0.0.1:1")
	ctx := context.Background()
	_ = s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1})
	s.flushPending(ctx)

	n, err := s.replaySpool(ctx)
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if n != 0 {
		t.Errorf("replayed = %d, want 0 (still down)", n)
	}
	left, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if left != 1 {
		t.Errorf("spool = %d, want 1 (write kept)", left)
	}
	entries, err := database.ListSpooledMemories(ctx, db, 10)
	if err != nil {
		t.Fatalf("list spool: %v", err)
	}
	if entries[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", entries[0].Attempts)
	}
	if entries[0].LastError == "" {
		t.Error("LastError empty after failed replay")
	}
}

// TestPostMemory_EmitsHighEventOnce — the first failure of an outage logs a
// HIGH event; retries within the same outage do not spam duplicates.
func TestPostMemory_EmitsHighEventOnce(t *testing.T) {
	s, db := newTestDuckBrainSync(t, "http://127.0.0.1:1")
	ctx := context.Background()

	_ = s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1})
	_ = s.postMemory(ctx, "/fleet/namespaces", "config", map[string]any{"b": 2})
	s.flushPending(ctx)

	events, err := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("HIGH events = %d, want exactly 1 (no duplicate spam)", len(events))
	}

	// Recovery clears the outage flag.
	s.recordSuccess()
	_ = s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"c": 3}) // fails again
	s.flushPending(ctx)
	events2, err := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events2) != 2 {
		t.Errorf("HIGH events after recovery+refail = %d, want 2", len(events2))
	}
	info, err := database.ListEvents(ctx, db, "INFO", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list info events: %v", err)
	}
	if len(info) != 1 {
		t.Errorf("INFO recovery events = %d, want 1", len(info))
	}
}

// TestHealthSnapshot_Fields — Health returns the expected snapshot shape.
func TestHealthSnapshot_Fields(t *testing.T) {
	s, _ := newTestDuckBrainSync(t, "http://127.0.0.1:1")
	h := s.Health()
	if h.BaseURL != "http://127.0.0.1:1" {
		t.Errorf("BaseURL = %q", h.BaseURL)
	}
	if h.Interval != "5m0s" {
		t.Errorf("Interval = %q, want 5m0s", h.Interval)
	}
	if h.Spooled != 0 {
		t.Errorf("Spooled = %d, want 0", h.Spooled)
	}
	_ = time.Now() // keep time import honest in case of future edits
}
