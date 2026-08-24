package scheduler

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── SCHED-GAP-065: idle-tick model routing ──────────────────────────────
//
// Chain kind is picked from the board's pending-task count BEFORE dispatch:
// zero pending (or no board) = idle tick, resolved via the idle chain —
// the project idle tier (fleet.toml idle_model/idle_provider) plus the
// spawner-level env idle lane PREPENDED to the regular SCHED-GAP-064 chain.
// Empty idle fields are not present() and fall through naturally, so a
// project with no idle config behaves exactly as before (no regression).
// The gateway 401/403 retry advances within the SAME chain the tick was
// spawned with, so a rejected idle tick never jumps straight to the work
// lane.

// writeGap065Board writes a JSONL board with the given status lines into
// workdir/.coding-hermes/board/tasks.jsonl.
func writeGap065Board(t *testing.T, workdir string, lines ...string) {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", boardDir, err)
	}
	content := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestSpawnChainForKind_IdleResolution pins the idle-chain semantics on the
// pure resolver: idle tiers prepend, empty idle fields fall through, the
// env idle lane is gated by NoGlobalFallback like the other global tiers,
// and the work kind ignores idle tiers entirely.
func TestSpawnChainForKind_IdleResolution(t *testing.T) {
	tests := []struct {
		name    string
		project PackedProject
		idle    bool
		wantM   string
		wantP   string
	}{
		{
			name:    "work kind ignores idle tiers",
			project: PackedProject{Model: "work-model", Provider: "work-provider", IdleModel: "idle-model", IdleProvider: "idle-provider"},
			idle:    false,
			wantM:   "work-model",
			wantP:   "work-provider",
		},
		{
			name:    "idle kind uses project idle tier",
			project: PackedProject{Model: "work-model", Provider: "work-provider", IdleModel: "idle-model", IdleProvider: "idle-provider"},
			idle:    true,
			wantM:   "idle-model",
			wantP:   "idle-provider",
		},
		{
			name:    "idle kind with empty project idle tier uses global idle lane",
			project: PackedProject{Model: "work-model", Provider: "work-provider"},
			idle:    true,
			wantM:   "global-idle-model",
			wantP:   "global-idle-provider",
		},
		{
			name:    "idle kind with no idle config falls through to project primary",
			project: PackedProject{Model: "work-model", Provider: "work-provider"},
			idle:    true,
			wantM:   "global-idle-model", // global idle lane sits ahead of the regular chain
			wantP:   "global-idle-provider",
		},
		{
			name:    "idle model only — provider falls through to regular chain",
			project: PackedProject{Model: "work-model", Provider: "work-provider", IdleModel: "idle-model"},
			idle:    true,
			wantM:   "idle-model",
			wantP:   "global-idle-provider",
		},
		{
			name:    "no_global_fallback gates the global idle lane and globals",
			project: PackedProject{Model: "work-model", Provider: "work-provider", IdleModel: "idle-model", NoGlobalFallback: true},
			idle:    true,
			wantM:   "idle-model",
			wantP:   "work-provider", // env idle lane gated → provider falls to project primary
		},
		{
			name:    "no_global_fallback with empty idle tier resolves like the regular chain under the flag",
			project: PackedProject{Model: "work-model", Provider: "work-provider", NoGlobalFallback: true},
			idle:    true,
			wantM:   "work-model",
			wantP:   "work-provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newChainSpawner(nil, 1)
			s.idleModel = "global-idle-model"
			s.idleProvider = "global-idle-provider"
			m, p := resolveChain(s.spawnChainForKind(tt.project, tt.idle))
			if m != tt.wantM || p != tt.wantP {
				t.Errorf("resolveChain(spawnChainForKind(%+v, idle=%v)) = (%q, %q), want (%q, %q)",
					tt.project, tt.idle, m, p, tt.wantM, tt.wantP)
			}
		})
	}
}

// TestSpawnChainForKind_NoIdleConfigIdenticalToWork pins the no-regression
// contract: with every idle field empty (project + env), the idle chain is
// entry-for-entry identical to the regular work chain.
func TestSpawnChainForKind_NoIdleConfigIdenticalToWork(t *testing.T) {
	s := newChainSpawner(nil, 1)
	projects := []PackedProject{
		{},
		{Model: "m", Provider: "p"},
		{Model: "m", FallbackModel: "fm"},
		{NoGlobalFallback: true},
		{Model: "m", Provider: "p", NoGlobalFallback: true},
	}
	for _, proj := range projects {
		workM, workP := s.resolveModelProvider(proj)
		idleM, idleP := resolveChain(s.spawnChainForKind(proj, true))
		if workM != idleM || workP != idleP {
			t.Errorf("project %+v: idle chain resolved (%q, %q), work chain (%q, %q) — must be identical with no idle config",
				proj, idleM, idleP, workM, workP)
		}
	}
}

// TestTickIsIdle pins the chain-kind selection: nil counter biases to work,
// a board with pending rows is work, a board with only terminal rows (or no
// board at all) is idle.
func TestTickIsIdle(t *testing.T) {
	// Nil counter — conservative work default.
	s := newChainSpawner(nil, 1)
	if s.tickIsIdle(PackedProject{Workdir: t.TempDir()}) {
		t.Error("tickIsIdle with nil counter = true, want false (nil counter must bias to work)")
	}

	// Board with a pending row → work.
	workDir := t.TempDir()
	writeGap065Board(t, workDir, `{"id":"A","status":"pending"}`)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))
	if s.tickIsIdle(PackedProject{Workdir: workDir}) {
		t.Error("tickIsIdle with 1 pending row = true, want false")
	}

	// Board with only terminal rows → idle.
	idleDir := t.TempDir()
	writeGap065Board(t, idleDir, `{"id":"A","status":"complete"}`, `{"id":"B","status":"done"}`)
	if !s.tickIsIdle(PackedProject{Workdir: idleDir}) {
		t.Error("tickIsIdle with 0 pending rows = false, want true")
	}

	// No board file at all → idle (CountPending returns 0).
	if !s.tickIsIdle(PackedProject{Workdir: t.TempDir()}) {
		t.Error("tickIsIdle with no board file = false, want true")
	}
}

// TestSpawn_GatewayIdleTickUsesIdleLane is the PASS criterion: a project
// whose board has ZERO pending tasks dispatches on the idle lane — the
// gateway records the idle model/provider, not the work primary.
func TestSpawn_GatewayIdleTickUsesIdleLane(t *testing.T) {
	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	workdir := t.TempDir()
	writeGap065Board(t, workdir, `{"id":"DONE-A","status":"complete"}`)
	project := PackedProject{
		Name:         "gap065-idle",
		Workdir:      workdir,
		Model:        "work-model",
		Provider:     "work-provider",
		IdleModel:    "idle-model",
		IdleProvider: "idle-provider",
	}
	tick, err := s.Spawn(project, "gap065-idle-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	if dispatches[0].model != "idle-model" || dispatches[0].provider != "idle-provider" {
		t.Errorf("idle tick dispatched (%q, %q), want idle lane (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "idle-model", "idle-provider")
	}
}

// TestSpawn_GatewayWorkTickUsesPrimary: a board with a pending row keeps the
// work chain — the idle tier is NOT consulted.
func TestSpawn_GatewayWorkTickUsesPrimary(t *testing.T) {
	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	workdir := t.TempDir()
	writeGap065Board(t, workdir, `{"id":"TASK-A","status":"pending"}`)
	project := PackedProject{
		Name:         "gap065-work",
		Workdir:      workdir,
		Model:        "work-model",
		Provider:     "work-provider",
		IdleModel:    "idle-model",
		IdleProvider: "idle-provider",
	}
	tick, err := s.Spawn(project, "gap065-work-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	if dispatches[0].model != "work-model" || dispatches[0].provider != "work-provider" {
		t.Errorf("work tick dispatched (%q, %q), want primary (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "work-model", "work-provider")
	}
}

// TestSpawn_GatewayIdleRetryStaysInIdleChain: when the gateway 401s the idle
// tier, the retry advances within the SAME (idle) chain — here to the global
// idle lane — instead of jumping straight to the work model.
func TestSpawn_GatewayIdleRetryStaysInIdleChain(t *testing.T) {
	gw := &chainGatewayServer{rejectModel: "idle-model"}
	s := newChainSpawnerForGateway(t, gw)
	s.idleModel = "global-idle-model"
	s.idleProvider = "global-idle-provider"
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	workdir := t.TempDir() // no board → 0 pending → idle
	project := PackedProject{
		Name:         "gap065-idle-retry",
		Workdir:      workdir,
		Model:        "work-model",
		Provider:     "work-provider",
		IdleModel:    "idle-model",
		IdleProvider: "idle-provider",
	}
	tick, err := s.Spawn(project, "gap065-idle-retry-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway retry success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (idle tier + one retry within the idle chain)", len(dispatches))
	}
	if dispatches[0].model != "idle-model" || dispatches[0].status != http.StatusUnauthorized {
		t.Errorf("first dispatch = (%q, %d), want (idle-model, 401)", dispatches[0].model, dispatches[0].status)
	}
	if dispatches[1].model != "global-idle-model" || dispatches[1].provider != "global-idle-provider" || dispatches[1].status != http.StatusOK {
		t.Errorf("retry dispatch = (%q, %q, %d), want (global-idle-model, global-idle-provider, 200) — retry must advance within the idle chain",
			dispatches[1].model, dispatches[1].provider, dispatches[1].status)
	}
}

// TestSpawn_ExecIdleTickUsesIdleLane: the exec path must receive the
// idle-resolved -m/--provider values, verified against the fake hermes
// binary's captured argv.
func TestSpawn_ExecIdleTickUsesIdleLane(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 1)
	s.SetGatewayClient(nil) // force exec path
	s.SetNoExecFallback(false)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	dir := t.TempDir()
	captureFile := filepath.Join(dir, "capture.txt")
	script := `#!/bin/bash
echo "$@" > "` + captureFile + `"
exit 0
`
	scriptPath := filepath.Join(dir, "hermes")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	workdir := t.TempDir() // no board → idle
	project := PackedProject{
		Name:         "gap065-exec-idle",
		Workdir:      workdir,
		Model:        "work-model",
		Provider:     "work-provider",
		IdleModel:    "idle-model",
		IdleProvider: "idle-provider",
	}
	tick, err := s.Spawn(project, "gap065-exec-idle-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on exec path")
	}
	tick.Wait()

	args, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	joined := string(args)
	if !strings.Contains(joined, "-m idle-model") {
		t.Errorf("exec args missing idle model: %s", joined)
	}
	if !strings.Contains(joined, "--provider idle-provider") {
		t.Errorf("exec args missing idle provider: %s", joined)
	}
}

// TestSpawn_DispatchLogLine pins the PASS-criterion log line: every dispatch
// records the resolved model/provider AND the chain kind (idle|work).
func TestSpawn_DispatchLogLine(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	// Idle tick (no board).
	idleProject := PackedProject{
		Name:      "gap065-log-idle",
		Workdir:   t.TempDir(),
		Model:     "work-model",
		Provider:  "work-provider",
		IdleModel: "idle-model",
	}
	tick, err := s.Spawn(idleProject, "gap065-log-idle-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn idle: %v", err)
	}
	tick.Wait()

	// Work tick (pending row).
	workDir := t.TempDir()
	writeGap065Board(t, workDir, `{"id":"TASK-A","status":"pending"}`)
	workProject := PackedProject{
		Name:      "gap065-log-work",
		Workdir:   workDir,
		Model:     "work-model",
		Provider:  "work-provider",
		IdleModel: "idle-model",
	}
	tick, err = s.Spawn(workProject, "gap065-log-work-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn work: %v", err)
	}
	tick.Wait()

	out := buf.String()
	idleLine := "SPAWN: gap065-log-idle tick=gap065-log-idle-2026-08-24-00-00-00 chain=idle model=\"idle-model\""
	if !strings.Contains(out, idleLine) {
		t.Errorf("log missing idle dispatch line %q; got:\n%s", idleLine, out)
	}
	workLine := "SPAWN: gap065-log-work tick=gap065-log-work-2026-08-24-00-00-00 chain=work model=\"work-model\" provider=\"work-provider\""
	if !strings.Contains(out, workLine) {
		t.Errorf("log missing work dispatch line %q; got:\n%s", workLine, out)
	}
}

// TestSpawn_GatewayIdleNoIdleConfigFallsThrough: an idle tick on a project
// with NO idle config (and no env idle lane) dispatches on the regular
// chain — the no-regression contract at the spawn level.
func TestSpawn_GatewayIdleNoIdleConfigFallsThrough(t *testing.T) {
	gw := &chainGatewayServer{}
	s := newChainSpawnerForGateway(t, gw)
	s.SetPendingCounter(NewPendingTaskCounter(time.Minute))

	project := PackedProject{
		Name:     "gap065-idle-noconfig",
		Workdir:  t.TempDir(), // no board → idle, but no idle tier anywhere
		Model:    "work-model",
		Provider: "work-provider",
	}
	tick, err := s.Spawn(project, "gap065-idle-noconfig-2026-08-24-00-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
	}
	tick.Wait()

	dispatches := gw.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(dispatches))
	}
	if dispatches[0].model != "work-model" || dispatches[0].provider != "work-provider" {
		t.Errorf("idle tick with no idle config dispatched (%q, %q), want regular chain (%q, %q)",
			dispatches[0].model, dispatches[0].provider, "work-model", "work-provider")
	}
}
