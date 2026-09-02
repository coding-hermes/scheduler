package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestReplayStopsOn429 (Bane 2026-09-01): a 429 from the DuckBrain daemon
// must stop the spool sweep immediately — the remaining batch must NOT be
// blasted (each blast 429s too, burns attempt counters toward the
// 50-strike prune, and hammers the daemon). One 429 = one failed post per
// cycle; the interval ticker retries.
func TestReplayStopsOn429(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var posts atomic.Int64
	var four29 atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		// 429 the first request only; a buggy sweep would keep hitting us.
		if four29.CompareAndSwap(0, 1) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)

	// Spool 5 entries; entry #1 (oldest) will draw the 429.
	for i := 0; i < 5; i++ {
		if _, err := database.SpoolMemory(context.Background(), db,
			"/test/entry"+string(rune('a'+i)), "config", `{"n":`+string(rune('0'+i))+`}`); err != nil {
			t.Fatalf("SpoolMemory: %v", err)
		}
	}

	n, err := s.replaySpool(context.Background())
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if n != 0 {
		t.Errorf("replayed = %d, want 0", n)
	}

	got := posts.Load()
	if got != 1 {
		t.Errorf("daemon received %d posts, want exactly 1 (sweep must stop on first 429, no batch re-blasting)", got)
	}

	// The 429'd entry gets its attempt recorded; the other 4 must be untouched.
	entries, err := database.ListSpooledMemories(context.Background(), db, 100)
	if err != nil {
		t.Fatalf("ListSpooledMemories: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("spool len = %d, want 5", len(entries))
	}
	if entries[0].Attempts != 1 {
		t.Errorf("first entry attempts = %d, want 1", entries[0].Attempts)
	}
	for _, e := range entries[1:] {
		if e.Attempts != 0 {
			t.Errorf("entry %s attempts = %d, want 0 (must not be blasted after 429)", e.MemKey, e.Attempts)
		}
	}

	// Latch must be per-cycle: next cycle retries from the top.
	if !s.rateLimited() {
		t.Error("rateLimited latch should be set within the failing cycle")
	}
	d := s
	d.mu.Lock()
	d.rateLimitedFlag = false
	d.mu.Unlock()

	// Daemon is healthy now: sweep replays everything.
	n2, err := s.replaySpool(context.Background())
	if err != nil {
		t.Fatalf("replaySpool(2nd): %v", err)
	}
	if n2 != 5 {
		t.Errorf("2nd sweep replayed = %d, want 5", n2)
	}
}

// TestSetInterval guards the -duckbrain-interval wiring.
func TestSetInterval(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	s := NewDuckBrainSync(db, "test-ns", "http://localhost:3000")
	if s.interval != 5*time.Minute {
		t.Errorf("default interval = %v, want 5m", s.interval)
	}
	s.SetInterval(0) // must be ignored
	if s.interval != 5*time.Minute {
		t.Errorf("SetInterval(0) changed interval to %v", s.interval)
	}
	s.SetInterval(15 * time.Minute)
	if s.interval != 15*time.Minute {
		t.Errorf("SetInterval(15m) ignored, interval = %v", s.interval)
	}
}
