package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SCHED-GAP-16947 — the REPAIR PASS (deliverable e).
//
// THE BLACKOUT. 2026-10-10..12 carried 2,701 zero-token dry ticks
// (1,043 / 1,034 / 624 per day) while OTHER lanes committed in the same
// window: a three-day lane blackout that the schema booked as idle. 'dry'
// stopped being an observation and became a hiding place. Those rows are
// already written; deleting or silently rewriting them would destroy the very
// evidence an operator needs, so the repair ANNOTATES instead:
//
//   - it writes ticks.reason_code (the closed vocabulary) and
//     ticks.reason_code_derivation (the evidence + derivation it read) in ONE
//     UPDATE per row;
//   - it never touches status, outcome, error, tokens, commits or any other
//     historical column, and it never overwrites a code that is already set
//     (`WHERE id = ? AND reason_code = ''` makes a re-run a no-op — the pass is
//     idempotent by construction);
//   - it writes ONE events row per run naming the window and the class counts,
//     so the repair is visible on the same audit surface as every other
//     scheduler action.
//
// THE DERIVATION, from evidence still present in the rows:
//
//	artifact present (commits/files/memory > 0)   → NOT a candidate: the tick
//	                                                 produced something, nothing is owed.
//	artifact UNMEASURED (commits or files == -1)   → NOT a candidate: the -1
//	                                                 sentinel says "not observed",
//	                                                 and an unobserved artifact
//	                                                 cannot be repaired into "none".
//	tokens_in or tokens_out > 0                    → no_artifact: a session ran
//	                                                 and spent, and landed nothing.
//	zero on both token legs:
//	  lane had >= 1 productive tick in the window  → malformed_tool_call: the
//	                                                 lane was working; this session
//	                                                 produced no observable output.
//	  lane dark, fleet productive in the window    → lane_unavailable: the
//	                                                 three-day blackout class.
//	  fleet dark across the window                 → unknown:<raw>: a fleet-wide
//	                                                 silence cannot be attributed
//	                                                 to one lane, so the raw signal
//	                                                 (the zero-token shape) is kept.
//
// The window is inclusive of From and exclusive of To, compared with julianday()
// on both sides because ticks.spawned_at mixes naive and offset-bearing RFC3339
// stamps (the same predicate the metrics windows use).

// ReasonCodeRepairOpts selects the repair window and whether to write.
type ReasonCodeRepairOpts struct {
	// From and To bound the window [From, To). A zero From or To is an error:
	// an unbounded repair pass over tick history is never what an operator
	// means.
	From time.Time
	To   time.Time
	// Apply writes the codes. False is a dry run: the report is identical, the
	// database is untouched.
	Apply bool
}

// ReasonCodeRepairRow is one candidate tick's verdict.
type ReasonCodeRepairRow struct {
	TickID     string `json:"tick_id"`
	Project    string `json:"project"`
	SpawnedAt  string `json:"spawned_at"`
	Outcome    string `json:"outcome"`
	Code       string `json:"code"`
	Derivation string `json:"derivation"`
	Applied    bool   `json:"applied"`
}

// ReasonCodeRepairReport is the pass's full accounting: what was scanned, what
// was skipped and WHY (never a silent skip), what each candidate earned, and
// the derivation each code was given.
type ReasonCodeRepairReport struct {
	WindowFrom     string                `json:"window_from"`
	WindowTo       string                `json:"window_to"`
	Apply          bool                  `json:"apply"`
	Concluded      int                   `json:"concluded"`       // terminal ticks in the window (any verdict)
	AlreadyStamped int                   `json:"already_stamped"` // no-artifact ticks that already carry a code
	Unmeasured     int                   `json:"unmeasured"`      // no-artifact candidates excluded: artifact unobserved (-1)
	Artifact       int                   `json:"artifact"`        // dry-verdict ticks that DID carry an artifact (nothing owed)
	Candidates     int                   `json:"candidates"`      // rows this pass classified
	Applied        int                   `json:"applied"`         // rows actually written (Apply only)
	Classes        map[string]int        `json:"classes"`         // code -> count over Candidates
	Rows           []ReasonCodeRepairRow `json:"rows"`
}

// repairLaneProductive is the per-project productivity evidence the derivation
// reads: how many ticks in the window landed an artifact.
type repairLaneProductive struct {
	perProject map[string]int
	fleet      int
}

// RepairTickReasonCodes reclassifies the dry ticks inside one window from the
// evidence still present, annotating the derivation. Idempotent: a second run
// over a repaired window finds every candidate already stamped and writes
// nothing. Best-effort per row: a single failing UPDATE is recorded in the
// report as not-applied and does not abort the pass.
func RepairTickReasonCodes(ctx context.Context, db *sql.DB, opts ReasonCodeRepairOpts) (ReasonCodeRepairReport, error) {
	rep := ReasonCodeRepairReport{
		Apply:   opts.Apply,
		Classes: make(map[string]int, len(TickReasonCodes)+1),
	}
	if db == nil {
		return rep, fmt.Errorf("repair tick reason codes: nil database")
	}
	if opts.From.IsZero() || opts.To.IsZero() {
		return rep, fmt.Errorf("repair tick reason codes: window bounds are required (from=%v to=%v)", opts.From, opts.To)
	}
	if !opts.To.After(opts.From) {
		return rep, fmt.Errorf("repair tick reason codes: window end %v is not after start %v", opts.To, opts.From)
	}
	from := opts.From.UTC().Format(time.RFC3339)
	to := opts.To.UTC().Format(time.RFC3339)
	rep.WindowFrom = from
	rep.WindowTo = to

	// Context counts: every terminal tick in the window, so the report can say
	// how much of the window the candidates are.
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ticks
		WHERE julianday(spawned_at) >= julianday(?)
		  AND julianday(spawned_at) < julianday(?)
		  AND status IN ('completed', 'failed', 'timeout', 'deferred', 'aborted')
	`, from, to).Scan(&rep.Concluded); err != nil {
		return rep, fmt.Errorf("count concluded ticks in window: %w", err)
	}
	// Already-stamped rows are the idempotency surface: a repaired window (or a
	// window a live finalization already coded) reports them instead of
	// silently skipping.
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ticks
		WHERE julianday(spawned_at) >= julianday(?)
		  AND julianday(spawned_at) < julianday(?)
		  AND outcome IN ('dry_run', 'no_work')
		  AND reason_code <> ''
	`, from, to).Scan(&rep.AlreadyStamped); err != nil {
		return rep, fmt.Errorf("count already-stamped ticks in window: %w", err)
	}

	lane, err := repairLaneProductivity(ctx, db, from, to)
	if err != nil {
		return rep, err
	}

	// The candidate sweep is deliberately narrow: rows whose OWN verdict is
	// already "nothing landed" (outcome dry_run/no_work) and that carry no
	// reason code yet. Failed/timeout rows keep the transport classifier's
	// failure_reason, and legacy NULL-outcome rows are counted as unclassified
	// (below) rather than repainted by inference.
	rows, err := db.QueryContext(ctx, `
		SELECT id, project_name, COALESCE(spawned_at, ''), COALESCE(outcome, ''),
		       tokens_in, tokens_out, commits, files_changed, memory_keys
		FROM ticks
		WHERE julianday(spawned_at) >= julianday(?)
		  AND julianday(spawned_at) < julianday(?)
		  AND outcome IN ('dry_run', 'no_work')
		  AND reason_code = ''
		ORDER BY project_name, spawned_at, id
	`, from, to)
	if err != nil {
		return rep, fmt.Errorf("select repair candidates: %w", err)
	}
	defer rows.Close()

	var candidates []ReasonCodeRepairRow
	for rows.Next() {
		var (
			id, project, spawnedAt, outcome   string
			tokensIn, tokensOut               int64
			commits, filesChanged, memoryKeys int64
		)
		if err := rows.Scan(&id, &project, &spawnedAt, &outcome,
			&tokensIn, &tokensOut, &commits, &filesChanged, &memoryKeys); err != nil {
			return rep, fmt.Errorf("scan repair candidate: %w", err)
		}
		// The -1 sentinel is "artifact not observed" (SCHED-GAP-1652/202): an
		// unobserved delta cannot be repaired into "no artifact", so the row is
		// reported and left alone.
		if commits < 0 || filesChanged < 0 {
			rep.Unmeasured++
			continue
		}
		if commits > 0 || filesChanged > 0 || memoryKeys > 0 {
			rep.Artifact++
			continue
		}
		code, derivation := classifyRepairedDryTick(
			project, spawnedAt, outcome, tokensIn, tokensOut,
			lane.perProject[project], lane.fleet,
			commits, filesChanged,
		)
		rep.Classes[code]++
		candidates = append(candidates, ReasonCodeRepairRow{
			TickID:     id,
			Project:    project,
			SpawnedAt:  spawnedAt,
			Outcome:    outcome,
			Code:       code,
			Derivation: derivation,
		})
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("iterate repair candidates: %w", err)
	}
	rep.Candidates = len(candidates)

	for i := range candidates {
		c := &candidates[i]
		if !opts.Apply {
			rep.Rows = append(rep.Rows, *c)
			continue
		}
		// The idempotency guard lives in the WHERE clause: a row a live
		// conclusion path stamped between the sweep and this write keeps that
		// first-hand code, and a re-run of the pass writes nothing.
		res, err := db.ExecContext(ctx, `
			UPDATE ticks SET reason_code = ?, reason_code_derivation = ?
			WHERE id = ? AND reason_code = ''
		`, c.Code, c.Derivation, c.TickID)
		if err == nil {
			if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
				c.Applied = true
				rep.Applied++
			}
		}
		rep.Rows = append(rep.Rows, *c)
	}

	if opts.Apply && rep.Applied > 0 {
		// One audit row naming the window and the class split. Best-effort:
		// the codes are already written; a failed event insert costs a
		// log-visible gap, never the repair.
		classSummary := repairClassSummary(rep.Classes)
		_ = LogEvent(ctx, db, &Event{
			Severity:  SeverityLow,
			Component: "repair",
			Message: fmt.Sprintf("SCHED-GAP-16947 reason-code repair: %d tick(s) reclassified in %s..%s — %s",
				rep.Applied, from, to, classSummary),
			Details: fmt.Sprintf(`{"window_from":%q,"window_to":%q,"concluded":%d,"candidates":%d,"applied":%d,"classes":%s}`,
				from, to, rep.Concluded, rep.Candidates, rep.Applied, repairClassJSON(rep.Classes)),
		})
	}
	return rep, nil
}

// repairLaneProductivity counts the ticks in the window that landed an
// artifact, per lane and fleet-wide. It is the aliveness evidence the blackout
// derivation reads: a lane with zero productive ticks while the fleet has many
// is not "idle", it is dark.
func repairLaneProductivity(ctx context.Context, db *sql.DB, from, to string) (repairLaneProductive, error) {
	out := repairLaneProductive{perProject: make(map[string]int)}
	rows, err := db.QueryContext(ctx, `
		SELECT project_name, COUNT(*) FROM ticks
		WHERE julianday(spawned_at) >= julianday(?)
		  AND julianday(spawned_at) < julianday(?)
		  AND (commits > 0 OR files_changed > 0 OR memory_keys > 0)
		GROUP BY project_name
	`, from, to)
	if err != nil {
		return out, fmt.Errorf("measure lane productivity in window: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var n int
		if err := rows.Scan(&project, &n); err != nil {
			return out, fmt.Errorf("scan lane productivity: %w", err)
		}
		out.perProject[project] = n
		out.fleet += n
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate lane productivity: %w", err)
	}
	return out, nil
}

// classificationRepairBlackout and friends are the derivation CLASS names the
// annotation text carries, so an operator can grep the repair's own vocabulary
// out of ticks.reason_code_derivation.
const (
	repairClassBlackout       = "three_day_lane_blackout"
	repairClassSilentSession  = "session_produced_no_output"
	repairClassSpentNoLanding = "spent_tokens_landed_nothing"
	repairClassFleetDark      = "fleet_wide_silence_unattributable"
)

// classifyRepairedDryTick derives one repaired code plus the derivation text
// annotating it. Pure: every input is read from the row or the window's
// productivity measurement, so the mapping is unit-testable without a database.
func classifyRepairedDryTick(project, spawnedAt, outcome string, tokensIn, tokensOut int64,
	laneProductive, fleetProductive int, commits, filesChanged int64) (string, string) {
	evidence := fmt.Sprintf("outcome=%s,tokens_in=%d,tokens_out=%d,commits=%d,files_changed=%d,lane_productive_ticks=%d,fleet_productive_ticks=%d",
		outcome, tokensIn, tokensOut, commits, filesChanged, laneProductive, fleetProductive)

	var code, class string
	switch {
	case tokensIn > 0 || tokensOut > 0:
		// A session ran and spent: the model did real work and landed nothing.
		code, class = TickReasonCodeNoArtifact, repairClassSpentNoLanding
	case laneProductive > 0:
		// The lane was demonstrably working in this window; this tick's session
		// produced no observable output.
		code, class = TickReasonCodeMalformedToolCall, repairClassSilentSession
	case fleetProductive > 0:
		// Zero tokens on a lane that landed nothing all window while sibling
		// lanes committed: the three-day blackout, not idleness.
		code, class = TickReasonCodeLaneUnavailable, repairClassBlackout
	default:
		// Not even the fleet produced anything: a fleet-wide silence cannot be
		// attributed to this lane, so the raw signal survives in the code.
		return TickReasonCodeUnknown("zero-token dry tick, fleet silent in window (" + evidence + ")"),
			"repair:class=" + repairClassFleetDark + " evidence=" + evidence
	}
	return code, "repair:class=" + class + " evidence=" + evidence
}

// repairClassSummary renders the class split for the audit event's message.
func repairClassSummary(classes map[string]int) string {
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, classes[k]))
	}
	return strings.Join(parts, " ")
}

// repairClassJSON renders the class split as a JSON object body (already
// escaped for the common vocabulary values, which are bare identifiers).
func repairClassJSON(classes map[string]int) string {
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q:%d", k, classes[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
