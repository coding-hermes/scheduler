package scheduler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// GAP-035 regression guard + acceptance tests. The 2026-08-04 outage:
// a Hermes gateway update stopped accepting per-foreman "fk-*" keys, every
// spawn failed with a 401, and the daemon kept dispatching — 8208+ failed
// fleet ticks with no detection. These tests pin the permanent fix:
//
//  1. a per-project key is VALIDATED against the gateway BEFORE dispatch
//     (accepted key → spawn proceeds; rejected key → fail fast, zero
//     dispatch attempts, no exec fallback, HIGH event);
//  2. a gateway 401/403 is a distinct TERMINAL classification
//     (ErrGatewayKeyRejected) even when the probe can't catch it, and a
//     non-2xx status can never silently masquerade as a successful spawn.

// gatewayKeyGuardServer is a stub Hermes gateway that records every request
// in order. healthStatus/authOf configures the /health probe behavior;
// dispatchStatus configures /v1/responses.
type gatewayKeyGuardServer struct {
	mu sync.Mutex

	healthStatus   int // status for GET /health (probe)
	healthAuth     string
	dispatchStatus int // status for POST /v1/responses
	dispatchAuth   string

	calls     []string // request paths in arrival order
	dispatchN int      // how many /v1/responses requests arrived
	healthN   int      // how many /health requests arrived
}

func (g *gatewayKeyGuardServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls = append(g.calls, r.URL.Path)
		g.mu.Unlock()

		switch r.URL.Path {
		case "/health":
			g.mu.Lock()
			g.healthN++
			g.healthAuth = r.Header.Get("Authorization")
			status := g.healthStatus
			g.mu.Unlock()
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"type":    "auth_error",
						"message": "Invalid gateway API key",
					},
				})
				return
			}
			w.WriteHeader(status)
		case "/v1/responses":
			g.mu.Lock()
			g.dispatchN++
			g.dispatchAuth = r.Header.Get("Authorization")
			status := g.dispatchStatus
			g.mu.Unlock()
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"type":    "auth_error",
						"message": "Invalid gateway API key",
					},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "resp_gap035",
				"status": "completed",
				"output": []map[string]any{},
				"usage":  map[string]int{},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// snapshot copies the recorded state under the mutex.
func (g *gatewayKeyGuardServer) snapshot() (calls []string, healthAuth, dispatchAuth string, healthN, dispatchN int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls = append([]string(nil), g.calls...)
	return calls, g.healthAuth, g.dispatchAuth, g.healthN, g.dispatchN
}

// highAuthEventCount counts HIGH "gateway key rejected" spawn events for the
// given project.
func highAuthEventCount(t *testing.T, db *sql.DB, project string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity='HIGH' AND component='spawn' AND message='gateway key rejected' AND json_extract(details, '$.project') = ?`, project).Scan(&n); err != nil {
		t.Fatalf("count HIGH spawn events: %v", err)
	}
	return n
}

// TestSpawn_PerForemanKeyValidatedBeforeDispatch is the ACCEPTED-key half of
// the GAP-035 acceptance test: a project with GatewayKey set must be probed
// (GET /health with the per-foreman key) BEFORE the prompt is dispatched, and
// the spawn then proceeds normally with the per-foreman key on both requests.
func TestSpawn_PerForemanKeyValidatedBeforeDispatch(t *testing.T) {
	db := newTestDB(t)

	stub := &gatewayKeyGuardServer{healthStatus: http.StatusOK, dispatchStatus: http.StatusOK}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:       "gap035-accepted-key",
		Workdir:    t.TempDir(),
		GatewayKey: "fk-good-key",
	}
	tick, err := spawner.Spawn(project, "gap035-accepted-key-2026-08-13-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on accepted key")
		return
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}

	calls, healthAuth, dispatchAuth, healthN, dispatchN := stub.snapshot()
	wantCalls := []string{"/health", "/v1/responses"}
	if len(calls) != len(wantCalls) || calls[0] != wantCalls[0] || calls[1] != wantCalls[1] {
		t.Errorf("gateway calls = %v, want %v — the key probe must run BEFORE dispatch", calls, wantCalls)
	}
	if healthN != 1 || dispatchN != 1 {
		t.Errorf("request counts = (%d health, %d dispatch), want (1, 1)", healthN, dispatchN)
	}
	if healthAuth != "Bearer fk-good-key" {
		t.Errorf("probe Authorization = %q, want per-foreman 'Bearer fk-good-key'", healthAuth)
	}
	if dispatchAuth != "Bearer fk-good-key" {
		t.Errorf("dispatch Authorization = %q, want per-foreman 'Bearer fk-good-key'", dispatchAuth)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 1 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (1, 0)", httpCount, execCount)
	}
}

// TestSpawn_PerForemanKeyRejectedFailsFastBeforeDispatch is the REJECTED-key
// half of the GAP-035 acceptance test: a 401 on the pre-dispatch probe must
// fail the tick fast — zero prompt dispatches, no exec fallback even when
// noExecFallback=false, the tick row never marked completed, and an immediate
// HIGH event so the regression cannot go unnoticed again.
func TestSpawn_PerForemanKeyRejectedFailsFastBeforeDispatch(t *testing.T) {
	db := newTestDB(t)

	stub := &gatewayKeyGuardServer{healthStatus: http.StatusUnauthorized, dispatchStatus: http.StatusOK}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	// Fallback deliberately ENABLED: an auth rejection is terminal and must
	// not silently flip to exec.Command (which would mask the key regression
	// and keep flooding the fleet with disguised failures).
	spawner.SetNoExecFallback(false)
	spawner.SetEventLogger(NewEventLogger(db))

	const (
		projectName = "gap035-rejected-key"
		tickID      = "gap035-rejected-key-2026-08-13-00-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	project := PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		GatewayKey: "fk-revoked-key",
	}
	tick, err := spawner.Spawn(project, tickID)

	if err == nil {
		t.Fatal("Spawn returned nil error on rejected per-foreman key — the guard is not firing")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("error = %q, want it to wrap ErrGatewayKeyRejected (terminal auth classification)", err)
	}
	if !strings.Contains(err.Error(), "gateway key rejected") {
		t.Errorf("error = %q, want the distinct 'gateway key rejected' message", err)
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on key rejection — no tick should be produced")
	}

	calls, _, _, healthN, dispatchN := stub.snapshot()
	if dispatchN != 0 {
		t.Errorf("gateway saw %d dispatch request(s) — a rejected key must fail BEFORE dispatch (no blind send)", dispatchN)
	}
	if healthN != 1 {
		t.Errorf("probe count = %d, want exactly 1 (single cheap validation, no retry flood)", healthN)
	}
	if len(calls) != 1 || calls[0] != "/health" {
		t.Errorf("gateway calls = %v, want only [/health] — no dispatch, no retry", calls)
	}

	if got := tickStatusOf(t, db, tickID); got != "running" {
		t.Errorf("tick status = %q after rejected key, want 'running' — a failed spawn must NOT mark the tick completed", got)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0) — no HTTP success, no exec fallback on auth rejection", httpCount, execCount)
	}

	if n := highAuthEventCount(t, db, projectName); n != 1 {
		t.Errorf("HIGH 'gateway key rejected' events = %d, want 1 — key rejection must be immediately visible in the events table", n)
	}
}

// TestSpawn_Response401ClassifiedTerminal covers the dispatch-time backstop:
// a gateway whose /health does not authenticate keys lets the probe pass, so
// the rejection only surfaces on POST /v1/responses. The 401 must still be
// classified as terminal (ErrGatewayKeyRejected): exactly ONE dispatch
// attempt, no exec fallback, no retry, HIGH event emitted.
func TestSpawn_Response401ClassifiedTerminal(t *testing.T) {
	db := newTestDB(t)

	stub := &gatewayKeyGuardServer{healthStatus: http.StatusOK, dispatchStatus: http.StatusUnauthorized}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(false)
	spawner.SetEventLogger(NewEventLogger(db))

	const (
		projectName = "gap035-dispatch-401"
		tickID      = "gap035-dispatch-401-2026-08-13-00-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	project := PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		GatewayKey: "fk-revoked-key",
	}
	tick, err := spawner.Spawn(project, tickID)

	if err == nil {
		t.Fatal("Spawn returned nil error on gateway 401 — auth failure was silent")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("error = %q, want it to wrap ErrGatewayKeyRejected (terminal classification)", err)
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("error = %q, want the gateway's own 'auth_error' classification surfaced", err)
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on 401 — no tick should be produced")
	}

	_, _, _, _, dispatchN := stub.snapshot()
	if dispatchN != 1 {
		t.Errorf("dispatch attempts = %d, want exactly 1 — a 401 must fail the tick, not retry into a flood", dispatchN)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0) — no HTTP success, no exec fallback on 401", httpCount, execCount)
	}
	if n := highAuthEventCount(t, db, projectName); n != 1 {
		t.Errorf("HIGH 'gateway key rejected' events = %d, want 1", n)
	}
}

// TestGatewayClient_ValidateKey pins the client-level probe contract.
func TestGatewayClient_ValidateKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer fk-good":
			w.WriteHeader(http.StatusOK)
		case "Bearer fk-bad":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"type": "auth_error", "message": "Invalid gateway API key"},
			})
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second)

	if err := client.ValidateKey(t.Context(), "fk-good"); err != nil {
		t.Errorf("ValidateKey(accepted key) = %v, want nil", err)
	}

	err := client.ValidateKey(t.Context(), "fk-bad")
	if err == nil {
		t.Fatal("ValidateKey(rejected key) = nil, want ErrGatewayKeyRejected")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("ValidateKey(rejected key) = %q, want it to wrap ErrGatewayKeyRejected", err)
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("ValidateKey(rejected key) = %q, want the gateway's 'auth_error' detail surfaced", err)
	}

	err = client.ValidateKey(t.Context(), "fk-unknown")
	if err == nil {
		t.Fatal("ValidateKey(403 key) = nil, want ErrGatewayKeyRejected")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("ValidateKey(403 key) = %q, want it to wrap ErrGatewayKeyRejected", err)
	}
}

// TestGatewayClient_SendResponse_401WithNonErrorBodyStillFails closes the
// silent-swallow hole at the heart of the outage: before the status check, a
// 401 whose body was VALID JSON without an "error" field unmarshalled into an
// empty Response and SendResponse returned a "successful" spawn. Every 401
// must now be classified as ErrGatewayKeyRejected regardless of body shape.
func TestGatewayClient_SendResponse_401WithNonErrorBodyStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// Valid JSON, no "error" field — the pre-GAP-035 killer shape.
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_401",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second)
	resp, err := client.SendResponse(t.Context(), "prompt", "model", "", "fk-revoked")

	if err == nil {
		t.Fatal("SendResponse(401 non-error JSON body) = nil error — a key rejection was silently swallowed as success")
	}
	if !errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("error = %q, want it to wrap ErrGatewayKeyRejected", err)
	}
	if resp != nil {
		t.Errorf("SendResponse(401) returned non-nil response %+v — a rejected key must never look like a completed tick", resp)
	}
}

// TestGatewayClient_SendResponse_Non2xxFails covers the remaining non-2xx
// statuses: they must fail the request (no silent success), but only 401/403
// carry the terminal auth classification.
func TestGatewayClient_SendResponse_Non2xxFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "server_error", "message": "boom"}})
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second)
	resp, err := client.SendResponse(t.Context(), "prompt", "model", "", "")

	if err == nil {
		t.Fatal("SendResponse(500) = nil error — a non-2xx status must never succeed")
	}
	if errors.Is(err, ErrGatewayKeyRejected) {
		t.Errorf("500 was classified as ErrGatewayKeyRejected — only 401/403 are auth rejections: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want the HTTP status surfaced", err)
	}
	if resp != nil {
		t.Errorf("SendResponse(500) returned non-nil response %+v", resp)
	}
}
