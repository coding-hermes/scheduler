package scheduler

// ── SCHED-GAP-110: wave manifest ingest (S12 §9.3) ───────────────────────
//
// The Parse* tests drive ingestWaveManifest (parse + event emission are one
// contract: "0 workers + exactly one WARN event" is only observable end to
// end), on a temp DB seeded with one completed tick row. Every test uses a
// t.TempDir() workdir so manifests land at the real
// .coding-hermes/waves/<tick_id>.json path.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// waveSeedTick creates the project + terminal tick row ingestWaveManifest
// attributes to, and returns the tick's pre-ingest cost_usd for
// byte-identical comparison (the §12 W4 RED check).
func waveSeedTick(t *testing.T, db *sql.DB, tickID, workdir string) float64 {
	t.Helper()
	ctx := context.Background()
	p := &database.Project{
		Name:      "wave-proj",
		Workdir:   workdir,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Enabled:   true,
	}
	if err := database.CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := &database.Tick{
		ID:          tickID,
		ProjectName: p.Name,
		Status:      database.StatusCompleted,
		Outcome:     database.OutcomeCommitted,
		TokensIn:    1234,
		TokensOut:   567,
		CostUSD:     0.75,
	}
	if err := database.CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	return tk.CostUSD
}

// writeWaveManifest writes content at the canonical manifest path for
// tickID under workdir and returns that path.
func writeWaveManifest(t *testing.T, workdir, tickID, content string) string {
	t.Helper()
	path := waveManifestPath(workdir, tickID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir waves dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// waveEventCount counts manifest-ingest WARN events (component 'wave').
func waveEventCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'wave'`).Scan(&n); err != nil {
		t.Fatalf("count wave events: %v", err)
	}
	return n
}

// waveTickState reads (worker_count, cost_usd) for the tick row.
func waveTickState(t *testing.T, db *sql.DB, tickID string) (int, float64) {
	t.Helper()
	var wc int
	var cost float64
	if err := db.QueryRow(`SELECT worker_count, cost_usd FROM ticks WHERE id = ?`, tickID).Scan(&wc, &cost); err != nil {
		t.Fatalf("read tick %s: %v", tickID, err)
	}
	return wc, cost
}

// TestParseWaveManifest_Valid pins the happy path (§12): a 3-worker
// manifest round-trips every field through parseWaveManifest, and ingest
// sets worker_count=3, writes 3 attribution rows with the documented state
// derivation, emits no events, and leaves ticks.cost_usd untouched.
func TestParseWaveManifest_Valid(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-10-00-00"
	mIn := WaveManifest{
		TickID:     tickID,
		Project:    "wave-proj",
		StartedAt:  "2026-09-13T10:00:00Z",
		FinishedAt: "2026-09-13T11:40:00Z",
		Workers: []WaveWorker{
			{TaskID: "T-1", Branch: "wt/T-1", Worktree: "/wt/T-1", CommitSHA: "abc1234",
				Judge: "pass", Merge: "merged", CostUSD: 0.11, TokensIn: 1000, TokensOut: 200},
			{TaskID: "T-2", Branch: "wt/T-2", Worktree: "/wt/T-2", CommitSHA: "def5678",
				Judge: "", Merge: "", CostUSD: 0.22, TokensIn: 2000, TokensOut: 400},
			{TaskID: "T-3", Branch: "wt/T-3", Worktree: "/wt/T-3", CommitSHA: "90ab12",
				Judge: "withdrawn", Merge: "preserved", CostUSD: 0.33, TokensIn: 3000, TokensOut: 600},
		},
	}
	raw, err := json.Marshal(mIn)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := writeWaveManifest(t, workdir, tickID, string(raw))

	// Pure-parse round trip.
	m, err := parseWaveManifest(path, tickID)
	if err != nil {
		t.Fatalf("parseWaveManifest: %v", err)
	}
	if m.TickID != tickID || m.Project != "wave-proj" ||
		m.StartedAt != "2026-09-13T10:00:00Z" || m.FinishedAt != "2026-09-13T11:40:00Z" {
		t.Errorf("header fields not round-tripped: %+v", m)
	}
	if len(m.Workers) != 3 {
		t.Fatalf("workers = %d, want 3", len(m.Workers))
	}
	for i, want := range []WaveWorker{
		{TaskID: "T-1", Branch: "wt/T-1", Worktree: "/wt/T-1", CommitSHA: "abc1234",
			Judge: "pass", Merge: "merged", CostUSD: 0.11, TokensIn: 1000, TokensOut: 200},
		{TaskID: "T-2", Branch: "wt/T-2", Worktree: "/wt/T-2", CommitSHA: "def5678",
			Judge: "unknown", Merge: "pending", CostUSD: 0.22, TokensIn: 2000, TokensOut: 400},
		{TaskID: "T-3", Branch: "wt/T-3", Worktree: "/wt/T-3", CommitSHA: "90ab12",
			Judge: "withdrawn", Merge: "preserved", CostUSD: 0.33, TokensIn: 3000, TokensOut: 600},
	} {
		if m.Workers[i] != want {
			t.Errorf("worker[%d] = %+v, want %+v (empty judge/merge must degrade, not fail)", i, m.Workers[i], want)
		}
	}

	// Ingest: one transaction, attribution rows, no events, cost untouched.
	db := newTestDB(t)
	wantCost := waveSeedTick(t, db, tickID, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != 3 {
		t.Errorf("ingested = %d, want 3", n)
	}
	wc, cost := waveTickState(t, db, tickID)
	if wc != 3 {
		t.Errorf("ticks.worker_count = %d, want 3", wc)
	}
	if cost != wantCost {
		t.Errorf("ticks.cost_usd = %v, want byte-identical %v (W4: manifest cost must never land on the tick)", cost, wantCost)
	}
	rows, err := database.ListTickWorkersByTick(context.Background(), db, tickID)
	if err != nil {
		t.Fatalf("ListTickWorkersByTick: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("tick_workers rows = %d, want 3", len(rows))
	}
	// Documented state derivation: terminal verdict (pass/merged or
	// withdrawn/preserved) → done; unreported verdicts → running.
	wantStates := []string{"done", "running", "done"}
	for i, w := range rows {
		if w.State != wantStates[i] {
			t.Errorf("worker[%d].state = %q, want %q", i, w.State, wantStates[i])
		}
		if w.CostUSD != mIn.Workers[i].CostUSD {
			t.Errorf("worker[%d].cost_usd = %v, want manifest attribution %v", i, w.CostUSD, mIn.Workers[i].CostUSD)
		}
	}
	if got := waveEventCount(t, db); got != 0 {
		t.Errorf("wave events = %d, want 0 for a valid manifest", got)
	}
}

// TestParseWaveManifest_MissingFile pins §9.3 row 1: no manifest →
// worker_count 0, no tick_workers rows, and NO event (a serial tick is
// normal, not a warning).
func TestParseWaveManifest_MissingFile(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-11-00-00"

	m, err := parseWaveManifest(waveManifestPath(workdir, tickID), tickID)
	if err == nil || !strings.Contains(err.Error(), "wave manifest absent") {
		t.Fatalf("parseWaveManifest on missing file: err = %v, want errWaveManifestAbsent", err)
	}
	if m != nil {
		t.Errorf("manifest = %+v, want nil", m)
	}

	db := newTestDB(t)
	waveSeedTick(t, db, tickID, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != 0 {
		t.Errorf("ingested = %d, want 0", n)
	}
	wc, _ := waveTickState(t, db, tickID)
	if wc != 0 {
		t.Errorf("ticks.worker_count = %d, want 0", wc)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, tickID); len(rows) != 0 {
		t.Errorf("tick_workers rows = %d, want 0", len(rows))
	}
	if got := waveEventCount(t, db); got != 0 {
		t.Errorf("wave events = %d, want 0 for a serial tick", got)
	}
}

// TestParseWaveManifest_Malformed pins §9.3 row 2: broken JSON → nothing
// ingested + exactly one WARN event naming tick id and path.
func TestParseWaveManifest_Malformed(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-12-00-00"
	path := writeWaveManifest(t, workdir, tickID, `{"tick_id": "`+tickID+`", "workers": [ { BROKEN`)

	if m, err := parseWaveManifest(path, tickID); err == nil || m != nil {
		t.Fatalf("parseWaveManifest on malformed JSON: (%+v, %v), want (nil, error)", m, err)
	}

	db := newTestDB(t)
	waveSeedTick(t, db, tickID, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest returned error %v — malformed manifest must never fail the tick", err)
	}
	if n != 0 {
		t.Errorf("ingested = %d, want 0", n)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, tickID); len(rows) != 0 {
		t.Errorf("tick_workers rows = %d, want 0", len(rows))
	}
	if got := waveEventCount(t, db); got != 1 {
		t.Fatalf("wave events = %d, want exactly 1 WARN", got)
	}
	// The WARN must name the tick id and the path.
	var msg string
	if err := db.QueryRow(`SELECT message FROM events WHERE component = 'wave'`).Scan(&msg); err != nil {
		t.Fatalf("read wave event: %v", err)
	}
	if !strings.Contains(msg, tickID) || !strings.Contains(msg, path) {
		t.Errorf("WARN message %q must name tick id %q and path %q", msg, tickID, path)
	}
}

// TestParseWaveManifest_TooManyWorkers pins §9.3 row 3: 10 workers →
// first 8 ingested (worker_count 8), the rest dropped, one WARN event.
func TestParseWaveManifest_TooManyWorkers(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-13-00-00"
	mIn := WaveManifest{TickID: tickID, Project: "wave-proj",
		StartedAt: "2026-09-13T13:00:00Z", FinishedAt: "2026-09-13T14:00:00Z"}
	for i := 0; i < 10; i++ {
		mIn.Workers = append(mIn.Workers, WaveWorker{
			TaskID: "T-" + string(rune('A'+i)), Branch: "wt/T-" + string(rune('A'+i)),
			Judge: "pass", Merge: "merged",
		})
	}
	raw, err := json.Marshal(mIn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := writeWaveManifest(t, workdir, tickID, string(raw))

	m, err := parseWaveManifest(path, tickID)
	if err != nil {
		t.Fatalf("parseWaveManifest: %v", err)
	}
	if len(m.Workers) != waveManifestMaxWorkers {
		t.Fatalf("parsed workers = %d, want capped at %d", len(m.Workers), waveManifestMaxWorkers)
	}
	if m.TruncatedWorkers != 2 {
		t.Errorf("TruncatedWorkers = %d, want 2", m.TruncatedWorkers)
	}

	db := newTestDB(t)
	waveSeedTick(t, db, tickID, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != waveManifestMaxWorkers {
		t.Errorf("ingested = %d, want %d", n, waveManifestMaxWorkers)
	}
	wc, _ := waveTickState(t, db, tickID)
	if wc != waveManifestMaxWorkers {
		t.Errorf("ticks.worker_count = %d, want %d", wc, waveManifestMaxWorkers)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, tickID); len(rows) != waveManifestMaxWorkers {
		t.Errorf("tick_workers rows = %d, want %d", len(rows), waveManifestMaxWorkers)
	}
	if got := waveEventCount(t, db); got != 1 {
		t.Errorf("wave events = %d, want exactly 1 WARN for the bounded write", got)
	}
}

// TestParseWaveManifest_TickIDMismatch pins §9.3 row 7: a manifest whose
// tick_id is not the completing tick's → rejected, one WARN, nothing
// ingested (the row set belongs to a different tick).
func TestParseWaveManifest_TickIDMismatch(t *testing.T) {
	workdir := t.TempDir()
	completing := "wave-proj-2026-09-13-14-00-00"
	other := "wave-proj-2026-09-13-99-99-99"
	raw, _ := json.Marshal(WaveManifest{
		TickID: other, Project: "wave-proj",
		StartedAt: "2026-09-13T14:00:00Z", FinishedAt: "2026-09-13T15:00:00Z",
		Workers: []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1", Judge: "pass", Merge: "merged"}},
	})
	path := writeWaveManifest(t, workdir, completing, string(raw))

	if m, err := parseWaveManifest(path, completing); err == nil || m != nil {
		t.Fatalf("parseWaveManifest on tick_id mismatch: (%+v, %v), want (nil, error)", m, err)
	}

	db := newTestDB(t)
	waveSeedTick(t, db, completing, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", completing)
	if err != nil {
		t.Fatalf("ingestWaveManifest returned error %v — mismatch must never fail the tick", err)
	}
	if n != 0 {
		t.Errorf("ingested = %d, want 0", n)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, completing); len(rows) != 0 {
		t.Errorf("tick_workers rows = %d, want 0", len(rows))
	}
	if got := waveEventCount(t, db); got != 1 {
		t.Errorf("wave events = %d, want exactly 1 WARN", got)
	}
}

// TestIngestWaveManifest_OversizeFile pins §9.3 row 4: a manifest over the
// 64 KiB bound is not parsed — nothing ingested, exactly one WARN.
func TestIngestWaveManifest_OversizeFile(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-15-00-00"
	big := `{"tick_id": "` + tickID + `", "pad": "` + strings.Repeat("x", waveManifestMaxBytes) + `"}`
	path := writeWaveManifest(t, workdir, tickID, big)
	if fi, err := os.Stat(path); err != nil || fi.Size() <= waveManifestMaxBytes {
		t.Fatalf("fixture must exceed the bound: size=%d err=%v", fi.Size(), err)
	}

	if m, err := parseWaveManifest(path, tickID); err == nil || m != nil {
		t.Fatalf("parseWaveManifest on oversize file: (%+v, %v), want (nil, error)", m, err)
	}

	db := newTestDB(t)
	waveSeedTick(t, db, tickID, workdir)
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest returned error %v — oversize must never fail the tick", err)
	}
	if n != 0 {
		t.Errorf("ingested = %d, want 0", n)
	}
	wc, _ := waveTickState(t, db, tickID)
	if wc != 0 {
		t.Errorf("ticks.worker_count = %d, want 0", wc)
	}
	if got := waveEventCount(t, db); got != 1 {
		t.Errorf("wave events = %d, want exactly 1 WARN", got)
	}
}

// TestIngestWaveManifest_NoDoubleCount is the §12 W4 RED check: manifest
// workers carrying cost_usd attribution must leave ticks.cost_usd
// byte-identical. Ingesting the workers' cost into the tick (the
// double-count bug) fails this test.
func TestIngestWaveManifest_NoDoubleCount(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-16-00-00"
	raw, _ := json.Marshal(WaveManifest{
		TickID: tickID, Project: "wave-proj",
		StartedAt: "2026-09-13T16:00:00Z", FinishedAt: "2026-09-13T17:00:00Z",
		Workers: []WaveWorker{
			{TaskID: "T-1", Branch: "wt/T-1", Judge: "pass", Merge: "merged", CostUSD: 5.25, TokensIn: 9000, TokensOut: 900},
			{TaskID: "T-2", Branch: "wt/T-2", Judge: "pass", Merge: "merged", CostUSD: 6.75, TokensIn: 8000, TokensOut: 800},
		},
	})
	writeWaveManifest(t, workdir, tickID, string(raw))

	db := newTestDB(t)
	wantCost := waveSeedTick(t, db, tickID, workdir) // 0.75
	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != 2 {
		t.Fatalf("ingested = %d, want 2", n)
	}
	_, cost := waveTickState(t, db, tickID)
	if cost != wantCost {
		t.Errorf("ticks.cost_usd = %v, want byte-identical %v — worker attribution (12.00) leaked into the tick (W4 double count)", cost, wantCost)
	}
	// Attribution lands on the worker rows instead — counted once, there.
	rows, err := database.ListTickWorkersByTick(context.Background(), db, tickID)
	if err != nil {
		t.Fatalf("ListTickWorkersByTick: %v", err)
	}
	var sum float64
	for _, w := range rows {
		sum += w.CostUSD
	}
	if sum != 12.00 {
		t.Errorf("tick_workers cost sum = %v, want 12.00 (attribution only)", sum)
	}
}

// TestIngestWaveManifest_SerialTickNoEvent pins the no-manifest fleet
// default end to end: worker_count 0, no rows, no events — the pre-v27
// world stays byte-identical.
func TestIngestWaveManifest_SerialTickNoEvent(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-17-00-00"
	db := newTestDB(t)
	wantCost := waveSeedTick(t, db, tickID, workdir)

	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != 0 {
		t.Errorf("ingested = %d, want 0", n)
	}
	wc, cost := waveTickState(t, db, tickID)
	if wc != 0 {
		t.Errorf("ticks.worker_count = %d, want 0", wc)
	}
	if cost != wantCost {
		t.Errorf("ticks.cost_usd = %v, want %v", cost, wantCost)
	}
	if rows, _ := database.ListTickWorkersByTick(context.Background(), db, tickID); len(rows) != 0 {
		t.Errorf("tick_workers rows = %d, want 0", len(rows))
	}
	if got := waveEventCount(t, db); got != 0 {
		t.Errorf("wave events = %d, want 0", got)
	}
}

// TestWaveManifestUnfinished pins the SCHED-GAP-114 recovery-candidate
// helper (§9.3 rows 1/6): true only when the manifest parses AND
// finished_at is empty; closed, missing, malformed, and self-inconsistent
// manifests are all non-candidates.
func TestWaveManifestUnfinished(t *testing.T) {
	workdir := t.TempDir()
	tickID := "wave-proj-2026-09-13-18-00-00"

	write := func(finished string, tickIDIn string) string {
		raw, _ := json.Marshal(WaveManifest{
			TickID: tickIDIn, Project: "wave-proj", StartedAt: "2026-09-13T18:00:00Z",
			FinishedAt: finished,
			Workers:    []WaveWorker{{TaskID: "T-1", Branch: "wt/T-1"}},
		})
		return writeWaveManifest(t, workdir, tickID, string(raw))
	}

	if p := write("", tickID); !waveManifestUnfinished(p) {
		t.Error("unfinished (crashed) manifest: want true")
	}
	if p := write("2026-09-13T19:00:00Z", tickID); waveManifestUnfinished(p) {
		t.Error("foreman-closed manifest: want false")
	}
	if waveManifestUnfinished(waveManifestPath(workdir, "no-such-tick")) {
		t.Error("missing manifest: want false")
	}
	if p := writeWaveManifest(t, workdir, "wave-proj-2026-09-13-20-00-00", "{broken"); waveManifestUnfinished(p) {
		t.Error("malformed manifest: want false")
	}
	// tick_id disagreeing with its own filename: untrusted, non-candidate.
	if p := write("", "some-other-tick"); waveManifestUnfinished(p) {
		t.Error("self-inconsistent manifest (tick_id ≠ filename): want false")
	}
}
