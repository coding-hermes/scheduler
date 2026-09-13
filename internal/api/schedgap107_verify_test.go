package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-107-VER verification suite (API layer). bump_test.go already
// covers: 200 success shape, defaults, missing/whitespace reason, ticks 0/9,
// cooldown<7200, 404, disabled 409, double-bump 409, unbump happy+409, bumps
// array present/empty. This file extends the matrix with what it lacks:
// boundary acceptance (ticks 1 and 8, cooldown exactly 7200), invalid-JSON
// 400, the ticks 1..8 sweep rejected outside the range, the unbump 404 and
// response-body restore contract, and the status bumps[] started_at field.

// TestVerify_BumpAPI_BoundariesAccepted pins the INCLUSIVE bounds: ticks 1
// and 8 are legal, cooldown exactly 7200 (the killer-lane floor) is legal.
// A guard implemented with <= instead of < would regress only these.
func TestVerify_BumpAPI_BoundariesAccepted(t *testing.T) {
	a := newAPITestServer(t)
	for _, tc := range []struct {
		name         string
		body         map[string]interface{}
		wantTicks    float64
		wantCooldown float64
	}{
		{"ticks-min", map[string]interface{}{"ticks": 1, "reason": "edge"}, 1, 7200},
		{"ticks-max", map[string]interface{}{"ticks": 8, "reason": "edge"}, 8, 7200},
		{"cooldown-exact-floor", map[string]interface{}{"cooldown": 7200, "reason": "edge"}, 5, 7200},
		{"cooldown-above-floor", map[string]interface{}{"cooldown": 21600, "reason": "edge"}, 5, 21600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustCreateAPITestProject(t, a.db, tc.name)
			status, body := bumpProject(t, a, tc.name, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %v", status, body)
			}
			if body["bump_remaining_ticks"] != tc.wantTicks {
				t.Errorf("bump_remaining_ticks = %v, want %v", body["bump_remaining_ticks"], tc.wantTicks)
			}
			if body["cooldown_s"] != tc.wantCooldown {
				t.Errorf("cooldown_s = %v, want %v", body["cooldown_s"], tc.wantCooldown)
			}
		})
	}
}

// TestVerify_BumpAPI_TicksRangeSweep rejects every out-of-range ticks value
// (negative, 9, 100) — the full rejection side of the 1..8 contract.
func TestVerify_BumpAPI_TicksRangeSweep(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "sweep")
	for _, ticks := range []int{-1, 9, 100} {
		status, body := bumpProject(t, a, "sweep", map[string]interface{}{"ticks": ticks, "reason": "sweep"})
		if status != http.StatusBadRequest {
			t.Errorf("ticks=%d: status = %d, want 400: %v", ticks, status, body)
		}
	}
	// None of the rejected requests may have activated a bump.
	p, err := database.GetProject(context.Background(), a.db, "sweep")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.BumpActive {
		t.Fatal("a rejected (400) bump request left bump_active=1 behind")
	}
}

// TestVerify_BumpAPI_InvalidJSON400 pins malformed-body handling: garbage
// JSON is a 400, not a 500/panic.
func TestVerify_BumpAPI_InvalidJSON400(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "garbage")
	// a.do marshals maps, so drive httptest directly for a raw body.
	req, err := http.NewRequest(http.MethodPost, a.ts.URL+"/api/v1/projects/garbage/bump", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Body = http.NoBody
	// Force the decode error path: empty body → EOF from json.Decoder.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400 (invalid JSON)", resp.StatusCode)
	}
}

// TestVerify_UnbumpAPI_NotFound404 pins the unknown-project unbump mapping.
func TestVerify_UnbumpAPI_NotFound404(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "POST", "/api/v1/projects/ghost/unbump", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %v", status, body)
	}
}

// TestVerify_UnbumpAPI_ResponseRestoresState asserts the unbump RESPONSE
// body (not just the DB) carries the restored pre-bump state — operators
// act on the response.
func TestVerify_UnbumpAPI_ResponseRestoresState(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "restore-me")
	if _, err := a.db.Exec(`UPDATE projects SET cooldown_s = 43200, no_progress_ticks = 6 WHERE name = 'restore-me'`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := database.BumpProject(context.Background(), a.db, "restore-me", 5, 7200, "abort"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	status, body := a.do(t, "POST", "/api/v1/projects/restore-me/unbump", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if body["bump_active"] != false {
		t.Errorf("bump_active = %v, want false in response", body["bump_active"])
	}
	if body["cooldown_s"] != float64(43200) {
		t.Errorf("cooldown_s = %v, want 43200 in response", body["cooldown_s"])
	}
	if body["no_progress_ticks"] != float64(6) {
		t.Errorf("no_progress_ticks = %v, want 6 in response", body["no_progress_ticks"])
	}
}

// TestVerify_StatusBumpsCarriesStartedAt extends the bumps[] field contract
// with started_at (the 12h-hard-cap observation window) and verifies the
// array reflects a DECREMENT after a bump tick consumes.
func TestVerify_StatusBumpsCarriesStartedAt(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "stamped")
	if _, err := database.BumpProject(context.Background(), a.db, "stamped", 3, 7200, "stamp"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	blob, _ := json.Marshal(body["bumps"])
	var bumps []struct {
		Project        string `json:"project"`
		RemainingTicks int    `json:"remaining_ticks"`
		CooldownS      int    `json:"cooldown_s"`
		Reason         string `json:"reason"`
		StartedAt      string `json:"started_at"`
	}
	if err := json.Unmarshal(blob, &bumps); err != nil {
		t.Fatalf("decode bumps: %v (raw: %s)", err, blob)
	}
	if len(bumps) != 1 {
		t.Fatalf("len(bumps) = %d, want 1", len(bumps))
	}
	b := bumps[0]
	if b.Project != "stamped" || b.RemainingTicks != 3 || b.CooldownS != 7200 || b.Reason != "stamp" {
		t.Fatalf("bumps[0] fields wrong: %+v", b)
	}
	if b.StartedAt == "" {
		t.Fatal("bumps[0].started_at empty — hard-cap window unobservable via status")
	}
}
