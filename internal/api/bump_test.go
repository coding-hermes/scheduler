package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// bumpProject activates a bump via the API and asserts the 200 contract.
func bumpProject(t *testing.T, a *apiTestServer, name string, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	return a.do(t, "POST", "/api/v1/projects/"+name+"/bump", body)
}

func TestAPI_BumpProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "bumpy")
	// Park it at a high cooldown with an adaptive policy + streak so the
	// snapshot is non-trivial.
	if _, err := a.db.Exec(`UPDATE projects SET cooldown_s = 43200,
		adaptive_cooldown = 1, cooldown_floor_s = 21600, cooldown_ceiling_s = 604800,
		no_progress_threshold = 10, no_progress_ticks = 4 WHERE name = 'bumpy'`); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	status, body := bumpProject(t, a, "bumpy", map[string]interface{}{
		"ticks": 5, "cooldown": 7200, "reason": "clear backlog",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	// Response carries the full bump contract.
	if body["bump_active"] != true || body["bump_remaining_ticks"] != float64(5) {
		t.Fatalf("bump fields wrong: %v", body)
	}
	if body["cooldown_s"] != float64(7200) {
		t.Fatalf("cooldown_s = %v, want 7200 (immediate effect)", body["cooldown_s"])
	}
	if body["bump_saved_cooldown_s"] != float64(43200) {
		t.Fatalf("saved cooldown = %v, want 43200", body["bump_saved_cooldown_s"])
	}

	// DB is the source of truth.
	p, err := database.GetProject(context.Background(), a.db, "bumpy")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !p.BumpActive || p.BumpRemainingTicks != 5 || p.BumpSavedNoProgress != 4 {
		t.Fatalf("db state wrong: %+v", p)
	}
}

func TestAPI_BumpProject_Defaults(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "defaults")
	status, body := bumpProject(t, a, "defaults", map[string]interface{}{"reason": "defaults"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if body["bump_remaining_ticks"] != float64(5) || body["cooldown_s"] != float64(7200) {
		t.Fatalf("defaults not applied: ticks=%v cooldown=%v", body["bump_remaining_ticks"], body["cooldown_s"])
	}
}

func TestAPI_BumpProject_MissingReason(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "noreason")
	status, _ := bumpProject(t, a, "noreason", map[string]interface{}{"ticks": 3})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (reason required)", status)
	}
	// Whitespace-only reason is also missing.
	status, _ = bumpProject(t, a, "noreason", map[string]interface{}{"reason": "   "})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (whitespace reason)", status)
	}
}

func TestAPI_BumpProject_TicksOutOfRange(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "ticks")
	status, _ := bumpProject(t, a, "ticks", map[string]interface{}{"ticks": 9, "reason": "too many"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (ticks > 8)", status)
	}
	status, _ = bumpProject(t, a, "ticks", map[string]interface{}{"ticks": 0, "reason": "zero"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (ticks < 1)", status)
	}
}

func TestAPI_BumpProject_CooldownBelowFloor(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "floor")
	status, _ := bumpProject(t, a, "floor", map[string]interface{}{"cooldown": 3600, "reason": "too fast"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (cooldown < 7200 floor)", status)
	}
}

func TestAPI_BumpProject_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := bumpProject(t, a, "ghost", map[string]interface{}{"reason": "x"})
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestAPI_BumpProject_Disabled(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "paused")
	if _, err := a.db.Exec(`UPDATE projects SET enabled = 0 WHERE name = 'paused'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	status, _ := bumpProject(t, a, "paused", map[string]interface{}{"reason": "x"})
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409 (disabled project)", status)
	}
}

func TestAPI_BumpProject_AlreadyBumped(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "twice")
	if _, err := database.BumpProject(context.Background(), a.db, "twice", 5, 7200, "first"); err != nil {
		t.Fatalf("first bump: %v", err)
	}
	status, _ := bumpProject(t, a, "twice", map[string]interface{}{"reason": "second"})
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409 (bump already active)", status)
	}
}

func TestAPI_UnbumpProject(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "unbump")
	if _, err := a.db.Exec(`UPDATE projects SET cooldown_s = 43200, no_progress_ticks = 6 WHERE name = 'unbump'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := database.BumpProject(context.Background(), a.db, "unbump", 5, 7200, "abort me"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	status, body := a.do(t, "POST", "/api/v1/projects/unbump/unbump", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	p, err := database.GetProject(context.Background(), a.db, "unbump")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.BumpActive || p.CooldownS != 43200 || p.BumpSavedNoProgress != 0 {
		t.Fatalf("unbump restore wrong: active=%v cd=%d savedStreak=%d", p.BumpActive, p.CooldownS, p.BumpSavedNoProgress)
	}
	// No active bump → 409.
	status, _ = a.do(t, "POST", "/api/v1/projects/unbump/unbump", nil)
	if status != http.StatusConflict {
		t.Errorf("second unbump status = %d, want 409", status)
	}
}

func TestAPI_StatusShowsBumps(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "badged")
	if _, err := database.BumpProject(context.Background(), a.db, "badged", 3, 14400, "status badge"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	raw, ok := body["bumps"]
	if !ok {
		t.Fatalf("status response missing bumps array: %v", body)
	}
	blob, _ := json.Marshal(raw)
	var bumps []struct {
		Project        string `json:"project"`
		RemainingTicks int    `json:"remaining_ticks"`
		CooldownS      int    `json:"cooldown_s"`
		Reason         string `json:"reason"`
	}
	if err := json.Unmarshal(blob, &bumps); err != nil {
		t.Fatalf("decode bumps: %v", err)
	}
	if len(bumps) != 1 || bumps[0].Project != "badged" || bumps[0].RemainingTicks != 3 ||
		bumps[0].CooldownS != 14400 || bumps[0].Reason != "status badge" {
		t.Fatalf("bumps array wrong: %+v", bumps)
	}
}

func TestAPI_StatusBumpsEmptyArrayWhenNone(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "plain")
	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	raw, ok := body["bumps"]
	if !ok {
		t.Fatalf("bumps key missing: %v", body)
	}
	arr, ok := raw.([]interface{})
	if !ok || len(arr) != 0 {
		t.Fatalf("bumps should be an empty array, got %T %v", raw, raw)
	}
}
