package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-072: DuckBrain sync key startup validation. During tick #479
// (2026-08-25) a restored auth.json missed the scheduler daemon's
// DUCKBRAIN_API_KEY entry; every sync write 401'd for 1.5h and spooled ~7.9K
// events with no distinct signal. These tests pin the fail-fast contract:
// probe 401 → distinct log + HIGH event + cycle skipped, no spool;
// write 401 → ErrDuckBrainKeyRejected sentinel, NOT spooled; 429 stays
// retryable+spooled; empty env keeps pre-auth behavior.

// TestValidateKey_ProbeRejected401 — a rejected key classifies as the
// sentinel, flags sync cycles off, bumps health failure state, and queues
// exactly one HIGH event naming the REJECTED KEY (not "unreachable").
func TestValidateKey_ProbeRejected401(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "wrong-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("probe method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/namespaces" {
			t.Errorf("probe path = %q, want /api/namespaces", r.URL.Path)
		}
		if r.URL.Query().Get("namespace") != "test-ns" {
			t.Errorf("probe namespace = %q, want test-ns", r.URL.Query().Get("namespace"))
		}
		if got := r.Header.Get("X-API-Key"); got != "wrong-key" {
			t.Errorf("probe X-API-Key = %q, want wrong-key", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Unauthorized: Invalid API key"}`))
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	err := s.validateKey(ctx)
	if !errors.Is(err, ErrDuckBrainKeyRejected) {
		t.Fatalf("validateKey err = %v, want ErrDuckBrainKeyRejected", err)
	}

	h := s.Health()
	if h.Reachable {
		t.Error("health.Reachable = true after rejection, want false")
	}
	if h.ConsecutiveErr != 1 {
		t.Errorf("ConsecutiveErr = %d, want 1", h.ConsecutiveErr)
	}
	if h.LastError == "" || !strings.Contains(h.LastError, "duckbrain key rejected") {
		t.Errorf("LastError = %q, want it to mention duckbrain key rejected", h.LastError)
	}

	// HIGH event queued (not yet flushed).
	s.flushPending(ctx)
	events, err := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("HIGH events = %d, want 1", len(events))
	}
	msg := events[0].Message
	if !strings.Contains(msg, "REJECTED") {
		t.Errorf("HIGH message = %q, want it to name the rejected key (not unreachable)", msg)
	}
	if strings.Contains(msg, "unreachable") {
		t.Errorf("HIGH message = %q, must NOT use unreachable semantics", msg)
	}
}

// TestSyncOnce_SkippedWhenKeyRejected — with the flag set, a full sync cycle
// performs ZERO writes: nothing spooled, nothing posted. (GET probes are
// expected — they are the periodic re-validation.) The HIGH event is
// flushed immediately so the alert is visible without waiting for recovery.
func TestSyncOnce_SkippedWhenKeyRejected(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "wrong-key")

	var probes, writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/namespaces" {
			probes.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)

	// Seed rows so an unskipped cycle would produce 6 posts.
	insertProject(t, db, "p1", "r1", "w1", 1)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns1', 10, 1, 100, 1)`); err != nil {
		t.Fatalf("insert ns: %v", err)
	}

	// Startup path: validateKey flags rejection.
	if err := s.validateKey(ctx); !errors.Is(err, ErrDuckBrainKeyRejected) {
		t.Fatalf("validateKey = %v, want ErrDuckBrainKeyRejected", err)
	}

	s.syncOnce(ctx)

	if got := probes.Load(); got != 2 {
		t.Errorf("probe requests = %d, want 2 (startup + in-cycle re-validation)", got)
	}
	if got := writes.Load(); got != 0 {
		t.Errorf("write requests = %d, want 0 (cycle must be skipped on key rejection)", got)
	}
	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 0 {
		t.Errorf("spooled = %d, want 0 (key-rejected writes must not spool)", n)
	}
	events, err := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("HIGH events = %d, want exactly 1 (flushed by the skipped cycle)", len(events))
	}
}

// TestValidateKey_ProbeAccepted200 — a good key passes the probe, clears a
// previously-set rejection flag, fires the standard recovery INFO event, and
// lets the next sync cycle proceed normally.
func TestValidateKey_ProbeAccepted200(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "good-key")

	mode := int32(1) // 1 = reject first probe, then accept
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&mode) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if err := s.validateKey(ctx); !errors.Is(err, ErrDuckBrainKeyRejected) {
		t.Fatalf("first validateKey = %v, want rejection", err)
	}
	s.flushPending(ctx) // persist the rejection HIGH

	// Key fixed server-side: probe succeeds, flag must flip so writes resume.
	atomic.StoreInt32(&mode, 2)
	if err := s.validateKey(ctx); err != nil {
		t.Fatalf("second validateKey = %v, want nil after fix", err)
	}
	h := s.Health()
	if !h.Reachable {
		t.Error("health.Reachable = false after accepted probe, want true (recordSuccess path)")
	}
	if d := s.keyRejected(); d {
		t.Error("keyRejected still true after accepted probe, want false")
	}
	s.flushPending(ctx) // the recovery INFO was queued by recordSuccess
	info, err := database.ListEvents(ctx, db, "INFO", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list info events: %v", err)
	}
	if len(info) != 1 {
		t.Fatalf("INFO recovery events = %d, want 1", len(info))
	}
}

// TestPostMemory_Write401_ClassifiedNotSpooled — a write that gets 401'd
// returns a wrapped ErrDuckBrainKeyRejected sentinel and does NOT spool
// (replaying with the same rejected key is what flooded sync_spool in
// tick #479). Health failure state still bumps and ONE HIGH fires.
func TestPostMemory_Write401_ClassifiedNotSpooled(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "wrong-key")

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Unauthorized: Invalid API key"}`))
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1})
	if !errors.Is(err, ErrDuckBrainKeyRejected) {
		t.Fatalf("postMemory err = %v, want wrapped ErrDuckBrainKeyRejected", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err text = %q, want it to carry HTTP 401", err.Error())
	}

	// A second rejected write must not duplicate the HIGH event.
	_ = s.postMemory(ctx, "/fleet/namespaces", "config", map[string]any{"b": 2})
	s.flushPending(ctx)

	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 0 {
		t.Errorf("spooled = %d, want 0 (key-rejected writes are terminal)", n)
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("posts = %d, want 2 (both writes attempted)", got)
	}

	h := s.Health()
	if h.ConsecutiveErr != 2 {
		t.Errorf("ConsecutiveErr = %d, want 2 (failure state still tracked)", h.ConsecutiveErr)
	}
	events, err := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("HIGH events = %d, want 1 (one per outage)", len(events))
	}
	if !strings.Contains(events[0].Message, "REJECTED") {
		t.Errorf("HIGH message = %q, want key-rejection wording", events[0].Message)
	}
}

// TestPostMemory_Write429_StillRetryableAndSpooled — 429 keeps today's
// behavior exactly: generic retryable error, write IS spooled.
func TestPostMemory_Write429_StillRetryableAndSpooled(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "good-key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if errors.Is(err, ErrDuckBrainKeyRejected) {
		t.Fatalf("429 misclassified as key rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v, want rate-limited wording", err)
	}
	s.flushPending(ctx)
	n, err := database.CountSpooledMemories(ctx, db)
	if err != nil {
		t.Fatalf("count spool: %v", err)
	}
	if n != 1 {
		t.Errorf("spooled = %d, want 1 (429 stays spooled)", n)
	}
}

// TestValidateKey_EmptyEnv_NoProbeNoHeader — pre-auth compatibility: with
// DUCKBRAIN_API_KEY empty/unset there is NO probe request and writes send no
// X-API-Key header (DB-GAP-039 contract preserved).
func TestValidateKey_EmptyEnv_NoProbeNoHeader(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "")

	var probes, writes atomic.Int32
	headerSeen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Api-Key"]; ok {
			headerSeen = true
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/namespaces" {
			probes.Add(1)
		} else if r.Method == http.MethodPost {
			writes.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, _ := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if err := s.validateKey(ctx); err != nil {
		t.Fatalf("validateKey with empty env = %v, want nil (no probe)", err)
	}
	if got := probes.Load(); got != 0 {
		t.Errorf("probe requests = %d, want 0 when DUCKBRAIN_API_KEY is empty", got)
	}

	if err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"x": 1}); err != nil {
		t.Fatalf("postMemory: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Errorf("writes = %d, want 1", got)
	}
	if headerSeen {
		t.Error("X-API-Key header sent despite empty env — pre-auth contract broken")
	}
}

// TestRun_FailFastStartup — Run's startup validation runs BEFORE the first
// syncOnce: with a rejected key the initial cycle is skipped entirely (zero
// POSTs), proving fail-fast ordering at the daemon entrypoint.
func TestRun_FailFastStartup(t *testing.T) {
	t.Setenv("DUCKBRAIN_API_KEY", "wrong-key")

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/namespaces" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s, db := newTestDuckBrainSync(t, srv.URL)
	s.interval = time.Hour // never tick again during the test

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	// Give the startup validateKey + syncOnce time to run.
	time.Sleep(300 * time.Millisecond)

	events, err := database.ListEvents(context.Background(), db, "HIGH", "duckbrain-sync", 10, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if got := posts.Load(); got != 0 {
		t.Errorf("POSTs = %d, want 0 (startup validation must gate the first cycle)", got)
	}
	if len(events) != 1 {
		t.Fatalf("HIGH events = %d, want 1 from startup validation", len(events))
	}
	if !strings.Contains(events[0].Message, "REJECTED") {
		t.Errorf("HIGH message = %q, want key-rejection wording", events[0].Message)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}
