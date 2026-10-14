package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1572: DuckBrain sync re-writes byte-identical snapshots (24.5% of
// the coding-hermes namespace, 36.7% of scheduler). These tests pin the
// duplicate-suppression machinery: change detection must skip an unchanged
// payload (0 appended records on a re-run), post exactly one record on a real
// state change (a change BACK to a previous value is a change, not suppressed),
// and expose a skipped-duplicate counter so suppression is visible.

func TestPostMemory_UnchangedPayloadSkipsAndCounts(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	summary := map[string]any{"total_projects": 3, "synced_at": "2026-10-06T12:00:00Z"}
	if err := s.postMemory(ctx, "/fleet/summary", "config", summary); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// Byte-identical re-write (only synced_at differs, which the canonical
	// hash strips): the POST must be SKIPPED and the skip must be counted
	// so suppression is observable (row acceptance criterion 5).
	summary["synced_at"] = "2026-10-06T12:05:00Z"
	if err := s.postMemory(ctx, "/fleet/summary", "config", summary); err != nil {
		t.Fatalf("unchanged post: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts = %d, want 1 (duplicate snapshot re-written)", got)
	}
	if got := s.DuplicatesSkipped(); got != 1 {
		t.Fatalf("DuplicatesSkipped = %d, want 1", got)
	}

	// A genuine change posts exactly one new record.
	summary["total_projects"] = 4
	if err := s.postMemory(ctx, "/fleet/summary", "config", summary); err != nil {
		t.Fatalf("changed post: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("posts after change = %d, want 2", got)
	}
	if got := s.DuplicatesSkipped(); got != 1 {
		t.Fatalf("DuplicatesSkipped after change = %d, want 1", got)
	}
}

// A failed post must NOT be cached as seen: the retry writes the payload.
func TestPostMemory_FailedPostNotCachedAsDuplicate(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var fail atomic.Bool
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	fail.Store(true)
	payload := map[string]any{"n": 1}
	if err := s.postMemory(ctx, "/fleet/x", "config", payload); err == nil {
		t.Fatal("expected post failure")
	}
	fail.Store(false)
	// Same payload retried after recovery: NOT a duplicate — it never posted.
	if err := s.postMemory(ctx, "/fleet/x", "config", payload); err != nil {
		t.Fatalf("retry post: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("posts = %d, want 2 (failed write must be retried, not deduped)", got)
	}
	if got := s.DuplicatesSkipped(); got != 0 {
		t.Fatalf("DuplicatesSkipped = %d, want 0", got)
	}
}

// Two consecutive full sweeps with unchanged state append ZERO new records
// (row acceptance criterion 3), while a state change appends exactly 1
// (criterion 4).
func TestSyncOnce_TwoSweepsUnchangedStateAppendNothing(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	first := posts.Load()
	if first == 0 {
		t.Fatal("first sweep posted nothing")
	}
	// Second sweep, unchanged state (only the clock's synced_at moves):
	// zero new records.
	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := posts.Load(); got != first {
		t.Fatalf("posts after second sweep = %d, want %d (unchanged state re-written)", got, first)
	}
	if got := s.DuplicatesSkipped(); got < 1 {
		t.Fatalf("DuplicatesSkipped = %d, want >= 1", got)
	}
}
