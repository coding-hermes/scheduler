package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-074 (2026-08-27): gateway-spawned ticks were
// only sometimes linkable to their state.db session (78/200) because linkage
// relied on a time-window + prompt-marker heuristic over sessions whose only
// stable handle was the per-request resp_* id stored in ticks.session_id.
// The spawn path now sends X-Hermes-Session-Key: <tick id> on every
// /v1/responses POST (both primary and fallback-chain hops) so each run
// carries a durable, self-describing session handle for fleet-quality review.
// The S-GAP-003 placeholder contract stays intact: resp.ID still replaces the
// tick-id placeholder in ticks.session_id on success.

// TestSpawn_GatewaySendsTickIDAsSessionKey pins the wire behavior end to end:
// Spawner.Spawn → GatewayClient.SendResponseWithSessionKey → HTTP header.
// The header value must be EXACTLY the tick id — the reviewer matches on it
// verbatim via sessions.session_key.
func TestSpawn_GatewaySendsTickIDAsSessionKey(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "sgap074-sesskey"
		tickID      = "sgap074-sesskey-2026-08-27-10-00-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	var gotHeader atomic.Value // string; empty until first request lands
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader.Store(r.Header.Get("X-Hermes-Session-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_sgap074",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("expected non-nil SpawnedTick")
		return
	}

	got, _ := gotHeader.Load().(string)
	if got != tickID {
		t.Fatalf("X-Hermes-Session-Key sent during spawn = %q, want exactly %q — review linkage regressed to time-window guessing", got, tickID)
	}

	// S-GAP-003 regression guard: the real resp id still lands in
	// ticks.session_id when the gateway returns one (placeholder only
	// covers the in-flight window; completion is slot_pool's job — see
	// sgap003_spawn_test.go for the placeholder-phase assertions).
	if tick.SessionID != "resp_sgap074" {
		t.Fatalf("SpawnedTick.SessionID = %q, want resp_sgap074 (real gateway id must win over the placeholder)", tick.SessionID)
	}

	t.Logf("OK: spawn POSTed X-Hermes-Session-Key=%q and kept SessionID=%q", got, tick.SessionID)
}
