package scheduler

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-142 — the orphan-nudge path must honour per-namespace
// max_concurrent. Live defect it pins: on the 2026-09-17 restart three
// orphaned <project>-sync ticks were re-nudged in one pass against
// duckbrain-sync's cap of 1, and the burst took global slots before any
// qa/pm/dogfood lane could be admitted.
func TestGAP142_NudgeAdmissionHonoursNamespaceCap(t *testing.T) {
	cases := []struct {
		name     string
		cap      int
		inflight int
		admitted int
		wantOpen bool
		why      string
	}{
		{"cap 1, nothing in flight admits the first", 1, 0, 0, true, "first nudge fits"},
		{"cap 1, one admitted this pass blocks the second", 1, 0, 1, false, "same-pass burst is the live defect"},
		{"cap 1, one already running blocks", 1, 1, 0, false, "running counts"},
		{"cap 1, one already queued blocks", 1, 1, 0, false, "queued counts (awaits a slot)"},
		{"cap 8 admits until full", 8, 7, 0, true, "foremen namespace still admits"},
		{"cap 8 at the limit blocks", 8, 7, 1, false, "cap is a hard ceiling"},
		{"cap 0 means unlimited", 0, 99, 5, true, "0 = unlimited in the schema"},
		{"negative cap fails open", -3, 99, 5, true, "defensive: never freeze recovery"},
	}
	for _, c := range cases {
		if got := nudgeAdmissionOpen(c.cap, c.inflight, c.admitted); got != c.wantOpen {
			t.Errorf("%s: nudgeAdmissionOpen(cap=%d, inflight=%d, admitted=%d) = %v, want %v — %s",
				c.name, c.cap, c.inflight, c.admitted, got, c.wantOpen, c.why)
		}
	}

	// The three-sync burst, replayed: cap 1, nothing in flight, three
	// candidates in the same pass. Exactly ONE may be admitted.
	admitted := 0
	passed := 0
	for i := 0; i < 3; i++ {
		if nudgeAdmissionOpen(1, 0, admitted) {
			admitted++
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("burst replay: %d of 3 nudges admitted against cap 1, want exactly 1", passed)
	}
}

// The DB-backed halves: the cap lookup and the in-flight count the nudge path
// relies on. A wrong count here would defeat the gate above.
func TestGAP142_NamespaceCapAndInflight(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	capped := "sync-lanes"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: capped, Weight: 10, Reserved: 1, HardCap: 15, MaxConcurrent: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "unlimited", Weight: 10, Reserved: 1, HardCap: 15, MaxConcurrent: 0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace unlimited: %v", err)
	}

	mk := func(name, ns string) {
		nsID := ns
		p := &database.Project{
			Name: name, RepoURL: "https://example.com/" + name, Workdir: t.TempDir(),
			Weight: 3, Priority: 5, CooldownS: 21600, DecayRate: 1, Enabled: true, NamespaceID: &nsID,
		}
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	mk("lane-a-sync", capped)
	mk("lane-b-sync", capped)
	mk("other-sync", "unlimited")

	// lane-a-sync running, lane-b-sync queued → two in flight for the capped ns.
	seedTick := func(id, project, status string) {
		if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
			VALUES (?, ?, ?, datetime('now'), datetime('now'))`, id, project, status); err != nil {
			t.Fatalf("seed tick %s: %v", id, err)
		}
	}
	seedTick("t-a", "lane-a-sync", "running")
	seedTick("t-b", "lane-b-sync", "queued")
	seedTick("t-c", "other-sync", "completed") // completed must NOT count

	l := &Loop{db: db}

	if got := l.namespaceCap(ctx, capped); got != 1 {
		t.Errorf("namespaceCap(%s) = %d, want 1", capped, got)
	}
	if got := l.namespaceCap(ctx, "unlimited"); got != 0 {
		t.Errorf("namespaceCap(unlimited) = %d, want 0 (unlimited)", got)
	}
	if got := l.namespaceCap(ctx, "no-such-namespace"); got != 0 {
		t.Errorf("namespaceCap(unknown) = %d, want 0 (fail open)", got)
	}
	if got := l.namespaceInflight(ctx, capped); got != 2 {
		t.Errorf("namespaceInflight(%s) = %d, want 2 (running + queued)", capped, got)
	}
	if got := l.namespaceInflight(ctx, "unlimited"); got != 0 {
		t.Errorf("namespaceInflight(unlimited) = %d, want 0 (completed does not count)", got)
	}

	// With the cap reached, a third orphan in the same namespace is refused.
	if nudgeAdmissionOpen(1, l.namespaceInflight(ctx, capped), 0) {
		t.Error("third orphan admitted while the namespace was already at its cap of 1")
	}
}
