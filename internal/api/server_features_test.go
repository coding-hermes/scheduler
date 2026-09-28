package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// featureTestDB opens an in-memory migrated database. The api package's
// internal tests use InitDB directly (the GAP-060 template helper lives in the
// external api_test package), matching the metrics test's convention.
func featureTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// featureGet issues a GET against the server's mux and decodes the JSON body.
func featureGet(t *testing.T, s *Server, path string) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rr.Code, rr.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal GET %s: %v", path, err)
	}
	return body
}

// featureIsolated resets the package-level in-memory feature counters so one
// test's records never leak into the next.
func featureIsolated(t *testing.T) {
	t.Helper()
	database.ResetFeatureUsage()
	t.Cleanup(database.ResetFeatureUsage)
}

// TestFeaturesEndpoint asserts the /api/v1/features shape: all five mechanisms
// present, recorded counts landed, and the admission_mode row gauge populated.
func TestFeaturesEndpoint(t *testing.T) {
	featureIsolated(t)
	db := featureTestDB(t)
	ctx := context.Background()

	// Seed two mechanisms' counters through the real fire path.
	database.RecordFeatureUse(database.FeatureBumpArming)
	database.RecordFeatureUse(database.FeatureBumpArming)
	database.RecordFeatureUse(database.FeatureWaveTicks)
	if err := database.FlushFeatureUsage(ctx, db); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// One tasks-mode project and namespace for the admission_mode row gauge.
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "tasks-ns", Weight: 10, MaxConcurrent: 0, Enabled: true, AdmissionMode: database.AdmissionModeTasks,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "tasks-p", RepoURL: "https://example.com/tasks-p", Workdir: "/tmp/tasks-p",
		Weight: 10, Priority: 5, CooldownS: 60, DecayRate: 1.0, Model: "m", Provider: "p",
		Enabled: true, AdmissionMode: database.AdmissionModeTasks,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	s := NewServer(db, nil)
	body := featureGet(t, s, "/api/v1/features")

	features, ok := body["features"].([]interface{})
	if !ok {
		t.Fatalf("features is %T, want array", body["features"])
	}
	if len(features) != len(database.FeatureDefinitions) {
		t.Fatalf("features has %d entries, want %d", len(features), len(database.FeatureDefinitions))
	}

	byName := map[string]map[string]interface{}{}
	for _, raw := range features {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("feature entry is %T, want object", raw)
		}
		name, _ := entry["feature"].(string)
		byName[name] = entry
	}

	bump := byName["bump_arming"]
	if bump == nil {
		t.Fatal("bump_arming missing from features")
	}
	if got := num(bump, "use_count"); got != 2 {
		t.Errorf("bump_arming use_count = %v, want 2", bump["use_count"])
	}
	if bump["description"] == "" {
		t.Error("bump_arming has no description")
	}
	if bump["first_used_at"] == "" || bump["last_used_at"] == "" {
		t.Errorf("bump_arming timestamps not set: first=%v last=%v", bump["first_used_at"], bump["last_used_at"])
	}

	if got := num(byName["wave_ticks"], "use_count"); got != 1 {
		t.Errorf("wave_ticks use_count = %v, want 1", byName["wave_ticks"]["use_count"])
	}
	if got := num(byName["dedupe_suppressions"], "use_count"); got != 0 {
		t.Errorf("dedupe_suppressions use_count = %v, want 0 (never recorded)", byName["dedupe_suppressions"]["use_count"])
	}

	rows, ok := body["admission_mode_rows"].(map[string]interface{})
	if !ok {
		t.Fatalf("admission_mode_rows is %T, want object", body["admission_mode_rows"])
	}
	if num(rows, "projects_tasks") != 1 || num(rows, "namespaces_tasks") != 1 {
		t.Errorf("admission_mode_rows = %v, want {projects_tasks:1, namespaces_tasks:1}", rows)
	}
	if num(body, "prune_weeks") != featurePruneWeeksDefault {
		t.Errorf("prune_weeks = %v, want %d", body["prune_weeks"], featurePruneWeeksDefault)
	}
}

// TestFeaturesPruneCandidates asserts the reaper report: never-used and stale
// features are flagged, a recently-used feature is not, and ?weeks= overrides
// the window.
func TestFeaturesPruneCandidates(t *testing.T) {
	featureIsolated(t)
	db := featureTestDB(t)
	ctx := context.Background()

	// bump_arming is used THIS process (recent) → not a candidate.
	database.RecordFeatureUse(database.FeatureBumpArming)
	if err := database.FlushFeatureUsage(ctx, db); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// admission_mode was last used 70 days ago — clearly older than the 8-week
	// (56-day) default window, and older than any ?weeks= override below.
	staleAt := time.Now().UTC().AddDate(0, 0, -70).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO feature_usage (feature, use_count, first_used_at, last_used_at)
		VALUES ('admission_mode', 3, ?, ?)`, staleAt, staleAt); err != nil {
		t.Fatalf("seed stale feature: %v", err)
	}

	s := NewServer(db, nil)
	body := featureGet(t, s, "/api/v1/features/prune-candidates")

	if body["cutoff"] == "" {
		t.Error("prune-candidates response has no cutoff")
	}
	candidates, ok := body["candidates"].([]interface{})
	if !ok {
		t.Fatalf("candidates is %T, want array (never null)", body["candidates"])
	}

	names := map[string]bool{}
	for _, raw := range candidates {
		entry := raw.(map[string]interface{})
		names[entry["feature"].(string)] = true
	}
	if !names["admission_mode"] {
		t.Errorf("stale feature admission_mode missing from candidates: %v", names)
	}
	if names["bump_arming"] {
		t.Errorf("recently-used bump_arming must not be a candidate: %v", names)
	}
	if !names["wave_ticks"] || !names["load_gate_deferrals"] || !names["dedupe_suppressions"] {
		t.Errorf("never-used features must all be candidates: %v", names)
	}

	// The ?weeks= override is honored (an invalid value falls back to default).
	body = featureGet(t, s, "/api/v1/features/prune-candidates?weeks=1")
	if got := num(body, "prune_weeks"); got != 1 {
		t.Errorf("prune_weeks = %v, want 1 (query override)", body["prune_weeks"])
	}
	body = featureGet(t, s, "/api/v1/features/prune-candidates?weeks=bogus")
	if got := num(body, "prune_weeks"); got != featurePruneWeeksDefault {
		t.Errorf("prune_weeks = %v, want %d (bogus falls back to default)", body["prune_weeks"], featurePruneWeeksDefault)
	}
}

// num reads a numeric JSON value (float64) as an int, failing on absence.
func num(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return -1
}
