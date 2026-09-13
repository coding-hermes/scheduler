package scheduler_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── S12 §12 (SCHED-GAP-113): admission layer + slot accounting invariants.
// Composition-layer (WAVE_BUDGET prompt) tests live in
// schedgap113_prompt_test.go (package scheduler, for byte-identity against
// buildForemanPrompt).

// insertRunningWaveTick inserts a running tick row carrying worker_count —
// the DB footprint of one live wave (ticks.status='running',
// ticks.worker_count>0; SCHED-GAP-112 reads exactly this).
func insertRunningWaveTick(t *testing.T, db *sql.DB, project string, workerCount int) {
	t.Helper()
	ctx := context.Background()
	tick := &database.Tick{
		ID:          project + "-wave-113",
		ProjectName: project,
		Status:      database.StatusRunning,
		SpawnedAt:   time.Now().UTC().Format(time.RFC3339),
		WorkerCount: workerCount,
	}
	if err := database.CreateTick(ctx, db, tick); err != nil {
		t.Fatalf("CreateTick %s: %v", tick.ID, err)
	}
}

// ── Slot accounting invariants (S12 §12: "slots must not see workers") ────

// TestSlotPool_WaveTickOccupiesOneSlot: one Acquire for a project whose
// tick carries worker_count=3 (a wave) occupies exactly one slot — SlotPool
// has no worker awareness (W1).
func TestSlotPool_WaveTickOccupiesOneSlot(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 10)
	pool := scheduler.NewSlotPool(8, sp, lc)

	// A 3-worker wave tick: one slot, one running entry, one RunningSet entry.
	if !pool.Acquire(context.Background(), "wave-proj") {
		t.Fatal("Acquire failed")
	}
	if got := pool.Running(); got != 1 {
		t.Errorf("Running() = %d after one 3-worker wave tick, want 1 (W1: slots count ticks, not workers)", got)
	}
	if got := pool.Available(); got != 7 {
		t.Errorf("Available() = %d, want 7", got)
	}
	set := pool.RunningSet()
	if !set["wave-proj"] || len(set) != 1 {
		t.Errorf("RunningSet() = %v after one wave tick, want exactly [wave-proj] (W3)", set)
	}
	pool.Release("wave-proj")
	if got := pool.Running(); got != 0 {
		t.Errorf("Running() = %d after release, want 0", got)
	}
}

// TestRunningSet_UnaffectedByWorkerCount: worker_count changes on tick rows
// never change the running set — RunningSet is name-keyed refcounting over
// ticks only (W3).
func TestRunningSet_UnaffectedByWorkerCount(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "w3"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	for _, n := range []string{"w3-a", "w3-b"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &ns
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 10)
	pool := scheduler.NewSlotPool(8, sp, lc)

	pool.Acquire(context.Background(), "w3-a")
	pool.Acquire(context.Background(), "w3-b")

	// Live waves land on the tick rows (worker_count 3 and 8) — the
	// running set must be unchanged by either.
	insertRunningWaveTick(t, db, "w3-a", 3)
	insertRunningWaveTick(t, db, "w3-b", 8)

	set := pool.RunningSet()
	if len(set) != 2 || !set["w3-a"] || !set["w3-b"] {
		t.Errorf("RunningSet() = %v, want exactly {w3-a, w3-b} regardless of worker_count (W3)", set)
	}
	if got := pool.Running(); got != 2 {
		t.Errorf("Running() = %d, want 2 (ticks), not worker sums", got)
	}
}

// TestNamespaceCap_CountsWaveTickAsOne: a running 3-worker wave occupies
// ONE namespace max_concurrent unit — W2. With max_concurrent=2 and one
// 3-worker wave live, exactly one more tick of the namespace packs (the cap
// counts ticks); a third does not. The RED-check stated in the AC (reverse
// the cap to count workers → this test fails) is verified in a scratch
// worktree, not shipped.
func TestNamespaceCap_CountsWaveTickAsOne(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "w2"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100,
		MaxConcurrent: 2, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	for _, n := range []string{"w2-a", "w2-b", "w2-c"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &ns
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	// One live 3-worker wave tick in the namespace (w2-a running).
	insertRunningWaveTick(t, db, "w2-a", 3)

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)

	// w2-b packs (1 running tick + 1 selected = cap 2: the 3 WORKERS count
	// as ONE tick); w2-c must not.
	got, err := p.Pick(time.Now(), map[string]bool{"w2-a": true})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	packedW2 := 0
	for _, gp := range got {
		if gp.NamespaceID == "w2" {
			packedW2++
		}
	}
	if packedW2 != 1 {
		t.Errorf("namespace w2 packed %d projects with a 3-worker wave live (cap=2 ticks), want exactly 1 more (W2: the wave counts as one tick)", packedW2)
	}
}

// ── Admission layer: tick-boundary shed (S12 §12 integration step 7) ──────

// TestPack_WaveShedSerialOnly: wave_workers_cap=3 with a live 3-wave → the
// next eval still packs the namespace's project (it is not blocked) but
// ONLY into the serial path — PackedProject.WaveSerial is set, which
// resolves to WAVE_BUDGET: 0 at spawn. Another namespace is unaffected.
func TestPack_WaveShedSerialOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	shedNs, otherNs := "shed", "other"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: shedNs, Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace shed: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: otherNs, Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace other: %v", err)
	}
	for _, n := range []string{"shed-a", "shed-b"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &shedNs
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	p := makeProject("other-a", 1, 5, 0, 1.0)
	p.NamespaceID = &otherNs
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject other-a: %v", err)
	}

	// shed-a runs a live 3-worker wave.
	insertRunningWaveTick(t, db, "shed-a", 3)

	nss, err := database.ListNamespaces(ctx, db, false)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	projs, err := database.ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	mp.SetWaveShedDB(db)

	result := mp.Pack(projs, nss, calc, nil, []string{"shed-a"}, time.Now())

	var shedPacked, otherPacked int
	shedSerial, otherSerial := false, false
	for _, pp := range result.Projects {
		switch pp.Name {
		case "shed-b":
			shedPacked++
			shedSerial = pp.WaveSerial
		case "other-a":
			otherPacked++
			otherSerial = pp.WaveSerial
		}
	}
	if shedPacked != 1 {
		t.Errorf("shed-b packed = %d, want 1 — a shed namespace still packs (serial), it is not blocked", shedPacked)
	}
	if !shedSerial {
		t.Error("shed-b must carry WaveSerial=true (WAVE_BUDGET: 0 — no second wave in the namespace this tick)")
	}
	if otherPacked != 1 {
		t.Errorf("other-a packed = %d, want 1 (uncapped namespace unaffected)", otherPacked)
	}
	if otherSerial {
		t.Error("other-a must NOT be WaveSerial — an uncapped namespace is never shed")
	}
}

// TestPack_WaveShedRequiresCap: a live wave in a namespace WITHOUT a cap
// never sheds — unlimited namespaces keep pre-113 packing (default-off).
func TestPack_WaveShedRequiresCap(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "noshed"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	for _, n := range []string{"ns-a", "ns-b"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &ns
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	insertRunningWaveTick(t, db, "ns-a", 3)

	nss, _ := database.ListNamespaces(ctx, db, false)
	projs, _ := database.ListProjects(ctx, db, false)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	mp := scheduler.NewMultiPoolPacker(100, 10, nil)
	mp.SetWaveShedDB(db)

	result := mp.Pack(projs, nss, calc, nil, []string{"ns-a"}, time.Now())
	for _, pp := range result.Projects {
		if pp.Name == "ns-b" && pp.WaveSerial {
			t.Error("ns-b must not be WaveSerial — namespace has no wave_workers_cap (unlimited, default-off)")
		}
	}
}

// TestPack_WaveShedDisabledWithoutDB: SetWaveShedDB(nil) — the default —
// leaves packing byte-identical: a capped namespace with a live wave packs
// non-serial because no shed scan ran.
func TestPack_WaveShedDisabledWithoutDB(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns := "off"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: ns, Weight: 10, Reserved: 1, HardCap: 100,
		Enabled: true, WaveWorkersCap: 3,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	for _, n := range []string{"off-a", "off-b"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &ns
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	insertRunningWaveTick(t, db, "off-a", 3)

	nss, _ := database.ListNamespaces(ctx, db, false)
	projs, _ := database.ListProjects(ctx, db, false)
	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	mp := scheduler.NewMultiPoolPacker(100, 10, nil) // no SetWaveShedDB

	result := mp.Pack(projs, nss, calc, nil, []string{"off-a"}, time.Now())
	for _, pp := range result.Projects {
		if pp.Name == "off-b" && pp.WaveSerial {
			t.Error("off-b must not be WaveSerial — shed disabled (nil DB), packing stays pre-113")
		}
	}
}
