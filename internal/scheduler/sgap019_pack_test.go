package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func writeBoardJSONL(t *testing.T, workdir string, pending, complete int) {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", boardDir, err)
	}
	content := ""
	for i := 0; i < pending; i++ {
		content += `{"id":"TASK-` + string(rune('A'+i)) + `","status":"pending"}` + "\n"
	}
	for i := 0; i < complete; i++ {
		content += `{"id":"DONE-` + string(rune('A'+i)) + `","status":"complete"}` + "\n"
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestPendingBoost_NamespacePack_PendingWinsOverHigherPriority(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	withBoard := t.TempDir()
	withoutBoard := t.TempDir()
	writeBoardJSONL(t, withBoard, 1, 0)

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))

	hp := makeProject("high-prio-no-pending", 10, 10, 0, 1.0)
	hp.NamespaceID = strPtr("coding-hermes")
	hp.Workdir = withoutBoard
	if err := database.CreateProject(ctx, db, hp); err != nil {
		t.Fatalf("CreateProject high-prio: %v", err)
	}

	lp := makeProject("low-prio-pending", 10, 1, 0, 1.0)
	lp.NamespaceID = strPtr("coding-hermes")
	lp.Workdir = withBoard
	if err := database.CreateProject(ctx, db, lp); err != nil {
		t.Fatalf("CreateProject low-prio: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	now := time.Now().UTC()
	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), nil, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "low-prio-pending" {
		t.Errorf("Pack selected %q, want %q - pending board boost did not fire", got[0], "low-prio-pending")
	}
}

func TestPendingBoost_FlatFallback_PendingWinsOverHigherPriority(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	withBoard := t.TempDir()
	withoutBoard := t.TempDir()
	writeBoardJSONL(t, withBoard, 1, 0)

	hp := makeProject("high-prio-no-pending", 10, 10, 0, 1.0)
	hp.Workdir = withoutBoard
	if err := database.CreateProject(ctx, db, hp); err != nil {
		t.Fatalf("CreateProject high-prio: %v", err)
	}

	lp := makeProject("low-prio-pending", 10, 1, 0, 1.0)
	lp.Workdir = withBoard
	if err := database.CreateProject(ctx, db, lp); err != nil {
		t.Fatalf("CreateProject low-prio: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	now := time.Now().UTC()
	result := mp.Pack(projects, nil, prodUrgencyCalc(), nil, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "low-prio-pending" {
		t.Errorf("Pack selected %q, want %q - pending board boost did not fire in flat fallback", got[0], "low-prio-pending")
	}
}

func TestPendingBoost_FlatPacker_Twin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	withBoard := t.TempDir()
	withoutBoard := t.TempDir()
	writeBoardJSONL(t, withBoard, 1, 0)

	hp := makeProject("high-prio-no-pending", 10, 10, 0, 1.0)
	hp.Workdir = withoutBoard
	if err := database.CreateProject(ctx, db, hp); err != nil {
		t.Fatalf("CreateProject high-prio: %v", err)
	}

	lp := makeProject("low-prio-pending", 10, 1, 0, 1.0)
	lp.Workdir = withBoard
	if err := database.CreateProject(ctx, db, lp); err != nil {
		t.Fatalf("CreateProject low-prio: %v", err)
	}

	p := scheduler.NewPacker(db, prodUrgencyCalc(), 100, 1, nil)
	p.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	picked, err := p.Pick(time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(picked) != 1 {
		t.Fatalf("Pick selected %d projects, want exactly 1 (maxConcurrent=1)", len(picked))
	}
	if picked[0].Name != "low-prio-pending" {
		t.Errorf("Pick selected %q, want %q - pending boost missing in flat Packer.Pick", picked[0].Name, "low-prio-pending")
	}
}

func TestPendingBoost_StarvationBeatsPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	withBoard := t.TempDir()
	writeBoardJSONL(t, withBoard, 5, 0)

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))

	starved := makeProject("starved-prio5", 10, 5, 900, 1.0)
	starved.NamespaceID = strPtr("coding-hermes")
	starved.Workdir = t.TempDir()
	if err := database.CreateProject(ctx, db, starved); err != nil {
		t.Fatalf("CreateProject starved: %v", err)
	}

	pending := makeProject("pending-prio10", 10, 10, 900, 1.0)
	pending.NamespaceID = strPtr("coding-hermes")
	pending.Workdir = withBoard
	if err := database.CreateProject(ctx, db, pending); err != nil {
		t.Fatalf("CreateProject pending: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"starved-prio5":  now.Add(-2 * time.Hour),
		"pending-prio10": now.Add(-16 * time.Minute),
	}

	mp := scheduler.NewMultiPoolPacker(100, 1, nil)
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	got := packNames(result)
	if len(got) != 1 {
		t.Fatalf("Pack selected %d projects %v, want exactly 1 (maxConcurrent=1)", len(got), got)
	}
	if got[0] != "starved-prio5" {
		t.Errorf("Pack selected %q, want %q - starvation boost must outrank pending boost", got[0], "starved-prio5")
	}
}

func TestPendingBoost_NoBoard_FilesAnywhere(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))

	p1 := makeProject("proj-a", 10, 3, 0, 1.0)
	p1.NamespaceID = strPtr("coding-hermes")
	p1.Workdir = t.TempDir()
	if err := database.CreateProject(ctx, db, p1); err != nil {
		t.Fatalf("CreateProject a: %v", err)
	}

	p2 := makeProject("proj-b", 10, 10, 0, 1.0)
	p2.NamespaceID = strPtr("coding-hermes")
	p2.Workdir = t.TempDir()
	if err := database.CreateProject(ctx, db, p2); err != nil {
		t.Fatalf("CreateProject b: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), nil, nil, time.Now().UTC())

	if len(result.Projects) != 2 {
		t.Fatalf("Pack selected %d projects, want 2 (no boards = normal behavior)", len(result.Projects))
	}
	if result.Projects[0].Name != "proj-b" {
		t.Errorf("first project = %q, want proj-b (higher priority, no boost interference)", result.Projects[0].Name)
	}
}

func TestPendingBoost_CooldownNotBypassed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	withBoard := t.TempDir()
	writeBoardJSONL(t, withBoard, 3, 0)

	mustCreateNamespace(t, db, makeNamespace("coding-hermes", 10, 1, 100, true))

	p := makeProject("cooling-pending", 10, 1, 3600, 1.0)
	p.NamespaceID = strPtr("coding-hermes")
	p.Workdir = withBoard
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	projects, err := database.ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	namespaces, err := database.ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}

	now := time.Now().UTC()
	lastCompleted := map[string]time.Time{
		"cooling-pending": now,
	}

	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	mp.SetPendingCounter(scheduler.NewPendingTaskCounter(60 * time.Second))

	result := mp.Pack(projects, namespaces, prodUrgencyCalc(), lastCompleted, nil, now)

	if len(result.Projects) != 0 {
		t.Errorf("Pack selected %d projects - pending boost must NOT bypass cooldown", len(result.Projects))
	}
}
