package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── SCHED-GAP-064 spawn-path tests ────────────────────────────────────────
//
// These pin the PASS criteria at the Spawn() level:
//   - the gateway path passes the RESOLVED (chain) model/provider to
//     SendResponse (a broken project primary resolves away to the project
//     fallback tier BEFORE dispatch);
//   - a gateway 401/403 on SendResponse retries ONCE with the next chain
//     entry; success on the retry completes the spawn;
//   - when the chain is exhausted the terminal GAP-035 classification
//     returns (bounded dispatches — no retry flood);
//   - a single-tier project (no fallback tier) keeps the legacy GAP-035
//     fail-fast behavior: exactly ONE dispatch on 401;
//   - the exec path passes the RESOLVED model/provider via -m/--provider.

// chainGatewayServer is a stub Hermes gateway. rejectAll rejects every
// /v1/responses with 401; otherwise it rejects dispatches whose model or
// provider matches rejectModel/rejectProvider. Every dispatch's resolved
// model/provider is recorded from the request body.
type chainGatewayServer struct {
	mu             sync.Mutex
	rejectAll      bool
	rejectModel    string
	rejectProvider string
	dispatches     []dispatchRecord
}

type dispatchRecord struct {
	model    string
	provider string
	status   int
}

func (g *chainGatewayServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			var req struct {
				Model    string `json:"model"`
				Provider string `json:"provider"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			status := http.StatusOK
			if g.rejectAll ||
				(req.Model != "" && req.Model == g.rejectModel) ||
				(req.Provider != "" && req.Provider == g.rejectProvider) {
				status = http.StatusUnauthorized
			}
			g.mu.Lock()
			g.dispatches = append(g.dispatches, dispatchRecord{model: req.Model, provider: req.Provider, status: status})
			g.mu.Unlock()
			if status != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"type": "auth_error", "message": "provider rejected"},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "resp_gap064",
				"status": "completed",
				"output": []map[string]any{},
				"usage":  map[string]int{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (g *chainGatewayServer) snapshot() []dispatchRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]dispatchRecord(nil), g.dispatches...)
}

// newChainSpawnerForGateway builds a spawner with fixed global tiers plus a
// live gateway client; noExecFallback keeps failures loud and local.
func newChainSpawnerForGateway(t *testing.T, gw *chainGatewayServer) *Spawner {
	t.Helper()
	db := newTestDB(t)
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)
	s := newChainSpawner(db, 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	s.SetNoExecFallback(true)
	return s
}

// TestSpawn_GatewayUsesResolvedFallbackProvider is the PASS criterion: a
// project whose PRIMARY provider is empty but whose chain has a valid
// fallback must dispatch with the fallback provider — never the spawner
// default. The stub never rejects, so a single 200 dispatch carrying the
// fallback proves resolution happened before dispatch.
func TestSpawn_GatewayUsesResolvedFallbackProvider(t *testing.T) {
	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "gap064-fallback-provider",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "", // empty primary → provider resolves through the chain
		FallbackProvider: "good-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap064-fallback-provider-2026-08-23-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1 (resolved before dispatch, no retry)", len(dispatches))
	}
	if dispatches[0].status != http.StatusOK {
		t.Fatalf("dispatch status = %d, want 200", dispatches[0].status)
	}
	if dispatches[0].provider != "good-fallback-provider" {
		t.Errorf("gateway saw provider = %q, want %q (resolved project fallback)", dispatches[0].provider, "good-fallback-provider")
	}
	if dispatches[0].model != "proj-model" {
		t.Errorf("gateway saw model = %q, want %q (project primary)", dispatches[0].model, "proj-model")
	}
}

// TestSpawn_GatewayBrokenPrimaryProviderRetriesToFallback covers the literal
// "broken-provider" PASS criterion: the gateway REJECTS the broken primary
// provider with an auth-class 401, and the spawn retries once with the next
// chain entry (the valid fallback provider) — which succeeds. This is the
// 2026-08-23 cost-audit scenario: a misrouted/broken provider must not sink
// the tick when a fallback exists.
func TestSpawn_GatewayBrokenPrimaryProviderRetriesToFallback(t *testing.T) {
	gw := &chainGatewayServer{rejectProvider: "broken-provider"}
	s := newChainSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "gap064-broken-provider",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "broken-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "good-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap064-broken-provider-2026-08-23-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway retry success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (broken primary + one retry)", len(dispatches))
	}
	if dispatches[0].provider != "broken-provider" || dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("first dispatch = (%q, %d), want (broken-provider, 401)", dispatches[0].provider, dispatches[0].status)
	}
	if dispatches[1].provider != "good-fallback-provider" || dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch = (%q, %d), want (good-fallback-provider, 200)", dispatches[1].provider, dispatches[1].status)
	}
}

// TestSpawn_GatewayRetriesOnceOnAuthRejection: when the gateway returns 401
// for the RESOLVED primary combination and a next chain entry exists, the
// spawn retries exactly once with the next entry; the retry succeeds and the
// tick completes.
func TestSpawn_GatewayRetriesOnceOnAuthRejection(t *testing.T) {
	gw := &chainGatewayServer{rejectModel: "proj-model"}
	s := newChainSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "gap064-retry",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap064-retry-2026-08-23-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway retry success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (primary + one retry)", len(dispatches))
	}
	if dispatches[0].model != "proj-model" || dispatches[0].provider != "proj-provider" {
		t.Errorf("first dispatch = (%q, %q), want primary (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "proj-model", "proj-provider")
	}
	if dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("first dispatch status = %d, want 401 (auth rejection)", dispatches[0].status)
	}
	if dispatches[1].model != "proj-fallback-model" || dispatches[1].provider != "proj-fallback-provider" {
		t.Errorf("retry dispatch = (%q, %q), want fallback (%q, %q)",
			dispatches[1].model, dispatches[1].provider, "proj-fallback-model", "proj-fallback-provider")
	}
	if dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch status = %d, want 200", dispatches[1].status)
	}
}

// TestSpawn_GatewayRetryExhaustedReturnsError: when the 401 persists on the
// retry, Spawn returns the terminal classification (gateway auth_error
// surfaced) — exactly two dispatches, no flood.
func TestSpawn_GatewayRetryExhaustedReturnsError(t *testing.T) {
	gw := &chainGatewayServer{rejectAll: true}
	s := newChainSpawnerForGateway(t, gw)

	project := PackedProject{
		Name:             "gap064-retry-exhausted",
		Workdir:          t.TempDir(),
		Model:            "proj-model",
		Provider:         "proj-provider",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "proj-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap064-retry-exhausted-2026-08-23-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error when chain exhausted — 401 must surface")
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("error = %q, want gateway auth_error detail surfaced", err.Error())
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on exhausted auth failure")
	}
	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Errorf("dispatch count = %d, want exactly 2 (primary + one retry, then stop)", len(dispatches))
	}
}

// TestSpawn_GatewayNoRetryWithoutFallback pins the GAP-035 legacy contract:
// a single-tier project (no fallback tier anywhere in the chain) keeps ONE
// dispatch on 401 — no retry flood for the common case. The spawner's
// global tiers are empty here, so the chain has exactly one entry.
func TestSpawn_GatewayNoRetryWithoutFallback(t *testing.T) {
	gw := &chainGatewayServer{rejectAll: true}
	db := newTestDB(t)
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)
	s := &Spawner{
		db:                db,
		maxConcurrent:     4,
		active:            make(map[string]*exec.Cmd),
		timeout:           30 * time.Minute,
		heartbeatInterval: 5 * time.Minute,
		// global tiers empty → the project's single entry is the whole chain
	}
	s.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	s.SetNoExecFallback(true)

	project := PackedProject{
		Name:     "gap064-no-fallback",
		Workdir:  t.TempDir(),
		Model:    "proj-model",
		Provider: "proj-provider",
	}
	_, err := s.Spawn(project, "gap064-no-fallback-2026-08-23-00-00-00")
	if err == nil {
		t.Fatal("Spawn returned nil error on 401 with no fallback")
	}
	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Errorf("dispatch count = %d, want exactly 1 (no fallback tier — GAP-035 fail-fast)", len(dispatches))
	}
}

// TestSpawn_ExecUsesResolvedValues: the exec path must receive the RESOLVED
// -m/--provider values (project fallback tier, since the primary provider is
// broken), verified against the fake hermes binary's captured argv.
func TestSpawn_ExecUsesResolvedValues(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 1)
	s.SetGatewayClient(nil) // force exec path
	s.SetNoExecFallback(false)

	dir := t.TempDir()
	captureFile := filepath.Join(dir, "capture.txt")
	script := `#!/bin/bash
echo "$@" > "` + captureFile + `"
exit 0
`
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	project := PackedProject{
		Name:             "gap064-exec",
		Workdir:          t.TempDir(),
		Model:            "", // empty primary → both fields resolve through the chain
		Provider:         "",
		FallbackModel:    "proj-fallback-model",
		FallbackProvider: "good-fallback-provider",
	}
	tick, err := s.Spawn(project, "gap064-exec-2026-08-23-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on exec path")
	}
	tick.Wait()

	args, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	joined := string(args)
	if !strings.Contains(joined, "-m proj-fallback-model") {
		t.Errorf("exec args missing resolved model: %s", joined)
	}
	if !strings.Contains(joined, "--provider good-fallback-provider") {
		t.Errorf("exec args missing resolved provider: %s", joined)
	}
}
