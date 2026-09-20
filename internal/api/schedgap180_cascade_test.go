package api

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-180: pause/resume cascade to satellite lanes. The cascade
// marker/reason constants (cascadeMarker, cascadeReasonPrefix) live in
// server_projects.go next to the helpers that stamp them; this test pins
// them through the DB surface, not by re-declaring them.

// cascadeTestProjects is the lane family the brief mandates:
// <name>, <name>-qa, <name>-pm, <name>-sync, <name>-dogfood.
var cascadeTestProjects = []string{"", "-qa", "-pm", "-sync", "-dogfood"}

// mustCreateCascadeLane creates one lane of the family as enabled. Separate
// workdirs per lane (the helper's /tmp/<name> default is fine — every lane has
// a distinct name, hence a distinct workdir, so no uniqueness guard trips).
func mustCreateCascadeLane(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) error
}, name string) {
	t.Helper()
	_ = db
	_ = name
}

// TestSCHEDGAP180_PauseCascadesToSatelliteLanes proves pausing a primary
// project through POST /projects/{name}/pause also disables its satellite
// lanes (name-suffix family) with cascade provenance.
func TestSCHEDGAP180_PauseCascadesToSatelliteLanes(t *testing.T) {
	// The regen exec must never shell out to the real policy script from a
	// unit test (it would rewrite the live ~/.hermes/fleet.toml). Same
	// injection shape schedgap137b_test.go uses.
	prev := regenFleetTomlExec
	t.Cleanup(func() { regenFleetTomlExec = prev })
	regenFleetTomlExec = func() error { return nil }

	const primary = "gap180"
	db := mustOpenGap180DB(t)
	s := NewServer(db, nil)

	// Precondition: all 5 lanes enabled.
	for _, suffix := range cascadeTestProjects {
		mustCreateHelperTestProject(t, db, primary+suffix)
	}

	req := httptest.NewRequest("POST", "/api/v1/projects/gap180/pause", nil)
	rec := httptest.NewRecorder()
	s.pauseProject(rec, req, primary)
	if rec.Code != 200 {
		t.Fatalf("pause status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Primary: disabled with the plain pause provenance.
	p, err := database.GetProject(context.Background(), db, primary)
	if err != nil {
		t.Fatalf("GetProject primary: %v", err)
	}
	if p.Enabled {
		t.Error("primary still enabled after pause")
	}
	if p.DisabledBy != "api-pause" {
		t.Errorf("primary disabled_by = %q, want %q", p.DisabledBy, "api-pause")
	}

	// Satellites: all 4 disabled with cascade provenance naming the primary.
	for _, suffix := range cascadeTestProjects[1:] {
		sat := primary + suffix
		sp, err := database.GetProject(context.Background(), db, sat)
		if err != nil {
			t.Fatalf("GetProject satellite %s: %v", sat, err)
		}
		if sp.Enabled {
			t.Errorf("SCHED-GAP-180 FAILED: satellite %s still enabled after primary pause — pause does not cascade to satellite lanes", sat)
		}
		if sp.DisabledBy != cascadeMarker {
			t.Errorf("satellite %s disabled_by = %q, want %q (cascade marker)", sat, sp.DisabledBy, cascadeMarker)
		}
		if !strings.HasPrefix(sp.DisabledReason, cascadeReasonPrefix+primary) {
			t.Errorf("satellite %s disabled_reason = %q, want prefix %q", sat, sp.DisabledReason, cascadeReasonPrefix+primary)
		}
	}
}

// TestSCHEDGAP180_ResumeRestoresOnlyCascadePausedSatellites proves resume
// re-enables exactly the cascade-paused satellites (disabled_by=api-pause-cascade
// AND disabled_reason prefix "paused by target <primary>") and leaves a
// satellite disabled for its own reason (PUT enabled=false, disabled_by=api)
// untouched.
func TestSCHEDGAP180_ResumeRestoresOnlyCascadePausedSatellites(t *testing.T) {
	prev := regenFleetTomlExec
	t.Cleanup(func() { regenFleetTomlExec = prev })
	regenFleetTomlExec = func() error { return nil }

	const primary = "gap180r"
	db := mustOpenGap180DB(t)
	s := NewServer(db, nil)

	for _, suffix := range cascadeTestProjects {
		mustCreateHelperTestProject(t, db, primary+suffix)
	}
	// A 5th satellite disabled for its own reason via the PUT surface
	// (disabled_by=api — independent pause, never cascade-owned).
	const independent = primary + "-independent"
	mustCreateHelperTestProject(t, db, independent)
	putBody := strings.NewReader(`{"enabled": false}`)
	putReq := httptest.NewRequest("PUT", "/api/v1/projects/"+independent, putBody)
	putRec := httptest.NewRecorder()
	s.handleProjectByID(putRec, putReq)
	if putRec.Code != 200 {
		t.Fatalf("PUT disable of independent satellite status = %d, want 200 (body: %s)", putRec.Code, putRec.Body.String())
	}

	// Pause the primary (cascades to the 4 suffix satellites).
	pauseReq := httptest.NewRequest("POST", "/api/v1/projects/"+primary+"/pause", nil)
	pauseRec := httptest.NewRecorder()
	s.pauseProject(pauseRec, pauseReq, primary)
	if pauseRec.Code != 200 {
		t.Fatalf("pause status = %d, want 200 (body: %s)", pauseRec.Code, pauseRec.Body.String())
	}

	// Precondition: the independent satellite is disabled with plain "api"
	// provenance (NOT the cascade marker) before resume runs.
	indep, err := database.GetProject(context.Background(), db, independent)
	if err != nil {
		t.Fatalf("GetProject independent: %v", err)
	}
	if indep.Enabled || indep.DisabledBy != "api" {
		t.Fatalf("precondition failed: independent satellite enabled=%v disabled_by=%q, want disabled with disabled_by=%q", indep.Enabled, indep.DisabledBy, "api")
	}

	// Resume the primary.
	resumeReq := httptest.NewRequest("POST", "/api/v1/projects/"+primary+"/resume", nil)
	resumeRec := httptest.NewRecorder()
	s.resumeProject(resumeRec, resumeReq, primary)
	if resumeRec.Code != 200 {
		t.Fatalf("resume status = %d, want 200 (body: %s)", resumeRec.Code, resumeRec.Body.String())
	}

	// Primary re-enabled.
	p, err := database.GetProject(context.Background(), db, primary)
	if err != nil {
		t.Fatalf("GetProject primary after resume: %v", err)
	}
	if !p.Enabled {
		t.Error("primary still disabled after resume")
	}

	// Exactly the 4 cascade-paused satellites back to enabled, provenance cleared.
	for _, suffix := range cascadeTestProjects[1:] {
		sat := primary + suffix
		sp, err := database.GetProject(context.Background(), db, sat)
		if err != nil {
			t.Fatalf("GetProject satellite %s: %v", sat, err)
		}
		if !sp.Enabled {
			t.Errorf("SCHED-GAP-180 FAILED: cascade-paused satellite %s still disabled after primary resume — resume does not restore cascade lanes", sat)
		}
		if sp.DisabledBy != "" || sp.DisabledReason != "" {
			t.Errorf("satellite %s provenance not cleared after resume: disabled_by=%q disabled_reason=%q", sat, sp.DisabledBy, sp.DisabledReason)
		}
	}

	// The independently-paused satellite stays disabled.
	indep, err = database.GetProject(context.Background(), db, independent)
	if err != nil {
		t.Fatalf("GetProject independent after resume: %v", err)
	}
	if indep.Enabled {
		t.Errorf("independent satellite %s was re-enabled by resume — resume must only touch disabled_by=%s rows", independent, cascadeMarker)
	}
	if indep.DisabledBy != "api" {
		t.Errorf("independent satellite disabled_by = %q, want %q (untouched)", indep.DisabledBy, "api")
	}
}

// TestSCHEDGAP180_PUTCascade mirrors the POST pause cascade through the PUT
// surface (enabled=true→false): both entry points must cascade.
func TestSCHEDGAP180_PUTCascade(t *testing.T) {
	prev := regenFleetTomlExec
	t.Cleanup(func() { regenFleetTomlExec = prev })
	regenFleetTomlExec = func() error { return nil }

	const primary = "gap180put"
	db := mustOpenGap180DB(t)
	s := NewServer(db, nil)

	for _, suffix := range cascadeTestProjects {
		mustCreateHelperTestProject(t, db, primary+suffix)
	}

	putBody := strings.NewReader(`{"enabled": false}`)
	putReq := httptest.NewRequest("PUT", "/api/v1/projects/"+primary, putBody)
	putRec := httptest.NewRecorder()
	s.handleProjectByID(putRec, putReq)
	if putRec.Code != 200 {
		t.Fatalf("PUT disable status = %d, want 200 (body: %s)", putRec.Code, putRec.Body.String())
	}

	p, err := database.GetProject(context.Background(), db, primary)
	if err != nil {
		t.Fatalf("GetProject primary: %v", err)
	}
	if p.Enabled {
		t.Error("primary still enabled after PUT disable")
	}
	for _, suffix := range cascadeTestProjects[1:] {
		sat := primary + suffix
		sp, err := database.GetProject(context.Background(), db, sat)
		if err != nil {
			t.Fatalf("GetProject satellite %s: %v", sat, err)
		}
		if sp.Enabled {
			t.Errorf("SCHED-GAP-180 FAILED: satellite %s still enabled after PUT disable of primary — PUT path does not cascade", sat)
		}
		if sp.DisabledBy != cascadeMarker {
			t.Errorf("satellite %s disabled_by = %q, want %q (cascade marker)", sat, sp.DisabledBy, cascadeMarker)
		}
	}
}

// mustOpenGap180DB opens a fresh in-memory DB with the full schema.
func mustOpenGap180DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
