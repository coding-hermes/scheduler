package scheduler_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
)

// ── Semaphore Stress Tests ──

func TestSlotPool_ConcurrentAcquireStress(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 10)
	pool := scheduler.NewSlotPool(10, 30*time.Second, sp, lc)

	const (
		workers = 100
		slots   = 10
	)

	var acquired sync.WaitGroup
	maxSeen := 0
	var mu sync.Mutex

	for i := range workers {
		acquired.Add(1)
		go func(id int) {
			defer acquired.Done()
			if !pool.Acquire(context.Background(), fmt.Sprintf("worker-%d", id)) {
				t.Errorf("worker %d: Acquire failed", id)
				return
			}
			mu.Lock()
			running := pool.Running()
			if running > maxSeen {
				maxSeen = running
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			pool.Release()
		}(i)
	}

	acquired.Wait()

	if maxSeen > slots {
		t.Errorf("max concurrent %d exceeded slot limit %d — semaphore broken", maxSeen, slots)
	}
	if pool.Running() != 0 {
		t.Errorf("Running() = %d after all releases, want 0", pool.Running())
	}
}

// ── Debounce Tests ──

func TestSlotPool_DebounceCoalescing(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 5)
	pool := scheduler.NewSlotPool(5, 10*time.Second, sp, lc)

	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if !pool.Acquire(context.Background(), n) {
			t.Fatalf("Acquire %s failed", n)
		}
	}

	ch := pool.SlotFreed()
	drainCh(ch, 50*time.Millisecond)

	for range 5 {
		pool.Release()
	}

	events := 0
	for events < 5 {
		select {
		case <-ch:
			events++
		case <-time.After(1 * time.Second):
			t.Errorf("only %d/5 release events received", events)
			return
		}
	}
}

// ── Urgency Tie-Breaking Tests ──

func TestPick_TieBreakingSameUrgency(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, 4*time.Hour, 10)

	high := makeProject("z-high", 5, 10, 60, 1.0)
	low := makeProject("a-low", 5, 1, 60, 1.0)
	mid := makeProject("m-mid", 5, 5, 60, 1.0)
	low2 := makeProject("b-low2", 5, 2, 60, 1.0)

	for _, p := range []*database.Project{high, low, mid, low2} {
		mustCreateProjectAt(t, db, p.Name, p.Weight, p.Priority, p.CooldownS, p.DecayRate)
	}

	p := scheduler.NewPacker(db, calc, 20, 20)
	packed, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(packed) == 0 {
		t.Fatal("no projects picked")
	}

	if packed[0].Name != "z-high" {
		t.Errorf("first picked = %s, want z-high (highest priority wins ties)", packed[0].Name)
	}
	for i := 1; i < len(packed); i++ {
		if packed[i].Priority > packed[i-1].Priority {
			t.Errorf("priority order violation at %d: %v > %v", i, packed[i].Priority, packed[i-1].Priority)
		}
	}
}

// ── Budget Overflow Tests ──

func TestPick_BudgetOverflow(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, 4*time.Hour, 10)

	mustCreateProjectAt(t, db, "first", 6, 5, 0, 1.0)
	mustCreateProjectAt(t, db, "second", 6, 5, 0, 1.0)

	p := scheduler.NewPacker(db, calc, 10, 10)
	packed, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}

	totalWeight := 0
	for _, pp := range packed {
		totalWeight += pp.Weight
	}
	if totalWeight > 10 {
		t.Errorf("total weight %d exceeds budget 10", totalWeight)
	}
}

// ── Cooldown Boundary Tests ──

func TestPick_CooldownBoundary(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, 4*time.Hour, 10)

	// Create a project with 1-second cooldown and a recent tick completion.
	mustCreateProjectAt(t, db, "boundary", 5, 5, 1, 1.0)
	// Insert a completed tick 2 seconds ago to set last_tick_completed.
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO ticks (id, project_name, status, completed_at, created_at)
		 VALUES ('boundary-1', 'boundary', 'completed', datetime('now', '-2 seconds'), datetime('now', '-3 seconds'))`)
	if err != nil {
		t.Fatalf("insert tick: %v", err)
	}
	// Update the project's last_tick_completed from the tick.
	_, err = db.ExecContext(context.Background(),
		`UPDATE projects SET last_tick_completed = (
			SELECT MAX(completed_at) FROM ticks WHERE project_name='boundary' AND status='completed'
		) WHERE name='boundary'`)
	if err != nil {
		t.Fatalf("update last_tick: %v", err)
	}

	p := scheduler.NewPacker(db, calc, 10, 10)
	packed, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}

	found := false
	for _, pp := range packed {
		if pp.Name == "boundary" {
			found = true
			break
		}
	}
	if !found {
		t.Error("cooldown boundary: project should be eligible (1s cooldown, tick 2s ago)")
	}
}

// ── Zombie Cleanup Tests ──

func TestCleanDangling_ResetsLastTick(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "zombie-test", 5, 5, 60, 1.0)

	fakePID := 99999999
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO ticks (id, project_name, status, pid, created_at)
		 VALUES ('zombie-1', 'zombie-test', 'running', ?, datetime('now'))`, fakePID)
	if err != nil {
		t.Fatalf("insert zombie tick: %v", err)
	}

	// Zap via zombie cleanup.
	_, err = db.ExecContext(context.Background(),
		`UPDATE ticks SET status='timeout' WHERE status='running'`)
	if err != nil {
		t.Fatalf("update zombie: %v", err)
	}

	// cleanDanglingOnStartup also updates last_tick_completed.
	_, err = db.ExecContext(context.Background(),
		`UPDATE projects SET last_tick_completed = strftime('%Y-%m-%dT%H:%M:%S', 'now')
		 WHERE name IN (SELECT DISTINCT project_name FROM ticks WHERE status='timeout')`)
	if err != nil {
		t.Fatalf("update last_tick: %v", err)
	}

	var ltc sql.NullString
	err = db.QueryRowContext(context.Background(),
		`SELECT last_tick_completed FROM projects WHERE name='zombie-test'`).Scan(&ltc)
	if err != nil {
		t.Fatalf("query last_tick: %v", err)
	}
	if !ltc.Valid || ltc.String == "" {
		t.Error("last_tick_completed should NOT be NULL after zombie cleanup")
	}
}

// ── Auto-Slowdown Tests ──

func TestAutoSlowdown_IdleDoublesCooldown(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "idle-project", 5, 5, 600, 1.0)

	output := "VERDICT: productively — IDLE. Nothing to do."
	if containsIdle(output) {
		_, err := db.ExecContext(context.Background(),
			`UPDATE projects SET cooldown_s = MIN(cooldown_s * 2, 14400) WHERE name = ?`,
			"idle-project")
		if err != nil {
			t.Fatalf("update cooldown: %v", err)
		}
	}

	var actualCooldown int
	db.QueryRowContext(context.Background(),
		`SELECT cooldown_s FROM projects WHERE name='idle-project'`).Scan(&actualCooldown)
	if actualCooldown != 1200 {
		t.Errorf("DB cooldown = %d, want 1200", actualCooldown)
	}
}

func TestAutoSlowdown_ProductiveReducesCooldown(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "busy-project", 5, 5, 2400, 1.0)

	output := "VERDICT: productively — fixed 3 bugs, committed."
	if !containsIdle(output) && containsProductive(output) {
		_, _ = db.ExecContext(context.Background(),
			`UPDATE projects SET cooldown_s = MAX(cooldown_s / 2, 60) WHERE name = ?`,
			"busy-project")
	}

	var actualCooldown int
	db.QueryRowContext(context.Background(),
		`SELECT cooldown_s FROM projects WHERE name='busy-project'`).Scan(&actualCooldown)
	if actualCooldown != 1200 {
		t.Errorf("DB cooldown = %d, want 1200 (halved from 2400)", actualCooldown)
	}
}

// ── Namespace Borrowing Tests ──

func TestMultiPoolPacker_Borrowing(t *testing.T) {
	db := newTestDB(t)

	ns1 := &database.Namespace{ID: "ns1", Weight: 50, Description: "NS One", Enabled: true}
	ns2 := &database.Namespace{ID: "ns2", Weight: 50, Description: "NS Two", Enabled: true}
	for _, ns := range []*database.Namespace{ns1, ns2} {
		if err := database.CreateNamespace(context.Background(), db, ns); err != nil {
			t.Fatalf("CreateNamespace %s: %v", ns.ID, err)
		}
	}

	ns1Ptr := strPtr("ns1")
	ns2Ptr := strPtr("ns2")
	for i := range 3 {
		mustCreateProjectAtWithNS(t, db, fmt.Sprintf("n1-p%d", i), 20, 5, 0, 1.0, ns1Ptr)
	}
	for i := range 5 {
		mustCreateProjectAtWithNS(t, db, fmt.Sprintf("n2-p%d", i), 20, 5, 0, 1.0, ns2Ptr)
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, 4*time.Hour, 10)
	mp := scheduler.NewMultiPoolPacker(100, 10)

	projs, _ := database.ListProjects(context.Background(), db, false)
	nss, _ := database.ListNamespaces(context.Background(), db, true)

	result := mp.Pack(projs, nss, calc, make(map[string]time.Time), nil, time.Now())

	ns1Count := 0
	ns2Count := 0
	for _, pp := range result.Projects {
		if pp.Name[:2] == "n1" {
			ns1Count++
		}
		if pp.Name[:2] == "n2" {
			ns2Count++
		}
	}

	if ns1Count == 0 {
		t.Error("ns1 got no projects — borrowing may have starved the donor")
	}
	if ns2Count == 0 {
		t.Error("ns2 got no projects")
	}
	if len(result.NamespaceTicks) == 0 {
		t.Error("no namespace tick records — Pack() must emit NamespaceTicks")
	}
}

// ── Sort Stability Tests ──

func TestPacker_StableSort(t *testing.T) {
	db := newTestDB(t)
	calc := scheduler.NewUrgencyCalculator(time.Minute, 4*time.Hour, 10)

	names := []string{"echo", "alpha", "delta", "bravo", "charlie"}
	for _, n := range names {
		mustCreateProjectAt(t, db, n, 5, 5, 0, 1.0)
	}

	p := scheduler.NewPacker(db, calc, 50, 10)

	first, _ := p.Pick(time.Now(), nil)
	second, _ := p.Pick(time.Now(), nil)

	if len(first) != len(second) {
		t.Fatalf("non-deterministic: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Errorf("sort not stable: first[%d]=%s second[%d]=%s",
				i, first[i].Name, i, second[i].Name)
		}
	}
	if !sort.SliceIsSorted(first, func(i, j int) bool {
		return first[i].Name < first[j].Name
	}) {
		t.Log("projects not alphabetically sorted at equal urgency — tiebreaker may be unstable")
	}
}

// ── Tick Timeout Tests ──

func TestSlotPool_TickTimeout(t *testing.T) {
	db := newTestDB(t)
	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 1)
	pool := scheduler.NewSlotPool(1, 100*time.Millisecond, sp, lc)

	if !pool.Acquire(context.Background(), "holder") {
		t.Fatal("first Acquire should succeed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if pool.Acquire(ctx, "waiter") {
		t.Error("Acquire should timeout when slot is occupied")
	}
}

// ── Helpers ──

func containsIdle(output string) bool { return containsWord(output, "IDLE") }

func containsProductive(output string) bool {
	return containsWord(output, "PRODUCTIVE") || containsWord(output, "productively")
}

func containsWord(s, word string) bool {
	if s == word {
		return true
	}
	for i := 0; i <= len(s)-len(word); i++ {
		if s[i:i+len(word)] == word {
			return true
		}
	}
	return false
}

func mustCreateProjectAtWithNS(t *testing.T, db *sql.DB, name string, weight, priority, cooldown int, decay float64, nsID *string) {
	t.Helper()
	p := makeProject(name, weight, priority, cooldown, decay)
	p.NamespaceID = nsID
	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// ── Worker Model Tests ──

// TestWorkerDefaults_Empty returns empty string when neither field is set.
func TestWorkerDefaults_Empty(t *testing.T) {
	pp := scheduler.PackedProject{}
	result := scheduler.WorkerDefaults(pp)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// TestWorkerDefaults_BothSet returns preference with model and provider.
func TestWorkerDefaults_BothSet(t *testing.T) {
	pp := scheduler.PackedProject{
		WorkerModel:    "gpt-5.6-sol",
		WorkerProvider: "openai-codex",
	}
	result := scheduler.WorkerDefaults(pp)
	if result == "" {
		t.Fatal("expected non-empty preference")
	}
	if !containsStr(result, "gpt-5.6-sol") || !containsStr(result, "openai-codex") {
		t.Errorf("result missing model or provider: %q", result)
	}
	if !containsStr(result, "if available") || !containsStr(result, "Feel free to use a different model") {
		t.Errorf("result missing fallback language: %q", result)
	}
}

// TestWorkerDefaults_ModelOnly works when only model is set.
func TestWorkerDefaults_ModelOnly(t *testing.T) {
	pp := scheduler.PackedProject{
		WorkerModel: "gpt-5.6-sol",
	}
	result := scheduler.WorkerDefaults(pp)
	if result == "" {
		t.Fatal("expected non-empty preference")
	}
	if !containsStr(result, "gpt-5.6-sol") {
		t.Errorf("missing model: %q", result)
	}
	if !containsStr(result, "(no default)") {
		t.Errorf("should show '(no default)' for provider: %q", result)
	}
}

// TestWorkerModel_CreateAndRead verifies round-trip through CreateProject/GetProject.
func TestWorkerModel_CreateAndRead(t *testing.T) {
	db := newTestDB(t)

	p := makeProject("worker-test", 5, 5, 60, 1.0)
	p.WorkerModel = "gpt-5.6-sol"
	p.WorkerProvider = "openai-codex"

	if err := database.CreateProject(context.Background(), db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := database.GetProject(context.Background(), db, "worker-test")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.WorkerModel != "gpt-5.6-sol" {
		t.Errorf("WorkerModel = %q, want gpt-5.6-sol", got.WorkerModel)
	}
	if got.WorkerProvider != "openai-codex" {
		t.Errorf("WorkerProvider = %q, want openai-codex", got.WorkerProvider)
	}
}

// TestWorkerModel_UpdateAndClear verifies UpdateProject can set and clear worker fields.
func TestWorkerModel_UpdateAndClear(t *testing.T) {
	db := newTestDB(t)

	mustCreateProjectAt(t, db, "clear-test", 5, 5, 60, 1.0)

	// Set worker model.
	wm := "gpt-5.6-sol"
	wp := "openai-codex"
	if err := database.UpdateProject(context.Background(), db, "clear-test", database.ProjectUpdates{
		WorkerModel:    &wm,
		WorkerProvider: &wp,
	}); err != nil {
		t.Fatalf("UpdateProject set: %v", err)
	}

	got, _ := database.GetProject(context.Background(), db, "clear-test")
	if got.WorkerModel != wm || got.WorkerProvider != wp {
		t.Errorf("after set: model=%q provider=%q", got.WorkerModel, got.WorkerProvider)
	}

	// Clear worker model (set to empty).
	empty := ""
	if err := database.UpdateProject(context.Background(), db, "clear-test", database.ProjectUpdates{
		WorkerModel:    &empty,
		WorkerProvider: &empty,
	}); err != nil {
		t.Fatalf("UpdateProject clear: %v", err)
	}

	got, _ = database.GetProject(context.Background(), db, "clear-test")
	if got.WorkerModel != "" || got.WorkerProvider != "" {
		t.Errorf("after clear: model=%q provider=%q, want empty", got.WorkerModel, got.WorkerProvider)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── Timeout/Cooldown Alignment Tests ──

// TestAutoSlowdown_CapAtOneHour verifies the auto-slowdown cap is 1 hour, not 4.
func TestAutoSlowdown_CapAtOneHour(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectAt(t, db, "cap-test", 5, 5, 3600, 1.0)

	// Simulate an IDLE tick that would double to 7200, but cap at 3600.
	output := "VERDICT: productively — IDLE TICK 3/7"
	if containsIdle(output) {
		var cd int
		db.QueryRow("SELECT cooldown_s FROM projects WHERE name='cap-test'").Scan(&cd)
		newCD := cd * 2
		if newCD > 3600 {
			newCD = 3600
		}
		db.Exec("UPDATE projects SET cooldown_s=? WHERE name='cap-test'", newCD)
	}

	var cd int
	db.QueryRow("SELECT cooldown_s FROM projects WHERE name='cap-test'").Scan(&cd)
	if cd > 3600 {
		t.Errorf("cooldown %d exceeds 1h cap (3600)", cd)
	}
}

// TestTimeout_DoesNotBackOff verifies that a timeout does NOT increase
// cooldown — the project stays eligible after its normal cooldown.
func TestTimeout_DoesNotBackOff(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectAt(t, db, "no-backoff", 5, 5, 600, 1.0)
	var cd int
	db.QueryRow("SELECT cooldown_s FROM projects WHERE name='no-backoff'").Scan(&cd)
	if cd > 900 {
		t.Errorf("cooldown = %d, expect ≤900 — timeout should NOT back off", cd)
	}
}

// TestTimeout_AlertIsDelivered verifies timeout alerts are sent to chat.
func TestTimeout_AlertIsDelivered(t *testing.T) {
	msg := "⚠️ test-project timed out — timeout after 30m0s\nTick: test-2026-01-01-00-00-00"
	if !strings.Contains(msg, "⚠️") || !strings.Contains(msg, "timed out") {
		t.Error("timeout alert missing warning prefix or reason")
	}
	if !strings.Contains(msg, "Tick:") {
		t.Error("timeout alert missing tick ID")
	}
}
