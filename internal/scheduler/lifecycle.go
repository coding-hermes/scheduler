package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// TickStatus is the lifecycle state.
type TickStatus string

const (
	TickQueued    TickStatus = "queued"
	TickRunning   TickStatus = "running"
	TickCompleted TickStatus = "completed"
	TickFailed    TickStatus = "failed"
	TickTimeout   TickStatus = "timeout"
	// TickDeferred is the terminal status for a tick the harness DEFERRED
	// instead of failing (SCHED-GAP-203). It exists because the gateway-health
	// gate (SCHED-GAP-170) guards only the PRE-SPAWN admission point: a blip
	// that lands INSIDE a spawn — a refused connect, or an SSE stream that
	// ended without a terminal event mid-run — was booked as the lane's own
	// fault, burning a pick, a slot and a cooldown and polluting the project's
	// failure rate (which feeds --auto-disable-failure-rate). LIVE EVIDENCE
	// (scheduler.db, boot 2026-09-19T10:46 → 2026-09-20): 9 of 458 ticks
	// failed this way, every one of them with the error text
	// "gateway unreachable and exec fallback disabled: gateway transient
	// error: …", while /api/v1/status reported the gate armed, healthy and
	// deferrals_total=0.
	//
	// A DEFERRED tick is NOT a failure and NOT a success: it is work the
	// harness declined to charge the lane for. Consequences that are
	// deliberate, not accidental:
	//
	//   - lifecycle.Complete leaves failure_reason EMPTY (that column's
	//     transport-class stamp is only for failed/timeout rows) — the status
	//     itself now names the class, so a second marker would be redundant.
	//   - consecutive_failures is NOT reset and NOT incremented. Failed and
	//     timeout ticks also leave it alone (GAP-133's backoff gate reads it);
	//     only a completed tick clears it.
	//   - The gate's deferrals counter is incremented by the spawn path
	//     (Spawner.transientGatewayDeferral), so the deferral is visible on
	//     the same deferrals_total an operator already reads.
	//
	// Auth rejections are NEVER deferred: a 401/403 is ErrGatewayKeyRejected,
	// terminal by GAP-035, and keeps failing loudly.
	TickDeferred TickStatus = "deferred"
)

// Outcome converts the tick status to the outcome column value.
func (s TickStatus) Outcome() string {
	switch s {
	case TickCompleted:
		return "committed"
	case TickFailed:
		return "failed"
	case TickTimeout:
		return "timeout"
	case TickDeferred:
		// The outcome column must carry the deferral too: an audit reads BOTH
		// columns (status for the lifecycle state, outcome for the terminal
		// verdict), and a deferred row whose outcome fell through to the
		// "dry_run" default would look like a simulated tick.
		return "deferred"
	default:
		return "dry_run"
	}
}

// terminalOutcome derives the outcome column from OBSERVED ARTIFACTS rather than
// from the fact that the process exited cleanly (SCHED-GAP-1652 — Bane's fix
// order, item 1: "a completed tick with no measurable artifact is dry_run, not
// committed").
//
// Measured 2026-09-27 over 3,717 ticks: 2,533 carried outcome='committed' with
// commits=0, and a quarter of those had in fact committed. The old mapping
// returned the literal "committed" for every TickCompleted, so the column
// claimed credit for empty ticks AND could not reveal the mis-measured ones.
//
// The vocabulary is deliberately unchanged by the completed path — the schema
// CHECK constrains outcome to ('committed','dry_run','failed','timeout',
// 'deferred') (plus 'aborted:no_artifact' since v50) and the metrics
// aggregator seeds those keys. "Could not measure" is therefore
// carried by a -1 sentinel in commits/files_changed, which is the convention
// code_commits/board_commits already use and which every consumer already reads
// as "not positive" (migrations.go: COALESCE(code_commits,0) > 0).
//
// A dry_run is NOT a failure and carries no penalty anywhere: consecutive_failures
// only moves on failed/timeout, and satellite lanes whose product is a DuckBrain
// write, a filed row or a battery verdict are tracked by their own per-family
// counters (qa_output_count / pm_output_count / sync_output_count), never by this
// column. Bane, explicitly: "the satellites don't always need to commit."
//
// SCHED-GAP-1674: a GUARD-ABORTED tick carries its own verdict through the
// status-agnostic path — the guard sets GuardAbort on the TickOutcome, and
// this mapping returns 'aborted:no_artifact' regardless of Status. That
// verdict is only legal for a status=failed row (an aborted session did not
// complete), so any other status keeps its own Outcome() mapping — the
// guard's outcome can never ride a completed or deferred row.
//
// SCHED-GAP-1655: a COMPLETED tick whose session ran ZERO tool calls
// (NoTools set by the gateway success path — envelope has no tool-call
// item and the SSE trace saw no tool event) records outcome='no_work',
// the migration-v55 verdict for "the lane had nothing to do". Ordering
// inside the completed branch is load-bearing: the artifact check runs
// FIRST, so a tick that somehow landed a commit without a tool item
// (countGitChanges measures the repo, not the transcript) still records
// 'committed' — artifacts outrank the transcript, the SCHED-GAP-1652
// doctrine. no_work is therefore unreachable for an artifact-bearing row
// by construction, not by discipline.
// SCHED-GAP-1680: a COMPLETED tick's success is derived from the artifacts
// its lane can produce — git commits/files AND declared NON-COMMIT side
// effects. commits=0 alone is NOT a no-op: a sync lane whose product is
// DuckBrain memory keys records MemoryKeys > 0 and must never be derived as
// dry_run (the fleet's 7-ticks/6-zero-commit sync shape was a measurement
// artifact, not 85% waste). The non-commit leg sits in the SAME artifact
// branch as the git leg, so an artifact of either kind outranks the
// zero-tool no_work verdict by construction.
func terminalOutcome(o TickOutcome) string {
	if o.GuardAbort {
		if o.Status == TickFailed {
			return AbortOutcomeValue
		}
		return o.Status.Outcome()
	}
	if o.Status != TickCompleted {
		return o.Status.Outcome()
	}
	if o.Commits > 0 || o.FilesChanged > 0 || o.MemoryKeys > 0 {
		return "committed"
	}
	if o.NoTools {
		return string(database.OutcomeNoWork)
	}
	return "dry_run"
}

// TickOutcome holds the result of a completed tick.
type TickOutcome struct {
	TickID       string
	Project      string
	SessionID    string
	Started      time.Time
	Finished     time.Time
	Duration     time.Duration
	Status       TickStatus
	ExitCode     int
	Error        string
	TokensIn     int     // simulated or real
	TokensOut    int     // simulated or real
	CostUSD      float64 // simulated or real
	CostSource   string  // ADV-R09/G8: measured | gateway | estimated | simulated
	Commits      int     // simulated or real
	FilesChanged int     // simulated or real
	// SCHED-GAP-1653 dispatch accountability, decided by the SPAWN PATH
	// (the only code that knows whether a foreman was invoked) and
	// persisted by lifecycle.Complete. DispatchDispatched=false with an
	// empty DispatchReason reads as "the caller forgot" — Complete then
	// records an UNREASONED stand-down (reason=blocked, evidence:none in
	// the row comment) so the tick is never completed with a silent
	// no-dispatch. Sim ticks and gateway-completed ticks set the flag;
	// spawn-site refusals name a reason from the same closed vocabulary
	// the DB layer validates (database.DispatchReasons).
	DispatchDispatched bool
	DispatchReason     string
	// GuardAbort (SCHED-GAP-1674): the builder no-artifact guard cancelled
	// this tick's session. terminalOutcome maps the row to
	// outcome='aborted:no_artifact' (legal only with Status=failed); the
	// status/outcome pair therefore records the guard's verdict on every
	// surface without any second marker column.
	GuardAbort bool
	// NoTools (SCHED-GAP-1655): the completed gateway session ran ZERO
	// tool calls — the response envelope carried no tool-call output item
	// and the SSE trace never observed a tool event. terminalOutcome maps
	// the row to outcome='no_work' (migration v55), the honest verdict
	// for "the lane had nothing to do": distinct from dry_run (ran tools,
	// landed no artifact) so the measured waste class is queryable. Only
	// consulted on Status=completed — a failed/deferred/aborted row keeps
	// its own verdict regardless.
	NoTools bool
	// MemoryKeys (SCHED-GAP-1680): DuckBrain memory-key WRITES observed in
	// this tick's gateway session transcript — a NON-COMMIT artifact. A
	// lane whose product is not a git commit (the whole -sync family)
	// produces these instead of commits; terminalOutcome counts > 0 as a
	// landed artifact so a sync tick with five verified keys is never
	// derived as dry_run. 0 = none observed on the measured surface (the
	// session transcript; exec/local ticks have no such mapping and read
	// 0). A measurement failure can only leave it 0 — it can never
	// manufacture a no-op.
	MemoryKeys int
	// SCHED-GAP-1707: partial-telemetry mark for ticks terminated by a
	// non-terminal kill (tick deadline, silence watchdog, stale reap,
	// dispatch deadline). TelemetryPartial=true persists telemetry_partial=1
	// plus TelemetryPartialReason (the closed vocabulary in telemetry.go)
	// and TelemetrySilenceS (the watchdog's measured quiet duration, 0 on
	// every other path). A timeout row WITHOUT this mark reads 0/0/0 as an
	// idle session — WITH it, the same zeros read "dead before any
	// observable usage", and a nonzero figure reads as the undercount it
	// is. The completed path never sets it.
	TelemetryPartial       bool
	TelemetryPartialReason string
	// TelemetrySilenceS is the session-silence watchdog's measured quiet
	// duration in whole seconds (ticks.session_silence_s). Only meaningful
	// with TelemetryPartialReason=session_silent.
	TelemetrySilenceS int64
	// WorkersTerminal (SCHED-GAP-1707 deliverable 3): how many of this
	// wave tick's workers had reached a terminal state (tick_workers.state
	// 'done') when the wave manifest was ingested. -1 = unmeasured (the
	// package's -1 sentinel convention): legacy rows, serial ticks, and
	// ticks the deadline killed before the foreman wrote any manifest.
	// Set by wave-manifest ingest, never by Wait().
	WorkersTerminal int
	// Attempts (SCHED-GAP-1681) is the gateway POST count for this tick,
	// folded from the merged GatewayPOSTTrace by the spawn path (the only
	// code that sees the trace: every POST, primary + GAP-080 retries +
	// chain hops). lifecycle.Complete stamps it on the v67 ticks.attempts
	// column in the same single finalization UPDATE. 0 = no gateway POST
	// (exec spawns, spawn-site refusals, remote dispatch) — the column's
	// honest default, never a fabricated count.
	Attempts int
}

// resolveDispatch resolves the dispatch accountability pair for one
// terminal tick (SCHED-GAP-1653, criterion 1: every tick records
// dispatch=yes|no plus a reason from the closed vocabulary). The spawn
// path's decision wins when it set one; a no-dispatch outcome whose
// caller forgot the reason (or contradicted it) degrades to blocked —
// recorded as blocked with the evidence marker in the row comment —
// rather than an empty pair, so a terminal tick can never carry a
// SILENT stand-down: an operator reading dispatch_reason=” on a v48
// row sees an unrecorded legacy tick, never an accountability hole
// this build wrote.
func (o TickOutcome) resolveDispatch() (string, string) {
	if o.DispatchDispatched {
		return database.DispatchYes, database.DispatchReasonDispatched
	}
	if database.DispatchReasonIsValid(o.DispatchReason) && o.DispatchReason != database.DispatchReasonDispatched {
		return database.DispatchNo, o.DispatchReason
	}
	// Unreasoned (or contradictory) stand-down: still recorded, honestly.
	return database.DispatchNo, database.DispatchReasonBlocked
}

// LifecycleTracker manages the tick state machine and outcome persistence.
type LifecycleTracker struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk clockSeam
	db  *sql.DB
	// schedulerBus (REMOTE-004, §3 visibility flow) publishes the terminal
	// transition to the federation bus. nil = disabled: the Complete path
	// checks Enabled() and behaves byte-identically to pre-REMOTE-004.
	// A bus failure can never fail Complete — PublishTickTerminal is
	// contractually cannot-error (the autonomy law).
	schedulerBus *SchedulerBus
}

// NewLifecycleTracker creates a lifecycle tracker.
func NewLifecycleTracker(db *sql.DB) *LifecycleTracker {
	return &LifecycleTracker{db: db}
}

// SetSchedulerBus installs the visibility publisher (REMOTE-004). nil or a
// disabled bus leaves the tracker silent. Loop.SetSchedulerBus propagates
// here, matching the SetClock propagation convention.
func (lt *LifecycleTracker) SetSchedulerBus(b *SchedulerBus) {
	if b == nil || !b.Enabled() {
		lt.schedulerBus = nil
		return
	}
	lt.schedulerBus = b
}

// Enqueue creates a queued tick entry for the project.
func (lt *LifecycleTracker) Enqueue(project, tickID string) error {
	_, err := lt.db.Exec(`
		INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, tickID, project, TickQueued, lt.clock().Now().Format(time.RFC3339), lt.clock().Now().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("enqueue tick %s: %w", tickID, err)
	}
	return nil
}

// StartRunning transitions a tick from queued to running.
func (lt *LifecycleTracker) StartRunning(tickID string) error {
	_, err := lt.db.Exec(`
		UPDATE ticks SET status = ? WHERE id = ?
	`, TickRunning, tickID)
	if err != nil {
		return fmt.Errorf("start tick %s: %w", tickID, err)
	}
	return nil
}

// Complete writes the final outcome of a tick to the database.
// SCHED-GAP-029: also persists commits/files_changed (previously only
// tokens_in/tokens_out/cost_usd were written, leaving commits/files zero).
func (lt *LifecycleTracker) Complete(outcome TickOutcome) error {
	var exitCode interface{}
	if outcome.ExitCode >= 0 {
		exitCode = outcome.ExitCode
	}
	// SCHED-GAP-143: stamp the TRANSPORT-CLASS marker for failed/timeout
	// outcomes, so a tick the harness refused (drain 503, gateway down) is
	// distinguishable in SQL from a project-side failure without re-parsing
	// ticks.error. Empty = not transport-class, the safe default for every
	// legacy row and for successful ticks. Same classifier the spawn path
	// uses to decide whether a failure may touch consecutive_failures.
	//
	// SCHED-GAP-203: TickDeferred is deliberately NOT in this set. A deferred
	// row already carries its classification in the STATUS column ("deferred"
	// is itself the statement that the harness declined to charge the lane),
	// so a transport marker here would be a second, weaker copy of the same
	// fact — and the marker vocabulary exists for rows whose status cannot
	// express it.
	failureReason := ""
	if outcome.Status == TickFailed || outcome.Status == TickTimeout {
		failureReason = failureReasonClass(outcome.Error)
	}
	// SCHED-GAP-1653: resolve this tick's dispatch accountability pair and
	// apply criterion 3's gate — never book 'done' without a landed
	// artifact, a verified worker commit, or a recorded reasoned
	// stand-down. Kept in ONE finalization UPDATE (criterion 2's coverage
	// guarantee: every row this method writes carries the pair — no
	// second UPDATE that can silently miss). Criterion 3 is implemented
	// as the RECORDED REASONED STAND-DOWN branch: a tick completing with
	// zero measurable artifact and no dispatch keeps status=completed
	// (the codebase-wide disruption test) but the row is honestly marked
	// by the pair itself PLUS a tick-scoped event naming the stand-down,
	// so no completion is ever silent. A dispatched-but-empty completion
	// needs no stand-down event: its outcome column already reads
	// dry_run via terminalOutcome() (SCHED-GAP-1652's honest mapping),
	// which is exactly that statement.
	dispatchOutcome, dispatchReason := outcome.resolveDispatch()
	if outcome.Status == TickCompleted {
		// Landed-artifact evidence the OUTCOME carries: the measured git
		// delta (positive = artifact; -1 = unmeasured, never read as
		// zero — the SCHED-GAP-1652 sentinel rule), plus the non-commit
		// side effects a lane whose product is not a commit produces
		// (SCHED-GAP-1680: DuckBrain memory keys written). The wave's
		// per-worker rows are attributed AFTER this write by manifest
		// ingest, and code/board commit anatomy is split later from the
		// same raw count — neither carries evidence this method could
		// read yet.
		landed := outcome.Commits > 0 || outcome.FilesChanged > 0 || outcome.MemoryKeys > 0
		if !landed && !outcome.DispatchDispatched {
			log.Printf("STAND-DOWN: %s tick=%s completed with no landed artifact and no dispatch — recorded reason=%q (SCHED-GAP-1653)",
				outcome.Project, outcome.TickID, dispatchReason)
			// Best-effort observability: a failed event insert costs a
			// log line, never the completion. context.Background() (not
			// the component clock) matches the wave-ingest event path —
			// created_at is a wall-clock stamp on an audit row, the same
			// precedent slot_pool's ingest uses.
			_ = database.LogEvent(context.Background(), lt.db, &database.Event{
				Severity:  database.SeverityLow,
				Component: "lifecycle",
				Message:   fmt.Sprintf("stand-down: tick %s (%s) completed with no landed artifact — dispatch_reason=%s", outcome.TickID, outcome.Project, dispatchReason),
				Details:   fmt.Sprintf(`{"tick_id":%q,"project":%q,"dispatch_reason":%q}`, outcome.TickID, outcome.Project, dispatchReason),
			})
		}
	}
	// SCHED-GAP-1707: the partial-telemetry mark rides the SAME single
	// finalization UPDATE (criterion 2's coverage guarantee applies here
	// too — no second UPDATE that can silently miss). Only TIMEOUT rows may
	// carry it: the brief's kill class is status/outcome=timeout, a
	// guard-aborted row's telemetry legs were fully measured at the abort
	// site (not partial), and a completed row must never read partial even
	// if a caller set the field by mistake.
	telemetryPartial := 0
	telemetryReason := ""
	telemetrySilence := int64(0)
	if outcome.TelemetryPartial && outcome.Status == TickTimeout {
		if !telemetryPartialReasonIsValid(outcome.TelemetryPartialReason) {
			// Unreachable from the code paths (they all go through
			// timeoutTelemetry), but the write site refuses to invent a
			// named cause silently — name the most common class.
			outcome.TelemetryPartialReason = TelemetryPartialTickDeadline
		}
		telemetryPartial = 1
		telemetryReason = outcome.TelemetryPartialReason
		telemetrySilence = outcome.TelemetrySilenceS
	}
	// workers_terminal: Complete NEVER carries a measured count — the wave
	// manifest ingest writes its own post-Complete UPDATE (the only site
	// that knows the real convergence). Any zero here is the struct's unset
	// value, which maps to the -1 unmeasured sentinel (never a fabricated
	// "0 workers were done"); ingest overwrites it with the real count.
	workersTerminal := outcome.WorkersTerminal
	if workersTerminal == 0 {
		workersTerminal = -1
	}
	_, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, outcome = ?, completed_at = ?, exit_code = ?, error = ?, session_id = ?,
			tokens_in = ?, tokens_out = ?, cost_usd = ?, cost_source = ?,
			commits = ?, files_changed = ?, memory_keys = ?, failure_reason = ?,
			dispatch_outcome = ?, dispatch_reason = ?,
			telemetry_partial = ?, telemetry_partial_reason = ?, session_silence_s = ?, workers_terminal = ?,
			attempts = ?
		WHERE id = ?
	`, string(outcome.Status), terminalOutcome(outcome), outcome.Finished.Format(time.RFC3339), exitCode,
		stringOrNil(outcome.Error), stringOrNil(outcome.SessionID),
		outcome.TokensIn, outcome.TokensOut, outcome.CostUSD, outcome.CostSource,
		outcome.Commits, outcome.FilesChanged, outcome.MemoryKeys, failureReason,
		dispatchOutcome, dispatchReason,
		telemetryPartial, telemetryReason, telemetrySilence, workersTerminal,
		outcome.Attempts,
		outcome.TickID)
	if err != nil {
		return fmt.Errorf("complete tick %s: %w", outcome.TickID, err)
	}

	// REMOTE-004 (§3 visibility flow): the tick just reached a TERMINAL
	// state — publish the transition to the federation bus. The event's
	// status is the row's lifecycle verdict (completed / failed / deferred
	// / timeout), deliberately NOT the outcome column's artifact verdict.
	// BEST-EFFORT by construction: PublishTickTerminal cannot error and
	// cannot block beyond the client's own 5s deadline — a down, refused
	// or timed-out relay is logged and dropped inside, so this call can
	// never change Complete's result (the autonomy law, proven by
	// TestRemote004_PublishFailureDoesNotFailTick).
	if lt.schedulerBus != nil && lt.schedulerBus.Enabled() {
		lt.schedulerBus.PublishTickTerminal(context.Background(), outcome.Project, outcome.TickID, string(outcome.Status))
	}

	// Update project's last_tick_completed for ALL outcomes (completed, failed, timeout).
	// Previously only updated on TickCompleted — eduos-e2e demonstrated that projects
	// with only failed ticks need cooldown enforcement too, or they flood the scheduler.
	// SCHED-GAP-214: the SAME write now stamps last_tick_status — the terminal
	// status of this most-recent tick (completed | failed | timeout | deferred).
	// The tasks-mode cooldown waiver (SCHED-GAP-124) consults it: after a FAILED
	// tick the waiver stands down and the lane paces on its full effective
	// cooldown, so a gateway outage samples each tasks lane at the lane cadence
	// instead of re-spawning it every eval (the measured 91-ticks-in-90s crier
	// storm). One UPDATE, one row, atomic in the single completion write.
	lastStatus := ""
	switch outcome.Status {
	case TickCompleted:
		lastStatus = database.LastStatusCompleted
	case TickFailed:
		lastStatus = database.LastStatusFailed
	case TickTimeout:
		lastStatus = database.LastStatusTimeout
	case TickDeferred:
		lastStatus = database.LastStatusDeferred
	}
	_, err = lt.db.Exec(`
		UPDATE projects SET last_tick_completed = ?, last_tick_status = ? WHERE name = ?
	`, outcome.Finished.Format(time.RFC3339), lastStatus, outcome.Project)
	if err != nil {
		log.Printf("WARN: failed to update last_tick_completed/last_tick_status for %s: %v", outcome.Project, err)
	}

	// SCHED-GAP-137a: a successful tick clears the consecutive-failure backoff
	// counter. Local-spawn ticks never pass through spawn.go's spawn-time reset
	// (that path only fires on gateway spawn / new spawn), so a project that
	// failed N times in a drain storm kept the residue across successful local
	// completions (observed: cf=91 on bunker/chimera-v2/crier despite recent
	// successful last_tick_completed).
	//
	// SCHED-GAP-1705 supersedes this block's old "failed and timeout outcomes
	// intentionally leave the counter alone" sentence. The counter now moves
	// on every terminal status, each in exactly one direction:
	//
	//   - TickCompleted: reset to 0 (unchanged; below). A completed tick is
	//     the project demonstrating a good run.
	//   - TickTimeout: +1 (below). A tick that ran the full wall and produced
	//     nothing IS the lane's fault. Measured 2026-10-02: two
	//     task-router-foreman wave ticks ended status=timeout at exactly
	//     3h0m0s with worker_count=0 and tokens 0/0, while the working tick
	//     the same day produced 8.4M tokens in 94 min — silence is
	//     distinguishable from real work. The transport-class carve-out
	//     (SCHED-GAP-143) is preserved exactly: when
	//     failureReasonClass(outcome.Error) != "" (gateway drain, gateway
	//     unreachable, a tick deadline that tore down a gateway POST, the
	//     session-silence watchdog kill), the counter is untouched — the
	//     harness, not the lane, ended that tick. The relative increment
	//     matches noteSpawnFailure's SQL, so spawn-path failures and
	//     timeout-path failures accumulate in the one counter GAP-133's
	//     FailureBackoff gate already reads (effectiveCooldown, packer.go).
	//     Deliberately NO new auto-disable wiring rides this: SCHED-GAP-018's
	//     breaker reads failure_rate, not this counter (the
	//     consecutive-failures alert does read it and now sees silent
	//     timeouts too).
	//   - TickFailed: left alone — the spawn path already counted it at
	//     noteSpawnFailureClassed; a Complete-side increment would
	//     double-charge the lane. Timeouts were the one terminal status no
	//     counter observed; before SCHED-GAP-1705 that is why a dead wave
	//     retried on its bare cooldown forever.
	//   - TickDeferred (SCHED-GAP-203): untouched in both directions — it
	//     neither clears the counter (the project has not demonstrated a
	//     good tick) nor increments it (the spawn path never calls
	//     noteSpawnFailure for a deferral: a gateway blip is not the
	//     lane's failure).
	if outcome.Status == TickCompleted {
		if _, err := lt.db.Exec(`
			UPDATE projects SET consecutive_failures = 0 WHERE name = ?
		`, outcome.Project); err != nil {
			log.Printf("WARN: failed to reset consecutive_failures for %s: %v", outcome.Project, err)
		}
	}
	// SCHED-GAP-1705: a TIMEOUT counts as a lane failure for backoff —
	// unless the failure is transport-class (SCHED-GAP-143's rule, applied
	// through failureReasonClass — the same single classifier the spawn
	// path uses, so the two paths can never disagree). Mirrors
	// noteSpawnFailure's relative increment; best-effort: a DB error here
	// is a WARN, never a Complete failure.
	if outcome.Status == TickTimeout && failureReasonClass(outcome.Error) == "" {
		if _, err := lt.db.Exec(`
			UPDATE projects SET consecutive_failures = consecutive_failures + 1 WHERE name = ?
		`, outcome.Project); err != nil {
			log.Printf("WARN: failed to increment consecutive_failures for %s: %v", outcome.Project, err)
		}
	}

	return nil
}

// SCHED-GAP-1652 removed ExportSession. It was a stub that returned a zero-valued
// SessionStats and parsed nothing, while specs/S05-spawn-engine-lifecycle.md
// implied it supplied commits/files_changed/outcome from `hermes sessions export`.
// It could not have: the export record carries 59 fields and none of them is a git
// fact (they are input_tokens/output_tokens/cache_read_tokens/estimated_cost_usd/
// message_count/tool_call_count/messages). Commit counts come from the git delta
// in gitmetrics.go; there is deliberately no second, phantom source. It had no
// production callers.

// CleanupStale clears running ticks older than the given duration.
func (lt *LifecycleTracker) CleanupStale(maxAge time.Duration) (int, error) {
	_, n, err := lt.CleanupStaleProjects(maxAge)
	return n, err
}

// CleanupStaleProjects is CleanupStale with SCHED-GAP-186 reporting: it also
// returns the DISTINCT project names whose running rows were flipped terminal,
// so the caller can reconcile the SlotPool claims those rows owned (the UPDATE
// itself never touches the in-process slot pool). The status/completed_at/
// error UPDATE is byte-identical to the original CleanupStale.
func (lt *LifecycleTracker) CleanupStaleProjects(maxAge time.Duration) ([]string, int, error) {
	cutoff := lt.clock().Now().Add(-maxAge)
	rows, err := lt.db.Query(`
		SELECT DISTINCT project_name FROM ticks
		WHERE status = ? AND spawned_at < ?
	`, TickRunning, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, 0, err
	}
	var projects []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		projects = append(projects, name)
	}
	rows.Close()
	// SCHED-GAP-1707: the stale reap is a non-terminal kill — flip the
	// status AND stamp the partial mark in the SAME UPDATE, so the row is
	// never indistinguishable from an idle 0/0/0 timeout. No Wait() path
	// ever ran for these rows, so the telemetry legs stay whatever they were
	// (0/0/0 for gateway rows: the mark is the honest statement that the
	// session was killed unobserved, named stale_reap).
	res, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, completed_at = ?, error = ?,
			telemetry_partial = 1, telemetry_partial_reason = ?
		WHERE status = ? AND spawned_at < ?
	`, TickTimeout, lt.clock().Now().Format(time.RFC3339), "stale — timeout at "+maxAge.String(),
		TelemetryPartialStaleReap, TickRunning, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("CLEANUP: %d stale running ticks timed out", n)
	}
	return projects, int(n), nil
}

func stringOrNil(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// RunningCount returns the number of currently running ticks.
func (lt *LifecycleTracker) RunningCount() int {
	var n int
	lt.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n)
	return n
}

// SetClock installs the clock this LifecycleTracker reads and waits on (SCHED-GAP-169).
// nil keeps the wall clock.
func (lt *LifecycleTracker) SetClock(c clock.Clock) { lt.clk.Set(c) }

// clock returns the component's clock, never nil.
func (lt *LifecycleTracker) clock() clock.Clock { return lt.clk.Get() }
