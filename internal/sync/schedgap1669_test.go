package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// domainEnforcingServer mimics DuckBrain's write route: a domain outside the
// server-side enum is rejected with 400 VALIDATION_ERROR before anything is
// stored (src/http/routes/memories.ts). Every accepted write records the
// domain it saw so the test can assert what went on the wire.
type domainRecordingServer struct {
	mu      sync.Mutex
	domains []string
	posts   atomic.Int64
}

func (r *domainRecordingServer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.domains...)
}

func newDomainEnforcingServer(t *testing.T) (*httptest.Server, *domainRecordingServer) {
	t.Helper()
	rec := &domainRecordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		rec.posts.Add(1)
		if !domainAllowed(body.Domain) {
			// Verbatim DuckBrain rejection shape.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"Invalid domain '%s'. Must be one of: %s","code":"VALIDATION_ERROR"}`,
				body.Domain, allowedDomains())
			return
		}
		rec.mu.Lock()
		rec.domains = append(rec.domains, body.Domain)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestResolveDomain_AllowlistAndAliases — the allowlist is the contract with
// DuckBrain's DomainEnum; "metrics" is a wire-compatibility alias only.
func TestResolveDomain_AllowlistAndAliases(t *testing.T) {
	for _, want := range []string{"person", "event", "concept", "message", "config", "raw_note"} {
		got, ok := resolveDomain(want)
		if !ok || got != want {
			t.Errorf("resolveDomain(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
	}

	// The value this repo used to send.
	got, ok := resolveDomain("metrics")
	if !ok {
		t.Fatal(`resolveDomain("metrics") = rejected; the alias must keep it postable`)
	}
	if got != "config" {
		t.Errorf(`resolveDomain("metrics") = %q, want "config"`, got)
	}

	// "fleet" is NOT a DuckBrain domain — the brief floated it; guard the
	// allowlist against someone "fixing" the call site to it later.
	if _, ok := resolveDomain("fleet"); ok {
		t.Error(`resolveDomain("fleet") accepted — DuckBrain has no "fleet" domain`)
	}
	for _, bad := range []string{"", "Metrics", "metric", "logs", "tick"} {
		if got, ok := resolveDomain(bad); ok {
			t.Errorf("resolveDomain(%q) = (%q, true), want rejected", bad, got)
		}
	}
}

// TestPostMemory_RemapsLegacyMetricsDomain — the regression for SCHED-GAP-1669:
// a write asking for the legacy domain must go out as an accepted one and
// succeed, instead of 400ing every cycle.
func TestPostMemory_RemapsLegacyMetricsDomain(t *testing.T) {
	srv, rec := newDomainEnforcingServer(t)
	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/fleet/lane-output", "metrics", map[string]any{"families": 1}); err != nil {
		t.Fatalf(`postMemory(domain="metrics"): %v`, err)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("posted domains = %v, want exactly 1", got)
	}
	if got[0] != "config" {
		t.Errorf(`wire domain = %q, want "config"`, got[0])
	}

	if n, err := database.CountSpooledMemories(ctx, db); err != nil {
		t.Fatalf("count spool: %v", err)
	} else if n != 0 {
		t.Errorf("spool = %d, want 0 (a remapped write must not spool)", n)
	}
	if h := s.Health(); !h.Reachable {
		t.Errorf("Reachable = false, LastError = %q — a remap is not an outage", h.LastError)
	}
}

// TestPostMemory_SkipsUnknownDomain — an unknown domain is dropped at the
// source: no request, no spool row, no health damage.
func TestPostMemory_SkipsUnknownDomain(t *testing.T) {
	srv, rec := newDomainEnforcingServer(t)
	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	// Establish a known-healthy baseline first: a skip must leave health
	// exactly as it found it (a fresh sync's Reachable is the zero value).
	if err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"ok": 1}); err != nil {
		t.Fatalf("baseline write: %v", err)
	}
	before := s.Health()

	err := s.postMemory(ctx, "/fleet/mystery", "telemetry", map[string]any{"x": 1})
	if !errors.Is(err, ErrDuckBrainDomainRejected) {
		t.Fatalf("postMemory err = %v, want ErrDuckBrainDomainRejected", err)
	}
	if n := rec.posts.Load(); n != 1 {
		t.Errorf("POSTs = %d, want 1 (only the baseline; invalid data must never be sent)", n)
	}

	s.flushPending(ctx)
	if n, cErr := database.CountSpooledMemories(ctx, db); cErr != nil {
		t.Fatalf("count spool: %v", cErr)
	} else if n != 0 {
		t.Errorf("spool = %d, want 0 (a permanent defect must not be queued for replay)", n)
	}

	events, lErr := database.ListEvents(ctx, db, "HIGH", "duckbrain-sync", 10, 0)
	if lErr != nil {
		t.Fatalf("list events: %v", lErr)
	}
	if len(events) != 0 {
		t.Errorf("HIGH events = %d, want 0 — a bad domain must not raise an outage alert", len(events))
	}

	after := s.Health()
	if after != before {
		t.Errorf("health changed by a skipped write: before=%+v after=%+v", before, after)
	}
	if !after.Reachable || after.ConsecutiveErr != 0 {
		t.Errorf("health = %+v, want reachable with 0 consecutive errors", after)
	}
}

// TestPostMemory_UnknownDomainDoesNotPoisonHealth — the amplification half of
// the bug: a skip must not be mistaken for DuckBrain being unreachable, and
// must not clear a healthy state.
func TestPostMemory_UnknownDomainDoesNotPoisonHealth(t *testing.T) {
	srv, _ := newDomainEnforcingServer(t)
	s, _ := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if err := s.postMemory(ctx, "/fleet/summary", "config", map[string]any{"a": 1}); err != nil {
		t.Fatalf("healthy write: %v", err)
	}
	if err := s.postMemory(ctx, "/fleet/lane-output", "metrics", map[string]any{"b": 2}); err != nil {
		t.Fatalf("legacy-domain write: %v", err)
	}
	_ = s.postMemory(ctx, "/fleet/unknown", "nope", map[string]any{"c": 3})

	h := s.Health()
	if !h.Reachable {
		t.Errorf("Reachable = false after a skipped domain: %+v", h)
	}
	if h.ConsecutiveErr != 0 {
		t.Errorf("ConsecutiveErr = %d, want 0", h.ConsecutiveErr)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
}

// TestReplaySpool_RemapsLegacyDomainAndDrains — rows already spooled with the
// legacy domain (53 were pending in the live DB) must drain, not 400 forever.
func TestReplaySpool_RemapsLegacyDomainAndDrains(t *testing.T) {
	srv, rec := newDomainEnforcingServer(t)
	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	ans, err := database.SpoolMemory(ctx, db, "/fleet/lane-output", "metrics", `{"families":{}}`)
	if err != nil {
		t.Fatalf("seed spool: %v", err)
	}
	if ans == 0 {
		t.Fatal("seed spool returned id 0")
	}

	n, err := s.replaySpool(ctx)
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if n != 1 {
		t.Errorf("replayed = %d, want 1 (the remapped row must post)", n)
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != "config" {
		t.Errorf("replayed domains = %v, want [config]", got)
	}
	if left, cErr := database.CountSpooledMemories(ctx, db); cErr != nil {
		t.Fatalf("count spool: %v", cErr)
	} else if left != 0 {
		t.Errorf("spool = %d, want 0 (drained)", left)
	}
}

// TestReplaySpool_UnknownDomainNotPosted — a spooled row with a domain no
// alias covers is never posted; it bumps the attempt counter so the existing
// 50-strike prune reaps it rather than parking in the batch head forever.
func TestReplaySpool_UnknownDomainNotPosted(t *testing.T) {
	srv, rec := newDomainEnforcingServer(t)
	s, db := newTestDuckBrainSync(t, srv.URL)
	ctx := context.Background()

	if _, err := database.SpoolMemory(ctx, db, "/fleet/legacy", "telemetry", `{"x":1}`); err != nil {
		t.Fatalf("seed spool: %v", err)
	}

	n, err := s.replaySpool(ctx)
	if err != nil {
		t.Fatalf("replaySpool: %v", err)
	}
	if n != 0 {
		t.Errorf("replayed = %d, want 0", n)
	}
	if got := rec.posts.Load(); got != 0 {
		t.Errorf("POSTs = %d, want 0 (never send a domain DuckBrain rejects)", got)
	}

	entries, lErr := database.ListSpooledMemories(ctx, db, 10)
	if lErr != nil {
		t.Fatalf("list spool: %v", lErr)
	}
	if len(entries) != 1 {
		t.Fatalf("spool rows = %d, want 1", len(entries))
	}
	if entries[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (so prune can reap it)", entries[0].Attempts)
	}
}

// TestSyncLaneFamilyOutput_SendsAcceptedDomain — end-to-end regression for the
// original defect: the real call site must put an accepted domain on the wire.
func TestSyncLaneFamilyOutput_SendsAcceptedDomain(t *testing.T) {
	srv, rec := newDomainEnforcingServer(t)
	s, _ := newTestDuckBrainSync(t, srv.URL)

	if err := s.syncLaneFamilyOutput(context.Background()); err != nil {
		t.Fatalf("syncLaneFamilyOutput: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("posted domains = %v, want exactly 1", got)
	}
	if !domainAllowed(got[0]) {
		t.Errorf("lane-output sent domain %q, which DuckBrain rejects", got[0])
	}
	if h := s.Health(); !h.Reachable {
		t.Errorf("Reachable = false after lane-output sync: %+v", h)
	}
}
