package api

// SCHED-GAP-1575-B acceptance tests.
//
// The row: /api/v1/status issued ~12 sequential DB calls on ONE serialized
// SQLite connection (SetMaxOpenConns(1)) through a bare context.Background()
// with no deadline. One stalled helper — spendByCostSource (PERF-001),
// computeProjectFailureRates (SCHED-PERF-002) or configDriftBlock — hung the
// whole handler with no bytes and no status code. Measured live 2026-09-24
// ~07:00 local, pre-fix: /api/v1/live 0.6ms, /api/v1/config 0.3ms,
// /api/v1/projects 0.17s, /api/v1/status = curl exit 000 after an 8s bound
// with 0 bytes received.
//
// What these tests pin (each one asserts the MECHANISM, not just a status
// code):
//
//  1. StatusRespectsDeadline — with a NAMED status step stalled past the
//     deadline, the handler returns 504 INSIDE the budget (not after the
//     stall), the body names the step that blew it, and the response is not
//     a 200 with degraded data.
//  2. LivenessStillFast — the healthy path still answers 200 well under the
//     deadline and never emits the deadline body (the fix must not turn a
//     healthy status call into a 504 on a box whose loadavg is merely high).
//  3. ConfigExposesNewTimeout — /api/v1/config reports the ARMED deadline
//     (api_read_timeout), and it is the same duration the runtime enforces.
//  4. SlowRequestLogsOneWarn — a request that runs past 80% of its budget
//     emits exactly ONE structured WARN line naming the handler and the
//     slowest step, so an operator can see WHICH call is stalling before the
//     deadline trips.
//  5. ContextParentCancellation — the deadline is derived from the REQUEST
//     context (the anti-pattern the row forbids is a bare
//     context.WithTimeout(context.Background(), ...)).
//
// The stall seam is Server.readStepHook (nil in production): it fires with
// the step name as each instrumented step starts, which is exactly the
// "which of the 12 calls stalled" question the row asks.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// gap1575StalledStep is the status step the deadline tests stall. It is one of
// the three helpers the row names as the suspected wedge.
const gap1575StalledStep = "spendByCostSource"

// newGap1575Server builds a Server on a fresh temp-file SQLite database with a
// (non-running) loop, mirroring the daemon's construction closely enough that
// the real handlers run end to end.
func newGap1575Server(t *testing.T) *Server {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// budget=0 keeps Pick empty so no real spawn can be triggered.
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	return NewServer(db, loop)
}

// stallGap1575Step installs the readStepHook seam: the named step sleeps past
// the deadline, every other step runs normally. The sleep is deliberately only
// ~1.5x the budget: the point is that the DEADLINE bounds the response, not
// that the handler waits out a long stall (the SQLite driver returns as soon
// as the context it was handed is done).
func stallGap1575Step(s *Server, step string, stall time.Duration) {
	s.readStepHook = func(current string) {
		if current == step {
			time.Sleep(stall)
		}
	}
}

// TestSCHEDGAP1575B_StatusRespectsDeadline proves the status handler is
// bounded by its deadline and names the helper that blew it.
func TestSCHEDGAP1575B_StatusRespectsDeadline(t *testing.T) {
	srv := newGap1575Server(t)
	const budget = 200 * time.Millisecond
	const slack = 500 * time.Millisecond
	srv.SetReadTimeout(budget)
	stallGap1575Step(srv, gap1575StalledStep, 300*time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	started := time.Now()
	srv.status(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/api/v1/status with %s stalled past the deadline = %d, want 504 (or 503); body=%s",
			gap1575StalledStep, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed > budget+slack {
		t.Errorf("/api/v1/status returned after %s — the %s deadline did not bound the request (want <= %s)",
			elapsed, budget, budget+slack)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("deadline body is not a JSON object (%v): %s", err, rec.Body.String())
	}
	if body["error"] != "deadline exceeded" {
		t.Errorf(`body "error" = %q, want "deadline exceeded"`, body["error"])
	}
	if body["helper"] != gap1575StalledStep {
		t.Errorf(`body "helper" = %q, want %q — the response must name WHICH helper blew the deadline`,
			body["helper"], gap1575StalledStep)
	}
	if !strings.Contains(body["detail"], "deadline exceeded") {
		t.Errorf(`body "detail" = %q, want it to carry the context error (context deadline exceeded)`, body["detail"])
	}
	// The response must be the structured deadline body, never the fleet
	// overview with missing/zeroed blocks (a 200 with degraded data would
	// hide the stall from every operator surface).
	if strings.Contains(rec.Body.String(), "active_projects") {
		t.Error("the deadline response carried the status payload — it must be the timeout body only")
	}
}

// TestSCHEDGAP1575B_LivenessStillFast proves the deadline is not armed so
// tightly that a healthy status call trips it.
func TestSCHEDGAP1575B_LivenessStillFast(t *testing.T) {
	srv := newGap1575Server(t)
	// The documented default budget (5s): --api-read-timeout's default.
	srv.SetReadTimeout(5 * time.Second)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	started := time.Now()
	srv.Handler().ServeHTTP(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthy /api/v1/status = %d, want 200; body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed >= time.Second {
		t.Errorf("healthy /api/v1/status took %s — it must complete well under the 5s deadline", elapsed)
	}
	if strings.Contains(rec.Body.String(), "deadline exceeded") {
		t.Errorf("healthy /api/v1/status answered with the deadline body: %s", strings.TrimSpace(rec.Body.String()))
	}
	// The healthy payload still carries the fleet overview blocks.
	if !strings.Contains(rec.Body.String(), "active_projects") {
		t.Error("healthy /api/v1/status payload is missing active_projects — the response shape regressed")
	}
	if !strings.Contains(rec.Body.String(), "config_drift") {
		t.Error("healthy /api/v1/status payload is missing config_drift — the last step never ran")
	}
}

// TestSCHEDGAP1575B_ConfigExposesNewTimeout proves /api/v1/config publishes the
// ARMED deadline, and that it is the duration the handlers actually enforce.
func TestSCHEDGAP1575B_ConfigExposesNewTimeout(t *testing.T) {
	srv := newGap1575Server(t)
	srv.SetReadTimeout(2 * time.Second)

	rec := httptest.NewRecorder()
	srv.config(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/config = %d, want 200; body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/v1/config: %v", err)
	}
	got, ok := body["api_read_timeout"].(string)
	if !ok {
		t.Fatalf("api_read_timeout missing from /api/v1/config (or wrong type %T)", body["api_read_timeout"])
	}
	if got != "2s" {
		t.Errorf("api_read_timeout = %q, want %q", got, "2s")
	}
	// The published value and the enforced value are ONE number: the same
	// SetReadTimeout call writes both, so they cannot drift.
	if enforce := srv.readTimeout(); enforce != 2*time.Second {
		t.Errorf("readTimeout() = %s, want 2s — /api/v1/config would report a deadline the handlers do not enforce", enforce)
	}
	// An unset deadline still resolves to the documented default (5s), so the
	// field is never blank on a daemon that never received the flag.
	fresh := newGap1575Server(t)
	if fresh.readTimeout() != readTimeoutDefault {
		t.Errorf("unset readTimeout() = %s, want the default %s", fresh.readTimeout(), readTimeoutDefault)
	}
	// The /api/v1/config surface must report the DEFAULT too when nothing was
	// armed, otherwise an operator cannot read the effective deadline off a
	// stock daemon. main.go writes the resolved value unconditionally; this
	// pins that the JSON field exists on that path.
	fresh.SetReadTimeout(readTimeoutDefault)
	rec = httptest.NewRecorder()
	fresh.config(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if !strings.Contains(rec.Body.String(), `"api_read_timeout":"5s"`) {
		t.Errorf("default /api/v1/config is missing api_read_timeout=5s: %s", strings.TrimSpace(rec.Body.String()))
	}
}

// TestSCHEDGAP1575B_SlowRequestLogsOneWarn proves the slow-request log: one
// structured WARN line naming the handler and the slowest step.
func TestSCHEDGAP1575B_SlowRequestLogsOneWarn(t *testing.T) {
	srv := newGap1575Server(t)
	srv.SetReadTimeout(200 * time.Millisecond)
	stallGap1575Step(srv, gap1575StalledStep, 170*time.Millisecond)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	rec := httptest.NewRecorder()
	srv.status(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	out := buf.String()
	if n := strings.Count(out, "WARN: slow request"); n != 1 {
		t.Fatalf("want exactly 1 slow-request WARN line, got %d:\n%s", n, out)
	}
	line := out[strings.Index(out, "WARN: slow request"):]
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	for _, want := range []string{
		"handler=status",
		"slowest_step=" + gap1575StalledStep,
		"budget=200ms",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("slow-request WARN line missing %q:\n%s", want, line)
		}
	}
	// The log must never carry the SQL text or the request body — these logs
	// are publicly replayable. The helper NAME is the whole point.
	for _, forbidden := range []string{"SELECT", "FROM ticks"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("slow-request WARN line leaked query text (%q):\n%s", forbidden, line)
		}
	}
}

// TestSCHEDGAP1575B_OtherReadSurfacesBounded proves the same treatment reached
// the other heavy read handlers the row names (projects/namespaces/ticks): a
// stalled step answers 504 naming it instead of hanging.
func TestSCHEDGAP1575B_OtherReadSurfacesBounded(t *testing.T) {
	cases := []struct {
		name string
		step string
		call func(s *Server, w http.ResponseWriter, r *http.Request)
	}{
		{
			// SCHED-GAP-1622: the projects read is paginated; the stalled
			// step is the page query (the deadline must still bound the
			// handler and name the step).
			name: "projects", step: "ListProjectsPage",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.listProjects(w, r) },
		},
		{
			name: "namespaces", step: "ListNamespaces",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.listNamespaces(w, r) },
		},
		{
			name: "ticks", step: "listTicks",
			call: func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleTicks(w, r) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newGap1575Server(t)
			const budget = 200 * time.Millisecond
			srv.SetReadTimeout(budget)
			stallGap1575Step(srv, tc.step, 300*time.Millisecond)

			rec := httptest.NewRecorder()
			started := time.Now()
			tc.call(srv, rec, httptest.NewRequest(http.MethodGet, "/api/v1/"+tc.name, nil))
			elapsed := time.Since(started)

			if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("/api/v1/%s with %s stalled = %d, want 504; body=%s",
					tc.name, tc.step, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if elapsed > budget+500*time.Millisecond {
				t.Errorf("/api/v1/%s returned after %s — the deadline did not bound it", tc.name, elapsed)
			}
			if !strings.Contains(rec.Body.String(), `"helper":"`+tc.step+`"`) {
				t.Errorf("/api/v1/%s deadline body does not name %s: %s", tc.name, tc.step, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// TestSCHEDGAP1575B_ContextParentCancellation pins the anti-pattern the row
// forbids: the deadline must be derived from the REQUEST context, never a bare
// context.Background(). A cancelled request context therefore trips the same
// path — no work continues after the client is gone.
func TestSCHEDGAP1575B_ContextParentCancellation(t *testing.T) {
	srv := newGap1575Server(t)
	srv.SetReadTimeout(30 * time.Second) // long: the PARENT is what trips

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	srv.status(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("cancelled request context: /api/v1/status = %d, want 504 (the deadline body); body=%s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(rec.Body.String(), `"error":"deadline exceeded"`) {
		t.Errorf("cancelled request context body = %s", strings.TrimSpace(rec.Body.String()))
	}
}

// TestSCHEDGAP1575B_ObserverTimeoutBodyShape pins writeTimeoutError's wire
// shape directly (it is the contract the 504 bodies above rely on).
func TestSCHEDGAP1575B_ObserverTimeoutBodyShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTimeoutError(rec, gap1575StalledStep, context.DeadlineExceeded)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("writeTimeoutError code = %d, want 504", rec.Code)
	}
	want := `{"error":"deadline exceeded","helper":"spendByCostSource","detail":"context deadline exceeded"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("writeTimeoutError body = %s, want %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// A nil error still produces a well-formed body (detail defaults to the
	// context error text) instead of panicking on err.Error().
	rec = httptest.NewRecorder()
	writeTimeoutError(rec, "configDriftBlock", nil)
	if !strings.Contains(rec.Body.String(), `"helper":"configDriftBlock"`) {
		t.Errorf("nil-err body = %s", strings.TrimSpace(rec.Body.String()))
	}
}
