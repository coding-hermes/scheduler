package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// postMemoryBody is the JSON envelope that postMemory sends to DuckBrain.
type postMemoryBody struct {
	Key        string         `json:"key"`
	Domain     string         `json:"domain"`
	Content    string         `json:"content"`
	Attributes map[string]any `json:"attributes"`
}

// decodeDuckBrainContent decodes the DuckBrain postMemory envelope and
// unmarshals the inner content string into target.
func decodeDuckBrainContent(t *testing.T, r *http.Request, target interface{}) {
	t.Helper()
	var env postMemoryBody
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		t.Errorf("decode envelope: %v", err)
		return
	}
	if err := json.Unmarshal([]byte(env.Content), target); err != nil {
		t.Errorf("unmarshal content: %v", err)
	}
}

func TestNewDuckBrainSync(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	s := NewDuckBrainSync(db, "test-ns", "http://localhost:3000")

	if s.db != db {
		t.Error("db not set")
	}
	if s.namespace != "test-ns" {
		t.Errorf("namespace = %q, want test-ns", s.namespace)
	}
	if s.baseURL != "http://localhost:3000" {
		t.Errorf("baseURL = %q", s.baseURL)
	}
	if s.httpClient == nil {
		t.Fatal("httpClient is nil")
		return
	}
	if s.httpClient.Timeout != 10*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 10s", s.httpClient.Timeout)
	}
	if s.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", s.interval)
	}
}

func TestPostMemory_Success(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if r.URL.Query().Get("namespace") != "test-ns" {
			t.Errorf("namespace param = %q", r.URL.Query().Get("namespace"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	err = s.postMemory(ctx, "/test/key", "config", map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("postMemory: %v", err)
	}

	if receivedBody["key"] != "/test/key" {
		t.Errorf("key = %q", receivedBody["key"])
	}
	if receivedBody["domain"] != "config" {
		t.Errorf("domain = %q", receivedBody["domain"])
	}
}

func TestPostMemory_HTTPError(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	err = s.postMemory(ctx, "/key", "config", "val")
	if err == nil {
		t.Fatal("expected error for 500 status, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500, got: %v", err)
	}
}

func TestPostMemory_NetworkError(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// Use a URL that will refuse connections.
	s := NewDuckBrainSync(db, "test-ns", "http://127.0.0.1:0")
	ctx := context.Background()

	err = s.postMemory(ctx, "/key", "config", "val")
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

func TestPostMemory_ChangeDetection(t *testing.T) {
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

	status := projectStatus{
		Name: "p1", Weight: 10, Priority: 5, Enabled: true,
		CooldownS: 900, DecayRate: 1.0, Model: "m", Provider: "p",
		LastTick: "2026-08-01T00:00:00Z", LastTickStart: "2026-08-01T00:00:00Z",
		SyncedAt: "2026-08-05T18:00:00Z",
	}

	// First post always goes through.
	if err := s.postMemory(ctx, "/fleet/projects/p1/status", "config", status); err != nil {
		t.Fatalf("first postMemory: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts after first = %d, want 1", got)
	}

	// Same payload with only synced_at changed → SKIPPED (no POST).
	status.SyncedAt = "2026-08-05T18:05:00Z"
	if err := s.postMemory(ctx, "/fleet/projects/p1/status", "config", status); err != nil {
		t.Fatalf("unchanged postMemory: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("posts after unchanged retry = %d, want 1 (dedupe failed)", got)
	}

	// A real field change → posted again.
	status.CooldownS = 7200
	if err := s.postMemory(ctx, "/fleet/projects/p1/status", "config", status); err != nil {
		t.Fatalf("changed postMemory: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("posts after changed payload = %d, want 2", got)
	}
}

func TestPostMemory_FailureNotCached(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	status := projectStatus{
		Name: "p1", Weight: 10, Priority: 5, Enabled: true,
		CooldownS: 900, DecayRate: 1.0, Model: "m", Provider: "p",
		SyncedAt: "2026-08-05T18:00:00Z",
	}

	// First attempt fails — must NOT be recorded as synced.
	if err := s.postMemory(ctx, "/fleet/projects/p1/status", "config", status); err == nil {
		t.Fatal("expected error on first (500) post, got nil")
	}
	// Identical payload retried after failure → must POST again.
	if err := s.postMemory(ctx, "/fleet/projects/p1/status", "config", status); err != nil {
		t.Fatalf("retry postMemory: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("posts = %d, want 2 (failed write must not be cached as synced)", got)
	}
}

func insertProject(t *testing.T, db *sql.DB, name, repoURL, workdir string, enabled int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, weight, priority, enabled, cooldown_s, decay_rate, model, provider, created_at, updated_at)
		VALUES (?, ?, ?, 10, 5, ?, 900, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', ?, ?)`,
		name, repoURL, workdir, enabled, now, now)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

func TestSyncFleetSummary(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	insertProject(t, db, "p2", "r2", "w2", 0)

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.TotalProjects != 2 {
		t.Errorf("TotalProjects = %d, want 2", received.TotalProjects)
	}
	if received.Enabled != 1 {
		t.Errorf("Enabled = %d, want 1", received.Enabled)
	}
	if received.ActiveTicks != 0 {
		t.Errorf("ActiveTicks = %d, want 0", received.ActiveTicks)
	}
	if received.SyncedAt == "" {
		t.Error("SyncedAt is empty")
	}
}

func TestSyncProjectStatuses(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, weight, priority, enabled, cooldown_s, decay_rate, model, provider, last_tick_completed, last_tick_started, created_at, updated_at)
		VALUES ('alpha', 'r1', 'w1', 10, 5, 1, 900, 1.5, 'deepseek-v4-pro', 'deepseek-foreman', '2026-07-20T00:00:00Z', '2026-07-19T23:55:00Z', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var receivedCount int
	var lastStatus projectStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCount++
		decodeDuckBrainContent(t, r, &lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncProjectStatuses(ctx); err != nil {
		t.Fatalf("syncProjectStatuses: %v", err)
	}

	if receivedCount != 1 {
		t.Errorf("receivedCount = %d, want 1", receivedCount)
	}
	if lastStatus.Name != "alpha" {
		t.Errorf("Name = %q, want alpha", lastStatus.Name)
	}
	if lastStatus.Weight != 10 {
		t.Errorf("Weight = %d, want 10", lastStatus.Weight)
	}
	if lastStatus.Priority != 5 {
		t.Errorf("Priority = %d, want 5", lastStatus.Priority)
	}
	if !lastStatus.Enabled {
		t.Error("Enabled should be true")
	}
	if lastStatus.CooldownS != 900 {
		t.Errorf("CooldownS = %d, want 900", lastStatus.CooldownS)
	}
	if lastStatus.DecayRate != 1.5 {
		t.Errorf("DecayRate = %f, want 1.5", lastStatus.DecayRate)
	}
	if lastStatus.Model != "deepseek-v4-pro" {
		t.Errorf("Model = %q", lastStatus.Model)
	}
	if lastStatus.Provider != "deepseek-foreman" {
		t.Errorf("Provider = %q", lastStatus.Provider)
	}
	if lastStatus.LastTick != "2026-07-20T00:00:00Z" {
		t.Errorf("LastTick = %q", lastStatus.LastTick)
	}
	if lastStatus.LastTickStart != "2026-07-19T23:55:00Z" {
		t.Errorf("LastTickStart = %q", lastStatus.LastTickStart)
	}
}

func TestSyncProjectStatuses_WithNullTimestamps(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	// Insert project without last_tick timestamps (NULL defaults).
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, weight, priority, enabled, cooldown_s, decay_rate, model, provider, created_at, updated_at)
		VALUES ('beta', 'r2', 'w2', 5, 3, 0, 600, 1.0, 'gpt-4', 'openai', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var lastStatus projectStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncProjectStatuses(ctx); err != nil {
		t.Fatalf("syncProjectStatuses: %v", err)
	}

	if lastStatus.LastTick != "" {
		t.Errorf("LastTick = %q, want empty", lastStatus.LastTick)
	}
	if lastStatus.LastTickStart != "" {
		t.Errorf("LastTickStart = %q, want empty", lastStatus.LastTickStart)
	}
	if lastStatus.Enabled {
		t.Error("Enabled should be false")
	}
}

func TestSyncNamespaceSummary(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns1', 10, 2, 100, 1)`); err != nil {
		t.Fatalf("insert ns1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns2', 20, 5, 50, 1)`); err != nil {
		t.Fatalf("insert ns2: %v", err)
	}

	var received namespaceSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncNamespaceSummary(ctx); err != nil {
		t.Fatalf("syncNamespaceSummary: %v", err)
	}

	if received.Count != 2 {
		t.Errorf("Count = %d, want 2", received.Count)
	}
	if received.TotalWeight != 30 {
		t.Errorf("TotalWeight = %d, want 30", received.TotalWeight)
	}
	if received.TotalReserved != 7 {
		t.Errorf("TotalReserved = %d, want 7", received.TotalReserved)
	}
}

func TestSyncNamespaceStatuses(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled, description) VALUES ('ns1', 10, 2, 100, 1, 'First namespace')`); err != nil {
		t.Fatalf("insert ns1: %v", err)
	}

	var lastStatus namespaceStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncNamespaceStatuses(ctx); err != nil {
		t.Fatalf("syncNamespaceStatuses: %v", err)
	}

	if lastStatus.ID != "ns1" {
		t.Errorf("ID = %q, want ns1", lastStatus.ID)
	}
	if lastStatus.Weight != 10 {
		t.Errorf("Weight = %d, want 10", lastStatus.Weight)
	}
	if lastStatus.Reserved != 2 {
		t.Errorf("Reserved = %d, want 2", lastStatus.Reserved)
	}
	if lastStatus.HardCap != 100 {
		t.Errorf("HardCap = %d, want 100", lastStatus.HardCap)
	}
	if !lastStatus.Enabled {
		t.Error("Enabled should be true")
	}
	if lastStatus.Description != "First namespace" {
		t.Errorf("Description = %q", lastStatus.Description)
	}
}

func TestSyncNamespaceStatuses_NullDescription(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// Insert without description (NULL).
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns-nodesc', 5, 1, 50, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var lastStatus namespaceStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncNamespaceStatuses(ctx); err != nil {
		t.Fatalf("syncNamespaceStatuses: %v", err)
	}

	if lastStatus.Description != "" {
		t.Errorf("Description = %q, want empty (COALESCE from NULL)", lastStatus.Description)
	}
	if lastStatus.Enabled {
		t.Error("Enabled should be false (enabled=0)")
	}
}

func TestSyncSDLC_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncSDLC(ctx); err != nil {
		t.Fatalf("syncSDLC: %v", err)
	}

	if callCount != 0 {
		t.Errorf("callCount = %d, want 0 (no events)", callCount)
	}
}

func TestSyncSDLC_PostsEvents(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	e1 := &database.Event{
		Severity:  database.SeverityHigh,
		Component: "scheduler",
		Message:   "escalation triggered for tick t1",
		Details:   `{"tick":"t1"}`,
	}
	e2 := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "mcp",
		Message:   "MCP mutation applied",
		Details:   "{}",
	}
	if err := database.LogEvent(ctx, db, e1); err != nil {
		t.Fatalf("LogEvent e1: %v", err)
	}
	if err := database.LogEvent(ctx, db, e2); err != nil {
		t.Fatalf("LogEvent e2: %v", err)
	}

	received := map[string]sdlcEvent{}
	domains := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env postMemoryBody
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		var ev sdlcEvent
		if err := json.Unmarshal([]byte(env.Content), &ev); err != nil {
			t.Errorf("unmarshal content: %v", err)
		}
		received[env.Key] = ev
		domains[env.Key] = env.Domain
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)

	if err := s.syncSDLC(ctx); err != nil {
		t.Fatalf("syncSDLC: %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("received %d posts, want 2", len(received))
	}

	key1 := fmt.Sprintf("/fleet/events/%d", e1.ID)
	key2 := fmt.Sprintf("/fleet/events/%d", e2.ID)

	if domains[key1] != "event" || domains[key2] != "event" {
		t.Errorf("domains = %v, want event for both", domains)
	}

	got1, ok := received[key1]
	if !ok {
		t.Fatalf("missing key %s", key1)
	}
	if got1.ID != e1.ID || got1.Severity != "HIGH" || got1.Component != "scheduler" ||
		got1.Message != "escalation triggered for tick t1" || got1.Details != `{"tick":"t1"}` {
		t.Errorf("event 1 payload = %+v", got1)
	}
	if got1.CreatedAt == "" {
		t.Error("event 1 CreatedAt is empty")
	}

	got2, ok := received[key2]
	if !ok {
		t.Fatalf("missing key %s", key2)
	}
	if got2.ID != e2.ID || got2.Severity != "INFO" || got2.Component != "mcp" ||
		got2.Message != "MCP mutation applied" {
		t.Errorf("event 2 payload = %+v", got2)
	}
}

func TestSyncTicks_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncTickLifecycle(ctx); err != nil {
		t.Fatalf("syncTickLifecycle: %v", err)
	}

	if callCount != 0 {
		t.Errorf("callCount = %d, want 0 (no ticks)", callCount)
	}
}

func TestSyncTicks_PostsTicks(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	now := time.Now().UTC().Format(time.RFC3339)

	// Completed tick with a full lifecycle record.
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, cost_usd, urgency, error, created_at)
		VALUES ('p1-2026-07-31-10-00-00', 'p1', 'completed', 'committed', '2026-07-31T10:00:00Z', '2026-07-31T10:35:00Z', 0, 3, 12, 0.42, 7.5, '', ?)`, now); err != nil {
		t.Fatalf("insert completed tick: %v", err)
	}

	// Failed tick with NULLs on every nullable column (COALESCE path).
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES ('p1-2026-07-31-11-00-00', 'p1', 'failed', ?)`, now); err != nil {
		t.Fatalf("insert failed tick: %v", err)
	}

	received := map[string]tickLifecycle{}
	domains := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env postMemoryBody
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		var tl tickLifecycle
		if err := json.Unmarshal([]byte(env.Content), &tl); err != nil {
			t.Errorf("unmarshal content: %v", err)
		}
		received[env.Key] = tl
		domains[env.Key] = env.Domain
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncTickLifecycle(ctx); err != nil {
		t.Fatalf("syncTickLifecycle: %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("received %d posts, want 2", len(received))
	}

	keyOK := "/fleet/projects/p1/ticks/p1-2026-07-31-10-00-00"
	keyFail := "/fleet/projects/p1/ticks/p1-2026-07-31-11-00-00"

	if domains[keyOK] != "event" || domains[keyFail] != "event" {
		t.Errorf("domains = %v, want event for both", domains)
	}

	gotOK, ok := received[keyOK]
	if !ok {
		t.Fatalf("missing key %s", keyOK)
	}
	if gotOK.ProjectName != "p1" || gotOK.Status != "completed" || gotOK.Outcome != "committed" ||
		gotOK.SpawnedAt != "2026-07-31T10:00:00Z" || gotOK.CompletedAt != "2026-07-31T10:35:00Z" ||
		gotOK.ExitCode != 0 || gotOK.Commits != 3 || gotOK.FilesChanged != 12 ||
		gotOK.CostUSD != 0.42 || gotOK.Urgency != 7.5 || gotOK.Error != "" {
		t.Errorf("completed tick payload = %+v", gotOK)
	}

	gotFail, ok := received[keyFail]
	if !ok {
		t.Fatalf("missing key %s", keyFail)
	}
	// NULL columns must arrive as zero values via COALESCE.
	if gotFail.Status != "failed" || gotFail.Outcome != "" || gotFail.SpawnedAt != "" ||
		gotFail.CompletedAt != "" || gotFail.ExitCode != 0 || gotFail.Commits != 0 ||
		gotFail.FilesChanged != 0 || gotFail.CostUSD != 0.0 || gotFail.Urgency != 0.0 ||
		gotFail.Error != "" {
		t.Errorf("failed tick payload (NULL COALESCE) = %+v", gotFail)
	}
}

func TestSyncOnce_CallsAllSix(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns1', 10, 1, 100, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ctx := context.Background()
	if err := database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "sync",
		Message:   "sync cycle event",
	}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('p1-2026-07-31-12-00-00', 'p1', 'completed', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)

	s.syncOnce(ctx)

	// Fleet summary (1) + project status (1) + namespace summary (1) + namespace status (1)
	// + SDLC events (1) + tick lifecycle (1) = 6
	if callCount != 6 {
		t.Errorf("callCount = %d, want 6", callCount)
	}
}

func TestSyncOnce_ContinuesOnError(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	// Seed data so all 6 sync functions produce at least one POST.
	insertProject(t, db, "p1", "r1", "w1", 1)
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns1', 10, 1, 100, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ctx := context.Background()
	if err := database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "sync",
		Message:   "sync cycle event",
	}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('p1-2026-07-31-12-00-00', 'p1', 'completed', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)

	// Should not panic — all errors are logged, not returned.
	s.syncOnce(ctx)

	// Fleet summary (1) + project status (1) + namespace summary (1) + namespace status (1)
	// + SDLC events (1) + tick lifecycle (1) = 6
	if callCount != 6 {
		t.Errorf("callCount = %d, want 6", callCount)
	}
}

func TestRun_StartsAndStops(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)

	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count writes only: the SCHED-GAP-072 key-validation probe is a GET
		// (/api/namespaces) and fires whenever DUCKBRAIN_API_KEY is set in the
		// environment — counting it would make this test environment-dependent.
		if r.Method == http.MethodPost {
			callCount++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	s.interval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	// Wait for the initial syncOnce + ticks to run.
	time.Sleep(500 * time.Millisecond)

	cancel()

	select {
	case <-runDone:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of cancel")
	}

	// New contract (change-detection): the initial syncOnce posts fleet
	// summary + 1 project status + namespace summary = 3; every later cycle
	// finds the payloads unchanged (synced_at stripped from the hash) and
	// posts nothing. Ticks firing is proven by Run returning promptly on
	// cancel; the dedupe itself is covered by TestPostMemory_ChangeDetection.
	if callCount != 3 {
		t.Errorf("callCount = %d, want 3 (initial syncOnce; unchanged payloads dedupe on later cycles)", callCount)
	}
}

func TestSyncFleetSummary_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.TotalProjects != 0 {
		t.Errorf("TotalProjects = %d, want 0", received.TotalProjects)
	}
	if received.Enabled != 0 {
		t.Errorf("Enabled = %d, want 0", received.Enabled)
	}
}

func TestSyncFleetSummary_WithRunningTicks(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('t1', 'p1', 'running', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	var received fleetSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncFleetSummary(ctx); err != nil {
		t.Fatalf("syncFleetSummary: %v", err)
	}

	if received.ActiveTicks != 1 {
		t.Errorf("ActiveTicks = %d, want 1", received.ActiveTicks)
	}
}

func TestSyncProjectStatuses_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncProjectStatuses(ctx); err != nil {
		t.Fatalf("syncProjectStatuses: %v", err)
	}

	if callCount != 0 {
		t.Errorf("callCount = %d, want 0 (no projects)", callCount)
	}
}

func TestSyncNamespaceSummary_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	var received namespaceSummary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeDuckBrainContent(t, r, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncNamespaceSummary(ctx); err != nil {
		t.Fatalf("syncNamespaceSummary: %v", err)
	}

	if received.Count != 0 {
		t.Errorf("Count = %d, want 0", received.Count)
	}
	if received.TotalWeight != 0 {
		t.Errorf("TotalWeight = %d, want 0", received.TotalWeight)
	}
	if received.TotalReserved != 0 {
		t.Errorf("TotalReserved = %d, want 0", received.TotalReserved)
	}
}

func TestSyncNamespaceStatuses_EmptyDB(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	if err := s.syncNamespaceStatuses(ctx); err != nil {
		t.Fatalf("syncNamespaceStatuses: %v", err)
	}

	if callCount != 0 {
		t.Errorf("callCount = %d, want 0 (no namespaces)", callCount)
	}
}

func TestSyncOnce_Concurrent(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	insertProject(t, db, "p1", "r1", "w1", 1)
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled) VALUES ('ns1', 10, 1, 100, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewDuckBrainSync(db, "test-ns", srv.URL)
	ctx := context.Background()

	// Run 3 syncOnce calls concurrently — they should not deadlock.
	done := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		go func() {
			s.syncOnce(ctx)
			done <- struct{}{}
		}()
	}

	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("syncOnce did not complete within 5s — possible deadlock")
		}
	}
}

func TestRun_PreCancelled(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	s := NewDuckBrainSync(db, "test-ns", "http://localhost:0")
	s.interval = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	select {
	case <-runDone:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit within 5s of pre-cancelled context")
	}
}

func TestNewDuckBrainSync_NilDB(t *testing.T) {
	s := NewDuckBrainSync((*sql.DB)(nil), "ns", "http://localhost")
	if s.db != nil {
		t.Error("db should be nil")
	}
	if s.namespace != "ns" {
		t.Errorf("namespace = %q", s.namespace)
	}
}
