package database

import (
	"context"
	"database/sql"
	"testing"
)

// featureUsageIsolated resets the package-level in-memory counters before and
// after a test so suites that share one process never leak a prior test's
// counts into the next.
func featureUsageIsolated(t *testing.T) {
	t.Helper()
	ResetFeatureUsage()
	t.Cleanup(ResetFeatureUsage)
}

// featureUsageCounts flushes pending counters into db and returns the persisted
// use_count keyed by feature, so a test can assert a real end-to-end count.
func featureUsageCounts(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	if err := FlushFeatureUsage(context.Background(), db); err != nil {
		t.Fatalf("flush feature usage: %v", err)
	}
	rows, err := LoadFeatureUsage(context.Background(), db)
	if err != nil {
		t.Fatalf("load feature usage: %v", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Feature] = r.UseCount
	}
	return out
}

func TestRecordFeatureUseAndFlush(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	RecordFeatureUse(FeatureBumpArming)
	RecordFeatureUse(FeatureBumpArming)
	RecordFeatureUse(FeatureWaveTicks)

	counts := featureUsageCounts(t, db)
	if counts[FeatureBumpArming] != 2 {
		t.Errorf("bump_arming count = %d, want 2 (%v)", counts[FeatureBumpArming], counts)
	}
	if counts[FeatureWaveTicks] != 1 {
		t.Errorf("wave_ticks count = %d, want 1 (%v)", counts[FeatureWaveTicks], counts)
	}
	if counts[FeatureAdmissionMode] != 0 {
		t.Errorf("admission_mode count = %d, want 0 (never recorded)", counts[FeatureAdmissionMode])
	}

	// First/last stamps must be set and ordered; RFC3339 UTC is fixed-width so
	// lexical order == chronological order.
	rows, err := LoadFeatureUsage(context.Background(), db)
	if err != nil {
		t.Fatalf("load feature usage: %v", err)
	}
	for _, r := range rows {
		if r.Feature != FeatureBumpArming {
			continue
		}
		if r.FirstUsedAt == "" || r.LastUsedAt == "" {
			t.Fatalf("bump_arming timestamps empty: first=%q last=%q", r.FirstUsedAt, r.LastUsedAt)
		}
		if r.FirstUsedAt > r.LastUsedAt {
			t.Errorf("bump_arming first_used_at %q > last_used_at %q", r.FirstUsedAt, r.LastUsedAt)
		}
	}

	// A second flush after more uses ACCUMULATES — the in-memory delta is
	// cleared on flush, so the next flush adds only the new counts.
	RecordFeatureUse(FeatureBumpArming)
	counts = featureUsageCounts(t, db)
	if counts[FeatureBumpArming] != 3 {
		t.Errorf("bump_arming count after second flush = %d, want 3", counts[FeatureBumpArming])
	}
}

func TestLoadFeatureUsage_FullVocabulary(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	rows, err := LoadFeatureUsage(context.Background(), db)
	if err != nil {
		t.Fatalf("load feature usage: %v", err)
	}
	if len(rows) != len(FeatureDefinitions) {
		t.Fatalf("LoadFeatureUsage returned %d rows, want the full %d-feature vocabulary", len(rows), len(FeatureDefinitions))
	}
	for i, def := range FeatureDefinitions {
		r := rows[i]
		if r.Feature != def.Name {
			t.Fatalf("row %d feature = %q, want %q (canonical order)", i, r.Feature, def.Name)
		}
		if r.UseCount != 0 {
			t.Errorf("row %q use_count = %d, want 0 on a fresh DB", r.Feature, r.UseCount)
		}
		if r.FirstUsedAt != "" || r.LastUsedAt != "" {
			t.Errorf("row %q timestamps = (%q, %q), want empty on a fresh DB", r.Feature, r.FirstUsedAt, r.LastUsedAt)
		}
	}
}

func TestFeaturePruneCandidates(t *testing.T) {
	rows := []FeatureUsage{
		{Feature: "never", UseCount: 0, LastUsedAt: ""},
		{Feature: "stale", UseCount: 5, LastUsedAt: "2026-08-01T00:00:00Z"},
		{Feature: "recent", UseCount: 3, LastUsedAt: "2026-09-28T00:00:00Z"},
	}
	candidates := FeaturePruneCandidates(rows, "2026-09-01T00:00:00Z")

	got := map[string]bool{}
	for _, c := range candidates {
		got[c.Feature] = true
	}
	if len(got) != 2 || !got["never"] || !got["stale"] {
		t.Fatalf("prune candidates = %v, want exactly {never, stale}", got)
	}
	if got["recent"] {
		t.Errorf("recent (used after cutoff) must not be a prune candidate")
	}
}

func TestAdmissionModeRowCounts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateNamespace(ctx, db, &Namespace{
		ID: "tasks-ns", Weight: 10, MaxConcurrent: 0, Enabled: true, AdmissionMode: AdmissionModeTasks,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := CreateNamespace(ctx, db, &Namespace{
		ID: "cooldown-ns", Weight: 10, MaxConcurrent: 0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := CreateProject(ctx, db, &Project{
		Name: "tasks-p", RepoURL: "https://example.com/tasks-p", Workdir: "/tmp/tasks-p",
		Weight: 10, Priority: 5, CooldownS: 60, DecayRate: 1.0, Model: "m", Provider: "p",
		Enabled: true, AdmissionMode: AdmissionModeTasks,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := CreateProject(ctx, db, &Project{
		Name: "cooldown-p", RepoURL: "https://example.com/cooldown-p", Workdir: "/tmp/cooldown-p",
		Weight: 10, Priority: 5, CooldownS: 60, DecayRate: 1.0, Model: "m", Provider: "p",
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	projectsTasks, namespacesTasks, err := AdmissionModeRowCounts(ctx, db)
	if err != nil {
		t.Fatalf("AdmissionModeRowCounts: %v", err)
	}
	if projectsTasks != 1 {
		t.Errorf("projects_tasks = %d, want 1", projectsTasks)
	}
	if namespacesTasks != 1 {
		t.Errorf("namespaces_tasks = %d, want 1", namespacesTasks)
	}
}

// TestBumpProjectRecordsFeatureUse proves the bump-arming fire point: a
// successful BumpProject arms a bump AND records the feature use, while a
// second bump on the same project (already active) does not double-count.
func TestBumpProjectRecordsFeatureUse(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateProject(ctx, db, &Project{
		Name: "bump-p", RepoURL: "https://example.com/bump-p", Workdir: "/tmp/bump-p",
		Weight: 10, Priority: 5, CooldownS: 3600, DecayRate: 1.0, Model: "m", Provider: "p",
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := BumpProject(ctx, db, "bump-p", 3, 900, "test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// A second bump while one is active must fail and must NOT record.
	if _, err := BumpProject(ctx, db, "bump-p", 3, 900, "test"); err == nil {
		t.Fatal("second BumpProject on an active bump should error")
	}

	counts := featureUsageCounts(t, db)
	if counts[FeatureBumpArming] != 1 {
		t.Errorf("bump_arming count = %d, want 1 (%v)", counts[FeatureBumpArming], counts)
	}
}
