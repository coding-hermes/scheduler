package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	scheduler "github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-124 tests: admission modes. Bane directive 2026-09-16:
// the foreman namespace runs projects back-to-back driven by board work
// (within namespace concurrency + weight budget, priority-ordered); when
// only perpetual/never-done rows remain, the cooldown pin resumes. Other
// namespaces (dogfood/qa/pm/sync) keep cron admission. The mode is pure
// config at every layer — namespace default + project override, settable
// via API, CLI, and fleet.toml. Nothing here hardcodes a name.

func writeModeBoard(t *testing.T, workdir string, rows ...string) {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := ""
	for _, r := range rows {
		content += r + "\n"
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func modeProject(name, nsID, workdir, mode string) database.Project {
	p := database.Project{
		Name:          name,
		RepoURL:       "local://" + name,
		Workdir:       workdir,
		Weight:        10,
		Priority:      5,
		CooldownS:     14400, // 4h pin — tasks mode must beat it; cooldown mode must honor it
		Enabled:       true,
		CreatedAt:     time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		AdmissionMode: mode,
	}
	if nsID != "" {
		ns := nsID
		p.NamespaceID = &ns
	}
	return p
}

func packNamespaces(t *testing.T, projects []database.Project, namespaces []database.Namespace, lastCompleted map[string]time.Time) scheduler.PackResult {
	t.Helper()
	mp := scheduler.NewMultiPoolPacker(400, 8, nil)
	return mp.Pack(projects, namespaces, defaultUrgencyCalc(), lastCompleted, nil, time.Now().UTC())
}

func modeSelected(t *testing.T, res scheduler.PackResult, name string) bool {
	t.Helper()
	for _, p := range res.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

func tasksNs(id string) database.Namespace {
	return database.Namespace{ID: id, Weight: 100, Reserved: 1, HardCap: 100, Enabled: true, MaxConcurrent: 4, AdmissionMode: database.AdmissionModeTasks}
}

func cooldownNs(id string) database.Namespace {
	return database.Namespace{ID: id, Weight: 100, Reserved: 1, HardCap: 100, Enabled: true, MaxConcurrent: 4, AdmissionMode: database.AdmissionModeCooldown}
}

// T-MODE-1: namespace admission_mode=tasks + non-perpetual pending work +
// last tick 1h ago against a 4h pin → SELECTED despite cooldown.
func TestTasksMode_PendingWorkBeatsCooldownPin(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd,
		`{"id":"REAL-1","status":"pending"}`,
		`{"id":"NEVER-DONE","status":"pending","perpetual":true}`,
	)
	p := modeProject("mode-a", "coding-hermes", wd, "")
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"mode-a": now.Add(-time.Hour)})
	if !modeSelected(t, res, "mode-a") {
		t.Fatalf("T-MODE-1 FAIL: tasks-mode project with pending work not selected — cooldown pin should be waived")
	}
}

// T-MODE-2: same setup but the board holds ONLY a perpetual fixture → NOT
// selected inside the cooldown window (never-done does not count as work).
func TestTasksMode_PerpetualOnlyBoardFallsBackToCooldown(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd,
		`{"id":"NEVER-DONE","status":"pending","perpetual":true}`,
	)
	p := modeProject("mode-b", "coding-hermes", wd, "")
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"mode-b": now.Add(-time.Hour)})
	if modeSelected(t, res, "mode-b") {
		t.Fatalf("T-MODE-2 FAIL: perpetual-only board admitted — never-done must fall back to cooldown")
	}
}

// T-MODE-3: cooldown-mode namespace (the cron default, e.g. qa/pm/sync) —
// identical pending board, project NOT selected inside the window.
func TestCooldownMode_PendingWorkDoesNotBeatCooldown(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-2","status":"pending"}`)
	p := modeProject("mode-c", "qa", wd, "")
	ns := cooldownNs("qa")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"mode-c": now.Add(-time.Hour)})
	if modeSelected(t, res, "mode-c") {
		t.Fatalf("T-MODE-3 FAIL: cooldown-mode namespace admitted inside the cooldown window")
	}
}

// T-MODE-4: project override beats namespace default both ways.
func TestProjectOverride_WinsOverNamespaceDefault(t *testing.T) {
	wdA := t.TempDir()
	writeModeBoard(t, wdA, `{"id":"REAL-3","status":"pending"}`)
	wdB := t.TempDir()
	writeModeBoard(t, wdB, `{"id":"REAL-4","status":"pending"}`)
	override := modeProject("mode-override", "qa", wdA, database.AdmissionModeTasks)
	cronPin := modeProject("mode-cronpin", "qa", wdB, database.AdmissionModeCooldown)
	ns := cooldownNs("qa")
	ns.MaxConcurrent = 8

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{override, cronPin}, []database.Namespace{ns},
		map[string]time.Time{
			"mode-override": now.Add(-time.Hour),
			"mode-cronpin":  now.Add(-time.Hour),
		})
	if !modeSelected(t, res, "mode-override") {
		t.Fatalf("T-MODE-4a FAIL: project override=tasks not admitted inside cooldown window")
	}
	if modeSelected(t, res, "mode-cronpin") {
		t.Fatalf("T-MODE-4b FAIL: project override=cooldown admitted inside cooldown window")
	}
}

// T-MODE-5: tasks-mode ALSO admits a never-ticked project (lastCompleted
// absent) — first wake after a fresh board write is immediate.
func TestTasksMode_NeverTickedProjectAdmitsImmediately(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd, `{"id":"REAL-5","status":"pending"}`)
	p := modeProject("mode-first", "coding-hermes", wd, "")
	ns := tasksNs("coding-hermes")

	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns}, map[string]time.Time{})
	if !modeSelected(t, res, "mode-first") {
		t.Fatalf("T-MODE-5 FAIL: never-ticked tasks-mode project with pending work not selected")
	}
}

// T-MODE-6: validation — UpdateNamespace/UpdateProject reject invalid
// modes, accept valid ones, and "" clears a project back to inheritance.
func TestAdmissionMode_ValidationAndRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns := database.Namespace{ID: "ns-val", Weight: 100, Reserved: 1, HardCap: 100, Enabled: true, AdmissionMode: database.AdmissionModeCooldown}
	if err := database.CreateNamespace(ctx, db, &ns); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if err := database.UpdateNamespace(ctx, db, "ns-val", database.NamespacePatch{AdmissionMode: modeStrPtr("turbo")}); err == nil {
		t.Fatalf("T-MODE-6a FAIL: namespace accepted invalid admission_mode")
	}
	if err := database.UpdateNamespace(ctx, db, "ns-val", database.NamespacePatch{AdmissionMode: modeStrPtr(database.AdmissionModeTasks)}); err != nil {
		t.Fatalf("T-MODE-6b FAIL: namespace rejected valid tasks mode: %v", err)
	}
	got, _ := database.GetNamespace(ctx, db, "ns-val")
	if got.AdmissionMode != database.AdmissionModeTasks {
		t.Fatalf("T-MODE-6c FAIL: namespace mode round-trip got %q", got.AdmissionMode)
	}

	wd := t.TempDir()
	p := modeProject("mode-val", "ns-val", wd, "")
	if err := database.CreateProject(ctx, db, &p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := database.UpdateProject(ctx, db, "mode-val", database.ProjectUpdates{AdmissionMode: modeStrPtr("yolo")}); err == nil {
		t.Fatalf("T-MODE-6d FAIL: project accepted invalid admission_mode")
	}
	if err := database.UpdateProject(ctx, db, "mode-val", database.ProjectUpdates{AdmissionMode: modeStrPtr(database.AdmissionModeTasks)}); err != nil {
		t.Fatalf("T-MODE-6e FAIL: project rejected valid tasks mode: %v", err)
	}
	gp, _ := database.GetProject(ctx, db, "mode-val")
	if gp.AdmissionMode != database.AdmissionModeTasks {
		t.Fatalf("T-MODE-6f FAIL: project mode round-trip got %q", gp.AdmissionMode)
	}
	if err := database.UpdateProject(ctx, db, "mode-val", database.ProjectUpdates{AdmissionMode: modeStrPtr("")}); err != nil {
		t.Fatalf("T-MODE-6g FAIL: project rejected clear-to-inherit: %v", err)
	}
	gp, _ = database.GetProject(ctx, db, "mode-val")
	if gp.AdmissionMode != "" {
		t.Fatalf("T-MODE-6h FAIL: project mode clear round-trip got %q", gp.AdmissionMode)
	}
}

func modeStrPtr(s string) *string { return &s }

// T-MODE-4: tasks-mode project with consecutive failures — FailureBackoff
// gates admission even when board work is pending (SCHED-GAP-133).
func TestTasksMode_FailureBackoffGatesAdmission(t *testing.T) {
	wd := t.TempDir()
	writeModeBoard(t, wd,
		`{"id":"REAL-1","status":"pending"}`,
	)
	p := modeProject("mode-backoff", "coding-hermes", wd, "")
	// FailureBackoff(14400s, 5) = 14400 * 2^4 = 230400s (64h)
	p.ConsecutiveFailures = 5
	ns := tasksNs("coding-hermes")

	now := time.Now().UTC()
	res := packNamespaces(t, []database.Project{p}, []database.Namespace{ns},
		map[string]time.Time{"mode-backoff": now.Add(-time.Hour)})
	if modeSelected(t, res, "mode-backoff") {
		t.Fatalf("T-MODE-4 FAIL: tasks-mode project with consecutive_failures=5 was selected — FailureBackoff must gate admission")
	}
}
