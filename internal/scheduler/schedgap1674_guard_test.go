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

// SCHED-GAP-1674 acceptance tests. The four criteria map to:
//
//   AC 1 (nudge then abort):  TestSCHEDGAP1674_BuilderTickNudgedThenAborted
//   AC 2 (reporter exempt):   TestSCHEDGAP1674_ReporterLaneNotAborted
//   AC 3 (write-class spans): TestSCHEDGAP1674_WriteClassifier and
//                             TestSCHEDGAP1674_ExecuteCodeWriteBlocksAbort
//   AC 4 (config N/T):        TestSCHEDGAP1674_NamespaceKnobsParseAndApply
//
// The guard's state.db telemetry is redirected at a per-test fixture
// database via SCHEDULER_HERMES_STATE_DB (t.Setenv) — no test ever touches
// the developer's real ~/.hermes/state.db (the ambient-write rule).
//
// TIME. Every pass reads its decision instant through the loop's clock seam,
// so a SimClock drives the whole scenario deterministically (SCHED-GAP-169):
// spawn at T0, Advance(window) → nudge, Advance(window) again → abort.

// guardFixture is one test's world: the scheduler DB, the loop on a sim
// clock, and the fixture state.db path.
type guardFixture struct {
	db      *sql.DB
	stateDB string
	loop    *Loop
	now     time.Time
}

// newGuardFixture builds the world: migrated scheduler DB, a project in the
// given namespace with a running gateway tick (pid=0) spawned at the sim
// clock's instant, and a fresh (empty) Hermes state.db fixture.
func newGuardFixture(t *testing.T, nsID string, nsKnobs database.Namespace) *guardFixture {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()

	// Namespace with the requested guard config.
	nsKnobs.ID = nsID
	if err := database.CreateNamespace(ctx, db, &nsKnobs); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	// Project in that namespace (weight/priority must satisfy the CHECKs).
	nsRef := nsID
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "guard-proj", RepoURL: "https://example.com/guard", Workdir: t.TempDir(),
		Weight: 10, Priority: 5, NamespaceID: &nsRef,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Running gateway tick: pid=0, spawned_at = the sim clock's instant.
	now := time.Now().Truncate(time.Second)
	tickID := "guard-proj-2026-09-29-00-00-00"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, pid, spawned_at, created_at)
		 VALUES (?, ?, 'running', 0, ?, ?)`,
		tickID, "guard-proj", now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatalf("seed running tick: %v", err)
	}

	// Empty Hermes state.db fixture (real schema, no rows).
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
	if _, err := sdb.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
		role TEXT NOT NULL, content TEXT, tool_calls TEXT, tool_name TEXT,
		timestamp REAL NOT NULL)`); err != nil {
		t.Fatalf("create fixture messages: %v", err)
	}
	sdb.Close()
	t.Setenv(hermesStateDBPathEnv, stateDB)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	sim := clock.NewSimClockAt(1, now)
	loop.SetClock(sim)

	return &guardFixture{db: db, stateDB: stateDB, loop: loop, now: now}
}

// advance moves the sim clock and returns the new instant.
func (f *guardFixture) advance(d time.Duration) time.Time {
	c := f.loop.clock().(*clock.SimClock)
	c.Advance(d)
	return c.Now()
}

// seedSession writes a gateway session + transcript for the fixture tick:
// msgs plain messages and toolCalls tool-result interactions of the given
// tool name. Returns the session row id.
func (f *guardFixture) seedSession(t *testing.T, tickID string, msgs, toolCalls int, toolName string) {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer sdb.Close()
	start := float64(f.now.UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, message_count, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?, ?)`,
		"sess-"+tickID, tickID, start, msgs, toolCalls); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i := 0; i < toolCalls; i++ {
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_name, content, timestamp)
			 VALUES (?, 'tool', ?, '{}', ?)`,
			"sess-"+tickID, toolName, start+float64(i)); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
}

// seedSessionWithArgs is seedSession with the tool RESULTS carrying the
// given content (the write-class marker channel) — used by the
// execute_code/heredoc acceptance arm.
func (f *guardFixture) seedSessionWithArgs(t *testing.T, tickID string, msgs, toolCalls int, toolName, resultContent string) {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer sdb.Close()
	start := float64(f.now.UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, message_count, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?, ?)`,
		"sess-"+tickID, tickID, start, msgs, toolCalls); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i := 0; i < toolCalls; i++ {
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_name, content, timestamp)
			 VALUES (?, 'tool', ?, ?, ?)`,
			"sess-"+tickID, toolName, resultContent, start+float64(i)); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
}

// guardTickOutcome reads back the fixture tick's terminal columns.
func (f *guardFixture) guardTickOutcome(t *testing.T, tickID string) (status, outcome string, nudges int) {
	t.Helper()
	var nudgeCount int
	var nudgedAt sql.NullString
	if err := f.db.QueryRow(
		`SELECT status, COALESCE(outcome,''), nudge_count, COALESCE(guard_nudged_at,'')
		 FROM ticks WHERE id = ?`, tickID).
		Scan(&status, &outcome, &nudgeCount, &nudgedAt); err != nil {
		t.Fatalf("read tick row: %v", err)
	}
	_ = nudgedAt
	return status, outcome, nudgeCount
}

// TestSCHEDGAP1674_BuilderTickNudgedThenAborted is acceptance 1: a builder
// tick with 30 recon interactions and no writes is nudged at the first
// window and aborted with outcome 'aborted:no_artifact' after the second.
func TestSCHEDGAP1674_BuilderTickNudgedThenAborted(t *testing.T) {
	f := newGuardFixture(t, "coding-hermes", database.Namespace{Weight: 10})
	tickID := "guard-proj-2026-09-29-00-00-00"
	// 30 recon interactions: 5 messages + 25 read-only tool calls (>= N=25).
	f.seedSession(t, tickID, 5, 25, "read_file")

	// Window 1: elapsed reaches T=20m with zero writes → NUDGE.
	f.advance(20 * time.Minute)
	nudged, aborted := f.loop.builderGuardPass()
	if nudged != 1 || aborted != 0 {
		t.Fatalf("first window: nudged=%d aborted=%d, want 1/0", nudged, aborted)
	}
	if _, _, nudges := f.guardTickOutcome(t, tickID); nudges != 1 {
		t.Errorf("nudge_count after window 1 = %d, want 1", nudges)
	}

	// Still inside the second window: no abort yet.
	f.advance(10 * time.Minute)
	if _, aborted := f.loop.builderGuardPass(); aborted != 0 {
		t.Fatalf("mid-second-window pass aborted early (window is 20m from the nudge)")
	}

	// Window 2: another full window past the nudge → ABORT. The abort
	// cancels the session (no live session is registered in this unit
	// fixture, so CancelTickSession reports false — the guard records that
	// honestly in its event) and the spawn-path contract maps the
	// outcome. The terminal verdict itself is pinned by
	// TestSCHEDGAP1674_GuardAbortOutcomePersistence below.
	f.advance(10 * time.Minute) // now 20m past the nudge
	nudged2, aborted2 := f.loop.builderGuardPass()
	if nudged2 != 0 || aborted2 != 1 {
		t.Fatalf("second window: nudged=%d aborted=%d, want 0/1", nudged2, aborted2)
	}
}

// TestSCHEDGAP1674_GuardAbortOutcomePersistence pins the terminal half of
// acceptance 1 at the persistence layer: a guard-aborted SpawnedTick's
// Wait() → lifecycle.Complete writes status=failed AND
// outcome='aborted:no_artifact' — never dry_run, never committed.
func TestSCHEDGAP1674_GuardAbortOutcomePersistence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "aborted-proj", RepoURL: "https://example.com/a", Workdir: t.TempDir(),
		Weight: 10, Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tickID := "aborted-proj-2026-09-29-00-00-01"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, pid, spawned_at, created_at)
		 VALUES (?, ?, 'running', 0, '2026-09-29T00:00:00Z', '2026-09-29T00:00:00Z')`, tickID, "aborted-proj"); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	s := NewSpawner(db, 2)
	now := time.Now()
	st := &SpawnedTick{
		TickID:     tickID,
		Project:    "aborted-proj",
		SessionID:  tickID,
		Started:    now.Add(-45 * time.Minute),
		spawner:    s,
		completed:  false,
		completeAt: now,
		guardAbort: true,
	}
	lt := NewLifecycleTracker(db)
	if err := lt.Complete(st.Wait()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var status, outcome string
	if err := db.QueryRow(`SELECT status, outcome FROM ticks WHERE id = ?`, tickID).
		Scan(&status, &outcome); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "failed" || outcome != "aborted:no_artifact" {
		t.Errorf("guard-aborted row = (%s,%s), want (failed,aborted:no_artifact)", status, outcome)
	}
}

// TestSCHEDGAP1674_ReporterLaneNotAborted is acceptance 2: a reporter-class
// lane with the IDENTICAL no-write profile is neither nudged nor aborted.
func TestSCHEDGAP1674_ReporterLaneNotAborted(t *testing.T) {
	f := newGuardFixture(t, "duckbrain-sync", database.Namespace{
		Weight: 10, ReporterClass: "reporter",
	})
	tickID := "guard-proj-2026-09-29-00-00-00"
	f.seedSession(t, tickID, 5, 25, "read_file")

	// Two full windows: the reporter lane is exempt the whole time.
	f.advance(40 * time.Minute)
	nudged, aborted := f.loop.builderGuardPass()
	if nudged != 0 || aborted != 0 {
		t.Fatalf("reporter lane touched: nudged=%d aborted=%d, want 0/0", nudged, aborted)
	}
	if _, _, nudges := f.guardTickOutcome(t, tickID); nudges != 0 {
		t.Errorf("reporter tick nudge_count = %d, want 0", nudges)
	}
	// The row is still running.
	if status, _, _ := f.guardTickOutcome(t, tickID); status != "running" {
		t.Errorf("reporter tick status = %s, want running", status)
	}
}

// TestSCHEDGAP1674_WriteClassifier is acceptance 3 at the unit level: the
// write-class classifier counts execute_code, heredoc/bash writes and git
// commits as writes — not just write_file.
func TestSCHEDGAP1674_WriteClassifier(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		content  string
		callArgs string
		want     bool
	}{
		{"write_file tool is a write", "write_file", "", "", true},
		{"str_replace tool is a write", "str_replace", "", "", true},
		{"execute_code with plain read is recon", "execute_code", "", `{"code":"print(open('f').read())"}`, false},
		{"execute_code with heredoc is a write", "execute_code", "", `{"code":"open('f','w').write('x')\n<<EOF"}`, true},
		{"terminal with redirect is a write", "terminal", "", `{"command":"echo hi > out.txt"}`, true},
		{"terminal with heredoc is a write", "terminal", "", `{"command":"cat << EOF > f.txt\nbody\nEOF"}`, true},
		{"terminal git commit is a write", "terminal", "", `{"command":"git commit -m 'x'"}`, true},
		{"terminal git status is recon", "terminal", "", `{"command":"git status"}`, false},
		{"terminal read_file-ish is recon", "terminal", "", `{"command":"cat f.txt"}`, false},
		{"write RESULT marker is a write", "terminal", `{"bytes_written": 3285, "verified": true}`, "", true},
		{"unknown tool with clean args is recon", "skill_view", "", `{"name":"x"}`, false},
	}
	for _, tc := range cases {
		if got := writeClassifier(tc.tool, tc.content, tc.callArgs); got != tc.want {
			t.Errorf("writeClassifier(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSCHEDGAP1674_ExecuteCodeWriteBlocksAbort is acceptance 3 end to end:
// a tick whose transcript shows ONLY execute_code calls that WROTE (heredoc
// markers in the results) is never nudged — the write-class count protects
// it across both windows.
func TestSCHEDGAP1674_ExecuteCodeWriteBlocksAbort(t *testing.T) {
	f := newGuardFixture(t, "coding-hermes", database.Namespace{Weight: 10})
	tickID := "guard-proj-2026-09-29-00-00-00"
	// 30 execute_code interactions whose RESULTS carry the real write
	// receipt (the live tool answers {"bytes_written": N, ...} after a
	// successful write) — write-class across the whole edit surface.
	f.seedSessionWithArgs(t, tickID, 30, 30, "execute_code",
		`{"status":"success","bytes_written": 2048,"verified": true}`)

	f.advance(45 * time.Minute)
	nudged, aborted := f.loop.builderGuardPass()
	if nudged != 0 || aborted != 0 {
		t.Fatalf("write-class tick touched: nudged=%d aborted=%d, want 0/0", nudged, aborted)
	}
}

// TestSCHEDGAP1674_NamespaceKnobsParseAndApply is acceptance 4: a
// namespace-level override of N and T parses and applies — a small window
// (2m) and floor (3) fire the nudge+abort at 2m/4m instead of 20m/40m; the
// defaults remain 20m/25 for a namespace that does not set them.
func TestSCHEDGAP1674_NamespaceKnobsParseAndApply(t *testing.T) {
	// Defaults namespace.
	f := newGuardFixture(t, "coding-hermes", database.Namespace{Weight: 10})
	knobs := resolveNoArtifactKnobs(context.Background(), f.db, "coding-hermes")
	if knobs.window != DefaultNoArtifactWindow || knobs.reconFloor != DefaultNoArtifactReconFloor {
		t.Errorf("default knobs = (%v, %d), want (%v, %d)",
			knobs.window, knobs.reconFloor, DefaultNoArtifactWindow, DefaultNoArtifactReconFloor)
	}
	if DefaultNoArtifactWindow != 20*time.Minute || DefaultNoArtifactReconFloor != 25 {
		t.Errorf("documented defaults drifted: window=%v floor=%d, want 20m0s/25",
			DefaultNoArtifactWindow, DefaultNoArtifactReconFloor)
	}

	// Override namespace: window=2m, floor=3.
	f2 := newGuardFixture(t, "fast-ns", database.Namespace{
		Weight: 10, NoArtifactWindow: "2m", NoArtifactReconFloor: "3",
	})
	knobs2 := resolveNoArtifactKnobs(context.Background(), f2.db, "fast-ns")
	if knobs2.window != 2*time.Minute || knobs2.reconFloor != 3 {
		t.Fatalf("override knobs = (%v, %d), want (2m0s, 3)", knobs2.window, knobs2.reconFloor)
	}
	tickID := "guard-proj-2026-09-29-00-00-00"
	// 3 recon interactions — under the DEFAULT floor (25) but over THIS
	// namespace's floor (3): the override must be what applies.
	f2.seedSession(t, tickID, 0, 3, "read_file")

	f2.advance(2 * time.Minute)
	if nudged, _ := f2.loop.builderGuardPass(); nudged != 1 {
		t.Fatalf("override window: nudged=%d, want 1 (2m window, 3-interaction floor)", nudged)
	}
	f2.advance(2 * time.Minute)
	if _, aborted := f2.loop.builderGuardPass(); aborted != 1 {
		t.Fatalf("override second window: aborted=%d, want 1", aborted)
	}

	// A garbage window value falls back to the default (never zero/immortal).
	if err := database.CreateNamespace(context.Background(), f.db, &database.Namespace{
		ID: "broken-ns", Weight: 10, NoArtifactWindow: "banana",
	}); err != nil {
		t.Fatalf("CreateNamespace broken: %v", err)
	}
	knobs3 := resolveNoArtifactKnobs(context.Background(), f.db, "broken-ns")
	if knobs3.window != DefaultNoArtifactWindow {
		t.Errorf("unparseable window resolved to %v, want the %v default", knobs3.window, DefaultNoArtifactWindow)
	}
}

// TestSCHEDGAP1674_WriteObservedResetsGuard pins the safety direction: a
// tick that wrote ANYTHING inside the window (git artifacts present —
// seeded via a commit-looking ticks row is not possible here, so the
// session-side write signal carries the arm) is never touched, even after
// two windows.
func TestSCHEDGAP1674_WriteObservedResetsGuard(t *testing.T) {
	f := newGuardFixture(t, "coding-hermes", database.Namespace{Weight: 10})
	tickID := "guard-proj-2026-09-29-00-00-00"
	// 30 recon calls PLUS one write_file call.
	f.seedSession(t, tickID, 5, 30, "read_file")
	sdb, _ := sql.Open("sqlite", f.stateDB)
	defer sdb.Close()
	start := float64(f.now.UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO messages (session_id, role, tool_name, content, timestamp)
		 VALUES ('sess-`+tickID+`', 'tool', 'write_file', '{}', ?)`, start); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	f.advance(45 * time.Minute)
	nudged, aborted := f.loop.builderGuardPass()
	if nudged != 0 || aborted != 0 {
		t.Fatalf("tick with a write was touched: nudged=%d aborted=%d, want 0/0", nudged, aborted)
	}
}

// TestSCHEDGAP1674_CancelRegistryRoundTrip pins the abort mechanism: a
// registered session context is cancellable through CancelTickSession (and
// consumed exactly once), an unknown tick id reports false.
func TestSCHEDGAP1674_CancelRegistryRoundTrip(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 2)
	if s.CancelTickSession("nope") {
		t.Error("CancelTickSession on an unknown tick returned true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.RegisterTickSessionContext("tick-a", cancel)
	if !s.CancelTickSession("tick-a") {
		t.Error("CancelTickSession on a registered tick returned false")
	}
	if ctx.Err() == nil {
		t.Error("the registered session context was not cancelled")
	}
	// Consumed: the registry entry was removed; a second cancel reports
	// false (the tick is finished).
	if s.CancelTickSession("tick-a") {
		t.Error("CancelTickSession fired twice for one registration")
	}
}

// TestSCHEDGAP1674_TerminalOutcomeMapping pins the status guard: the
// guard verdict rides ONLY a failed row; a (hypothetical, contract-violating)
// GuardAbort with a completed status falls back to that status's own mapping
// so the guard outcome can never ride a committed row.
func TestSCHEDGAP1674_TerminalOutcomeMapping(t *testing.T) {
	if got := terminalOutcome(TickOutcome{Status: TickFailed, GuardAbort: true}); got != "aborted:no_artifact" {
		t.Errorf("terminalOutcome(failed+guard) = %q, want aborted:no_artifact", got)
	}
	if got := terminalOutcome(TickOutcome{Status: TickCompleted, GuardAbort: true}); got != "committed" {
		// completed+GuardAbort is unreachable by construction (the spawn
		// path never sets the flag on a completed tick) — but if the
		// invariant ever breaks, the outcome must stay the status's own,
		// never laundered through the guard verdict on a success row.
		t.Errorf("terminalOutcome(completed+guard) = %q, want committed (guard verdict must not ride a success)", got)
	}
	if got := terminalOutcome(TickOutcome{Status: TickCompleted, Commits: 0}); got != "dry_run" {
		t.Errorf("plain completed-with-no-artifact = %q, want dry_run (SCHED-GAP-1652 unchanged)", got)
	}
}
