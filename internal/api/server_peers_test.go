package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// REMOTE-003 §2 acceptance: the three peers routes answer with the
// documented shapes, refuse unauthenticated calls (fail-closed), and render
// a peer past the freshness window as stale:true WITH its last_contact —
// never a "down" field.

// TestPeersUpsertListHeartbeat drives the full §2 lifecycle through the wire:
// register → list (documented shape) → heartbeat → list fresh. The window
// arithmetic itself is proven at the store layer
// (database.TestIsPeerStale_Window); this file proves the SURFACE: shapes,
// status codes, the no-"down" rendering law, and the gating.
func TestPeersUpsertListHeartbeat(t *testing.T) {
	a := newAPITestServer(t)

	// Register.
	status, resp := a.do(t, "POST", "/api/v1/peers", map[string]interface{}{
		"id":           "box-b",
		"url":          "http://box-b:9090",
		"version":      "v1.2.3",
		"capabilities": "control,query",
	})
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/peers status = %d, want 200: %v", status, resp)
	}

	// List carries the documented shape {id, url, last_contact, stale, version}.
	status, resp = a.do(t, "GET", "/api/v1/peers", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/peers status = %d, want 200", status)
	}
	peers, ok := resp["peers"].([]interface{})
	if !ok {
		t.Fatalf("GET /api/v1/peers body missing peers array: %v", resp)
	}
	if len(peers) != 1 {
		t.Fatalf("peers list = %v, want exactly one peer", peers)
	}
	p := peers[0].(map[string]interface{})
	for _, key := range []string{"id", "url", "last_contact", "stale", "version"} {
		if _, ok := p[key]; !ok {
			t.Errorf("peer entry missing documented key %q: %v", key, p)
		}
	}
	if _, down := p["down"]; down {
		t.Errorf("peer entry carries a \"down\" field — the rendering law forbids it: %v", p)
	}
	if p["id"] != "box-b" || p["url"] != "http://box-b:9090" || p["version"] != "v1.2.3" {
		t.Errorf("peer entry fields wrong: %v", p)
	}
	if p["last_contact"] != "" {
		t.Errorf("last_contact after registration = %v, want \"\" (registration is not liveness)", p["last_contact"])
	}
	if p["stale"] != true {
		t.Errorf("never-heartbeated peer stale = %v, want true", p["stale"])
	}

	// Heartbeat stamps liveness.
	status, resp = a.do(t, "POST", "/api/v1/peers/box-b/heartbeat", map[string]interface{}{})
	if status != http.StatusOK {
		t.Fatalf("POST heartbeat status = %d, want 200: %v", status, resp)
	}
	status, resp = a.do(t, "GET", "/api/v1/peers", nil)
	if status != http.StatusOK {
		t.Fatalf("GET after heartbeat status = %d", status)
	}
	p = resp["peers"].([]interface{})[0].(map[string]interface{})
	if p["last_contact"] == "" {
		t.Error("last_contact empty after heartbeat — the stamp did not land")
	}
	if p["stale"] != false {
		t.Errorf("just-heartbeated peer stale = %v, want false", p["stale"])
	}

	// Upsert refreshes identity without fabricating liveness.
	status, _ = a.do(t, "POST", "/api/v1/peers", map[string]interface{}{
		"id": "box-b", "url": "http://box-b:9091", "version": "v2.0.0",
	})
	if status != http.StatusOK {
		t.Fatalf("re-registration status = %d, want 200", status)
	}
	_, resp = a.do(t, "GET", "/api/v1/peers", nil)
	p = resp["peers"].([]interface{})[0].(map[string]interface{})
	if p["url"] != "http://box-b:9091" || p["version"] != "v2.0.0" {
		t.Errorf("re-registration did not refresh identity: %v", p)
	}
	if p["last_contact"] == "" {
		t.Error("re-registration wiped last_contact — identity refresh must never touch liveness")
	}

	// Heartbeat on an unknown id is 404 (no auto-register).
	status, _ = a.do(t, "POST", "/api/v1/peers/ghost/heartbeat", map[string]interface{}{})
	if status != http.StatusNotFound {
		t.Errorf("unknown-peer heartbeat status = %d, want 404", status)
	}
	// Missing id is 400.
	status, _ = a.do(t, "POST", "/api/v1/peers", map[string]interface{}{"url": "http://x"})
	if status != http.StatusBadRequest {
		t.Errorf("missing-id upsert status = %d, want 400", status)
	}
	// Wrong method on the collection is 405.
	status, _ = a.do(t, "DELETE", "/api/v1/peers", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/v1/peers status = %d, want 405", status)
	}
}

// TestPeersFailClosedUnauthenticated is acceptance #4's refusal half, proven
// on a BARE server (no SetAuthConfig → authOff): every peers route — GET
// included — answers 503 fail-closed, and nothing is written.
func TestPeersFailClosedUnauthenticated(t *testing.T) {
	a := newBareServer(t)

	status, _ := a.doAnon(t, "POST", "/api/v1/peers", map[string]interface{}{
		"id": "intruder", "url": "http://intruder:9090",
	})
	if status != http.StatusServiceUnavailable {
		t.Errorf("unauthenticated POST /api/v1/peers status = %d, want 503 fail-closed", status)
	}
	status, _ = a.doAnon(t, "GET", "/api/v1/peers", nil)
	if status != http.StatusServiceUnavailable {
		t.Errorf("unauthenticated GET /api/v1/peers status = %d, want 503 fail-closed (the peer list is not public read material)", status)
	}
	status, _ = a.doAnon(t, "POST", "/api/v1/peers/x/heartbeat", map[string]interface{}{})
	if status != http.StatusServiceUnavailable {
		t.Errorf("unauthenticated heartbeat status = %d, want 503 fail-closed", status)
	}
	// Nothing was written.
	peers, err := database.ListPeers(context.Background(), a.db)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("fail-closed refusals wrote %d peer rows — a refused mutation must touch nothing", len(peers))
	}
}

// TestPeersBadCredentialRefused proves the 401 arm on the gated surface.
func TestPeersBadCredentialRefused(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.doAuth(t, "POST", "/api/v1/peers", map[string]interface{}{"id": "x"}, "wrong-token")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong-credential POST /api/v1/peers status = %d, want 401", status)
	}
	status, _ = a.doAuth(t, "GET", "/api/v1/peers", nil, "wrong-token")
	if status != http.StatusUnauthorized {
		t.Errorf("wrong-credential GET /api/v1/peers status = %d, want 401", status)
	}
}
