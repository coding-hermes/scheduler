package scheduler

// ── SCHED-GAP-115 (S12 §11): wave cost attribution ──────────────────────
//
// Attribution semantics under test (board AC):
//   - ticks.cost_usd stays the SINGLE money figure; worker model cost is
//     attribution-only (W4) — the RED check asserts the tick total gains
//     ONLY worktree judge cost, never the worker sum;
//   - realized manifest figure wins per worker; a 0 figure gets the
//     per-worker average estimate (tick cost ÷ worker_count);
//   - serial ticks (worker_count=0) stay byte-identical to pre-v27;
//   - attribution survives the 114 recovery path — the abandoned parent
//     keeps its attribution, the recovery tick counts only its own wave.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ensureAttributionProject creates the shared "wave-proj" project row once
// per test DB (several tests seed two ticks on one project).
func ensureAttributionProject(t *testing.T, db *sql.DB, workdir string) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE name = 'wave-proj'`).Scan(&n); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if n > 0 {
		return
	}
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name: "wave-proj", Workdir: workdir, Weight: 10, Priority: 5,
		CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
}

// seedAttributionTick creates a terminal tick row with the given cost and
// one tick_workers row per spec (via manifest ingest, the real write path)
// and returns the tick's pre-attribution cost_usd.
func seedAttributionTick(t *testing.T, db *sql.DB, tickID, workdir string, tickCost float64, workers []WaveWorker) float64 {
	t.Helper()
	ctx := context.Background()
	ensureAttributionProject(t, db, workdir)
	tk := &database.Tick{
		ID:          tickID,
		ProjectName: "wave-proj",
		Status:      database.StatusCompleted,
		Outcome:     database.OutcomeCommitted,
		TokensIn:    1000,
		TokensOut:   200,
		CostUSD:     tickCost,
	}
	if err := database.CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if workers != nil {
		raw, err := json.Marshal(WaveManifest{
			TickID: tickID, Project: "wave-proj",
			StartedAt: "2026-09-13T10:00:00Z", FinishedAt: "2026-09-13T11:00:00Z",
			Workers: workers,
		})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		writeWaveManifest(t, workdir, tickID, string(raw))
		if _, err := ingestWaveManifest(ctx, db, workdir, "wave-proj", tickID); err != nil {
			t.Fatalf("ingestWaveManifest: %v", err)
		}
	}
	return tickCost
}

// writeJudgeUsage writes a .gitreins/usage.jsonl under root with the given
// (ts, tokensIn, tokensOut) rows — the worktree-side judge telemetry
// resolveRealTickCost never sees (gitignored file in a fresh worktree).
func writeJudgeUsage(t *testing.T, root string, rows [][3]float64) {
	t.Helper()
	dir := filepath.Join(root, ".gitreins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .gitreins: %v", err)
	}
	content := ""
	for _, r := range rows {
		content += fmt.Sprintf(`{"ts":%f,"tokens_in":%d,"tokens_out":%d}`+"\n", r[0], int(r[1]), int(r[2]))
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write usage.jsonl: %v", err)
	}
}

// TestAttributeWaveCost_ThreeWorkerAverage pins the attribution math against
// a 3-worker manifest with no realized figures: every worker gets the
// per-worker average (tick cost ÷ 3) and the split sums back to the tick's
// single figure — never more (no double count).
func TestAttributeWaveCost_ThreeWorkerAverage(t *testing.T) {
	workers := []database.TickWorker{
		{TickID: "t", TaskID: "T-1", Branch: "wt/T-1"},
		{TickID: "t", TaskID: "T-2", Branch: "wt/T-2"},
		{TickID: "t", TaskID: "T-3", Branch: "wt/T-3"},
	}
	attrs := attributeWaveCost(workers, 0.90, 0)
	if len(attrs) != 3 {
		t.Fatalf("len(attrs) = %d, want 3", len(attrs))
	}
	sum := 0.0
	for _, a := range attrs {
		if a.Realized {
			t.Errorf("worker %s marked realized with no manifest figure", a.TaskID)
		}
		if a.Cost < 0.30-1e-9 || a.Cost > 0.30+1e-9 {
			t.Errorf("worker %s cost = %v, want 0.30 (0.90/3)", a.TaskID, a.Cost)
		}
		sum += a.Cost
	}
	if sum < 0.90-1e-9 || sum > 0.90+1e-9 {
		t.Errorf("attribution sum = %v, want 0.90 (splits back to the single tick figure)", sum)
	}
}

// TestAttributeWaveCost_RealizedWinsNoDoubleCount: mixed realized+estimated
// — the realized manifest figure wins per worker, the 0-figure worker gets
// the estimated split, duplicate task ids produce ONE entry, and no entry
// is ever counted twice.
func TestAttributeWaveCost_RealizedWinsNoDoubleCount(t *testing.T) {
	workers := []database.TickWorker{
		{TickID: "t", TaskID: "T-1", Branch: "wt/T-1", CostUSD: 0.11},
		{TickID: "t", TaskID: "T-2", Branch: "wt/T-2"},                // no figure → estimate
		{TickID: "t", TaskID: "T-2", Branch: "wt/T-2", CostUSD: 0.99}, // dup task id → dropped
	}
	attrs := attributeWaveCost(workers, 0.60, 0)
	if len(attrs) != 2 {
		t.Fatalf("len(attrs) = %d, want 2 (dup task id deduped)", len(attrs))
	}
	if !attrs[0].Realized || attrs[0].Cost != 0.11 {
		t.Errorf("T-1 = %+v, want realized 0.11 (realized wins)", attrs[0])
	}
	if attrs[1].Realized || attrs[1].Cost < 0.30-1e-9 || attrs[1].Cost > 0.30+1e-9 {
		t.Errorf("T-2 = %+v, want estimated 0.30 (0.60/2)", attrs[1])
	}
}

// TestSumWorktreeJudgeCost: only usage rows inside the tick window count;
// a worktree without a usage file contributes 0.
func TestSumWorktreeJudgeCost(t *testing.T) {
	wt := t.TempDir()
	start := time.Now().UTC().Add(-10 * time.Minute)
	end := time.Now().UTC()
	writeJudgeUsage(t, wt, [][3]float64{
		{float64(start.Add(time.Minute).Unix()), 100000, 50000}, // in window
		{float64(end.Add(time.Hour).Unix()), 100000, 50000},     // after end → excluded
	})
	workers := []database.TickWorker{
		{TaskID: "T-1", Worktree: wt},
		{TaskID: "T-2", Worktree: t.TempDir()}, // no usage file → 0
		{TaskID: "T-3"},                        // no worktree → skipped
	}
	got := sumWorktreeJudgeCost(workers, start, end)
	want := 100000*estCostPerIn + 50000*estCostPerOut // the one in-window row
	if got < want-1e-12 || got > want+1e-12 {
		t.Errorf("sumWorktreeJudgeCost = %v, want %v (one in-window row only)", got, want)
	}
}

// TestAttributeTickWorkers_EndToEnd is the happy path AND the row's landing
// gate: a 3-worker wave (1 realized + 2 estimated) with worktree judge
// usage populates every tick_workers.cost_usd, and ticks.cost_usd gains
// ONLY the judge cost — the worker sum never leaks into the tick total.
// Reverse the W4 discipline (add worker costs into the tick) and the
// tick-total assertion fails: the double-count regression test.
func TestAttributeTickWorkers_EndToEnd(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-12-00-00"
	db := newTestDB(t)

	wt := t.TempDir() // worker T-1's worktree with judge usage
	start := time.Now().UTC().Add(-30 * time.Minute)
	end := time.Now().UTC()
	writeJudgeUsage(t, wt, [][3]float64{{float64(start.Add(time.Minute).Unix()), 200000, 100000}})
	judgeWant := 200000*estCostPerIn + 100000*estCostPerOut // $0.40 + $0.80

	seedCost := seedAttributionTick(t, db, tickID, workdir, 0.75, []WaveWorker{
		{TaskID: "T-1", Branch: "wt/T-1", Worktree: wt, Judge: "pass", Merge: "merged", CostUSD: 0.11},
		{TaskID: "T-2", Branch: "wt/T-2", Worktree: t.TempDir()},
		{TaskID: "T-3", Branch: "wt/T-3", Worktree: t.TempDir()},
	})

	rolled, err := attributeTickWorkers(context.Background(), db, tickID, start, end)
	if err != nil {
		t.Fatalf("attributeTickWorkers: %v", err)
	}
	if rolled < judgeWant-1e-12 || rolled > judgeWant+1e-12 {
		t.Errorf("rolled judge cost = %v, want %v", rolled, judgeWant)
	}

	// W4 RED CHECK: the tick total gained ONLY the judge money.
	_, tickCost := waveTickState(t, db, tickID)
	wantTotal := seedCost + judgeWant
	if tickCost < wantTotal-1e-12 || tickCost > wantTotal+1e-12 {
		t.Errorf("ticks.cost_usd = %v, want %v (seed %v + judge %v ONLY — worker attribution must never add: W4)",
			tickCost, wantTotal, seedCost, judgeWant)
	}
	doubleCount := seedCost + judgeWant + 0.11 // adding even one realized worker figure
	if tickCost >= doubleCount-1e-12 && tickCost <= doubleCount+1e-12 {
		t.Errorf("ticks.cost_usd suspiciously includes worker attribution (double count): %v", tickCost)
	}

	// Per-worker attribution: realized wins for T-1; T-2/T-3 get the
	// per-worker average estimate over (tick cost + judge).
	rows, err := database.ListTickWorkersByTick(context.Background(), db, tickID)
	if err != nil {
		t.Fatalf("ListTickWorkersByTick: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("tick_workers rows = %d, want 3", len(rows))
	}
	estWant := (seedCost + judgeWant) / 3
	for _, w := range rows {
		switch w.TaskID {
		case "T-1":
			if w.CostUSD < 0.11-1e-12 || w.CostUSD > 0.11+1e-12 {
				t.Errorf("T-1 cost = %v, want 0.11 (realized wins)", w.CostUSD)
			}
		default:
			if w.CostUSD < estWant-1e-9 || w.CostUSD > estWant+1e-9 {
				t.Errorf("%s cost = %v, want ~%.6f (average estimate)", w.TaskID, w.CostUSD, estWant)
			}
		}
	}
}

// TestAttributeTickWorkers_SerialByteIdentical: a serial tick (no manifest,
// no worker rows) is a no-op — cost byte-identical, no rows touched.
func TestAttributeTickWorkers_SerialByteIdentical(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-13-00-00"
	db := newTestDB(t)
	wantCost := seedAttributionTick(t, db, tickID, workdir, 0.75, nil)

	rolled, err := attributeTickWorkers(context.Background(), db, tickID, time.Now().UTC().Add(-time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("attributeTickWorkers: %v", err)
	}
	if rolled != 0 {
		t.Errorf("rolled = %v, want 0 (serial tick)", rolled)
	}
	_, cost := waveTickState(t, db, tickID)
	if cost != wantCost {
		t.Errorf("serial tick cost = %v, want byte-identical %v", cost, wantCost)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, tickID); len(rows) != 0 {
		t.Errorf("tick_workers rows = %d, want 0", len(rows))
	}
}

// TestAttributeTickWorkers_RecoverySurvives (114 path): the abandoned
// parent keeps its attributed rows and its tick total stays fixed; the
// recovery tick counts ONLY its own wave — re-emitting the parent's
// manifest shape under the recovery tick id never re-bills the parent.
func TestAttributeTickWorkers_RecoverySurvives(t *testing.T) {
	workdir := t.TempDir()
	parentID := "wave-proj-2026-09-13-14-00-00"
	recoveryID := "wave-proj-2026-09-13-15-00-00"
	db := newTestDB(t)

	parentSeed := seedAttributionTick(t, db, parentID, workdir, 0.60, []WaveWorker{
		{TaskID: "T-1", Branch: "wt/T-1", Worktree: t.TempDir()},
		{TaskID: "T-2", Branch: "wt/T-2", Worktree: t.TempDir()},
	})
	// Parent timed out mid-wave: completion-hook attribution ran...
	if _, err := attributeTickWorkers(context.Background(), db, parentID,
		time.Now().UTC().Add(-2*time.Hour), time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("parent attribution: %v", err)
	}
	// ...then the 114 reaper abandoned the rows.
	if _, _, err := abandonTickWorkers(context.Background(), db, parentID); err != nil {
		t.Fatalf("abandonTickWorkers: %v", err)
	}
	_, parentCost := waveTickState(t, db, parentID)
	if parentCost != parentSeed { // no worktrees carried judge usage → judge=0
		t.Fatalf("parent cost = %v, want %v (judge-less wave: nothing additive)", parentCost, parentSeed)
	}
	parentRows, err := database.ListTickWorkersByTick(context.Background(), db, parentID)
	if err != nil {
		t.Fatalf("parent rows: %v", err)
	}
	for _, w := range parentRows {
		if w.State != database.TickWorkerStateAbandoned {
			t.Errorf("parent worker %s state = %s, want abandoned", w.TaskID, w.State)
		}
		if w.CostUSD <= 0 {
			t.Errorf("parent worker %s lost its attribution (cost %v) — attribution must survive abandonment", w.TaskID, w.CostUSD)
		}
	}

	// Recovery tick: SAME project, its own manifest (a fixup worker for
	// T-1's branch). Only the recovery wave is attributed to it.
	recoverySeed := seedAttributionTick(t, db, recoveryID, workdir, 0.25, []WaveWorker{
		{TaskID: "T-1", Branch: "wt/T-1-fixup", Worktree: t.TempDir(), CostUSD: 0.09},
	})
	if _, err := attributeTickWorkers(context.Background(), db, recoveryID,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC()); err != nil {
		t.Fatalf("recovery attribution: %v", err)
	}
	_, recoveryCost := waveTickState(t, db, recoveryID)
	if recoveryCost != recoverySeed {
		t.Errorf("recovery cost = %v, want %v — the abandoned parent's costs must NOT be re-counted here", recoveryCost, recoverySeed)
	}
	_, parentAfter := waveTickState(t, db, parentID)
	if parentAfter != parentCost {
		t.Errorf("parent cost moved %v → %v after the recovery tick — abandoned parent must stay fixed", parentCost, parentAfter)
	}
	recRows, _ := database.ListTickWorkersByTick(context.Background(), db, recoveryID)
	if len(recRows) != 1 || recRows[0].CostUSD < 0.09-1e-12 || recRows[0].CostUSD > 0.09+1e-12 {
		t.Errorf("recovery rows = %+v, want exactly one realized 0.09 fixup worker", recRows)
	}
}

// TestAttributeTickWorkers_FailSafe: an unknown tick id is
// indistinguishable from a serial tick (no worker rows) → a silent no-op,
// never a panic — the completion path can never fail on attribution.
func TestAttributeTickWorkers_FailSafe(t *testing.T) {
	db := newTestDB(t)
	rolled, err := attributeTickWorkers(context.Background(), db, "no-such-tick",
		time.Now().UTC().Add(-time.Hour), time.Now().UTC())
	if err != nil || rolled != 0 {
		t.Errorf("unknown tick = (%v, %v), want (0, nil) — no-op like a serial tick", rolled, err)
	}
}
