package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1674 — builder-class no-artifact guard: nudge then abort a
// running tick that reads without ever writing.
//
// PROBLEM (measured 2026-09-28/29 on the live fleet): a BUILDER-class worker
// ran 56.7 minutes across 72 messages and ~40 read-only recon calls
// (read_file / terminal) with ZERO writes and zero commits, and was killed by
// hand. Fleet-wide, 990 sessions in 7 days match the signature (>= 15 recon
// calls, 0 writes); joined to ticks, 613 ticks are involved, 520 (85%) carry
// zero commits, and 383 of those were still recorded outcome='committed' —
// false credit for empty work.
//
// WHAT THE GUARD DOES, per running builder tick, once per reaper pass (60s):
//
//  1. OBSERVE. The guard runs at the strongest signal level the daemon can
//     reach: the Hermes gateway's own session telemetry in the live agent
//     state store (~/.hermes/state.db), where every spawned tick's session
//     carries the tick id as its session_key (X-Hermes-Session-Key, SCHED-
//     GAP-074). Per running tick it reads session message_count +
//     tool_call_count (live counters, updated by the gateway as the turn
//     progresses) and the tool-level transcript (messages.tool_name +
//     assistant tool_calls), classifying every observed tool call as
//     WRITE-CLASS or RECON via writeClassifier. It ALSO counts write-class
//     git artifacts in the workdir over the tick window (countGitChanges) —
//     the whole edit surface is covered by BOTH signals together: state.db
//     tool telemetry (write_file/execute_code/terminal/heredoc/git commit
//     arguments) and the git window itself (commits and changed files).
//  2. NUDGE. When elapsed >= window (T, default 20m) AND zero write-class
//     artifacts AND interaction count >= floor (N, default 25), the guard
//     injects a mid-run nudge into the session — best-effort. The gateway
//     (v0.21.1) exposes no session-injection endpoint, so per the row's
//     contract the miss degrades to a structured WARN event (component
//     "builder_guard", event_type=builder_nudge_skipped) and the tick is
//     still tracked so the second window can abort it. The nudge stamps
//     ticks.nudge_count via the same bumpNudgeCount the resume path uses.
//  3. ABORT. When the same no-write condition persists for another full
//     window after the nudge, the guard cancels the tick's session through
//     the spawner's per-tick cancel registry — the same mechanism the
//     session-ctx deadline uses — and the spawn path's existing completion
//     handling closes the row with outcome "aborted:no_artifact" (never
//     dry_run, never committed). The registry lives on the Spawner and is
//     wired by sendTurn's session context.
//
// EXEMPTIONS. Reporter-class lanes — namespaces whose declared product is a
// report/DuckBrain key rather than code (satellite -sync/-qa/-pm/-dogfood/
// -review/-report families) — are exempt: their product is not a commit and
// the fleet already tracks their output through the per-family counters
// (SCHED-GAP-177: "the satellites don't always need to commit"). The class
// is config (namespace table column reporter_class), not a name heuristic.
// Ticks with a real pid (exec spawns) are also out of scope: the guard
// aborts through the gateway session ctx, which only gateway spawns carry.
//
// SAFETY. Every read is best-effort and bounded: an unreachable state.db, a
// missing sessions row (exec tick, session not yet persisted) or a query
// error all leave the tick untouched this pass — an observation gap can
// never manufacture an abort. The abort itself fires only on the SECOND
// consecutive full window, so a single miscounted pass cannot kill a live
// tick, and a write observed at ANY point (either signal) resets the tick's
// guard state entirely.
const (
	// DefaultNoArtifactWindow is the default T: elapsed time with zero
	// write-class artifacts before a builder tick is nudged, and again
	// before it is aborted.
	DefaultNoArtifactWindow = 20 * time.Minute
	// DefaultNoArtifactReconFloor is the default N: the minimum number of
	// interactions (gateway messages + tool calls, floor) a no-write builder
	// tick must show before the guard acts. 25 matches the row's measured
	// signature floor (the live offender showed 40 recon calls / 72
	// messages); a tick still in its opening exchanges is left alone.
	DefaultNoArtifactReconFloor = 25
	// AbortOutcomeValue is the outcome the guard stamps on an aborted tick.
	// Matches database.OutcomeAbortedNoArtifact; kept as a local constant so
	// the CHECK-constraint vocabulary lives next to the guard that writes it.
	AbortOutcomeValue = "aborted:no_artifact"
	// GuardEventComponent is the events.component value for every event the
	// guard emits, so an operator can query the guard's history in one WHERE.
	GuardEventComponent = "builder_guard"
)

// noArtifactKnobs is the resolved per-tick guard configuration: the window
// T and the recon floor N, read from the namespace row with the documented
// defaults when the columns are unset. Negative DB values normalize to the
// defaults (a typo must not disable or immortalize the guard).
type noArtifactKnobs struct {
	window     time.Duration
	reconFloor int
}

// DefaultNoArtifactKnobs returns the documented defaults (T=20m, N=25).
func DefaultNoArtifactKnobs() noArtifactKnobs {
	return noArtifactKnobs{window: DefaultNoArtifactWindow, reconFloor: DefaultNoArtifactReconFloor}
}

// resolveNoArtifactKnobs loads the namespace row's guard knobs. An unknown
// namespace, an unassigned project or a read error all return the defaults:
// the guard is on-by-default at the documented thresholds, and a config
// lookup failure must never widen or disable it silently.
func resolveNoArtifactKnobs(ctx context.Context, db *sql.DB, namespaceID string) noArtifactKnobs {
	knobs := DefaultNoArtifactKnobs()
	if db == nil || namespaceID == "" {
		return knobs
	}
	var windowS, floorRaw sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(no_artifact_window, ''), COALESCE(no_artifact_recon_floor, '')
		 FROM namespaces WHERE id = ?`, namespaceID).Scan(&windowS, &floorRaw)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("WARN: builder_guard knob lookup for namespace %q failed (%v) — using defaults", namespaceID, err)
		}
		return knobs
	}
	if windowS.Valid && windowS.String != "" {
		if d, perr := time.ParseDuration(windowS.String); perr == nil && d > 0 {
			knobs.window = d
		} else {
			log.Printf("WARN: namespace %q no_artifact_window=%q unparseable — using default %v", namespaceID, windowS.String, knobs.window)
		}
	}
	if floorRaw.Valid && floorRaw.String != "" {
		// The column is TEXT so an operator can store either an int or a
		// string; both parse. Unparseable keeps the default.
		if n, perr := strconv.Atoi(strings.TrimSpace(floorRaw.String)); perr == nil && n > 0 {
			knobs.reconFloor = n
		} else {
			log.Printf("WARN: namespace %q no_artifact_recon_floor=%q unparseable — using default %d", namespaceID, floorRaw.String, knobs.reconFloor)
		}
	}
	return knobs
}

// writeToolVocabulary is the write-class tool-name set (SCHED-GAP-1674 (d)):
// the whole edit surface, not just write_file. terminal and execute_code
// count as writes when their ARGUMENTS write (heredocs, shell redirects,
// git commit); write_file/write/str_replace/edit always write. A tool name
// absent from this table is recon unless the argument scan says otherwise.
var writeToolVocabulary = map[string]bool{
	"write_file":  true,
	"write":       true,
	"str_replace": true,
	"edit":        true,
	"patch":       true,
}

// writeArgumentMarkers are substrings that make a terminal/execute_code (or
// any shell-carrying tool) call WRITE-CLASS: heredocs, shell redirects to a
// file, git commit and file-mutating commands. Deliberately conservative in
// the write direction (a false "write" only delays a nudge/abort by one
// window; a false "recon" would abort a productive tick).
var writeArgumentMarkers = []string{
	"<<", ">>", ">", "tee ", "install -", "cp ", "mv ", "rm ", "touch ",
	"git commit", "git add", "git push", "git merge", "git rebase",
	"git checkout -b", "git switch", "git restore --staged",
	"git apply", "git am", "git cherry-pick", "git revert", "git reset",
	"sed -i", "chmod", "chown", "ln -s", "mkdir", "rmdir",
}

// toolCallsFromAssistantRow extracts the tool NAMES an assistant message
// invoked. The messages.tool_calls column carries a JSON array of
// {"function":{"name":...}} entries (OpenAI wire shape). Parse failures
// yield nil — the caller treats an unreadable call list as "nothing
// observed", never as evidence either way.
func toolCallsFromAssistantRow(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var calls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.Function.Name != "" {
			names = append(names, c.Function.Name)
		}
	}
	return names
}

// writeClassifier is the unit-testable core of (d): given one observed tool
// interaction (tool name, its result content, and for assistant calls the
// raw arguments blob), decide WRITE-CLASS vs RECON. rules:
//
//   - a write-vocabulary tool name (write_file, str_replace, ...) is a write;
//   - terminal / execute_code / any unknown tool is recon UNLESS the
//     argument text carries a write marker (heredoc, redirect, git commit,
//     file-mutating shell);
//   - a tool RESULT that reports a successful write (write_file's
//     "bytes_written", "created task", "appended") also counts — the
//     result proves the write landed even when the argument side was
//     redacted or unavailable.
//
// Any single write-class observation flips the whole tick to "has writes";
// the classifier is summed over the session transcript.
func writeClassifier(toolName, content, callArgs string) bool {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if writeToolVocabulary[name] {
		return true
	}
	lower := strings.ToLower(callArgs)
	for _, m := range writeArgumentMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	lower = strings.ToLower(content)
	for _, m := range writeResultMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// writeResultMarkers are result-side proof a write landed (tool outputs).
var writeResultMarkers = []string{
	"bytes_written", "written successfully", "file created", "created task",
	"appended", "committed", "insert success", "rows affected",
}

// sessionObs is one pass's observation of a running tick's gateway session.
type sessionObs struct {
	found     bool // a sessions row with this tick's session_key exists
	messages  int  // sessions.message_count (live counter)
	toolCalls int  // sessions.tool_call_count (live counter)
	writes    int  // write-class interactions across the transcript
	recon     int  // non-write-class tool interactions
}

// hermesStateDBPathEnv optionally overrides the state-db path the guard
// reads (tests point it at a fixture database with t.Setenv; production
// leaves it unset and DefaultHermesSessionDBPath wins).
const hermesStateDBPathEnv = "SCHEDULER_HERMES_STATE_DB"

// hermesStateDBPath resolves the agent state database the guard observes.
func hermesStateDBPath() string {
	if v := os.Getenv(hermesStateDBPathEnv); v != "" {
		return v
	}
	return database.DefaultHermesSessionDBPath()
}

// observeSessionTelemetry reads the gateway session for one running tick
// from the Hermes state store: the live counters plus a write/recon split
// over the message transcript. The state DB is opened read-ONLY (mode=ro)
// with a short busy timeout — the guard must never contend the live agent
// for a write lock. Any failure returns found=false (observe-nothing,
// fail-open for the tick).
func observeSessionTelemetry(tickID string) sessionObs {
	obs := sessionObs{}
	path := hermesStateDBPath()
	if path == "" || tickID == "" {
		return obs
	}
	sdb, err := database.OpenHermesStateReadOnly(path)
	if err != nil {
		return obs
	}
	defer sdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hermesGuardQueryTimeout)
	defer cancel()

	err = sdb.QueryRowContext(ctx,
		`SELECT COALESCE(message_count,0), COALESCE(tool_call_count,0)
		 FROM sessions WHERE session_key = ? ORDER BY started_at DESC LIMIT 1`, tickID).
		Scan(&obs.messages, &obs.toolCalls)
	if err != nil {
		return obs // not yet persisted, or unreadable — observe nothing
	}
	obs.found = true

	// Transcript split: tool RESULT rows carry tool_name + content;
	// assistant rows carry the tool_calls JSON (arguments). Scan bounded —
	// a session is dozens of rows, but the LIMIT keeps a pathological
	// session from scanning the table.
	rows, err := sdb.QueryContext(ctx, `
SELECT role, COALESCE(tool_name,''), COALESCE(content,''), COALESCE(tool_calls,'')
FROM messages
WHERE session_id = (SELECT id FROM sessions WHERE session_key = ? ORDER BY started_at DESC LIMIT 1)
ORDER BY timestamp DESC LIMIT 200`, tickID)
	if err != nil {
		// Counters without a transcript split: still usable — the floor
		// check uses messages+toolCalls, writes stay 0.
		return obs
	}
	defer rows.Close()
	for rows.Next() {
		var role, toolName, content, toolCalls string
		if err := rows.Scan(&role, &toolName, &content, &toolCalls); err != nil {
			continue
		}
		switch role {
		case "tool":
			if writeClassifier(toolName, content, "") {
				obs.writes++
			} else {
				obs.recon++
			}
		case "assistant":
			for _, name := range toolCallsFromAssistantRow(toolCalls) {
				if writeClassifier(name, "", "") {
					obs.writes++
				} else {
					obs.recon++
				}
			}
		}
	}
	return obs
}

// hermesGuardQueryTimeout bounds every per-tick telemetry query. Sixty
// running ticks × one pass each must never add meaningful latency to the
// 60s reaper cadence, and a wedged state.db must not stall the loop.
const hermesGuardQueryTimeout = 2 * time.Second

// builderGuardPass is one invocation of the guard over every currently
// running gateway tick. Called from the Loop's 60s reaper tick, after
// reapZombies. Returns the number of ticks nudged and aborted this pass.
func (l *Loop) builderGuardPass() (nudged, aborted int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Running gateway ticks joined to their namespace + spawn time + session.
	// pid=0 (gateway spawns) only: the abort mechanism cancels the gateway
	// session ctx, which exec spawns do not carry.
	rows, err := l.db.QueryContext(ctx, `
SELECT t.id, t.project_name, COALESCE(t.spawned_at,''), COALESCE(t.nudge_count,0),
       COALESCE(p.namespace_id,''), p.workdir
FROM ticks t
JOIN projects p ON p.name = t.project_name
WHERE t.status = 'running' AND t.pid = 0`)
	if err != nil {
		log.Printf("WARN: builder_guard running-tick query failed: %v", err)
		return 0, 0
	}
	type cand struct {
		id        string
		project   string
		spawnedAt string
		nudges    int
		nsID      string
		workdir   string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.project, &c.spawnedAt, &c.nudges, &c.nsID, &c.workdir); err != nil {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if len(cands) == 0 {
		return 0, 0
	}

	// Reporter-class namespaces are exempt (config-driven, cached per pass).
	reporter := map[string]bool{}
	knobs := map[string]noArtifactKnobs{}
	now := l.clock().Now()

	for _, c := range cands {
		if c.nsID != "" {
			if _, ok := reporter[c.nsID]; !ok {
				reporter[c.nsID] = namespaceIsReporterClass(ctx, l.db, c.nsID)
			}
			if reporter[c.nsID] {
				continue
			}
		}
		if _, ok := knobs[c.nsID]; !ok {
			knobs[c.nsID] = resolveNoArtifactKnobs(ctx, l.db, c.nsID)
		}
		k := knobs[c.nsID]

		spawned, perr := time.Parse(time.RFC3339, c.spawnedAt)
		if perr != nil || spawned.IsZero() {
			continue // no usable clock — never act on an unmeasurable tick
		}
		elapsed := now.Sub(spawned)
		if elapsed < k.window {
			continue // still inside the first window
		}

		obs := observeSessionTelemetry(c.id)
		if obs.found && obs.writes > 0 {
			continue // write-class work observed — not a no-artifact tick
		}
		// Git-window artifact check (d): commits or changed files inside the
		// tick window are writes regardless of what the transcript shows.
		if c.workdir != "" {
			if commits, files := countGitChanges(c.workdir, spawned, now); commits > 0 || files > 0 {
				continue
			}
		}
		interactions := obs.messages + obs.toolCalls
		if interactions < k.reconFloor {
			continue // not enough interaction volume to judge
		}

		if c.nudges == 0 {
			l.builderNudge(c.id, c.project, c.nsID, elapsed, interactions)
			nudged++
			continue
		}
		// Nudged already: abort only after ANOTHER full window elapsed since
		// the nudge. nudge_at is stamped by builderNudge; a missing stamp
		// (pre-nudge-stamp row) falls back to spawn time + one window.
		nudgedAt := l.builderNudgeTime(c.id)
		if nudgedAt.IsZero() {
			nudgedAt = spawned.Add(k.window)
		}
		if now.Sub(nudgedAt) < k.window {
			continue
		}
		if l.builderAbort(c.id, c.project, c.nsID, elapsed, interactions, obs) {
			aborted++
		}
	}
	return nudged, aborted
}

// namespaceIsReporterClass reads the namespace's reporter_class column.
// Only the exact value "reporter" exempts a namespace; "" (the default)
// and anything else are builder-class.
func namespaceIsReporterClass(ctx context.Context, db *sql.DB, nsID string) bool {
	if db == nil || nsID == "" {
		return false
	}
	var v sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(reporter_class,'') FROM namespaces WHERE id = ?`, nsID).Scan(&v); err != nil {
		return false
	}
	return strings.EqualFold(v.String, "reporter")
}

// builderNudge records the first-window decision. The gateway API (v0.21.1)
// exposes no session-injection endpoint, so the mid-run nudge cannot be
// delivered today; per the row's contract this degrades to a structured
// WARN event and the tracking stamp, so the second window can still abort.
// The nudge_count bump reuses the resume path's counter (same column, same
// semantics: "this tick has been nudged N times").
func (l *Loop) builderNudge(tickID, project, nsID string, elapsed time.Duration, interactions int) {
	l.bumpNudgeCount(tickID)
	l.stampBuilderNudge(tickID)
	database.RecordFeatureUse(database.FeatureBuilderGuard)
	msg := "builder tick " + project + " (" + tickID + ") has " +
		"read enough — make the change now: " +
		formatGuardEvidence(elapsed, interactions)
	log.Printf("BUILDER-GUARD: nudge %s (project %s): %s", tickID, project, msg)
	if l.events != nil {
		l.events.Emit(context.Background(), SeverityMedium, GuardEventComponent,
			"builder_nudge_skipped: no session-injection endpoint — nudge recorded, not delivered",
			map[string]any{
				"event_type":   "builder_nudge_skipped",
				"tick_id":      tickID,
				"project":      project,
				"namespace":    nsID,
				"elapsed_s":    int(elapsed.Seconds()),
				"interactions": interactions,
				"nudge_text":   msg,
				"window_next":  "abort on the same condition after another full window",
			})
	}
}

// builderAbort arms the guard abort for one tick: marks the tick
// guard-aborted, cancels the session through the spawner's per-tick cancel
// registry when this process owns it, and emits the HIGH event. Returns
// true — the abort DECISION is the guard's output. sessionCancelled in the
// event records honestly whether the in-process cancel landed: a tick
// spawned by a previous daemon (or an exec tick) has no registered session
// here, the cancel reports false, and the zombie reaper remains that row's
// backstop — but the decision, the event and the marker are already
// recorded.
func (l *Loop) builderAbort(tickID, project, nsID string, elapsed time.Duration, interactions int, obs sessionObs) bool {
	database.RecordFeatureUse(database.FeatureBuilderGuard)
	cancelled := l.spawner.CancelTickSession(tickID)
	log.Printf("BUILDER-GUARD: abort %s (project %s) after %v with %d interactions and zero writes (session cancel: %t)",
		tickID, project, elapsed.Round(time.Second), interactions, cancelled)
	if l.events != nil {
		details := map[string]any{
			"event_type":         "builder_abort",
			"tick_id":            tickID,
			"project":            project,
			"namespace":          nsID,
			"elapsed_s":          int(elapsed.Seconds()),
			"interactions":       interactions,
			"session_messages":   obs.messages,
			"session_tool_calls": obs.toolCalls,
			"recon_calls":        obs.recon,
			"write_calls":        obs.writes,
			"session_cancelled":  cancelled,
			"outcome":            AbortOutcomeValue,
		}
		l.events.Emit(context.Background(), SeverityHigh, GuardEventComponent,
			"builder tick aborted — read-only session past two guard windows with zero artifacts",
			details)
	}
	return true
}

// formatGuardEvidence renders one evidence line for the nudge text.
func formatGuardEvidence(elapsed time.Duration, interactions int) string {
	return formatGuardEvidenceString(elapsed.Round(time.Second).String(), interactions)
}

// formatGuardEvidenceString is the string-shaped core of formatGuardEvidence,
// split out so the unit test can pin the exact wording without a duration.
func formatGuardEvidenceString(elapsed string, interactions int) string {
	return "elapsed " + elapsed + ", " + itoa(interactions) +
		" interactions, zero write-class artifacts"
}

// stampBuilderNudge writes the RFC3339 instant the first-window nudge
// fired, so the abort window measures from the nudge, not the spawn.
func (l *Loop) stampBuilderNudge(tickID string) {
	_, err := l.db.Exec(
		`UPDATE ticks SET guard_nudged_at = ? WHERE id = ?`,
		l.clock().Now().Format(time.RFC3339), tickID)
	if err != nil {
		log.Printf("WARN: builder_guard nudge stamp for %s: %v", tickID, err)
	}
}

// builderNudgeTime reads back the stamped nudge instant (zero when absent).
func (l *Loop) builderNudgeTime(tickID string) time.Time {
	var ts sql.NullString
	if err := l.db.QueryRow(
		`SELECT guard_nudged_at FROM ticks WHERE id = ?`, tickID).Scan(&ts); err != nil || !ts.Valid {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, ts.String)
	if err != nil {
		return time.Time{}
	}
	return t
}
