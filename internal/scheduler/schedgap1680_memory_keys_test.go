package scheduler

// SCHED-GAP-1680 tests: a tick's success is derived from the artifacts its
// LANE can produce — git commits/files AND non-commit side effects.
//
// The measured problem (2026-09-30): `commits=0` was the fleet's success
// test, and it can only see git artifacts. Every sync lane showed the same
// 7-ticks / 6-zero-commit shape (85% "no-op") while auger-sync's own tick
// reported "Keys written (5 total, all verified)" with commits=0. These
// tests pin the non-commit leg end to end: the outcome mapping, the
// tool-call classifier, the transcript counter, the lifecycle persistence
// and the lane-output family metric.
//
// The fixture state.db is redirected per-test via SCHEDULER_HERMES_STATE_DB
// (t.Setenv) — no test ever touches the developer's real ~/.hermes/state.db
// (the ambient-write rule).

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSCHEDGAP1680_TerminalOutcomeCountsNonCommitArtifacts locks the outcome
// mapping: commits=0 with memory keys written is NOT dry_run, and the
// non-commit artifact outranks the zero-tool no_work verdict.
func TestSCHEDGAP1680_TerminalOutcomeCountsNonCommitArtifacts(t *testing.T) {
	cases := []struct {
		name string
		in   TickOutcome
		want string
	}{
		// The row's exact scenario: a sync tick with five verified keys and
		// commits=0 must never be derived as dry_run.
		{"five keys, zero commits is an artifact", TickOutcome{Status: TickCompleted, Commits: 0, FilesChanged: 0, MemoryKeys: 5}, "committed"},
		// A zero-tool session that DID write memory keys is still productive.
		{"keys outrank the zero-tool no_work verdict", TickOutcome{Status: TickCompleted, NoTools: true, Commits: 0, FilesChanged: 0, MemoryKeys: 2}, "committed"},
		// Commits still win (unchanged behavior).
		{"commits still map to committed", TickOutcome{Status: TickCompleted, Commits: 1}, "committed"},
		{"files changed still map to committed", TickOutcome{Status: TickCompleted, FilesChanged: 3}, "committed"},
		// No artifact of any kind keeps the pre-existing verdicts.
		{"no artifact at all stays dry_run", TickOutcome{Status: TickCompleted, Commits: 0, FilesChanged: 0, MemoryKeys: 0}, "dry_run"},
		{"zero-tool no-artifact stays no_work", TickOutcome{Status: TickCompleted, NoTools: true, Commits: 0, FilesChanged: 0, MemoryKeys: 0}, "no_work"},
		// Non-completed statuses ignore the non-commit signal entirely.
		{"failed ignores memory keys", TickOutcome{Status: TickFailed, MemoryKeys: 9}, "failed"},
		{"timeout ignores memory keys", TickOutcome{Status: TickTimeout, MemoryKeys: 9}, "timeout"},
		{"deferred ignores memory keys", TickOutcome{Status: TickDeferred, MemoryKeys: 9}, "deferred"},
		{"guard abort keeps its verdict", TickOutcome{Status: TickFailed, GuardAbort: true, MemoryKeys: 9}, AbortOutcomeValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalOutcome(tc.in); got != tc.want {
				t.Fatalf("terminalOutcome(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSCHEDGAP1680_MemoryWriteToolClassification pins the counting rule:
// only memory-store WRITE calls count — a file tool, a board tool, or a
// memory READ must not inflate the artifact count.
func TestSCHEDGAP1680_MemoryWriteToolClassification(t *testing.T) {
	writes := []string{
		"mcp__duckbrain__remember",
		"duckbrain_remember",
		"duckbrain_append_memory",
		"memory_write",
		"duckbrain_post_memory",
		"mcp__duckbrain__create_memory",
	}
	for _, name := range writes {
		if !memoryWriteToolName(name) {
			t.Errorf("memoryWriteToolName(%q) = false, want true (a memory-store write)", name)
		}
	}
	notWrites := []string{
		"",
		"write_file",             // a file tool is not a memory store
		"str_replace_editor",     //
		"terminal",               //
		"mcp__duckbrain__recall", // memory READ, not a write
		"duckbrain_search",       //
		"memory_list_keys",       // read side
		"board_append",           // board tool, not the memory store
	}
	for _, name := range notWrites {
		if memoryWriteToolName(name) {
			t.Errorf("memoryWriteToolName(%q) = true, want false (not a memory-store write)", name)
		}
	}
}

// memoryKeysFixture writes a Hermes-state fixture with one gateway session
// (session_key = tick id) and the given assistant tool-call NAMES, and
// redirects the counter at it.
func memoryKeysFixture(t *testing.T, tickID string, toolNames []string) {
	t.Helper()
	stateDB := filepath.Join(t.TempDir(), "state.db")
	sdb, err := sql.Open("sqlite", stateDB)
	if err != nil {
		t.Fatalf("open fixture state db: %v", err)
	}
	defer sdb.Close()
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
	start := float64(time.Now().UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, message_count, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?, ?)`,
		"sess-"+tickID, tickID, start, len(toolNames), len(toolNames)); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i, name := range toolNames {
		// Assistant rows carry the tool_calls JSON (OpenAI wire shape) —
		// the shape toolCallsFromAssistantRow parses.
		raw := `[{"function":{"name":"` + name + `"}}]`
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_calls, timestamp)
			 VALUES (?, 'assistant', ?, ?)`,
			"sess-"+tickID, raw, start+float64(i)); err != nil {
			t.Fatalf("seed assistant message: %v", err)
		}
	}
	t.Setenv(hermesStateDBPathEnv, stateDB)
}

// TestSCHEDGAP1680_CountsMemoryKeysFromSessionTranscript proves the counter
// reads the tick's own transcript and counts ONLY memory writes.
func TestSCHEDGAP1680_CountsMemoryKeysFromSessionTranscript(t *testing.T) {
	tickID := "auger-sync-2026-09-30-00-00-00"
	memoryKeysFixture(t, tickID, []string{
		"mcp__duckbrain__remember",
		"terminal",               // a shell call is not a counted memory write
		"mcp__duckbrain__recall", // a READ is not a write
		"duckbrain_remember",
		"write_file",
		"mcp__duckbrain__remember",
	})
	if got := countMemoryKeysInSession(tickID); got != 3 {
		t.Fatalf("countMemoryKeysInSession = %d, want 3 (only the memory writes)", got)
	}

	// A tick with no session row reads 0 — never a fabricated count.
	if got := countMemoryKeysInSession("no-such-tick"); got != 0 {
		t.Fatalf("countMemoryKeysInSession(unknown tick) = %d, want 0", got)
	}
	// An empty tick id reads 0 without touching the store.
	if got := countMemoryKeysInSession(""); got != 0 {
		t.Fatalf("countMemoryKeysInSession(\"\") = %d, want 0", got)
	}
}

// TestSCHEDGAP1680_LifecycleRecordsMemoryKeys drives the real completion
// write: a keys-only completed tick records outcome='committed' AND the
// memory_keys column, so a sync tick is distinguishable from a zero-output
// one on the row itself.
func TestSCHEDGAP1680_LifecycleRecordsMemoryKeys(t *testing.T) {
	db := newTestDB(t)
	lt := NewLifecycleTracker(db)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "sync-lane", RepoURL: "https://example.com/sync", Workdir: t.TempDir(),
		Weight: 10, Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tickID := "sync-lane-2026-09-30-00-00-00"
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES (?, 'sync-lane', 'running', ?, ?)`,
		tickID, now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	if err := lt.Complete(TickOutcome{
		TickID:     tickID,
		Project:    "sync-lane",
		Status:     TickCompleted,
		Started:    now.Add(-time.Minute),
		Finished:   now,
		Commits:    0,
		MemoryKeys: 5,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var outcome string
	var memoryKeys int
	if err := db.QueryRow(`SELECT COALESCE(outcome,''), COALESCE(memory_keys,0) FROM ticks WHERE id = ?`, tickID).
		Scan(&outcome, &memoryKeys); err != nil {
		t.Fatalf("read tick: %v", err)
	}
	if outcome != "committed" {
		t.Fatalf("outcome = %q, want committed — a five-key sync tick is NOT a no-op", outcome)
	}
	if memoryKeys != 5 {
		t.Fatalf("memory_keys = %d, want 5 (recorded on the row)", memoryKeys)
	}
}

// TestSCHEDGAP1680_LaneFamilyOutputCountsMemoryKeys proves the non-code
// family metric treats a keys-writing sync tick as OUTPUT: without the
// non-commit leg the sync family's zero-output streak climbed on real work.
func TestSCHEDGAP1680_LaneFamilyOutputCountsMemoryKeys(t *testing.T) {
	db := newTestDB(t)
	insertLaneOutputProject(t, db, "sync-lane", "duckbrain-sync")

	tickID := "sync-lane-2026-09-30-00-00-00"
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at, commits,
		 code_commits, board_commits, memory_keys, created_at)
		VALUES (?, 'sync-lane', 'completed', 'committed', datetime('now'), datetime('now'),
		 0, 0, 0, 5, datetime('now'))`, tickID); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	recordLaneFamilyOutput(db, "sync-lane", tickID)

	syncOut, syncStreak := readLaneFamilyCounters(t, db, "sync-lane")
	if syncOut != 1 {
		t.Fatalf("sync_output_count = %d, want 1 — a DuckBrain-writing tick is output", syncOut)
	}
	if syncStreak != 0 {
		t.Fatalf("sync_zero_output_streak = %d, want 0 — the streak must reset on real work", syncStreak)
	}

	// Control: the SAME shape with memory_keys = 0 is a zero-output tick —
	// the leg is what makes the difference, not the row's status.
	zeroID := "sync-lane-2026-09-30-00-01-00"
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at, commits,
		 code_commits, board_commits, memory_keys, created_at)
		VALUES (?, 'sync-lane', 'completed', 'dry_run', datetime('now'), datetime('now'),
		 0, 0, 0, 0, datetime('now'))`, zeroID); err != nil {
		t.Fatalf("insert zero-output tick: %v", err)
	}
	recordLaneFamilyOutput(db, "sync-lane", zeroID)

	syncOut, syncStreak = readLaneFamilyCounters(t, db, "sync-lane")
	if syncOut != 1 {
		t.Fatalf("sync_output_count = %d after a zero-output tick, want 1 (unchanged)", syncOut)
	}
	if syncStreak != 1 {
		t.Fatalf("sync_zero_output_streak = %d, want 1 (the control tick is genuinely zero-output)", syncStreak)
	}
}

// readLaneFamilyCounters reads one lane's sync-family (count, streak) pair.
func readLaneFamilyCounters(t *testing.T, db *sql.DB, name string) (count, streak int) {
	t.Helper()
	if err := db.QueryRow(`SELECT sync_output_count, sync_zero_output_streak
		FROM projects WHERE name = ?`, name).Scan(&count, &streak); err != nil {
		t.Fatalf("read lane counters for %s: %v", name, err)
	}
	return count, streak
}
