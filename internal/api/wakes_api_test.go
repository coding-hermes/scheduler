package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17002 — wake-table API round-trips over the REAL handler and a
// real temporary SQLite (the CTL-002 test-server pattern): nothing here calls
// an internal helper in place of the HTTP route.

func newWakeAPITest(t *testing.T) (*sql.DB, *httptest.Server, *Server) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	p := database.Project{
		Name: "wake-lane", Workdir: t.TempDir(), Weight: 1, Priority: 5,
		CooldownS: 600, Enabled: true,
		CreatedAt: "2026-11-07T00:00:00Z", UpdatedAt: "2026-11-07T00:00:00Z",
	}
	if err := database.CreateProject(ctx, db, &p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	s := NewServer(db, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return db, ts, s
}

func wakeRequest(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var resp *http.Response
	if body != nil {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, out
	}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, out
}

func TestWakeAPIRoundTrip(t *testing.T) {
	db, ts, _ := newWakeAPITest(t)

	// Create a one-shot wake.
	code, out := wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes", map[string]any{
		"project": "wake-lane",
		"at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"note":    "nightly verify",
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %v", out)
	}

	// The stored row reads back pending, with the note carried.
	wake, err := database.GetWake(context.Background(), db, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if wake.State != database.WakeStatePending || wake.Note != "nightly verify" {
		t.Fatalf("stored wake = %+v, want pending + note carried", wake)
	}

	// List filters by state.
	code, out = wakeRequest(t, http.MethodGet, ts.URL+"/api/v1/wakes?state=pending", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, out)
	}
	if n, _ := out["count"].(float64); n < 1 {
		t.Fatalf("list count = %v, want >= 1", out["count"])
	}

	// Exactly-one-clause law: at + daily is a 400, not a guess.
	code, _ = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes", map[string]any{
		"project": "wake-lane",
		"at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"daily":   "02:00",
	})
	if code != 400 {
		t.Fatalf("at+daily accepted with %d, want 400", code)
	}

	// A past 'at' is a 400 (that's a spawn request wearing a wake costume).
	code, _ = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes", map[string]any{
		"project": "wake-lane",
		"at":      time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	if code != 400 {
		t.Fatalf("past 'at' accepted with %d, want 400", code)
	}

	// A wake for an unknown lane is a 404.
	code, _ = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes", map[string]any{
		"project": "no-such-lane", "external": true,
	})
	if code != 404 {
		t.Fatalf("unknown project accepted with %d, want 404", code)
	}

	// Fire without a wired runner: 503 — never a silent fake fire.
	code, _ = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes/"+id+"/fire", nil)
	if code != 503 {
		t.Fatalf("unwired fire = %d, want 503", code)
	}

	// Cancel, then prove the state moved (DELETE twice = 409).
	code, out = wakeRequest(t, http.MethodDelete, ts.URL+"/api/v1/wakes/"+id, nil)
	if code != 200 {
		t.Fatalf("DELETE = %d %v, want 200", code, out)
	}
	code, _ = wakeRequest(t, http.MethodDelete, ts.URL+"/api/v1/wakes/"+id, nil)
	if code != 409 {
		t.Fatalf("second DELETE = %d, want 409", code)
	}
}

func TestWakeAPIExternalFireViaWiredRunner(t *testing.T) {
	_, ts, s := newWakeAPITest(t)

	code, out := wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes", map[string]any{
		"project": "wake-lane", "external": true, "payload": "deploy finished",
	})
	if code != 201 {
		t.Fatalf("create external: %d %v", code, out)
	}
	id, _ := out["id"].(string)

	// Wire the REAL runner over this DB — the same wiring the daemon does —
	// with a spawn hook that records and succeeds.
	fires := 0
	s.SetWakeFirer(func(ctx context.Context, wakeID string) (any, bool, error) {
		fires++
		if fires > 1 {
			return nil, false, nil // not pending anymore
		}
		return &database.Wake{ID: wakeID, Project: "wake-lane", Kind: database.WakeKindExternal}, true, nil
	})
	code, out = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes/"+id+"/fire", nil)
	if code != 202 {
		t.Fatalf("wired fire = %d %v, want 202", code, out)
	}
	if fires != 1 {
		t.Fatalf("firer called %d times, want 1", fires)
	}

	// Second fire: 409, and the firer must have been ASKED (it returns
	// not-pending) but must not have spawned twice.
	code, _ = wakeRequest(t, http.MethodPost, ts.URL+"/api/v1/wakes/"+id+"/fire", nil)
	if code != 409 {
		t.Fatalf("second fire = %d, want 409", code)
	}
	if fires != 2 {
		t.Fatalf("firer called %d times after the second fire, want 2 (asked, refused)", fires)
	}
}
