package scheduler

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1707 acceptance, watchdog arm. In-package so the test can drive
// sessionSilencePass on the simulator clock and seed the fixture state.db
// exactly like the SCHED-GAP-1674 guard tests do
// (SCHEDULER_HERMES_STATE_DB → t.Setenv; no test ever touches the real
// ~/.hermes/state.db).
//
// TIME. The pass reads its decision instant from the loop's clock seam, so
// one SimClock drives the whole scenario deterministically: spawn at T0,
// Advance(grace) → the silent tick dies, the productive twin survives.

// silenceCreateProject is the in-package project creator (the external-test
// mustCreateProject lives in the scheduler_test package).
func silenceCreateProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: t.TempDir(),
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestSCHEDGAP1707_WaveIngestRecordsConvergence pins deliverable 3: ingest
// counts the manifest's terminal-verdict workers onto ticks.workers_terminal
// via the SAME predicate waveWorkerTerminalState applies to the rows.
func TestSCHEDGAP1707_WaveIngestRecordsConvergence(t *testing.T) {
	db := newTestDB(t)
	workdir := t.TempDir()
	tickID := "wave-1707-2026-10-03-00-00-00"
	waveSeedTickProject(t, db, tickID, workdir, "wave-proj")

	manifest := `{"tick_id":"` + tickID + `","project":"wave-proj",
	  "started_at":"2026-10-03T00:00:00Z","finished_at":"2026-10-03T01:00:00Z",
	  "workers":[
	    {"task_id":"t1","branch":"wt/t1","commit_sha":"a1","judge":"pass","merge":"merged"},
	    {"task_id":"t2","branch":"wt/t2","commit_sha":"","judge":"unknown","merge":"pending"},
	    {"task_id":"t3","branch":"wt/t3","commit_sha":"a3","judge":"fail","merge":"preserved"}
	  ]}`
	writeWaveManifest(t, workdir, tickID, manifest)

	n, err := ingestWaveManifest(context.Background(), db, workdir, "wave-proj", tickID)
	if err != nil {
		t.Fatalf("ingestWaveManifest: %v", err)
	}
	if n != 3 {
		t.Fatalf("ingested %d workers, want 3", n)
	}
	var wc, term int
	if err := db.QueryRow(`SELECT worker_count, workers_terminal FROM ticks WHERE id = ?`, tickID).
		Scan(&wc, &term); err != nil {
		t.Fatalf("read tick: %v", err)
	}
	if wc != 3 {
		t.Errorf("worker_count = %d, want 3", wc)
	}
	if term != 2 {
		t.Errorf("workers_terminal = %d, want 2 (t1 pass/merged + t3 fail/preserved; t2 has no verdict)", term)
	}
}

// TestSCHEDGAP1707_CompletedRowNeverPartial: the completed path ignores the
// mark — only timeout rows may read partial.
func TestSCHEDGAP1707_CompletedRowNeverPartial(t *testing.T) {
	db := newTestDB(t)
	silenceCreateProject(t, db, "alpha")
	lt := NewLifecycleTracker(db)
	tickID := "alpha-1707-done"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(TickOutcome{
		TickID: tickID, Project: "alpha",
		Started: now.Add(-time.Minute), Finished: now,
		Status: TickCompleted, ExitCode: 0,
		TelemetryPartial: true, // must be IGNORED on the completed path
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var partial int
	if err := db.QueryRow(`SELECT telemetry_partial FROM ticks WHERE id = ?`, tickID).Scan(&partial); err != nil {
		t.Fatalf("read completed row: %v", err)
	}
	if partial != 0 {
		t.Errorf("telemetry_partial = %d on a COMPLETED row, want 0", partial)
	}
}

// TestSCHEDGAP1707_SerialTickKeepsUnmeasuredSentinel: a tick with no
// manifest keeps workers_terminal at -1 — never a fabricated 0.
func TestSCHEDGAP1707_SerialTickKeepsUnmeasuredSentinel(t *testing.T) {
	db := newTestDB(t)
	silenceCreateProject(t, db, "alpha")
	lt := NewLifecycleTracker(db)
	tickID := "alpha-1707-serial"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	now := time.Now().UTC()
	if err := lt.Complete(TickOutcome{
		TickID: tickID, Project: "alpha",
		Started: now.Add(-time.Minute), Finished: now,
		Status: TickCompleted, ExitCode: 0,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var workers int
	if err := db.QueryRow(`SELECT workers_terminal FROM ticks WHERE id = ?`, tickID).Scan(&workers); err != nil {
		t.Fatalf("read serial row: %v", err)
	}
	if workers != -1 {
		t.Errorf("workers_terminal = %d, want -1 (unmeasured)", workers)
	}
}

// TestSCHEDGAP1707_StaleReapMarksPartial pins the CleanupStaleProjects arm:
// the direct SQL flip stamps telemetry_partial=1 named stale_reap in the
// SAME update, so a reaped row is never an idle-looking 0/0/0 timeout.
func TestSCHEDGAP1707_StaleReapMarksPartial(t *testing.T) {
	db := newTestDB(t)
	silenceCreateProject(t, db, "alpha")
	lt := NewLifecycleTracker(db)
	lt.SetClock(clock.NewSimClockAt(1, time.Now()))
	tickID := "alpha-1707-stale"
	if err := lt.Enqueue("alpha", tickID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := lt.StartRunning(tickID); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	// Backdate the tick past the stale window via raw SQL (the tracker has
	// no backdating API — the spawn instant is the reap's clock input).
	if _, err := db.Exec(`UPDATE ticks SET spawned_at = ? WHERE id = ?`,
		time.Now().Add(-3*time.Hour).Format(time.RFC3339), tickID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	projects, n, err := lt.CleanupStaleProjects(2 * time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleProjects: %v", err)
	}
	if n != 1 || len(projects) != 1 || projects[0] != "alpha" {
		t.Fatalf("reap = (%v, %d), want ([alpha], 1)", projects, n)
	}
	var status, reason string
	var partial int
	if err := db.QueryRow(`SELECT status, telemetry_partial, telemetry_partial_reason
	   FROM ticks WHERE id = ?`, tickID).Scan(&status, &partial, &reason); err != nil {
		t.Fatalf("read reaped row: %v", err)
	}
	if status != string(TickTimeout) || partial != 1 || reason != TelemetryPartialStaleReap {
		t.Errorf("reaped row = (%s, %d, %q), want (timeout, 1, stale_reap)", status, partial, reason)
	}
}

// TestSCHEDGAP1707_TimeoutTelemetryCapture pins the pure capture helper: the
// mark always sets, the caller's numbers pass through, and an unknown reason
// falls back to the most common class.
func TestSCHEDGAP1707_TimeoutTelemetryCapture(t *testing.T) {
	tin, tout, cost, partial, reason := timeoutTelemetry(TelemetryPartialDispatchDeadline, 100, 5, 0.01)
	if !partial || reason != TelemetryPartialDispatchDeadline || tin != 100 || tout != 5 || cost != 0.01 {
		t.Fatalf("capture = (%d,%d,%v,%v,%q), want pass-through with the mark", tin, tout, cost, partial, reason)
	}
	_, _, _, _, reason = timeoutTelemetry("made_up", 0, 0, 0)
	if reason != TelemetryPartialTickDeadline {
		t.Errorf("unknown reason fell back to %q, want %q", reason, TelemetryPartialTickDeadline)
	}
	pt := markSilentTelemetry(7, 3, 0.5, 5400)
	if !pt.partial || pt.reason != TelemetryPartialSessionSilent || pt.silenceS != 5400 || pt.tokensIn != 7 {
		t.Errorf("markSilentTelemetry = %+v, want silent tuple with silence 5400", pt)
	}
}

// TestSCHEDGAP1707_VocabularyAndErrorText pins the closed vocabulary and the
// classifier marker (a watchdog row must stamp failure_reason=session_silent
// through failureReasonClass, never feed health accounting).
func TestSCHEDGAP1707_VocabularyAndErrorText(t *testing.T) {
	for _, r := range []string{
		TelemetryPartialTickDeadline, TelemetryPartialSessionSilent,
		TelemetryPartialStaleReap, TelemetryPartialDispatchDeadline,
	} {
		if !telemetryPartialReasonIsValid(r) {
			t.Errorf("reason %q must be valid", r)
		}
	}
	if telemetryPartialReasonIsValid("") || telemetryPartialReasonIsValid("made_up") {
		t.Error("empty/unknown reasons must be invalid")
	}
	text := sessionSilentError("90s")
	if !HarnessFailure(text) {
		t.Errorf("watchdog error text %q must classify as harness failure", text)
	}
	if got := failureReasonClass(text); got != "session_silent" {
		t.Errorf("failureReasonClass(%q) = %q, want session_silent", text, got)
	}
}

// silenceErrorTextForTest is the external-test bridge into
// sessionSilentError (schedgap1707_telemetry_test.go renders the watchdog's
// ticks.error text through it).
func silentErrorTextForTest() string {
	return sessionSilentError("5400s")
}

// silenceFixture is one test's world: the scheduler DB, a spawner + loop on
// a sim clock, and a fixture state.db path.
type silenceFixture struct {
	db      *sql.DB
	stateDB string
	loop    *Loop
	spawner *Spawner
	now     time.Time
}

// newSilenceFixture builds the world: migrated scheduler DB, a project with
// a running gateway tick (pid=0) spawned at the sim clock's instant, a
// spawner wired to the loop, and a fresh (empty) Hermes state.db fixture.
func newSilenceFixture(t *testing.T, projectName string) *silenceFixture {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()

	if err := database.CreateProject(ctx, db, &database.Project{
		Name: projectName, RepoURL: "https://example.com/" + projectName, Workdir: t.TempDir(),
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tickID := projectName + "-2026-10-03-00-00-00"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, pid, spawned_at, created_at, session_id)
		 VALUES (?, ?, 'running', 0, ?, ?, ?)`,
		tickID, projectName, now.Format(time.RFC3339), now.Format(time.RFC3339), tickID); err != nil {
		t.Fatalf("seed running tick: %v", err)
	}

	// Empty Hermes state.db fixture (real schema, no rows) — the same
	// fixture shape the guard tests use, plus session_model_usage so the
	// watchdog's token probe has its table.
	stateDB := filepath.Join(t.TempDir(), "state.db")
	sdb, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("open fixture state db: %v", err)
	}
	if _, err := sdb.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, source TEXT NOT NULL, session_key TEXT,
		started_at REAL NOT NULL, ended_at REAL, message_count INTEGER DEFAULT 0,
		tool_call_count INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create fixture sessions: %v", err)
	}
	if _, err := sdb.Exec(`CREATE TABLE session_model_usage (
		session_id TEXT, model TEXT, billing_provider TEXT DEFAULT '',
		estimated_cost_usd REAL DEFAULT 0, actual_cost_usd REAL DEFAULT 0,
		first_seen REAL, last_seen REAL,
		input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0)`); err != nil {
		t.Fatalf("create fixture session_model_usage: %v", err)
	}
	sdb.Close()
	t.Setenv(hermesStateDBPathEnv, stateDB)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	spawner := NewSpawner(db, 4)
	loop.spawner = spawner
	sim := clock.NewSimClockAt(1, now)
	loop.SetClock(sim)

	return &silenceFixture{db: db, stateDB: stateDB, loop: loop, spawner: spawner, now: now}
}

// silenceTickID returns the fixture's tick id for its project.
func (f *silenceFixture) tickID(projectName string) string {
	return projectName + "-2026-10-03-00-00-00"
}

// seedActivity registers a sessions row for the tick (session_key = tick id)
// and, when tokens > 0, a session_model_usage row carrying them — the two
// legs the watchdog's probe reads.
func (f *silenceFixture) seedActivity(t *testing.T, projectName string, tokens, toolCalls int64) {
	t.Helper()
	tickID := f.tickID(projectName)
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer sdb.Close()
	start := float64(f.now.UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?)`,
		"sess-"+tickID, tickID, start, toolCalls); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if tokens > 0 {
		if _, err := sdb.Exec(
			`INSERT INTO session_model_usage (session_id, model, input_tokens, output_tokens, first_seen, last_seen)
			 VALUES (?, 'm', ?, ?, ?, ?)`,
			"sess-"+tickID, tokens/2, tokens-tokens/2, start, start+60); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
}

// TestSCHEDGAP1707_WatchdogKillsSilentSessionSparesActiveOne is the
// acceptance core: after the grace elapses, a tick whose session shows zero
// token delta and zero tool activity is terminated (a session_silent
// verdict registered on the spawner), while its ACTIVE twin — a genuinely
// long-running session with real tokens and tool calls — survives the same
// pass. Off by default: with the knob unset both survive (library default).
func TestSCHEDGAP1707_WatchdogKillsSilentSessionSparesActiveOne(t *testing.T) {
	f := newSilenceFixture(t, "silent-proj")
	silent := f.tickID("silent-proj")

	// The ACTIVE twin: a second project + tick + a PRODUCING session (real
	// tokens and tool calls — the genuinely-long-running shape).
	if err := database.CreateProject(context.Background(), f.db, &database.Project{
		Name: "active-proj", RepoURL: "https://example.com/active", Workdir: t.TempDir(),
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject active: %v", err)
	}
	active := "active-proj-2026-10-03-00-00-00"
	if _, err := f.db.Exec(
		`INSERT INTO ticks (id, project_name, status, pid, spawned_at, created_at, session_id)
		 VALUES (?, 'active-proj', 'running', 0, ?, ?, ?)`,
		active, f.now.Format(time.RFC3339), f.now.Format(time.RFC3339), active); err != nil {
		t.Fatalf("seed active tick: %v", err)
	}
	f.seedActivity(t, "active-proj", 512000, 140)

	// Library default: the watchdog is OFF — nothing is killed even past
	// any grace.
	f.advance(2 * time.Hour)
	if killed := f.loop.sessionSilencePass(f.loopClockNow()); killed != 0 {
		t.Fatalf("watchdog fired with the knob unset: killed=%d, want 0 (library default is off)", killed)
	}
	if _, ok := f.spawner.sessionSilenceFor(silent); ok {
		t.Fatal("silent tick verdict registered while the watchdog is off")
	}

	// Arm the grace and run the pass INSIDE it: both survive.
	SetSessionSilenceGrace(45 * time.Minute)
	t.Cleanup(func() { SetSessionSilenceGrace(0) })
	f.resetTo(f.now.Add(10 * time.Minute))
	if killed := f.loop.sessionSilencePass(f.loopClockNow()); killed != 0 {
		t.Fatalf("pass killed %d tick(s) inside the grace window, want 0", killed)
	}

	// Register a LIVE session ctx for the silent tick (the realistic
	// watchdog shape: a running gateway spawn holds its cancel in the
	// registry; CancelTickSession needs it to report true).
	silentCtx, silentCancel := context.WithCancel(context.Background())
	defer silentCancel()
	defer func() { _ = silentCtx }()
	f.spawner.RegisterTickSessionContext(silent, silentCancel)

	// Past the grace: the silent tick dies, the active one survives.
	f.resetTo(f.now.Add(50 * time.Minute))
	killed := f.loop.sessionSilencePass(f.loopClockNow())
	if killed != 1 {
		t.Fatalf("pass killed %d tick(s), want 1 (the silent one)", killed)
	}
	pt, ok := f.spawner.sessionSilenceFor(silent)
	if !ok {
		t.Fatal("watchdog did not register the session_silent verdict for the silent tick")
	}
	if pt.reason != TelemetryPartialSessionSilent || !pt.partial {
		t.Errorf("verdict = (%v, %q), want partial/session_silent", pt.partial, pt.reason)
	}
	if pt.silenceS != int64((50 * time.Minute).Seconds()) {
		t.Errorf("silence = %ds, want %d", pt.silenceS, int64((50 * time.Minute).Seconds()))
	}
	if _, consumed := f.spawner.sessionSilenceFor(silent); consumed {
		t.Error("verdict not consume-once")
	}
	if _, ok := f.spawner.sessionSilenceFor(active); ok {
		t.Fatal("watchdog killed the PRODUCING session — it must never kill a session with token/tool activity")
	}

	// The guard registry (CancelTickSession stamped guardAbortedTicks on
	// the way to the cancel): the ACTIVE tick must not carry it.
	if f.spawner.guardAborted(active) {
		t.Error("active tick marked guard-aborted")
	}
}

// TestSCHEDGAP1707_WatchdogZeroActivityRowIsStillSilent covers the
// never-registered arm: a tick whose gateway session was never persisted
// into the agent store measures as zero-activity-from-spawn and IS killed
// (the purest silence).
func TestSCHEDGAP1707_WatchdogZeroActivityRowIsStillSilent(t *testing.T) {
	f := newSilenceFixture(t, "unregistered-proj")
	SetSessionSilenceGrace(15 * time.Minute)
	t.Cleanup(func() { SetSessionSilenceGrace(0) })

	// No seedActivity call — the store has no row for this tick at all.
	// Register the live session ctx so the cancel path reports success.
	ctxID := f.tickID("unregistered-proj")
	_, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	f.spawner.RegisterTickSessionContext(ctxID, ctxCancel)

	f.advance(20 * time.Minute)
	if killed := f.loop.sessionSilencePass(f.loopClockNow()); killed != 1 {
		t.Fatalf("killed=%d, want 1 (an unregistered session is zero-activity silence)", killed)
	}
}

// advance moves the sim clock forward from anywhere.
func (f *silenceFixture) advance(d time.Duration) {
	f.loop.clock().(*clock.SimClock).Advance(d)
}

// resetTo moves the sim clock to an absolute instant (the fixture's anchor
// arithmetic stays readable).
func (f *silenceFixture) resetTo(t time.Time) {
	f.loop.clock().(*clock.SimClock).SetNow(t)
}

// loopClockNow reads the loop's current sim instant.
func (f *silenceFixture) loopClockNow() time.Time {
	return f.loop.clock().Now()
}
