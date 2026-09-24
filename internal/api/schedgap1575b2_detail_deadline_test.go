package api

// SCHED-GAP-1575-B2 acceptance tests.
//
// The row: the parent commit fb100da7 instrumented the LIST surfaces
// (/api/v1/status, /api/v1/projects, /api/v1/namespaces, /api/v1/ticks)
// under the new --api-read-timeout / api_read_timeout deadline, but the
// DETAIL handlers — /api/v1/projects/{name} (getProject) and
// /api/v1/namespaces/{name} (getNamespace) — still used a bare
// context.Background(). They share the single serialized SQLite
// connection (SetMaxOpenConns(1)) with the list surfaces, so a stalled
// database.GetProject / getLatestTick / database.GetNamespace would
// hang the dashboard detail page the same way /status hung.
//
// What these tests pin (each asserts the MECHANISM, not just a code):
//
//  1. ProjectDetailRespectsDeadline — with a NAMED detail step stalled
//     past the deadline, /api/v1/projects/{name} returns 504 INSIDE the
//     budget (not after the stall), the body names the step that blew
//     it, and the response is not a 200 with degraded data.
//  2. NamespaceDetailRespectsDeadline — same assertion for
//     /api/v1/namespaces/{name}.
//  3. ProjectDetailHealthy — the healthy path still answers 200 well
//     under the deadline and never emits the deadline body (the fix
//     must not turn a healthy detail-page call into a 504).
//  4. NamespaceDetailHealthy — same for namespaces.
//  5. DetailContextParentCancellation — the deadline is derived from
//     the REQUEST context (the anti-pattern the row forbids is a bare
//     context.WithTimeout(context.Background(), ...)). A cancelled
//     request context therefore trips the same path — no work continues
//     after the client is gone.
//
// The stall seam is Server.readStepHook (nil in production): it fires
// with the step name as each instrumented step starts, which is exactly
// the "which of the calls stalled" question the row asks.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// mustCreateGap1575B2Project inserts a minimal enabled project for the
// detail-deadline tests. Mirrors mustCreateAPITestProject in
// server_test.go (which lives in the api_test package and is therefore
// not importable from package api).
func mustCreateGap1575B2Project(t *testing.T, srv *Server, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), srv.db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// mustCreateGap1575B2Namespace inserts a minimal enabled namespace for
// the detail-deadline tests. Mirrors createTestNamespace in
// namespace_handlers_test.go (which lives in the api_test package and
// is therefore not importable from package api).
func mustCreateGap1575B2Namespace(t *testing.T, srv *Server, id string) {
	t.Helper()
	ns := &database.Namespace{
		ID:       id,
		Weight:   20,
		Reserved: 5,
		HardCap:  30,
		Enabled:  true,
	}
	if err := database.CreateNamespace(context.Background(), srv.db, ns); err != nil {
		t.Fatalf("CreateNamespace %s: %v", id, err)
	}
}

// TestSCHEDGAP1575B2_ProjectDetailRespectsDeadline proves the project
// detail handler is bounded by its deadline and names the helper that
// blew it.
func TestSCHEDGAP1575B2_ProjectDetailRespectsDeadline(t *testing.T) {
	srv := newGap1575Server(t)
	mustCreateGap1575B2Project(t, srv, "alpha")
	const budget = 200 * time.Millisecond
	const slack = 500 * time.Millisecond
	srv.SetReadTimeout(budget)
	stallGap1575Step(srv, "GetProject", 300*time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/alpha", nil)
	started := time.Now()
	srv.handleProjectByID(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/api/v1/projects/alpha with GetProject stalled past the deadline = %d, want 504; body=%s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed > budget+slack {
		t.Errorf("/api/v1/projects/alpha returned after %s — the %s deadline did not bound the request (want <= %s)",
			elapsed, budget, budget+slack)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("deadline body is not a JSON object (%v): %s", err, rec.Body.String())
	}
	if body["error"] != "deadline exceeded" {
		t.Errorf(`body "error" = %q, want "deadline exceeded"`, body["error"])
	}
	if body["helper"] != "GetProject" {
		t.Errorf(`body "helper" = %q, want "GetProject" — the response must name WHICH helper blew the deadline`,
			body["helper"])
	}
	if !strings.Contains(body["detail"], "deadline exceeded") {
		t.Errorf(`body "detail" = %q, want it to carry the context error (context deadline exceeded)`, body["detail"])
	}
	// The response must be the structured deadline body, never the project
	// payload with missing/zeroed fields (a 200 with degraded data would
	// hide the stall from every operator surface).
	if strings.Contains(rec.Body.String(), `"project"`) {
		t.Error("the deadline response carried the project payload — it must be the timeout body only")
	}
}

// TestSCHEDGAP1575B2_NamespaceDetailRespectsDeadline proves the namespace
// detail handler is bounded by its deadline and names the helper that
// blew it.
func TestSCHEDGAP1575B2_NamespaceDetailRespectsDeadline(t *testing.T) {
	srv := newGap1575Server(t)
	mustCreateGap1575B2Namespace(t, srv, "beta")
	const budget = 200 * time.Millisecond
	const slack = 500 * time.Millisecond
	srv.SetReadTimeout(budget)
	stallGap1575Step(srv, "GetNamespace", 300*time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/beta", nil)
	started := time.Now()
	srv.handleNamespaceByID(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/api/v1/namespaces/beta with GetNamespace stalled past the deadline = %d, want 504; body=%s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed > budget+slack {
		t.Errorf("/api/v1/namespaces/beta returned after %s — the %s deadline did not bound the request (want <= %s)",
			elapsed, budget, budget+slack)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("deadline body is not a JSON object (%v): %s", err, rec.Body.String())
	}
	if body["error"] != "deadline exceeded" {
		t.Errorf(`body "error" = %q, want "deadline exceeded"`, body["error"])
	}
	if body["helper"] != "GetNamespace" {
		t.Errorf(`body "helper" = %q, want "GetNamespace" — the response must name WHICH helper blew the deadline`,
			body["helper"])
	}
	if !strings.Contains(body["detail"], "deadline exceeded") {
		t.Errorf(`body "detail" = %q, want it to carry the context error (context deadline exceeded)`, body["detail"])
	}
}

// TestSCHEDGAP1575B2_ProjectDetailHealthy proves the deadline is not
// armed so tightly that a healthy detail-page call trips it.
func TestSCHEDGAP1575B2_ProjectDetailHealthy(t *testing.T) {
	srv := newGap1575Server(t)
	mustCreateGap1575B2Project(t, srv, "alpha")
	// The documented default budget (5s): --api-read-timeout's default.
	srv.SetReadTimeout(5 * time.Second)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/alpha", nil)
	started := time.Now()
	srv.handleProjectByID(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthy /api/v1/projects/alpha = %d, want 200; body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed >= time.Second {
		t.Errorf("healthy /api/v1/projects/alpha took %s — it must complete well under the 5s deadline", elapsed)
	}
	if strings.Contains(rec.Body.String(), "deadline exceeded") {
		t.Errorf("healthy /api/v1/projects/alpha answered with the deadline body: %s", strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(rec.Body.String(), `"project"`) {
		t.Error("healthy /api/v1/projects/alpha payload is missing the project block — the response shape regressed")
	}
}

// TestSCHEDGAP1575B2_NamespaceDetailHealthy proves the deadline is not
// armed so tightly that a healthy detail-page call trips it.
func TestSCHEDGAP1575B2_NamespaceDetailHealthy(t *testing.T) {
	srv := newGap1575Server(t)
	mustCreateGap1575B2Namespace(t, srv, "beta")
	srv.SetReadTimeout(5 * time.Second)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/beta", nil)
	started := time.Now()
	srv.handleNamespaceByID(rec, req)
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthy /api/v1/namespaces/beta = %d, want 200; body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if elapsed >= time.Second {
		t.Errorf("healthy /api/v1/namespaces/beta took %s — it must complete well under the 5s deadline", elapsed)
	}
	if strings.Contains(rec.Body.String(), "deadline exceeded") {
		t.Errorf("healthy /api/v1/namespaces/beta answered with the deadline body: %s", strings.TrimSpace(rec.Body.String()))
	}
	// The namespace response is the bare namespace struct.
	if !strings.Contains(rec.Body.String(), `"id":"beta"`) {
		t.Errorf("healthy /api/v1/namespaces/beta payload is missing the id field: %s", strings.TrimSpace(rec.Body.String()))
	}
}

// TestSCHEDGAP1575B2_DetailContextParentCancellation pins the anti-pattern
// the row forbids: the deadline must be derived from the REQUEST context,
// never a bare context.WithTimeout(context.Background(), ...). A cancelled
// request context therefore trips the same path — no work continues after
// the client is gone.
func TestSCHEDGAP1575B2_DetailContextParentCancellation(t *testing.T) {
	srv := newGap1575Server(t)
	mustCreateGap1575B2Project(t, srv, "alpha")
	srv.SetReadTimeout(30 * time.Second) // long: the PARENT is what trips

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/alpha", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	srv.handleProjectByID(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("cancelled request context: /api/v1/projects/alpha = %d, want 504 (the deadline body); body=%s",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(rec.Body.String(), `"error":"deadline exceeded"`) {
		t.Errorf("cancelled request context body = %s", strings.TrimSpace(rec.Body.String()))
	}
}
