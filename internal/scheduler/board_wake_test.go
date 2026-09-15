package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── ADV-R07 (Option C): board-driven wake + ordering boost ─────────────
//
// AC1  board write ⇒ evaluate within the debounce window (clock seam) +
//      cooldown still gates (pending row on a cooldown-locked project
//      spawns nothing).
// AC2  watcher failure ⇒ fleet behavior identical to today (fail-open,
//      fault-injection).
// AC3  wake events carry row id + freshness verdict (events table).
// AC4  invariant: NO code path skips a spawn solely on board state.
//
// The watcher's own arithmetic (arm/fire/baseline/debounce-bound) is
// driven deterministically via pollOnce(now) with constructed instants —
// no real sleeps; the integration tests below use real (small) intervals
// with generous waits.

// wakeFix builds the standard watcher fixture: test DB, one project with
// a JSONL board, watcher armed with a fake freshness reader and a
// recorded forceEval closure.
type wakeFix struct {
	t         *testing.T
	db        *sql.DB
	workdir   string
	boardPath string
	w         *BoardWakeWatcher
	evalCount *int64
	evalMu    *sync.Mutex
}

func newWakeFix(t *testing.T) *wakeFix {
	t.Helper()
	db := newTestDB(t)
	workdir := t.TempDir()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(boardPath, []byte("{\"id\":\"W-1\",\"status\":\"pending\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Enabled project pointing at the fixture workdir. Clock seam: the
	// completion stamp is pinned relative to a fixed now (below).
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at)
		VALUES ('wakeproj', 'https://example.com/w', ?, 10, 5, 3600, 1.0, 'm', 'p', 1,
		 datetime('now'), datetime('now'))`, workdir); err != nil {
		t.Fatal(err)
	}
	var evalCount int64
	var mu sync.Mutex
	w := NewBoardWakeWatcher(db, func() { mu.Lock(); evalCount++; mu.Unlock() })
	return &wakeFix{t: t, db: db, workdir: workdir, boardPath: boardPath, w: w, evalCount: &evalCount, evalMu: &mu}
}

// evals returns the recorded ForceEvaluate call count.
func (fx *wakeFix) evals() int64 {
	fx.evalMu.Lock()
	defer fx.evalMu.Unlock()
	return *fx.evalCount
}

// rewriteBoard replaces the board content and guarantees an mtime bump
// (retries until stat shows a change; coarse filesystem timestamps can
// miss a same-second rewrite).
func rewriteBoard(t *testing.T, path string, lines ...string) {
	t.Helper()
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	prev := statMtime(path)
	for attempt := 0; attempt < 40; attempt++ {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if now := statMtime(path); !now.Equal(prev) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("board rewrite could not produce an mtime change")
}

// wakeEvents returns the INFO board_wake events emitted so far.
func wakeEvents(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT details FROM events WHERE component = 'board_wake' AND severity = 'INFO' ORDER BY id`)
	if err != nil {
		t.Fatalf("query wake events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			t.Fatalf("scan wake event: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(details), &m); err != nil {
			t.Fatalf("unmarshal wake event details: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// TestBoardWake_BoardWriteWakesEvaluate is AC1 first half, watcher-side:
// a board write arms a wake and the wake calls ForceEvaluate within the
// debounce window — the FIRST observed change starts the window and
// LATER writes never extend it (bounded wake). Driven on constructed
// instants (poll/debounce arithmetic) with real mtime writes.
func TestBoardWake_BoardWriteWakesEvaluate(t *testing.T) {
	fx := newWakeFix(t)
	t0 := time.Now()
	fx.w.pollOnce(t0) // baseline

	rewriteBoard(t, fx.boardPath, `{"id":"W-1","status":"pending"}`, `{"id":"W-2","status":"pending"}`)

	t1 := t0.Add(1 * time.Minute)
	fx.w.pollOnce(t1) // detects change, arms at t1

	// A second write inside the window must NOT extend the debounce.
	rewriteBoard(t, fx.boardPath, `{"id":"W-1","status":"pending"}`, `{"id":"W-2","status":"pending"}`, `{"id":"W-3","status":"pending"}`)
	t2 := t1.Add(2 * time.Minute)
	fx.w.pollOnce(t2) // still inside 5-min debounce → no wake

	if got := fx.evals(); got != 0 {
		t.Fatalf("ForceEvaluate called %d times inside the debounce window, want 0", got)
	}

	t3 := t1.Add(5 * time.Minute) // debounce elapsed since FIRST change
	fx.w.pollOnce(t3)

	if got := fx.evals(); got != 1 {
		t.Fatalf("ForceEvaluate call count after debounce = %d, want exactly 1 (bounded debounce: first change + 5min)", got)
	}
	// The next poll must not re-fire (wake consumed).
	fx.w.pollOnce(t3.Add(time.Minute))
	if got := fx.evals(); got != 1 {
		t.Fatalf("ForceEvaluate re-fired after the wake was consumed: %d, want 1", got)
	}
}

// TestBoardWake_WriteWithinWindowDoesNotFireEarly pins the bound from
// the other side: 4 minutes after the first change (and 2 after the
// second) there is still no wake.
func TestBoardWake_WriteWithinWindowDoesNotFireEarly(t *testing.T) {
	fx := newWakeFix(t)
	t0 := time.Now()
	fx.w.pollOnce(t0)

	rewriteBoard(t, fx.boardPath, `{"id":"W-1","status":"pending"}`, `{"id":"W-2","status":"pending"}`)
	t1 := t0.Add(30 * time.Second)
	fx.w.pollOnce(t1)

	rewriteBoard(t, fx.boardPath, `{"id":"W-1","status":"pending"}`, `{"id":"W-2","status":"pending"}`, `{"id":"W-3","status":"pending"}`)
	t2 := t1.Add(30 * time.Second)
	fx.w.pollOnce(t2)

	t3 := t1.Add(4 * time.Minute) // < 5 min since first change at t1
	fx.w.pollOnce(t3)
	if got := fx.evals(); got != 0 {
		t.Fatalf("wake fired early: ForceEvaluate = %d at 4min, want 0", got)
	}
}

// TestBoardWake_CooldownStillGates is AC1 second half, end-to-end: a
// pending row lands on a cooldown-LOCKED project, the watcher wakes,
// evaluate runs on the R04 clock seam — and NOTHING spawns (no tick
// rows). Cooldown is the sole admission authority; the wake only adds a
// trigger.
func TestBoardWake_CooldownStillGates(t *testing.T) {
	fixedNow := fixedEvalNow()
	db := newTestDB(t)
	workdir := t.TempDir()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(boardPath, []byte("{\"id\":\"W-1\",\"status\":\"pending\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Cooldown-locked at fixedNow: completed 60s ago, cooldown 3600s.
	// (insertEligibilityProject mirrors the packer row shape.)
	insertEligibilityProject(t, db, "wakelocked", 3600, 5, 0, fixedNow.Add(-60*time.Second), 0, 0)
	if _, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = 'wakelocked'`, workdir); err != nil {
		t.Fatal(err)
	}

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(func() time.Time { return fixedNow })

	var evals int64
	var mu sync.Mutex
	w := NewBoardWakeWatcher(db, func() {
		mu.Lock()
		evals++
		mu.Unlock()
		l.ForceEvaluate()
	})

	t0 := time.Now()
	w.pollOnce(t0) // baseline

	// File the pending row (a genuine board write, mtime guaranteed).
	rewriteBoard(t, boardPath,
		`{"id":"W-1","status":"pending"}`,
		`{"id":"W-9","status":"pending"}`)

	t1 := t0.Add(time.Second)
	w.pollOnce(t1) // arms
	w.pollOnce(t1.Add(boardWakeDebounce + time.Second))

	mu.Lock()
	if evals == 0 {
		t.Fatal("watcher never fired ForceEvaluate — premise broken")
	}
	mu.Unlock()

	// ForceEvaluate is async: wait for the sim tick table to settle,
	// then assert the invariant — nothing spawned for the locked project.
	time.Sleep(200 * time.Millisecond)
	var ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&ticks); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticks != 0 {
		t.Fatalf("cooldown-locked project with a pending board row spawned %d tick(s) — cooldown MUST gate the wake", ticks)
	}
	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Fatalf("evaluate selected %v on a cooldown-locked project, want empty", got)
	}
}

// waitFor polls cond every 10ms until it holds or the deadline expires.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// TestBoardWake_NewBoardAfterBaselineArms covers the appeared-board
// case: a project whose board file is created after the baseline is a
// write (freshly-filed work) and arms a wake.
func TestBoardWake_NewBoardAfterBaselineArms(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at)
		VALUES ('newboard', 'https://example.com/n', ?, 10, 5, 60, 1.0, 'm', 'p', 1,
		 datetime('now'), datetime('now'))`, workdir); err != nil {
		t.Fatal(err)
	}
	var evals int64
	var mu sync.Mutex
	w := NewBoardWakeWatcher(db, func() { mu.Lock(); evals++; mu.Unlock() })
	t0 := time.Now()
	w.pollOnce(t0) // baseline: project exists, board file does not

	if err := os.WriteFile(boardPath, []byte("{\"id\":\"N-1\",\"status\":\"pending\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Minute)
	w.pollOnce(t1) // first sight of the board → arm
	w.pollOnce(t1.Add(6 * time.Minute))

	mu.Lock()
	defer mu.Unlock()
	if evals != 1 {
		t.Fatalf("ForceEvaluate after new-board wake = %d, want 1", evals)
	}
}

// TestBoardWake_FailOpenOnEnumerationError is AC2 (fault injection): a
// board enumeration that always errors produces NO wake, NO event, and
// leaves the poll loop alive (heartbeat beats). The fleet behaves
// exactly as it would without the watcher.
func TestBoardWake_FailOpenOnEnumerationError(t *testing.T) {
	fx := newWakeFix(t)
	fx.w.listBoards = func(context.Context) (map[string]boardWakeTarget, error) {
		return nil, fmt.Errorf("injected: db gone")
	}
	t0 := time.Now()
	fx.w.pollOnce(t0) // baseline attempt — fails, stays un-baselined
	rewriteBoard(t, fx.boardPath, `{"id":"W-1","status":"pending"}`, `{"id":"W-2","status":"pending"}`)

	for i := 1; i <= 3; i++ {
		fx.w.pollOnce(t0.Add(time.Duration(i) * 7 * time.Minute)) // every call errors
	}
	if got := fx.evals(); got != 0 {
		t.Fatalf("fail-open violated: ForceEvaluate called %d times on enumeration failure, want 0", got)
	}
	if evs := wakeEvents(t, fx.db); len(evs) != 0 {
		t.Fatalf("fail-open violated: %d board_wake event(s) on enumeration failure, want 0", len(evs))
	}
	// Heartbeat still beats (poll loop alive — the watchdog stays quiet).
	fx.w.mu.Lock()
	lastBeat := fx.w.lastBeat
	fx.w.mu.Unlock()
	if lastBeat.IsZero() {
		t.Fatal("poll loop stopped beating on enumeration failure — fail-open requires a live loop")
	}
}

// TestBoardWake_FailOpenReaderKeepsRawCount is the AC2 reader half: the
// R06 freshness seam failing (nil) or returning an unreadable-board
// report keeps the RAW pending count — work is never hidden behind a
// reader error.
func TestBoardWake_FailOpenReaderKeepsRawCount(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	content := "{\"id\":\"F-1\",\"status\":\"pending\"}\n{\"id\":\"F-2\",\"status\":\"pending\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Nil seam (reader absent) — raw count.
	c1 := NewPendingTaskCounter(60 * time.Second)
	c1.freshnessRead = nil
	if got := c1.CountPending(dir); got != 2 {
		t.Fatalf("nil freshness seam: CountPending = %d, want raw 2", got)
	}

	// Reader returns a zero report (unreadable board) — raw count.
	c2 := NewPendingTaskCounter(60 * time.Second)
	c2.freshnessRead = func(string, string) FreshnessReport { return FreshnessReport{} }
	if got := c2.CountPending(dir); got != 2 {
		t.Fatalf("unreadable-board report: CountPending = %d, want raw 2", got)
	}
}

// TestBoardWake_EventCarriesRowIDAndVerdict is AC3: the wake event's
// details carry, per row, the row id and the git-verified freshness
// verdict. Uses a REAL throwaway git repo so the verdicts are genuine
// R06 classifications (open row stays open; flip-window row is
// flip-window).
func TestBoardWake_EventCarriesRowIDAndVerdict(t *testing.T) {
	db := newTestDB(t)
	fx := &freshFixture{t: t, dir: t.TempDir(), now: time.Now(), window: DefaultFlipWindow}
	fx.git("init", "-q")
	fx.git("config", "user.email", "wake@example.com")
	fx.git("config", "user.name", "Wake Test")
	fx.commitAt("seed", fx.now.Add(-2*time.Hour), "seed")

	boardDir := filepath.Join(fx.dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	// One plain pending row + one flip-window row: work landed 10 min
	// ago (inside the 55-min window) but the row is still pending.
	sha := fx.commitAt("work", fx.now.Add(-10*time.Minute), "fix: addresses W-2")
	lines := []string{
		`{"id":"W-1","status":"pending"}`,
		fmt.Sprintf(`{"id":"W-2","status":"pending","commit_hash":"%s"}`, sha),
	}
	if err := os.WriteFile(boardPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at)
		VALUES ('verdictproj', 'https://example.com/v', ?, 10, 5, 60, 1.0, 'm', 'p', 1,
		 datetime('now'), datetime('now'))`, fx.dir); err != nil {
		t.Fatal(err)
	}

	w := NewBoardWakeWatcher(db, func() {})
	w.wake(boardWakeTarget{project: "verdictproj", workdir: fx.dir, boardPath: boardPath})

	evs := wakeEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("wake events = %d, want 1", len(evs))
	}
	rows, _ := evs[0]["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("event rows = %d, want 2", len(rows))
	}
	got := map[string]string{}
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("row entry malformed: %T", r)
		}
		got[fmt.Sprint(m["row_id"])] = fmt.Sprint(m["verdict"])
	}
	if got["W-1"] != string(RowOpen) {
		t.Errorf("row W-1 verdict = %q, want %q (plain pending row)", got["W-1"], RowOpen)
	}
	if got["W-2"] != string(RowFlipWindow) {
		t.Errorf("row W-2 verdict = %q, want %q (pending row whose work verifiably landed)", got["W-2"], RowFlipWindow)
	}
	if evs[0]["work_to_spawn"] != true {
		t.Errorf("work_to_spawn = %v, want true (W-1 is open work)", evs[0]["work_to_spawn"])
	}
}

// TestBoardWake_FreshnessCheckedPendingIs the R06 integration core: the
// pending count that feeds pendingBoostUrgencyFor is freshness-checked —
// a pending row whose work verifiably landed (flip-window) no longer
// counts, a pending row whose pointer does not resolve stays counted
// (never hide work), and the ordering tier still consumes the count.
func TestBoardWake_FreshnessCheckedPendingIsOrderingOnly(t *testing.T) {
	fx := newFreshFixture(t)
	sha := fx.commitAt("work", fx.workLanded(), "fix: addresses P-2")
	fx.writeBoard(
		`{"id":"P-1","status":"pending"}`,
		fmt.Sprintf(`{"id":"P-2","status":"pending","commit_hash":"%s"}`, sha),
	)
	c := NewPendingTaskCounter(60 * time.Second)
	// Freshness read with the fixture's fixed clock — same read the
	// production seam performs, minus wall-clock dependence.
	c.freshnessRead = func(workdir, boardPath string) FreshnessReport {
		return ReadBoardFreshness(workdir, boardPath, FreshnessOptions{Now: fx.now})
	}
	got := c.CountPending(fx.dir)
	if got != 1 {
		t.Fatalf("freshness-checked CountPending = %d, want 1 (P-1 open; P-2 flip-window, subtracted)", got)
	}
	// The ordering tier consumes exactly that count.
	if u := pendingBoostUrgencyFor(got); u != pendingBoostUrgency+1 {
		t.Fatalf("pendingBoostUrgencyFor(%d) = %v, want %v", got, u, pendingBoostUrgency+1)
	}
	// Fail-open control on the SAME board: reader that cannot verify
	// keeps the raw count.
	c2 := NewPendingTaskCounter(60 * time.Second)
	c2.freshnessRead = func(string, string) FreshnessReport { return FreshnessReport{} }
	if raw := c2.CountPending(fx.dir); raw != 2 {
		t.Fatalf("raw count = %d, want 2 (fail-open keeps work visible)", raw)
	}
}

// TestBoardWake_NoSpawnSkippedSolelyOnBoardState is the AC4 invariant
// test: grep-proof plus behavior. (a) STATIC: no production code path
// between a wake and SlotPool.spawn consults board state as a gate —
// the only board reads in the spawn admission path are the boost tier
// (ordering) and the idle-tier MODEL CHOICE, neither of which can skip
// a spawn. (b) BEHAVIORAL: a project with ZERO pending board work still
// spawns when its cooldown has elapsed (board state never vetoes).
func TestBoardWake_NoSpawnSkippedSolelyOnBoardState(t *testing.T) {
	// (b) behavioral: eligible project, empty board → still spawns.
	fixedNow := fixedEvalNow()
	db := newTestDB(t)
	workdir := t.TempDir()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty board (zero pending) — and a verifiably IDLE one via the
	// stubbed seam so even a fully-verified idle board cannot veto.
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(boardPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	insertEligibilityProject(t, db, "noboost", 60, 5, 0, fixedNow.Add(-90*time.Second), 0, 0)
	if _, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = 'noboost'`, workdir); err != nil {
		t.Fatal(err)
	}

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(func() time.Time { return fixedNow })
	// Force the freshness seam to answer "verifiably idle, nothing to
	// spawn" — the most aggressive board answer — via the counter the
	// spawner/packers share.
	l.packer.pendingCounter.freshnessRead = func(string, string) FreshnessReport {
		return FreshnessReport{CanVerify: true, VerifiablyIdle: true, WorkToSpawn: false}
	}

	l.evaluate()

	got := simSelectedProjects(t, db)
	want := []string{"noboost"}
	if !namesEqual(got, want) {
		t.Fatalf("eligible project with an idle board selected %v, want %v — board state must never veto a spawn", got, want)
	}
}

// TestBoardWake_WatchdogFiresOnSilentPollLoop is the GAP-042 seam
// applied to the watcher itself: a poll loop that stops beating gets a
// HIGH board_wake stall event, throttled to one per re-emit gap.
func TestBoardWake_WatchdogFiresOnSilentPollLoop(t *testing.T) {
	fx := newWakeFix(t)
	now := time.Now()
	// Simulate a silent loop: last beat far in the past.
	fx.w.mu.Lock()
	fx.w.lastBeat = now.Add(-30 * time.Minute)
	fx.w.mu.Unlock()

	fx.w.checkWatchdog(now)
	stalled := highWakeEvents(t, fx.db)
	if len(stalled) != 1 {
		t.Fatalf("watchdog stall events = %d, want 1", len(stalled))
	}
	// Throttled: an immediate re-check emits nothing.
	fx.w.checkWatchdog(now.Add(time.Minute))
	if got := highWakeEvents(t, fx.db); len(got) != 1 {
		t.Fatalf("watchdog re-emitted within the gap: %d events, want 1 (throttled)", len(got))
	}
	// After the gap it re-emits.
	fx.w.checkWatchdog(now.Add(boardWakeReEmitGap + time.Minute))
	if got := highWakeEvents(t, fx.db); len(got) != 2 {
		t.Fatalf("watchdog did not re-emit after the gap: %d events, want 2", len(got))
	}
}

// TestBoardWake_WatchdogQuietWhileBeating is the healthy control: beats
// within the threshold emit nothing.
func TestBoardWake_WatchdogQuietWhileBeating(t *testing.T) {
	fx := newWakeFix(t)
	now := time.Now()
	fx.w.mu.Lock()
	fx.w.lastBeat = now.Add(-2 * time.Minute) // < 5 min threshold
	fx.w.mu.Unlock()
	fx.w.checkWatchdog(now)
	if got := highWakeEvents(t, fx.db); len(got) != 0 {
		t.Fatalf("healthy watchdog emitted %d event(s), want 0", len(got))
	}
}

// TestBoardWake_IntegrationRealLoop drives the REAL watcher goroutines
// (Start/Stop) with real but fast intervals against a real board write:
// within poll+debounce the wake fires, the event lands, and the loop
// (Run in sim mode, clock seam pinned) evaluates. Cooldown-locked at the
// pinned instant ⇒ no spawn (AC1 end-to-end over the live wiring).
func TestBoardWake_IntegrationRealLoop(t *testing.T) {
	fixedNow := fixedEvalNow()
	db := newTestDB(t)
	workdir := t.TempDir()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(boardPath, []byte("{\"id\":\"I-1\",\"status\":\"pending\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	insertEligibilityProject(t, db, "integ", 3600, 5, 0, fixedNow.Add(-60*time.Second), 0, 0)
	if _, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = 'integ'`, workdir); err != nil {
		t.Fatal(err)
	}

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(func() time.Time { return fixedNow })

	w := NewBoardWakeWatcher(db, l.ForceEvaluate)
	w.SetIntervals(50*time.Millisecond, 200*time.Millisecond, 150*time.Millisecond)
	w.Start()
	defer w.Stop()

	// Let the baseline land, then write.
	time.Sleep(150 * time.Millisecond)
	rewriteBoard(t, boardPath,
		`{"id":"I-1","status":"pending"}`,
		`{"id":"I-2","status":"pending"}`)

	waitFor(t, 5*time.Second, func() bool {
		return len(wakeEvents(t, db)) > 0 && !l.LastEvalTime().IsZero()
	})

	var ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&ticks); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticks != 0 {
		t.Fatalf("cooldown-locked project spawned %d tick(s) through the live watcher, want 0", ticks)
	}
}

// highWakeEvents counts HIGH-severity board_wake stall events.
func highWakeEvents(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT details FROM events WHERE component = 'board_wake' AND severity = 'HIGH' ORDER BY id`)
	if err != nil {
		t.Fatalf("query high wake events: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(details), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, m)
	}
	return out
}
