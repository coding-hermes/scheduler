package scheduler

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1698 acceptance, library core. In-package so the tests can drive
// sessionPollLoopPass on the simulator clock and seed the fixture state.db
// exactly like the SCHED-GAP-1707 guard tests do
// (SCHEDULER_HERMES_STATE_DB → t.Setenv; no test ever touches the real
// ~/.hermes/state.db).
//
// The poll session's shape is the MEASURED 2026-10-02 bunker-foreman
// incident: `sleep 165; ps --ppid <pid>; gitreins judge --status <job>`
// every ~167s, 62 of 81 tool calls over 2.9h. The fixture replays that
// timeline on the sim clock.
//
// TIME. The pass reads its decision instant from the loop's clock seam, so
// one SimClock drives the whole scenario deterministically: spawn at T0,
// resetTo(T0+age) → each guard pass probes the sliding 30-minute window
// [now-30m, now] against the seeded messages table.

// pollCreateProject is the in-package project creator (same shape the 1707
// fixture uses).
func pollCreateProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: t.TempDir(),
		Weight: 10, Priority: 5, CooldownS: 900, DecayRate: 1.0, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// pollSleepCommand is the incident's exact terminal payload, in the shape
// the messages.tool_calls COLUMN stores (the probe extracts
// $.function.arguments from it).
func pollSleepCommand() string {
	return terminalToolCalls("sleep 165; ps -o pid,stat,etime,pcpu --ppid 2408618 2>/dev/null || echo GONE; gitreins judge --status job-abc123 --json")
}

// terminalToolCalls wraps a shell command into the full tool_calls ARRAY
// JSON the messages column stores (gateway wire shape: `[{"function":…}]` —
// the probe reads `$[0].function.*`).
func terminalToolCalls(cmd string) string {
	return `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":` + quoteJSONString(`{"command":`+quoteJSONString(cmd)+`}`) + `}}]`
}

// seedPollTimeline is the fixture's tool-timeline writer: it creates the
// sessions row (session_key = tick id) and inserts tool_calls messages at
// the given offsets-from-spawn. commands carries one raw tool_calls JSON per
// entry (nil = the sleep-poll command). Same real-schema state.db fixture
// the 1707 tests use.
func (f *pollFixture) seedPollTimeline(t *testing.T, offsets []time.Duration, commands [][]byte) {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer sdb.Close()
	start := float64(f.now.UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO sessions (id, source, session_key, started_at, tool_call_count)
		 VALUES (?, 'api_server', ?, ?, ?)`,
		"sess-"+f.tick, f.tick, start, len(offsets)); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i, off := range offsets {
		payload := pollSleepCommand()
		if commands != nil && commands[i] != nil {
			payload = string(commands[i])
		}
		ts := float64(f.now.Add(off).UnixNano()) / 1e9
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_calls, timestamp) VALUES (?, 'assistant', ?, ?)`,
			"sess-"+f.tick, payload, ts); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}
}

// pollFixture is one test's world: the scheduler DB, a spawner + loop on a
// sim clock, and a fixture state.db path.
type pollFixture struct {
	db      *sql.DB
	stateDB string
	loop    *Loop
	spawner *Spawner
	now     time.Time
	tick    string
}

// newPollFixture builds the world: migrated scheduler DB, a project with a
// running gateway tick (pid=0) spawned at the sim clock's instant, a spawner
// wired to the loop, and a fresh (empty) Hermes state.db fixture.
func newPollFixture(t *testing.T, projectName string) *pollFixture {
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
	tickID := projectName + "-2026-10-08-00-00-00"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, pid, spawned_at, created_at, session_id)
		 VALUES (?, ?, 'running', 0, ?, ?, ?)`,
		tickID, projectName, now.Format(time.RFC3339), now.Format(time.RFC3339), tickID); err != nil {
		t.Fatalf("seed running tick: %v", err)
	}

	// Empty Hermes state.db fixture (real schema, no rows) — the same
	// fixture shape the guard tests use, plus the messages table the
	// poll-loop probe reads.
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
	if _, err := sdb.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL, role TEXT NOT NULL,
		content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
		timestamp REAL NOT NULL, token_count INTEGER)`); err != nil {
		t.Fatalf("create fixture messages: %v", err)
	}
	sdb.Close()
	t.Setenv(hermesStateDBPathEnv, stateDB)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 4)
	spawner := NewSpawner(db, 4)
	loop.spawner = spawner
	sim := clock.NewSimClockAt(1, now)
	loop.SetClock(sim)

	return &pollFixture{db: db, stateDB: stateDB, loop: loop, spawner: spawner, now: now, tick: tickID}
}

// resetTo moves the sim clock to an absolute instant.
func (f *pollFixture) resetTo(t time.Time) {
	f.loop.clock().(*clock.SimClock).SetNow(t)
}

// loopClockNow reads the loop's current sim instant.
func (f *pollFixture) loopClockNow() time.Time {
	return f.loop.clock().Now()
}

// at returns spawn + d (offset arithmetic against the fixture anchor).
func (f *pollFixture) at(d time.Duration) time.Time {
	return f.now.Add(d)
}

// incidentOffsets replays the measured fast cadence (~167s) across the
// trailing window: n cycles ending `endAgo` before the PROBE instant, which
// sits `age` after spawn (offsets are absolute-from-spawn — the probe window
// is [now-30m, now] with now = spawn+age).
func incidentOffsets(age, endAgo time.Duration, n int) []time.Duration {
	offs := make([]time.Duration, 0, n)
	t := age - endAgo
	for i := 0; i < n; i++ {
		offs = append(offs, t)
		t -= 167 * time.Second
	}
	// reverse into ascending order
	for i, j := 0, len(offs)-1; i < j; i, j = i+1, j-1 {
		offs[i], offs[j] = offs[j], offs[i]
	}
	return offs
}

// slowOffsets replays the brief's own shape — `sleep 390..480; gitreins
// judge --status` every ~7 minutes — ending `endAgo` before the probe
// instant at spawn+age.
func slowOffsets(age, endAgo time.Duration, n int) []time.Duration {
	offs := make([]time.Duration, 0, n)
	t := age - endAgo
	for i := 0; i < n; i++ {
		offs = append(offs, t)
		t -= 435 * time.Second
	}
	for i, j := 0, len(offs)-1; i < j; i, j = i+1, j-1 {
		offs[i], offs[j] = offs[j], offs[i]
	}
	return offs
}

// pollArgsTerminal is the ARGUMENTS-object string the production probe
// hands isPollCallPayload ($.function.arguments extracted) — used by the
// classifier unit test directly.
func pollArgsTerminal(cmd string) string {
	return `{"command":` + quoteJSONString(cmd) + `}`
}

// quoteJSONString renders s as a JSON string literal for embedding inside a
// tool_calls payload.
func quoteJSONString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '	':
			b = append(b, '\\', 't')
		default:
			b = append(b, byte(r))
		}
	}
	return string(append(b, '"'))
}

// TestSCHEDGAP1698_PollLoopSessionIsCancelled is acceptance criterion 1: a
// session whose tool activity is the measured sleep-poll shape (fixed ~167s
// cadence, zero advancement) is CANCELLED after pollLoopConfirmPasses
// consecutive passes past the minimum age, and the stored classification
// resolves to failure_reason=sleep_poll via failureReasonClass/HarnessFailure.
func TestSCHEDGAP1698_PollLoopSessionIsCancelled(t *testing.T) {
	f := newPollFixture(t, "poll-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	// The measured fast cadence, ~11 cycles filling the trailing window.
	f.seedPollTimeline(t, incidentOffsets(45*time.Minute, 40*time.Second, 11), nil)

	// Register the live session ctx (the realistic watchdog shape: a
	// running gateway spawn holds its cancel in the registry).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	// Age the tick past the minimum age and past the window.
	f.resetTo(f.at(45 * time.Minute))
	now := f.loopClockNow()

	// Passes 1 and 2 confirm the streak but do not fire.
	for i := 0; i < pollLoopConfirmPasses-1; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed %d tick(s), want 0 (streak not yet %d)",
				i+1, killed, pollLoopConfirmPasses)
		}
		// Sim ticks are 60s apart; the poll timeline keeps streaming.
		now = now.Add(time.Minute)
		f.appendPollCycles(t, now, 1)
		f.resetTo(now)
	}
	// Final pass: the streak completes and the kill lands.
	killed := f.loop.sessionPollLoopPass(now.Add(time.Minute))
	if killed != 1 {
		t.Fatalf("final pass killed %d tick(s), want 1 (confirmed poll-loop)", killed)
	}
	pt, ok := f.spawner.sessionPollLoopFor(f.tick)
	if !ok {
		t.Fatal("guard did not register the sleep_poll verdict for the poll-looping tick")
	}
	if pt.reason != TelemetryPartialSleepPoll || !pt.partial {
		t.Errorf("verdict = (%v, %q), want partial/%s", pt.partial, pt.reason, TelemetryPartialSleepPoll)
	}
	if pt.silenceS != int64(pollLoopWindow.Seconds()) {
		t.Errorf("span = %ds, want %d", pt.silenceS, int64(pollLoopWindow.Seconds()))
	}
	if _, consumed := f.spawner.sessionPollLoopFor(f.tick); consumed {
		t.Error("verdict not consume-once")
	}
	// The stored classification (AC 1 second half): the kill text the spawn
	// path will persist must resolve to failure_reason=sleep_poll through
	// the single classifier.
	text := sessionPollLoopError("1800s")
	if !HarnessFailure(text) {
		t.Errorf("poll-loop error text %q must classify as harness failure", text)
	}
	if got := failureReasonClass(text); got != "sleep_poll" {
		t.Errorf("failureReasonClass(%q) = %q, want sleep_poll", text, got)
	}
}

// appendPollCycles extends the fixture timeline with n more sleep cycles
// ending at endAt (the streaming session keeps polling while passes run).
func (f *pollFixture) appendPollCycles(t *testing.T, endAt time.Time, n int) {
	t.Helper()
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer sdb.Close()
	for i := 0; i < n; i++ {
		ts := float64(endAt.Add(-time.Duration(n-1-i)*167*time.Second).UnixNano()) / 1e9
		if _, err := sdb.Exec(
			`INSERT INTO messages (session_id, role, tool_calls, timestamp) VALUES (?, 'assistant', ?, ?)`,
			"sess-"+f.tick, pollSleepCommand(), ts); err != nil {
			t.Fatalf("append cycle %d: %v", i, err)
		}
	}
}

// TestSCHEDGAP1698_SlowCadenceBriefShapeIsCaught pins the brief's OWN
// measured shape — `sleep 390..480; gitreins judge --status` every ~7 min —
// which the 5-minute-stride arithmetic this detector replaced could never
// hold (0-1 calls per stride). The 30-minute sliding window must.
func TestSCHEDGAP1698_SlowCadenceBriefShapeIsCaught(t *testing.T) {
	f := newPollFixture(t, "slow-poll-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	// 5 cycles at ~435s covering [15m, 44m] after spawn — every pass window
	// [now-30m, now] for now in [45m, 48m] holds >= 4 of them at the fixed
	// cadence. No appends: the sliding window covers the whole pass run.
	f.seedPollTimeline(t, slowOffsets(45*time.Minute, 60*time.Second, 5), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(45 * time.Minute))
	now := f.loopClockNow()
	for i := 0; i < pollLoopConfirmPasses-1; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed %d, want 0", i+1, killed)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
	if killed := f.loop.sessionPollLoopPass(now.Add(time.Minute)); killed != 1 {
		t.Fatalf("final pass killed %d, want 1 (the slow brief shape must classify)", killed)
	}
}

// TestSCHEDGAP1698_ProducingSessionNeverKilled is acceptance criterion 2: a
// session with REAL activity — a worker writing code (edits, builds, test
// runs interleaved with its polls) — is NEVER killed by the poll-loop guard,
// at any pass count.
func TestSCHEDGAP1698_ProducingSessionNeverKilled(t *testing.T) {
	f := newPollFixture(t, "coder-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	// The genuinely-productive shape: the SAME ~167s cadence, but every
	// 4th call is real work (a patch apply / build / test) instead of a
	// poll. The cadence alone must never be the verdict.
	offs := incidentOffsets(45*time.Minute, 40*time.Second, 11)
	cmds := make([][]byte, len(offs))
	for i := range offs {
		if i%4 == 3 {
			// i=3: a non-terminal tool; i=7: a mutating terminal command.
			if i == 7 {
				cmds[i] = []byte(terminalToolCalls("go build ./... && go test ./internal/... -count=1"))
			} else {
				cmds[i] = []byte(`[{"id":"c2","type":"function","function":{"name":"apply_patch","arguments":"{\"patch\":\"*** Begin Patch\"}"}}]`)
			}
		}
	}
	f.seedPollTimeline(t, offs, cmds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(45 * time.Minute))
	now := f.loopClockNow()
	for i := 0; i < pollLoopConfirmPasses*3; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed the PRODUCING session — the guard must never kill a session whose windows carry real work", i+1)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
	if f.spawner.guardAborted(f.tick) {
		t.Error("producing tick marked guard-aborted")
	}
}

// TestSCHEDGAP1698_OffByDefaultNoCancellations is acceptance criterion 3:
// with the guard unset/zero (the library default), behavior is byte-identical
// to today — no cancellations, no verdict, even for a textbook poller past
// every threshold.
func TestSCHEDGAP1698_OffByDefaultNoCancellations(t *testing.T) {
	f := newPollFixture(t, "off-proj")
	// The knob is deliberately NOT armed (package-level default 0; also
	// reset it in case another test in the binary leaked a value).
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	f.seedPollTimeline(t, incidentOffsets(3*time.Hour, 40*time.Second, 11), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(3 * time.Hour))
	now := f.loopClockNow()
	for i := 0; i < 10; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed %d tick(s) with the knob UNSET — the library default must be off", i+1, killed)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
	if _, ok := f.spawner.sessionPollLoopFor(f.tick); ok {
		t.Fatal("poll-loop verdict registered while the guard is off")
	}
}

// TestSCHEDGAP1698_TooYoungTickSpared pins the minimum-age arm: a confirmed
// poller younger than pollLoopMinAge is never judged (early-session polling
// of a genuinely running build is legitimate).
func TestSCHEDGAP1698_TooYoungTickSpared(t *testing.T) {
	f := newPollFixture(t, "young-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	// A poll window that sits entirely INSIDE the first 30 minutes.
	f.seedPollTimeline(t, incidentOffsets(20*time.Minute, 40*time.Second, 11), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(20 * time.Minute))
	if killed := f.loop.sessionPollLoopPass(f.loopClockNow()); killed != 0 {
		t.Fatalf("killed %d tick(s) younger than the minimum age, want 0", killed)
	}
}

// TestSCHEDGAP1698_UnreadableStoreFailsOpen pins the fail-open contract: an
// empty store (no messages at all — probe ok=false) must reset the streak
// and never kill, and a tick whose store HAS no row is equally spared.
func TestSCHEDGAP1698_UnreadableStoreFailsOpen(t *testing.T) {
	f := newPollFixture(t, "dark-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	// No seedPollTimeline call — the store has no row for this tick at all.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(2 * time.Hour))
	now := f.loopClockNow()
	for i := 0; i < pollLoopConfirmPasses+2; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed a tick whose telemetry is unreadable — the guard must fail open", i+1)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
}

// TestSCHEDGAP1698_UnregisteredSessionSpared covers the cancel-registry arm:
// a confirmed poller with NO live registered session is logged and skipped,
// not counted as killed (the silence watchdog's exact semantics).
func TestSCHEDGAP1698_UnregisteredSessionSpared(t *testing.T) {
	f := newPollFixture(t, "noreg-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	f.seedPollTimeline(t, incidentOffsets(2*time.Hour, 40*time.Second, 11), nil)
	// No RegisterTickSessionContext — the gateway never registered here.
	f.resetTo(f.at(2 * time.Hour))
	now := f.loopClockNow()
	for i := 0; i < pollLoopConfirmPasses; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed=%d, want 0 (no live session to cancel)", i+1, killed)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
}

// TestSCHEDGAP1698_StreakResetsOnGap pins continuity: the verdict must rest
// on CONSECUTIVE passes — one pass whose window carries a real-work call
// resets the streak, so a poller that interleaves an edit with its polls
// survives (producing sessions are never killed, AC 2's other half).
func TestSCHEDGAP1698_StreakResetsOnGap(t *testing.T) {
	f := newPollFixture(t, "gap-proj")
	SetSessionPollLoopMinTicks(pollLoopMinTicks)
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })

	f.seedPollTimeline(t, incidentOffsets(45*time.Minute, 40*time.Second, 11), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = ctx }()
	f.spawner.RegisterTickSessionContext(f.tick, cancel)

	f.resetTo(f.at(45 * time.Minute))
	now := f.loopClockNow()
	// Two confirming passes...
	for i := 0; i < 2; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("pass %d killed %d, want 0", i+1, killed)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
	// ...then ONE pass whose window carries a real edit: the streak dies.
	sdb, err := sql.Open("sqlite", f.stateDB)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	ts := float64(now.Add(-30*time.Second).UnixNano()) / 1e9
	if _, err := sdb.Exec(
		`INSERT INTO messages (session_id, role, tool_calls, timestamp) VALUES (?, 'assistant', ?, ?)`,
		"sess-"+f.tick,
		`[{"id":"c9","type":"function","function":{"name":"apply_patch","arguments":"{\"patch\":\"*** Begin Patch\"}"}}]`,
		ts); err != nil {
		t.Fatalf("seed work call: %v", err)
	}
	sdb.Close()
	if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
		t.Fatalf("pass with a real-work window killed %d, want 0", killed)
	}
	// And the streak restarts from zero: pollLoopConfirmPasses-1 further
	// confirming passes must still NOT fire.
	now = now.Add(time.Minute)
	f.resetTo(now)
	for i := 0; i < pollLoopConfirmPasses-1; i++ {
		if killed := f.loop.sessionPollLoopPass(now); killed != 0 {
			t.Fatalf("recovery pass %d killed %d, want 0 (streak had reset)", i+1, killed)
		}
		now = now.Add(time.Minute)
		f.resetTo(now)
	}
}

// TestSCHEDGAP1698_KnobClampAndVocabulary pins the setter contract (the knob
// can only make the guard stricter — 1-5 clamp to the built-in floor) and
// the closed partial-reason vocabulary (sleep_poll valid, marker text
// stable).
func TestSCHEDGAP1698_KnobClampAndVocabulary(t *testing.T) {
	t.Cleanup(func() { SetSessionPollLoopMinTicks(0) })
	SetSessionPollLoopMinTicks(2)
	if got := sessionPollLoopGuard(); got != pollLoopMinTicks {
		t.Errorf("SetSessionPollLoopMinTicks(2) → guard=%d, want the built-in floor %d", got, pollLoopMinTicks)
	}
	SetSessionPollLoopMinTicks(9)
	if got := sessionPollLoopGuard(); got != 9 {
		t.Errorf("SetSessionPollLoopMinTicks(9) → guard=%d, want 9 (stricter arming passes through)", got)
	}
	if !telemetryPartialReasonIsValid(TelemetryPartialSleepPoll) {
		t.Errorf("reason %q must be valid", TelemetryPartialSleepPoll)
	}
	if telemetryPartialReasonIsValid("") || telemetryPartialReasonIsValid("poll_loop") {
		t.Error("empty/unknown reasons must be invalid")
	}
	text := sessionPollLoopError("30m0s")
	if !strings.HasPrefix(text, sessionPollLoopMarker) {
		t.Errorf("kill text %q must carry the stable leading marker %q", text, sessionPollLoopMarker)
	}
	if !strings.Contains(text, "zero advancement") {
		t.Errorf("kill text should describe the zero-advancement shape: %q", text)
	}
}

// TestSCHEDGAP1698_PayloadClassifier pins the terminal-payload classifier:
// sleep-led commands and the read-only probe allowlist are poll; builds,
// tests, edits, non-terminal tools, and malformed payloads are work (fail
// open).
func TestSCHEDGAP1698_PayloadClassifier(t *testing.T) {
	poll := []string{
		"sleep 165; ps -o pid,stat --ppid 1; gitreins judge --status job-1",
		"SLEEP 420; gitreins judge --status job-1", // case-insensitive
		"sleep 480",
		"git status --short",
		"git log --oneline -5",
		"ps -o pid,stat,etime,pcpu --ppid 2408618 2>/dev/null || echo GONE",
		"gitreins judge --status job-abc --json",
		"tail -20 /tmp/build.log",
		"ls internal/scheduler",
		"wc -l main.go",
	}
	for _, cmd := range poll {
		if !isPollCallPayload("terminal", pollArgsTerminal(cmd)) {
			t.Errorf("classify(%q) = work, want poll", cmd)
		}
	}
	work := []string{
		"go build ./... && go test ./internal/... -count=1",
		"git commit -F /tmp/msg.txt -- internal/scheduler/",
		"git add internal/scheduler/session_silence.go",
		"git push origin wt/task",
		"git stash",
		"make -j4 && make test -j4",
		"cargo test -p hilo-cli",
		"patch -p1 < /tmp/fix.diff",
		"git checkout -- internal/scheduler/", // history surgery = not a status read
		"rm -rf /tmp/scratch",
	}
	for _, cmd := range work {
		if isPollCallPayload("terminal", pollArgsTerminal(cmd)) {
			t.Errorf("classify(%q) = poll, want work", cmd)
		}
	}
	// A non-terminal tool is never poll, whatever its arguments.
	if isPollCallPayload("apply_patch", `{"patch":"*** Begin Patch\n*** Update File: x"}`) {
		t.Error("apply_patch classified as poll — only terminal calls can poll")
	}
	// Malformed / foreign payloads fail OPEN (not poll, not work — the
	// stride is simply unclassifiable through this call).
	if isPollCallPayload("terminal", "not-json") {
		t.Error("malformed payload classified as poll — must fail open")
	}
	if isPollCallPayload("terminal", "") {
		t.Error("empty payload classified as poll — must fail open")
	}
}
