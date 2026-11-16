package scheduler

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-430 — the closed DRY-REASON vocabulary.
//
// THE PROBLEM. ticks.failure_reason (migration v33) exists so a row can say WHY
// it ended the way it did without a human re-parsing ticks.error. Until this
// row only FAILED/TIMEOUT rows ever wrote it, and only for the TRANSPORT class
// (failureReasonClass: gateway_drain, gateway_transport, session_silent,
// sleep_poll, instant_turn, …). A tick that COMPLETED and landed nothing — a
// dry tick — wrote an empty column. A 2026-11-15 foreman drill measured 344
// dry ticks/week with 86.9% of them ownership-barrier, and the only way back to
// that verdict was transcript archaeology: the outcome column says "dry_run",
// which is a verdict about the RESULT, never about the CAUSE.
//
// THE VOCABULARY. Six tokens, closed. A dry tick writes exactly one of them,
// through the SAME single finalization UPDATE lifecycle.Complete already uses
// for the outcome (SCHED-GAP-1653's one-write coverage guarantee: no second
// UPDATE that can silently miss a row).
//
//	EMPTY_BOARD           nothing dispatchable was observed for the lane —
//	                      the board was read and held zero open rows, or the
//	                      lane has no readable board at all (an evidence gap
//	                      the no-work gates treat as fail-open, so the tick
//	                      ran). Bane's "there was nothing to do".
//	OWNERSHIP_BARRIER     the board HAS open rows but none of them is
//	                      dispatchable right now: every open row is claimed,
//	                      in_progress or in rework — owned by another in-flight
//	                      tick or worker. The measured 86.9% class.
//	GATE_WITHHELD         work was available and this tick still dispatched
//	                      nothing: the scheduler stood the lane down without a
//	                      dispatch (SCHED-GAP-1653's reasoned stand-down), or
//	                      the board held pending rows the tick's own gates
//	                      withheld (owner-gated, dependency-blocked, acceptance
//	                      pending). The tick ran, the gates said no.
//	DUPLICATE_PREVENTION  the tick was closed because a duplicate spawn was
//	                      refused for the lane (SCHED-GAP-030). DECLARED ONLY:
//	                      the pre-spawn dedup arms in the packer and in
//	                      Loop.spawnNowTargetedWithSource run BEFORE any tick
//	                      row exists, so no derived arm can ever prove this
//	                      class from the row alone — a caller that does own a
//	                      row (a queue-replay close-out) declares it on
//	                      TickOutcome.DryReason.
//	DEFERRED              the only open rows on the lane's board are declared
//	                      deferred (board row deferred:true — visible to a
//	                      human, deliberately not actionable, excluded from
//	                      the open-work count since SCHED-GAP-1727). The lane
//	                      had work and its owner said "not now".
//	WAVE_PERSISTED        the tick's product is a WAVE: a manifest with >= 1
//	                      worker exists for this tick id, so the work it
//	                      dispatched lives in per-worker rows and worktrees
//	                      (ticks.worker_count / tick_workers) rather than in a
//	                      commit attributed to the tick itself.
//
// WHAT IS DELIBERATELY UNTOUCHED. The transport-class classifier keeps its
// column for failed/timeout rows (gateway_drain, gateway_transport,
// session_silent, sleep_poll, instant_turn, operator_restart, hook_failed:<id>)
// — those tokens answer "did the HARNESS end this tick", which is a different
// question from "why did this tick produce nothing", and they must keep flowing
// unchanged. A DEFERRED-status row (SCHED-GAP-203: the harness declined to
// charge the lane) also keeps its empty column: the status column already names
// that class, and this vocabulary is exclusively about COMPLETED ticks that
// landed no artifact.
//
// FORENSICS. failure_reason IS NOT NULL AND failure_reason <> '' marks a
// post-SCHED-430 row whose cause is recorded. Pre-change rows keep NULL/'' (no
// backfill by design: the data needed to classify them was never written, and
// inventing it from today's board would be fabrication).

// The six dry-reason tokens. They are Go typed constants (not bare strings at
// the write sites) so a grep finds the whole vocabulary in one place and the
// compiler catches a typo.
const (
	// DryReasonEmptyBoard — nothing dispatchable was observed for the lane.
	DryReasonEmptyBoard = "EMPTY_BOARD"
	// DryReasonOwnershipBarrier — open rows exist but all are owned/in-flight.
	DryReasonOwnershipBarrier = "OWNERSHIP_BARRIER"
	// DryReasonGateWithheld — work existed, the tick's gates (or the scheduler's
	// own stand-down) withheld the dispatch.
	DryReasonGateWithheld = "GATE_WITHHELD"
	// DryReasonDuplicatePrevention — the row was closed because a duplicate
	// spawn was refused for the lane.
	DryReasonDuplicatePrevention = "DUPLICATE_PREVENTION"
	// DryReasonDeferred — every remaining row on the board is deferred:true.
	DryReasonDeferred = "DEFERRED"
	// DryReasonWavePersisted — the tick's product is its worker wave.
	DryReasonWavePersisted = "WAVE_PERSISTED"
)

// DryReasons is the closed vocabulary, in DECISION precedence order (the
// resolver below walks it from the most specific evidence to the most generic).
// Consumers that render every bucket read this slice instead of inventing keys.
var DryReasons = []string{
	DryReasonWavePersisted,
	DryReasonDuplicatePrevention,
	DryReasonDeferred,
	DryReasonOwnershipBarrier,
	DryReasonGateWithheld,
	DryReasonEmptyBoard,
}

// DryReasonIsValid reports whether v is a member of the closed vocabulary. An
// empty string is NOT valid: "no reason" is spelled by not setting the field,
// never by a token the resolver would have to special-case.
func DryReasonIsValid(v string) bool {
	for _, r := range DryReasons {
		if r == v {
			return true
		}
	}
	return false
}

// dryBoardEvidence is the board read the resolver classifies from. ONE scan
// produces all three counts, so the counts can never disagree about the same
// file (the fleet's second-parser ban: boardOpenRows below is now a view onto
// this scan, not a copy of it).
//
//	open     — rows boardOpenRows counts (the open vocabulary, fixture and
//	           deferred rows excluded, malformed rows counted).
//	pending  — the subset of open rows a foreman would pick right now: no
//	           status yet, pending/todo/new/ready. Everything else open is
//	           OWNED (in_progress/in-progress/claimed/rework).
//	deferred — rows excluded from open because they carry deferred:true.
//	read     — false when there is no board / it cannot be opened (an evidence
//	           gap, never "empty").
type dryBoardEvidence struct {
	open     int
	pending  int
	deferred int
	read     bool
}

// boardDryEvidence scans the lane's board once. The walk, the fixture registry,
// the open vocabulary and the deferred:true rule are the SAME ones boardOpenRows
// has always applied — this function is that logic with the pending/deferred
// split added, and boardOpenRows delegates here.
func boardDryEvidence(workdir string) dryBoardEvidence {
	if strings.TrimSpace(workdir) == "" {
		return dryBoardEvidence{}
	}
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return dryBoardEvidence{}
	}
	ev, ok := scanBoardDryEvidence(boardPath)
	if !ok {
		return dryBoardEvidence{}
	}
	return ev
}

// dryReasonFromBoardEvidence maps one board read onto a token. The mapping is
// exhaustive on purpose: a dry tick must always name a cause (that is the whole
// point of the row), and each arm is the most specific statement the evidence
// supports.
func dryReasonFromBoardEvidence(ev dryBoardEvidence) string {
	if !ev.read {
		// No board to read: the lanes whose workdir has no board at all, or a
		// board that exists but could not be opened. The no-work gates treat
		// that as fail-open (builderBoardHasWork), so the tick ran — and it
		// dispatched nothing. "Nothing dispatchable was observed" is the honest
		// bucket; claiming a barrier or a withheld gate here would be a verdict
		// about evidence the scheduler never had.
		return DryReasonEmptyBoard
	}
	switch {
	case ev.open == 0 && ev.deferred > 0:
		// The lane's only remaining work is declared deferred: visible to the
		// owner, deliberately not actionable.
		return DryReasonDeferred
	case ev.open == 0:
		return DryReasonEmptyBoard
	case ev.pending == 0:
		// Open work exists, not one row is dispatchable: every row is claimed /
		// in_progress / rework, i.e. owned by another in-flight tick or worker.
		return DryReasonOwnershipBarrier
	default:
		// Dispatchable rows were on the board and this tick still landed
		// nothing without an ownership barrier in the way: the tick's own gates
		// withheld them (owner-gated, dependency-blocked, acceptance pending).
		return DryReasonGateWithheld
	}
}

// resolveDryReason returns the vocabulary token for a COMPLETED tick that landed
// no artifact, or "" when the row is not dry (or is a failure/deferral, whose
// classes are carried elsewhere). It is called from exactly one place —
// LifecycleTracker.Complete — so the token rides the SAME single finalization
// UPDATE as the outcome.
//
// Precedence is DECLARED > wave > no-dispatch > board, i.e. from the strongest
// evidence to the weakest: a caller that knows the cause wins; then the
// scheduler's own artifacts (the wave manifest it will ingest right after this
// write); then whether it dispatched at all; then the board read.
func resolveDryReason(db *sql.DB, o TickOutcome) string {
	if o.Status != TickCompleted {
		return ""
	}
	switch terminalOutcome(o) {
	case "dry_run", string(database.OutcomeNoWork):
		// dry_run = ran, landed nothing. no_work = ran zero tool calls. Both are
		// the "dry" class this vocabulary exists for. A completed tick WITH an
		// artifact is productive and keeps its empty column.
	default:
		return ""
	}
	if DryReasonIsValid(o.DryReason) {
		log.Printf("DRY-REASON: %s tick=%s reason=%s source=declared", o.Project, o.TickID, o.DryReason)
		return o.DryReason
	}
	workdir := projectWorkdirColumn(db, o.Project)
	if waveManifestHasWorkers(workdir, o.TickID) {
		log.Printf("DRY-REASON: %s tick=%s reason=%s source=wave_manifest workdir=%s", o.Project, o.TickID, DryReasonWavePersisted, workdir)
		return DryReasonWavePersisted
	}
	if !o.DispatchDispatched {
		// The scheduler admitted the lane and completed the row without ever
		// invoking a foreman — the SCHED-GAP-1653 reasoned stand-down. The gate
		// is the cause, and it is the scheduler's own decision, so nothing on
		// the board can outrank it.
		log.Printf("DRY-REASON: %s tick=%s reason=%s source=no_dispatch dispatch_reason=%s", o.Project, o.TickID, DryReasonGateWithheld, o.DispatchReason)
		return DryReasonGateWithheld
	}
	ev := boardDryEvidence(workdir)
	reason := dryReasonFromBoardEvidence(ev)
	log.Printf("DRY-REASON: %s tick=%s reason=%s source=board read=%t open=%d pending=%d deferred=%d workdir=%s",
		o.Project, o.TickID, reason, ev.read, ev.open, ev.pending, ev.deferred, workdir)
	return reason
}

// projectWorkdirColumn reads the lane's workdir as the board scan sees it. A
// missing project row (or a NULL workdir) reads as "" — the board scan then
// reports read=false and the resolver classifies from what it knows.
func projectWorkdirColumn(db *sql.DB, project string) string {
	if db == nil || strings.TrimSpace(project) == "" {
		return ""
	}
	var wd string
	if err := db.QueryRow(`SELECT COALESCE(workdir, '') FROM projects WHERE name = ?`, project).Scan(&wd); err != nil {
		return ""
	}
	return strings.TrimSpace(wd)
}

// waveManifestHasWorkers reports whether the tick wrote a wave manifest naming
// at least one worker — the artifact that makes a wave tick's product its
// worker rows rather than a commit of its own. Best-effort: a missing/malformed
// manifest is a serial tick, exactly as ingestWaveManifest treats it.
func waveManifestHasWorkers(workdir, tickID string) bool {
	if strings.TrimSpace(workdir) == "" || strings.TrimSpace(tickID) == "" {
		return false
	}
	m, err := parseWaveManifest(waveManifestPath(workdir, tickID), tickID)
	if err != nil || m == nil {
		return false
	}
	return len(m.Workers) > 0
}

// scanBoardDryEvidence reads one board file and counts open / pending /
// deferred rows. ok=false means the file could not be opened (the evidence gap
// the callers must never read as "empty").
func scanBoardDryEvidence(boardPath string) (dryBoardEvidence, bool) {
	f, err := os.Open(boardPath)
	if err != nil {
		return dryBoardEvidence{}, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var ev dryBoardEvidence
	ev.read = true
	isJSONL := strings.HasSuffix(boardPath, ".jsonl")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !isJSONL {
			// Markdown boards: unchecked headers are open, checked are done.
			// They carry no ownership concept, so every open header is also
			// dispatchable. Fixture rows (registry-declared, NEVER-DONE) are
			// excluded here too.
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) &&
				!registryDeclares(markdownTaskID(line), fixtureIDs) {
				ev.open++
				ev.pending++
			}
			continue
		}
		var row struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Perpetual bool   `json:"perpetual"`
			Deferred  bool   `json:"deferred"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			ev.open++ // malformed row — count as open, never hide work
			ev.pending++
			continue
		}
		if isFixtureRow(row.ID, row.Perpetual) || registryDeclares(row.ID, fixtureIDs) {
			continue // declared/perpetual fixture — not work (SCHED-GAP-106, ADV-R05)
		}
		if row.Deferred {
			ev.deferred++
			continue // deferred:true — visible but not actionable (SCHED-GAP-1727)
		}
		switch strings.ToLower(row.Status) {
		case "claimed", "in_progress", "in-progress", "rework":
			ev.open++ // open, but owned by another in-flight tick/worker
		case "", "pending", "open", "ready", "todo", "new":
			ev.open++
			ev.pending++
		}
	}
	return ev, true
}
