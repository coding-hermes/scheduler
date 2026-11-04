package api

// SCHED-GAP-16976 acceptance test.
//
// The row: GET /api/v1/status returned 504 under host load, so the operator
// surface went blind exactly when it was needed. Measured live 2026-11-03
// 20:59:10 — "WARN: slow request handler=status step=runningWaveContext" then
// "SLOW-REQ: GET /api/v1/status 5.068s status=504 bytes=99" — while the capacity
// guard was reading the endpoint, which is why its own report printed
// active_ticks = None during the storm.
//
// The contract this file pins: an OPTIONAL section that cannot finish inside its
// budget is NAMED in the response's "partial" list and the required fields are
// still served with HTTP 200. The required fields are the ones an operator acts
// on — active_ticks, paused, build_sha, active_projects, admission_law_violations
// — and they are cheap, so there is no honest reason to lose them behind an
// expensive reporting block.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSCHED16976_StatusDegradesInsteadOf504(t *testing.T) {
	srv := newGap1575Server(t)
	// A tight budget so the stall is unambiguous, but NOT below the soft
	// section budget: the point is that the SOFT budget bounds the section and
	// the overall budget bounds the request.
	srv.SetReadTimeout(2 * time.Second)
	// listRunningWaves is a documented optional section (S12 §9.4 live wave
	// load) — the section named in the live 504.
	stallGap1575Step(srv, "listRunningWaves", 3*time.Second)

	rec := httptest.NewRecorder()
	started := time.Now()
	srv.status(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("stalled optional section = %d, want 200 with partial; body=%s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	// NOTE ON THE STALL SEAM: readStepHook sleeps synchronously inside
	// obs.enter, so the 3s is un-interruptible by any context — that is the
	// harness, not production, where a stalled section is a DB query that
	// honours its context. What the fix must guarantee here is that the stall
	// is not paid SEVEN times: every later section sees the expired parent
	// context and fails immediately. So the bound is the stall plus slack,
	// not the soft budget.
	if elapsed > 3*time.Second+500*time.Millisecond {
		t.Errorf("handler took %s — the stalled section was not bounded and the later sections did not fail fast",
			elapsed)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	for _, field := range []string{"active_ticks", "paused", "build_sha", "active_projects", "admission_law_violations"} {
		if _, ok := body[field]; !ok {
			t.Errorf("degraded response is missing required field %q", field)
		}
	}
	partial, _ := body["partial"].([]interface{})
	if len(partial) == 0 {
		t.Fatalf("no \"partial\" list — the response must say what it could not compute: %s", rec.Body.String())
	}
	if !strings.Contains(strings.Join(stringSlice(partial), ","), "listRunningWaves") {
		t.Errorf("\"partial\" = %v, want it to name listRunningWaves", partial)
	}
	// The degraded section must be visible as absent-or-empty, never as a
	// fabricated zero that a reader would take for truth.
	if waves, ok := body["waves"].([]interface{}); ok && len(waves) > 0 {
		t.Errorf("\"waves\" = %v after the section timed out — a skipped section must not report data", waves)
	}
}

func stringSlice(in []interface{}) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestSCHED16976_HealthyStatusHasNoPartialField — the completeness marker must
// be ABSENT on a healthy run, so "no partial" is a meaningful signal and a
// reader can trust that nothing was skipped.
func TestSCHED16976_HealthyStatusHasNoPartialField(t *testing.T) {
	srv := newGap1575Server(t)
	srv.SetReadTimeout(5 * time.Second)

	rec := httptest.NewRecorder()
	srv.status(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy status = %d, want 200; body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if strings.Contains(rec.Body.String(), `"partial"`) {
		t.Errorf("healthy status carried a \"partial\" field — absence is the signal that every section answered: %s",
			rec.Body.String())
	}
}
