package api_test

// SCHED-GAP-1632: /api/v1/health must expose the daemon's clock mode + sim
// scale ("clock": {"mode": "sim"|"real", "scale": N}) sourced from the SAME
// clock.Describe() the boot line prints — on a --simulate daemon the API was
// otherwise indistinguishable from a real one (the only sim evidence was the
// stdout boot line "TIME: clock sim (scale=…)").

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// newClockHealthServer builds the minimal health-serving stack on clk (nil →
// wall clock): a loop with the clock installed (NewServer shares the loop's
// clock, SCHED-GAP-169), exec fallback disabled so nothing can spawn.
func newClockHealthServer(t *testing.T, clk clock.Clock) (*api.Server, *httptest.Server) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	t.Cleanup(loop.Stop)
	loop.SetNoExecFallback(true)
	if clk != nil {
		loop.SetClock(clk)
	}
	srv := api.NewServer(db, loop)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// getHealthMap GETs /api/v1/health and returns the decoded body.
func getHealthMap(t *testing.T, ts *httptest.Server) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("health body not JSON: %v", err)
	}
	return body
}

// TestHealth_ClockField_SimVsReal: the clock field mirrors the installed
// clock — mode "sim" + the sim clock's actual scale under a simulator
// (installed via the existing SetClock seam, no time.Now outside
// internal/clock), mode "real" + scale 1 on the wall-clock default stack.
func TestHealth_ClockField_SimVsReal(t *testing.T) {
	cases := []struct {
		name      string
		scale     float64
		install   bool
		wantMode  string
		wantScale float64
	}{
		{name: "sim clock at scale 1000", scale: 1000, install: true, wantMode: "sim", wantScale: 1000},
		{name: "sim clock at fractional scale", scale: 2.5, install: true, wantMode: "sim", wantScale: 2.5},
		{name: "wall clock default", install: false, wantMode: "real", wantScale: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var clk clock.Clock
			if tc.install {
				sim := clock.NewSimClockAt(tc.scale, time.Now())
				t.Cleanup(sim.Close)
				clk = sim
			}
			_, ts := newClockHealthServer(t, clk)
			body := getHealthMap(t, ts)

			raw, ok := body["clock"]
			if !ok {
				t.Fatalf("clock missing from /api/v1/health: %v", body)
			}
			clkMap, ok := raw.(map[string]interface{})
			if !ok {
				t.Fatalf("clock field not an object: %T (%v)", raw, raw)
			}
			mode, _ := clkMap["mode"].(string)
			if mode != tc.wantMode {
				t.Errorf("clock.mode = %q, want %q", mode, tc.wantMode)
			}
			gotScale, ok := clkMap["scale"].(float64)
			if !ok {
				t.Fatalf("clock.scale not a number: %T (%v)", clkMap["scale"], clkMap["scale"])
			}
			if gotScale != tc.wantScale {
				t.Errorf("clock.scale = %v, want %v", gotScale, tc.wantScale)
			}
		})
	}
}

// TestHealth_ClockField_TracksSetScale: SetScale on the live sim clock must
// be visible on the next health read — the field reads the clock's CURRENT
// scale, not a boot-time copy.
func TestHealth_ClockField_TracksSetScale(t *testing.T) {
	sim := clock.NewSimClockAt(10, time.Now())
	t.Cleanup(sim.Close)
	_, ts := newClockHealthServer(t, sim)

	sim.SetScale(500)
	body := getHealthMap(t, ts)
	clkMap := body["clock"].(map[string]interface{})
	if got := clkMap["scale"].(float64); got != 500 {
		t.Errorf("clock.scale after SetScale(500) = %v, want 500 (field must read the live clock)", got)
	}
	if mode := clkMap["mode"].(string); mode != "sim" {
		t.Errorf("clock.mode = %q, want sim", mode)
	}
}

// TestHealth_ClockField_DriftVsUptime pins the "same clock the rest of the
// surface uses" law: the clock field's mode must agree with the Describe()
// string uptime is measured through (both read s.clock()). Kept structural:
// a real clock serving "real" plus a sim clock serving "sim" (covered by the
// table above) is the drift an operator would see if the field ever stopped
// sharing the seam.
func TestHealth_ClockField_DriftVsUptime(t *testing.T) {
	_, ts := newClockHealthServer(t, nil)
	body := getHealthMap(t, ts)
	clkMap := body["clock"].(map[string]interface{})
	if clkMap["mode"] != "real" {
		t.Errorf("clock.mode = %v, want real on the default stack", clkMap["mode"])
	}
	if _, ok := body["uptime"]; !ok {
		t.Errorf("uptime missing — clock field must live beside the uptime it shares the seam with: %v", body)
	}
}
