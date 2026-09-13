package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §12 (SCHED-GAP-113): composition layer — WAVE_BUDGET prompt
// injection. Same package as schedgap078_prompt_test.go so the
// byte-identity assertions compare directly against buildForemanPrompt.

// TestBuildSpawnPrompt_WaveBudgetInjected verifies the exact injected line:
// cap=3, no live wave → "WAVE_BUDGET: 3".
func TestBuildSpawnPrompt_WaveBudgetInjected(t *testing.T) {
	db := newTestDB(t)
	if err := database.CreateNamespace(context.Background(), db, &database.Namespace{
		ID: "capped", Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	s := NewSpawner(db, 5)
	p := PackedProject{
		Name: "demo", Workdir: "/home/kara/demo",
		NamespacePrompt: "NS BASE", NamespaceID: "capped",
	}
	out := s.buildSpawnPrompt(p, "demo-tick")
	if !strings.Contains(out, "WAVE_BUDGET: 3") {
		t.Errorf("prompt must contain \"WAVE_BUDGET: 3\", got:\n%s", out)
	}
	if !strings.Contains(out, "NS BASE") {
		t.Error("wave line must not replace the prompt body")
	}
	// The line lands after the legacy builder's full output, on its own line.
	legacy := buildForemanPrompt(p, "demo-tick")
	if !strings.HasPrefix(out, legacy) || out != legacy+"\n"+waveBudgetLine(3) {
		t.Errorf("injected prompt must be legacy + \"\\n\" + WAVE_BUDGET line\n got: %q\nwant: %q", out, legacy+"\n"+waveBudgetLine(3))
	}
}

// TestBuildSpawnPrompt_WaveBudgetPartialDepth: cap=5, live 2-wave →
// WAVE_BUDGET: 3 (cap minus live depth).
func TestBuildSpawnPrompt_WaveBudgetPartialDepth(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "capped5"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 5,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	waveProj := database.Project{
		Name: "wave-host", RepoURL: "https://example.com/w", Workdir: "/tmp/w",
		Weight: 1, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true, NamespaceID: &ns,
	}
	if err := database.CreateProject(ctx, db, &waveProj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tick := &database.Tick{
		ID: "wave-host-t1", ProjectName: "wave-host",
		Status: database.StatusRunning, WorkerCount: 2,
		SpawnedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := database.CreateTick(ctx, db, tick); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	s := NewSpawner(db, 5)
	p := PackedProject{Name: "demo", Workdir: "/home/kara/demo", NamespaceID: ns}
	out := s.buildSpawnPrompt(p, "demo-tick")
	if !strings.Contains(out, "WAVE_BUDGET: 3") {
		t.Errorf("cap=5 + live depth 2 must inject \"WAVE_BUDGET: 3\", got:\n%s", out)
	}
}

// TestBuildSpawnPrompt_WaveBudgetZeroOnLiveWave: cap=3 with a live 3-wave
// in the namespace → the arithmetic itself clamps to WAVE_BUDGET: 0.
func TestBuildSpawnPrompt_WaveBudgetZeroOnLiveWave(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "capped2"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	waveProj := database.Project{
		Name: "wave-host", RepoURL: "https://example.com/w", Workdir: "/tmp/w",
		Weight: 1, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true, NamespaceID: &ns,
	}
	if err := database.CreateProject(ctx, db, &waveProj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tick := &database.Tick{
		ID: "wave-host-t1", ProjectName: "wave-host",
		Status: database.StatusRunning, WorkerCount: 3,
		SpawnedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := database.CreateTick(ctx, db, tick); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	s := NewSpawner(db, 5)
	p := PackedProject{Name: "demo", Workdir: "/home/kara/demo", NamespaceID: ns}
	out := s.buildSpawnPrompt(p, "demo-tick")
	if !strings.Contains(out, "WAVE_BUDGET: 0") {
		t.Errorf("cap=3 + live depth 3 must clamp to \"WAVE_BUDGET: 0\", got:\n%s", out)
	}
}

// TestBuildSpawnPrompt_WaveSerialForcesZeroBudget: WaveSerial (packer shed)
// short-circuits to budget 0 WITHOUT any depth query — the pack-time shed
// decision is authoritative.
func TestBuildSpawnPrompt_WaveSerialForcesZeroBudget(t *testing.T) {
	db := newTestDB(t)
	// Namespace does not even exist — WaveSerial must not need the lookup.
	s := NewSpawner(db, 5)
	p := PackedProject{
		Name: "demo", Workdir: "/home/kara/demo",
		NamespaceID: "ghost-ns", WaveSerial: true,
	}
	out := s.buildSpawnPrompt(p, "demo-tick")
	if !strings.Contains(out, "WAVE_BUDGET: 0") {
		t.Errorf("WaveSerial project must get \"WAVE_BUDGET: 0\" without a live-depth query, got:\n%s", out)
	}
}

// TestBuildSpawnPrompt_NoCapNoInjection: a namespace WITHOUT a cap (0 =
// unlimited) and an unnamespaced project inject NOTHING — the prompt is
// byte-identical to the pre-SCHED-GAP-113 buildForemanPrompt output.
func TestBuildSpawnPrompt_NoCapNoInjection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: "uncapped", Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	s := NewSpawner(db, 5)
	tickID := "demo-tick"

	// Unnamespaced project: no lookup at all.
	p := PackedProject{Name: "demo", Workdir: "/home/kara/demo", NamespacePrompt: "NS BASE"}
	out := s.buildSpawnPrompt(p, tickID)
	if strings.Contains(out, "WAVE_BUDGET") {
		t.Errorf("unnamespaced prompt must not contain WAVE_BUDGET, got:\n%s", out)
	}
	if out != buildForemanPrompt(p, tickID) {
		t.Error("unnamespaced prompt not byte-identical to legacy builder")
	}

	// Capped-0 namespace (the default): inject nothing, byte-identical.
	q := PackedProject{Name: "demo", Workdir: "/home/kara/demo", NamespacePrompt: "NS BASE", NamespaceID: "uncapped"}
	out2 := s.buildSpawnPrompt(q, tickID)
	if strings.Contains(out2, "WAVE_BUDGET") {
		t.Errorf("cap=0 namespace prompt must not contain WAVE_BUDGET, got:\n%s", out2)
	}
	if out2 != buildForemanPrompt(q, tickID) {
		t.Error("cap=0 namespace prompt not byte-identical to legacy builder")
	}

	// A replace-mode project prompt in a capped namespace keeps the line
	// (the footer ordering contract: nothing can lose it).
	r := PackedProject{
		Name: "demo", Workdir: "/home/kara/demo", NamespacePrompt: "NS BASE",
		Prompt: "REPLACEMENT", PromptMode: "replace", NamespaceID: "uncapped",
	}
	out3 := s.buildSpawnPrompt(r, tickID)
	if strings.Contains(out3, "WAVE_BUDGET") {
		t.Errorf("uncapped replace-mode must not contain WAVE_BUDGET, got:\n%s", out3)
	}
	if out3 != buildForemanPrompt(r, tickID) {
		t.Error("replace-mode prompt not byte-identical to legacy builder")
	}
}
