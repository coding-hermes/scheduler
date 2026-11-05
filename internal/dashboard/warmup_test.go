package dashboard

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestWarmOverviewBoot_StoresSnapshotBeforeFirstRender pins the PERF-001-R3-R3
// boot warm-up: after WarmOverviewBoot the generator holds a stored snapshot
// with the seeded lanes, so the first render serves the full page instead of
// degrading to the 40 KB partial shell the boot-window probe measured.
func TestWarmOverviewBoot_StoresSnapshotBeforeFirstRender(t *testing.T) {
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('warm-lane', 'https://example.invalid/warm-lane', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	g := NewGenerator(db, nil)
	started := time.Now()
	g.WarmOverviewBoot(context.Background(), 1)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("WarmOverviewBoot took %s for one tiny pass — the warm pass is unbounded", elapsed)
	}

	g.snap.mu.Lock()
	stamped := !g.snap.stamp.IsZero()
	projects := len(g.snap.data.Projects)
	inflight := g.snap.inflight
	g.snap.mu.Unlock()
	if !stamped {
		t.Fatal("WarmOverviewBoot stored no snapshot — the first render after boot would degrade to partial")
	}
	if projects != 1 {
		t.Fatalf("snapshot holds %d projects, want 1", projects)
	}
	if inflight {
		t.Fatal("warm pass left the snapshot's in-flight marker set — renders would queue on it forever")
	}
}

// TestWarmOverviewBoot_ConvergesOnEmptyCICache pins the early exit: with no
// CI workdirs to probe, the second pass must observe zero cache growth and
// stop instead of running the full pass cap.
func TestWarmOverviewBoot_ConvergesOnEmptyCICache(t *testing.T) {
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	g := NewGenerator(db, nil)
	started := time.Now()
	// A converged second pass returns promptly; an unconstrained loop over
	// the full pass cap with the inter-pass gap would still be fast here, so
	// the bound is the observable: exactly maxPasses-1 gap waits happen only
	// when the exit does NOT trip. Assert the exit tripped via the gap clock:
	// two passes cost one 200ms gap; a full 4-pass run costs three.
	g.WarmOverviewBoot(context.Background(), 4)
	if elapsed := time.Since(started); elapsed >= 3*warmOverviewBootGap {
		t.Fatalf("WarmOverviewBoot ran past the converged second pass (%s elapsed) — the early exit did not trip on zero cache growth", elapsed)
	}
}
