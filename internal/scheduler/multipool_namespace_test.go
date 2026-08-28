package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// TestMultiPoolPacker_NamespaceMaxConcurrent verifies the per-namespace
// concurrency cap in the LIVE namespace-mode path (Bane 2026-08-27): a
// namespace with max_concurrent=1 packs at most one project per cycle, and
// zero when one is already running — while an uncapped namespace packs freely.
func TestMultiPoolPacker_NamespaceMaxConcurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	syncNs := database.Namespace{
		ID: "sync", Weight: 10, Reserved: 1, HardCap: 100, MaxConcurrent: 1,
		Enabled: true, DefaultPrompt: "SYNC-PROMPT", ModelChain: `["ns-m@ns-p"]`,
	}
	foremanNs := database.Namespace{
		ID: "foreman", Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
	}
	mustCreateNamespace(t, db, &syncNs)
	mustCreateNamespace(t, db, &foremanNs)

	for _, n := range []string{"s1", "s2", "s3"} {
		mustCreateProjectInNS(t, db, n, "sync", 1, 5, 0, 1.0)
	}
	mustCreateProjectInNS(t, db, "f1", "foreman", 1, 5, 0, 1.0)

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10, nil)

	// Nothing running → at most ONE sync project (cap=1) + foreman packs.
	res := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())
	var syncCount, foremanCount int
	for _, p := range res.Projects {
		switch p.Name {
		case "s1", "s2", "s3":
			syncCount++
		case "f1":
			foremanCount++
		}
	}
	if syncCount != 1 {
		t.Errorf("sync namespace packed %d projects, want exactly 1 (max_concurrent=1)", syncCount)
	}
	if foremanCount != 1 {
		t.Errorf("foreman namespace packed %d projects, want 1 (uncapped)", foremanCount)
	}

	// One sync project already running → NO other sync project packs.
	res2 := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, []string{"s1"}, time.Now())
	syncCount = 0
	for _, p := range res2.Projects {
		if p.Name == "s1" || p.Name == "s2" || p.Name == "s3" {
			syncCount++
		}
	}
	if syncCount != 0 {
		t.Errorf("sync namespace packed %d projects with one running, want 0 (cap=1)", syncCount)
	}
}

// TestMultiPoolPacker_CarriesPromptAndChain verifies the LIVE namespace-mode
// path now carries per-project prompt + namespace prompt + namespace model
// chain into the packed project (Bane 2026-08-27 3-tier routing).
func TestMultiPoolPacker_CarriesPromptAndChain(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns := database.Namespace{
		ID: "ds", Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
		DefaultPrompt: "NAMESPACE-BASE", ModelChain: `["cheap-m@cheap-p"]`,
	}
	mustCreateNamespace(t, db, &ns)
	mustCreateProjectInNS(t, db, "blog-sync", "ds", 1, 5, 0, 1.0)

	// Set the per-project prompt via raw SQL (the loader writes it from
	// fleet.toml at boot; the DB is the source of truth here).
	if _, err := db.Exec("UPDATE projects SET prompt='PROJECT-PROMPT', prompt_mode='append' WHERE name='blog-sync'"); err != nil {
		t.Fatalf("set project prompt: %v", err)
	}

	projects, _ := database.ListProjects(ctx, db, true)
	namespaces, _ := database.ListNamespaces(ctx, db, true)
	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	res := mp.Pack(projects, namespaces, defaultUrgencyCalc(), nil, nil, time.Now())

	if len(res.Projects) != 1 {
		t.Fatalf("expected 1 packed project, got %d", len(res.Projects))
	}
	p := res.Projects[0]
	if p.NamespacePrompt != "NAMESPACE-BASE" {
		t.Errorf("NamespacePrompt = %q, want NAMESPACE-BASE", p.NamespacePrompt)
	}
	if p.NamespaceChain != `["cheap-m@cheap-p"]` {
		t.Errorf("NamespaceChain = %q, want cheap chain", p.NamespaceChain)
	}
	var wantPrompt string
	_ = db.QueryRow("SELECT prompt FROM projects WHERE name='blog-sync'").Scan(&wantPrompt)
	if wantPrompt != "" && p.Prompt != wantPrompt {
		t.Errorf("Prompt = %q, want %q", p.Prompt, wantPrompt)
	}
}
