package scheduler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §12 (SCHED-GAP-114): reaper abandonment + wave recovery trigger ──
//
// Board AC tests: TestReaper_MarksTickWorkersAbandoned,
// TestReaper_DoesNotTouchWorktrees, TestWaveRecoveryFlag_OnlyForTerminalUn-
// finishedManifest — plus the spawn-preamble prompt tests. Spec §12 RED
// check: removing the reaper's tick_workers step fails the first test.

// seedAbandonedWaveFixture inserts a dead-pid running wave tick with 3
// ingested tick_workers rows (1 already done, 2 running) under workdir's
// project and returns the tick id. The manifest itself is written to the
// workdir so the recovery scan can find it.
func seedAbandonedWaveFixture(t *testing.T, db *sql.DB, project, workdir string) string {
	t.Helper()
	ctx := context.Background()
	p := &database.Project{
		Name: project, Workdir: workdir, Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Model: "m", Provider: "p", Enabled: true,
	}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", project, err)
	}
	tickID := project + "-2026-09-13-10-00-00"
	insertRunningTick(t, db, tickID, project, deadTestPID)
	// Manifest: unfinished (finished_at empty), 3 workers.
	raw, _ := json.Marshal(WaveManifest{
		TickID: tickID, Project: project, StartedAt: "2026-09-13T10:00:00Z",
		Workers: []WaveWorker{
			{TaskID: "SCHED-GAP-1", Branch: "wt/SCHED-GAP-1", Worktree: "/home/kara/worktrees/x-1"},
			{TaskID: "SCHED-GAP-2", Branch: "wt/SCHED-GAP-2", Worktree: "/home/kara/worktrees/x-2"},
			{TaskID: "SCHED-GAP-3", Branch: "wt/SCHED-GAP-3", Worktree: "/home/kara/worktrees/x-3"},
		},
	})
	writeWaveManifest(t, workdir, tickID, string(raw))
	// Ingest the manifest the way the completion path would: 3 rows, the
	// first already carrying a judge verdict ('done' per the documented
	// state derivation), the other two verdict-less ('running').
	if _, err := ingestWaveManifest(ctx, db, workdir, project, tickID); err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	// Flip row 1 to done explicitly (simulating a worker that reported
	// done before the foreman died).
	if _, err := db.Exec(`UPDATE tick_workers SET state='done' WHERE tick_id = ? AND task_id = 'SCHED-GAP-1'`, tickID); err != nil {
		t.Fatalf("flip row 1 done: %v", err)
	}
	return tickID
}

// tickWorkerStates reads the state column of every worker row for tickID,
// in dispatch (id) order.
func tickWorkerStates(t *testing.T, db *sql.DB, tickID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT state FROM tick_workers WHERE tick_id = ? ORDER BY id`, tickID)
	if err != nil {
		t.Fatalf("query worker states %s: %v", tickID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan worker state: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// TestReaper_MarksTickWorkersAbandoned (§12): a deadline-expired (dead-pid)
// 3-worker wave reaped through EITHER reaper path leaves the tick timeout,
// outcome unset, completed_at stamped, worker_count unchanged, and its
// running tick_workers rows flipped to 'abandoned' while already-done rows
// stay 'done'. RED check: removing the reaper's tick_workers step
// (the reapWaveAbandoned calls) makes this fail.
func TestReaper_MarksTickWorkersAbandoned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reaper func(*Loop)
	}{
		{"cleanDanglingOnStartup", func(l *Loop) { l.cleanDanglingOnStartup() }},
		{"reapZombies", func(l *Loop) { l.reapZombies() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			workdir := t.TempDir()
			tickID := seedAbandonedWaveFixture(t, db, "sgap114-reap-"+strings.ToLower(tc.name), workdir)

			// Pin worker_count before the reap (§8.2 item 7: the reaper
			// never touches it — no double-counting).
			var wcBefore int
			if err := db.QueryRow(`SELECT worker_count FROM ticks WHERE id = ?`, tickID).Scan(&wcBefore); err != nil {
				t.Fatalf("read worker_count: %v", err)
			}
			if wcBefore != 3 {
				t.Fatalf("fixture worker_count = %d, want 3", wcBefore)
			}

			// Capture the REAPER log line for the field assertions.
			var buf lockedBuffer
			orig := log.Writer()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(orig) })

			loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
			tc.reaper(loop)

			if got := tickStatusOf(t, db, tickID); got != "timeout" {
				t.Errorf("reaped wave tick status = %q, want timeout", got)
			}
			if outcome := tickOutcomeOf(t, db, tickID); outcome.Valid {
				t.Errorf("reaped tick outcome = %q, want NULL (unchanged semantics)", outcome.String)
			}
			if got := tickCompletedAtOf(t, db, tickID); !got.Valid || got.String == "" {
				t.Errorf("reaped tick completed_at = %v, want stamped (GAP-045)", got)
			}
			states := tickWorkerStates(t, db, tickID)
			if len(states) != 3 {
				t.Fatalf("worker rows = %v, want 3", states)
			}
			for i, want := range []string{"done", "abandoned", "abandoned"} {
				if states[i] != want {
					t.Errorf("worker row %d state = %q, want %q (done never regresses)", i, states[i], want)
				}
			}
			var wcAfter int
			if err := db.QueryRow(`SELECT worker_count FROM ticks WHERE id = ?`, tickID).Scan(&wcAfter); err != nil {
				t.Fatalf("read worker_count: %v", err)
			}
			if wcAfter != wcBefore {
				t.Errorf("worker_count changed %d → %d — the reaper must not double-count progress", wcBefore, wcAfter)
			}

			// Grep-able REAPER line with tick id, wave size, abandoned count.
			line := buf.String()
			for _, want := range []string{"REAPER:", tickID, "wave_size=3", "abandoned=2"} {
				if !strings.Contains(line, want) {
					t.Errorf("log missing %q; got: %s", want, line)
				}
			}
		})
	}
}

// TestReaper_DoesNotTouchWorktrees (§12, invariant W6): the reaper performs
// no fs mutation and no git call on manifest/worktree data. We seed a waves
// dir + manifest + a fake worktree dir, run both reapers, and assert every
// file is byte-identical afterwards and the worktree dir still exists.
func TestReaper_DoesNotTouchWorktrees(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	project := "sgap114-w6"
	tickID := seedAbandonedWaveFixture(t, db, project, workdir)

	// Evidence artifacts: the manifest file + a live worktree with content.
	manifestPath := waveManifestPath(workdir, tickID)
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	wtDir := filepath.Join(workdir, "worktree-T1")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "branch.txt"), []byte("unmerged work"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	// A second, running wave tick with its own manifest (unfinished) — the
	// reaper must not touch its manifest either even though its tick dies.
	tick2 := project + "-2026-09-13-11-00-00"
	insertRunningTick(t, db, tick2, project, deadTestPID)
	raw2, _ := json.Marshal(WaveManifest{
		TickID: tick2, Project: project, StartedAt: "2026-09-13T11:00:00Z",
		Workers: []WaveWorker{{TaskID: "T-9", Branch: "wt/T-9"}},
	})
	writeWaveManifest(t, workdir, tick2, string(raw2))
	manifest2Path := waveManifestPath(workdir, tick2)
	manifest2Before, err := os.ReadFile(manifest2Path)
	if err != nil {
		t.Fatalf("read manifest 2: %v", err)
	}

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.cleanDanglingOnStartup()
	loop.reapZombies()

	// Both ticks reaped; the manifests and worktrees survive untouched.
	if got := tickStatusOf(t, db, tickID); got != "timeout" {
		t.Fatalf("tick 1 status = %q, want timeout", got)
	}
	if got := tickStatusOf(t, db, tick2); got != "timeout" {
		t.Fatalf("tick 2 status = %q, want timeout", got)
	}
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != string(manifestBefore) {
		t.Errorf("manifest 1 mutated or unreadable (err=%v) — reaper must not touch manifest data (W6)", err)
	}
	if got, err := os.ReadFile(manifest2Path); err != nil || string(got) != string(manifest2Before) {
		t.Errorf("manifest 2 mutated or unreadable (err=%v) — reaper must not touch manifest data (W6)", err)
	}
	if _, err := os.Stat(filepath.Join(wtDir, "branch.txt")); err != nil {
		t.Errorf("worktree file gone/unreadable: %v — reaper must never delete worktrees (W6)", err)
	}
}

// TestWaveRecoveryFlag_OnlyForTerminalUnfinishedManifest (§12): the
// recovery trigger fires only for a manifest whose tick row is TERMINAL and
// whose finished_at is empty. Live-tick manifests, foreman-closed manifests,
// malformed manifests, and unknown tick rows never trigger.
func TestWaveRecoveryFlag_OnlyForTerminalUnfinishedManifest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	workdir := t.TempDir()
	project := "sgap114-flag"

	p := &database.Project{
		Name: project, Workdir: workdir, Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	write := func(tickID, tickStatus, finished string) {
		t.Helper()
		if tickStatus != "" {
			tk := &database.Tick{
				ID: tickID, ProjectName: project, Status: database.TickStatus(tickStatus),
				SpawnedAt: "2026-09-13T10:00:00Z",
			}
			if err := database.CreateTick(ctx, db, tk); err != nil {
				t.Fatalf("CreateTick %s: %v", tickID, err)
			}
		}
		raw, _ := json.Marshal(WaveManifest{
			TickID: tickID, Project: project, StartedAt: "2026-09-13T10:00:00Z",
			FinishedAt: finished,
			Workers:    []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1"}},
		})
		writeWaveManifest(t, workdir, tickID, string(raw))
	}

	// The one true candidate: terminal tick + unfinished manifest.
	write(project+"-timeout-unfin", "timeout", "")
	// Non-candidates: live tick, completed-and-closed, failed-and-closed,
	// malformed, no tick row at all.
	write(project+"-running-unfin", "running", "")
	write(project+"-timeout-closed", "timeout", "2026-09-13T11:00:00Z")
	write(project+"-failed-closed", "failed", "2026-09-13T11:00:00Z")
	writeWaveManifest(t, workdir, project+"-malformed", "{broken")
	write(project+"-queued-unfin", "queued", "")

	got := unfinishedWaveManifests(ctx, db, workdir)
	if len(got) != 1 {
		t.Fatalf("unfinishedWaveManifests = %d manifest(s), want exactly 1 (terminal+unfinished); got %+v", len(got), got)
	}
	if got[0].TickID != project+"-timeout-unfin" {
		t.Errorf("candidate = %q, want %q", got[0].TickID, project+"-timeout-unfin")
	}
}

// TestWaveRecoveryFlag_StampedOnRecoverySpawn (§8.2 item 5 + §9.1): the
// spawn path stamps wave_recovery=1 on the recovery tick's own row and
// prepends the recover-before-dispatch preamble to the prompt.
func TestWaveRecoveryFlag_StampedOnRecoverySpawn(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	project := "sgap114-stamp"
	abandonedTick := seedAbandonedWaveFixture(t, db, project, workdir)

	// The abandoned wave's tick must be TERMINAL before it is a recovery
	// candidate: reap it first (the real sequence is crash → reaper → next
	// tick carries the preamble).
	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.reapZombies()
	if got := tickStatusOf(t, db, abandonedTick); got != "timeout" {
		t.Fatalf("abandoned tick status = %q, want timeout after reap", got)
	}

	// Enqueue the recovery tick row the way the spawn path does.
	recTickID := project + "-2026-09-13-12-00-00"
	lc := NewLifecycleTracker(db)
	if err := lc.Enqueue(project, recTickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	s := NewSpawner(db, 5)
	prompt := s.buildSpawnPrompt(PackedProject{
		Name: project, Workdir: workdir, NamespacePrompt: "DO THE TICK",
	}, recTickID)

	// wave_recovery=1 on the recovery tick's own row.
	var wr int
	if err := db.QueryRow(`SELECT wave_recovery FROM ticks WHERE id = ?`, recTickID).Scan(&wr); err != nil {
		t.Fatalf("read wave_recovery: %v", err)
	}
	if wr != 1 {
		t.Errorf("recovery tick wave_recovery = %d, want 1", wr)
	}

	// Preamble prepended, body preserved, linkage prefix intact.
	for _, want := range []string{
		"WAVE RECOVERY", "RECOVER BEFORE DISPATCH", "wt/SCHED-GAP-1",
		"wt/SCHED-GAP-2", "wt/SCHED-GAP-3", "DO THE TICK",
		"[Scheduler tick: " + recTickID + "]",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("recovery prompt missing %q;\nprompt:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, abandonedTick) {
		t.Errorf("recovery prompt must name the abandoned tick %s;\nprompt:\n%s", abandonedTick, prompt)
	}
	// The preamble is fenced.
	if !strings.Contains(prompt, "```") {
		t.Error("recovery preamble must be a fenced block")
	}
}

// TestBuildSpawnPrompt_CleanProjectByteIdentical (AC: clean project →
// preamble absent, prompt byte-identical to pre-change): a workdir with no
// waves dir (and one with only closed manifests) renders the exact legacy
// prompt bytes for both capped and uncapped namespaces.
func TestBuildSpawnPrompt_CleanProjectByteIdentical(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "capped", Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	clean := t.TempDir()      // no waves dir at all
	closedOnly := t.TempDir() // a foreman-closed manifest: not a candidate
	tickID := "proj-x-2026-09-13-09-00-00"
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "proj-x", Workdir: closedOnly, Weight: 1, Priority: 1,
		CooldownS: 0, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := database.CreateTick(ctx, db, &database.Tick{
		ID: tickID, ProjectName: "proj-x", Status: database.StatusCompleted,
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	raw, _ := json.Marshal(WaveManifest{
		TickID: tickID, Project: "proj-x", FinishedAt: "2026-09-13T10:00:00Z",
		Workers: []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1"}},
	})
	writeWaveManifest(t, closedOnly, tickID, string(raw))

	for _, tc := range []struct {
		name    string
		workdir string
		ns      string
	}{
		{"clean-uncapped", clean, ""},
		{"clean-capped", clean, "capped"},
		{"closed-manifest-uncapped", closedOnly, ""},
		{"closed-manifest-capped", closedOnly, "capped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSpawner(db, 5)
			p := PackedProject{
				Name: "proj-x", Workdir: tc.workdir,
				NamespacePrompt: "BODY", NamespaceID: tc.ns,
			}
			out := s.buildSpawnPrompt(p, "proj-x-tick-1")
			legacy := buildForemanPrompt(p, "proj-x-tick-1")
			want := legacy
			if tc.ns == "capped" {
				want = legacy + "\n" + waveBudgetLine(3) // pre-114 capped bytes
			}
			if out != want {
				t.Errorf("clean prompt not byte-identical:\n got: %q\nwant: %q", out, want)
			}
			if strings.Contains(out, "WAVE RECOVERY") {
				t.Error("clean project prompt must not contain the recovery preamble")
			}
		})
	}
}

// TestWaveRecoveryPreamble_TruncationAndEmpty: the pure renderer returns ""
// for no manifests and truncates the listing beyond wavePreambleMaxManifests
// with a visible note.
func TestWaveRecoveryPreamble_TruncationAndEmpty(t *testing.T) {
	if got := waveRecoveryPreamble(nil); got != "" {
		t.Errorf("preamble(nil) = %q, want empty", got)
	}
	var manifests []*WaveManifest
	for i := 0; i < wavePreambleMaxManifests+3; i++ {
		manifests = append(manifests, &WaveManifest{
			TickID: "t-trunc", StartedAt: "2026-09-13T10:00:00Z",
			Workers: []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1"}},
		})
	}
	got := waveRecoveryPreamble(manifests)
	if !strings.Contains(got, "3 additional unfinished manifest(s)") {
		t.Errorf("truncated preamble must note the 3 unlisted manifests; got:\n%s", got)
	}
	if n := strings.Count(got, "Unfinished wave tick="); n != wavePreambleMaxManifests {
		t.Errorf("listed %d manifests, want %d", n, wavePreambleMaxManifests)
	}
}

// lockedBuffer is a mutex-guarded log sink for the REAPER-line assertion.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
