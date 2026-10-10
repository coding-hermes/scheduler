package scheduler

import (
	"log"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1655 — the no-work policy: how the scheduler resolves "the tick
// ran and did nothing".
//
// The fleet's measured waste (~500 genuinely-wasted ticks/week) decomposes
// into two classes this row closes:
//
//   - 261 zero-tool sessions/week: a COOLDOWN-mode builder lane had nothing
//     dispatchable on its board but ticked anyway on its cooldown timer,
//     burning a slot, a cooldown and a session. The tasks_no_work admission
//     reason already exists for tasks-mode lanes (SCHED-GAP-124/155), but
//     cooldown-mode lanes ticked BLIND to the board — nothing consulted it
//     before dispatch. Deliverable (1) extends the existing admission gate
//     to them.
//
//   - 48 board-drained timer ticks recorded as completed: a tick that ran
//     ZERO tool calls was recorded outcome='dry_run' at best — the same
//     verdict a productive-but-artifact-less tick earns — so the waste was
//     invisible in the outcome column. Deliverable (2) records it as
//     outcome='no_work' (migration v55 widened the CHECK).
//
// Deliverable (3) is the reporter-lane exemption: the satellite families
// (-sync/-pm/-qa/-dogfood/-releng/-review/-docs/-readme/-perf) keep their
// TIMER cadence BY DESIGN — their product is a report/DuckBrain key/a filed
// row, not board work, and an empty board is not a fault for them. The
// class is explicit and testable (laneClass), never an inline suffix
// substring, and the namespace reporter_class config (SCHED-GAP-1674)
// overrides it declaratively.
//
// SCHED-GAP-1656 completes the policy on the other admission mode. The
// 1655 gate covers COOLDOWN-mode builders; a TASKS-mode builder lane still
// fell through its drained board to the wall-clock pin and dispatched a
// whole session once the pin elapsed. tasksBuilderAdmissionBlocked closes
// that half: a tasks builder whose OWN board was read and holds zero
// dispatchable rows is deferred with the SAME tasks_no_work reason the
// admission classifier already uses (no vocabulary is added), and is
// admitted by the existing tasks waiver — plus the board-wake flip — the
// moment a dispatchable row lands, so the deferral costs no latency.

// laneClassBuilder and laneClassReporter are the two classes of lane
// (SCHED-GAP-1655 deliverable 3). A BUILDER lane's product is code/board
// work: an empty board means "nothing to do" and ticking is waste. A
// REPORTER lane's product is a periodic report: the timer IS the design,
// and it must keep ticking on an empty board.
const (
	laneClassBuilder  = "builder"
	laneClassReporter = "reporter"
)

// reporterLaneSuffixes are the satellite-lane name suffixes whose product is
// a periodic report rather than board work (SCHED-GAP-1655 deliverable 3).
// Derived from the fleet's satellite families; the class check is
// suffix-based so it needs no project list and no DB read. The namespace
// reporter_class config column (SCHED-GAP-1674) is the declarative override
// on top: a namespace pinned reporter exempts ALL its lanes regardless of
// their names, and one pinned builder (the "" default) keeps name-derived
// reporter lanes governed anyway (the config wins in both directions).
var reporterLaneSuffixes = LaneRoleSuffixes()

// laneClass classifies a lane as builder or reporter (SCHED-GAP-1655
// deliverable 3) — the ONE function every no-work decision consults, so the
// classification is explicit and testable rather than an inline suffix
// check scattered across gates.
//
// reporterConfig is the lane's namespace reporter_class column value
// (” = builder default, "reporter" = exempt; the same column and resolver
// the SCHED-GAP-1674 builder guard reads). A non-empty valid value WINS
// over the name derivation in both directions; an empty value falls back to
// the suffix table.
//
// Unknown namespace states (an empty reporterConfig on a namespace that
// simply predates the column) resolve through the suffix table — the
// default for every existing row.
func laneClass(projectName, reporterConfig string) string {
	switch reporterConfig {
	case "reporter":
		return laneClassReporter
	case "":
		// The default (and the value every row predating the column
		// reads): the name derivation decides.
	default:
		// An unrecognized config value must never widen the gate: the
		// builder guard's resolver treats unknown values as their
		// documented defaults, so an unknown reporter_class here reads
		// as builder (governed).
		return laneClassBuilder
	}
	name := strings.ToLower(strings.TrimSpace(projectName))
	for _, suffix := range reporterLaneSuffixes {
		if strings.HasSuffix(name, suffix) {
			return laneClassReporter
		}
	}
	return laneClassBuilder
}

// laneIsBuilder reports whether the lane is BUILDER class for the no-work
// gate — the whole gate applies only to builder lanes (SCHED-GAP-1655
// deliverable 3: reporter lanes keep their timer cadence BY DESIGN).
func laneIsBuilder(projectName, reporterConfig string) bool {
	return laneClass(projectName, reporterConfig) == laneClassBuilder
}

// effectiveAdmissionModeFor resolves a database.Project's effective
// admission mode against the namespace mode map (SCHED-GAP-124 semantics:
// project override wins, else namespace default, else cooldown) — the
// Project-shaped twin of admissionCandidate.effectiveAdmissionMode, so the
// packers' queued-check and the selection gate classify a project's mode
// identically without re-deriving the fallback chain inline.
func effectiveAdmissionModeFor(p database.Project, nsModes map[string]string) string {
	return admissionModeFor(p.AdmissionMode, nsIDOf(p), nsModes)
}

// builderBoardHasWork resolves whether a BUILDER lane's board holds
// dispatchable work right now (SCHED-GAP-1655 deliverable 1).
//
// Dispatchable is the tasks-mode definition, REUSED not re-parsed: the
// boardOpenRows scanner (adaptive_cooldown.go) — pending/in_progress/claimed
// open vocabulary, perpetual fixtures excluded, malformed rows count open.
// The same board resolution (findBoardFile's .coding-hermes/board walk)
// locates the file, so a satellite's symlinked board reads as what it is.
// No second parser exists after this row.
//
// "Not blocked by an incomplete depends_on" is deliberately NOT narrowed
// further here: the fleet's foreman prompt picks status=="pending" rows and
// the wave planner enforces independence at dispatch; the board-level open
// vocabulary is the same signal every other consumer (pending boost,
// adaptive cooldown, the tasks waiver) acts on. A lane whose only open rows
// are dependency-blocked is upstream's problem to close or unblock, and a
// narrower parser here would be the second parser this row forbids.
//
// Return contract (mirror of tasksAdmissionDue, INCLUDING its fail-open):
//   - true  = dispatchable work exists, OR the board could not be read (no
//     workdir, missing board, unreadable file) → the lane proceeds on its
//     cooldown timer exactly as before. Fail-open is the fleet-wide
//     convention for an unreadable board signal (the GAP-141 ownership
//     gate, the GAP-105/106 scanner and the pending boost all fail open
//     the same way): an evidence gap must never silently starve every
//     builder lane, and the fleet's measured zero-tool waste is DRAINED
//     readable boards, not missing ones — a missing board defers nothing.
//   - false = the board was READ and holds zero dispatchable rows — the
//     proven-drained case, and the only case that defers. This is the
//     measured 261 zero-tool sessions/week.
func builderBoardHasWork(workdir string) bool {
	if strings.TrimSpace(workdir) == "" {
		return true // no workdir: no evidence either way — the timer decides
	}
	open, ok := boardOpenRows(workdir)
	if !ok {
		// Missing/unreadable board: no evidence of "no work" — proceed
		// (fail-open, the fleet-wide convention for an unreadable
		// board signal; a missing board never deferred a tick before
		// this row and must not start deferring one now).
		return true
	}
	return open > 0
}

// builderAdmissionBlocked is the SCHED-GAP-1655 deliverable (1) admission
// gate for ONE candidate: should this pass refuse to dispatch a
// cooldown-mode BUILDER lane because its board holds no dispatchable work?
//
// Gate order mirrors the packer's own consultation order: mode first, then
// class, then the board. Only the conjunction "cooldown-mode AND builder
// AND board-empty" blocks; every other shape is transparent (tasks lanes
// keep the SCHED-GAP-124 waiver, reporter lanes keep their timer).
//
// workdir/boardOwnership are the project row's fields; reporterConfig is
// the namespace reporter_class column (” = builder default). The ownership
// field is NOT consulted here — a cooldown-mode lane does not waive its pin
// on a board it reads, so ownership (a waiver concept, SCHED-GAP-141) has
// no role in this gate; the board is read WHERE THE LANE READS IT, which is
// findBoardFile's standard walk.
func builderAdmissionBlocked(projectName, workdir, admissionMode, reporterConfig string) bool {
	if admissionMode != database.AdmissionModeCooldown {
		return false // tasks mode: the GAP-124 waiver governs, untouched
	}
	if !laneIsBuilder(projectName, reporterConfig) {
		return false // reporter lane: timer cadence BY DESIGN, untouched
	}
	return !builderBoardHasWork(workdir)
}

// noteBuilderNoWorkDeferral logs the grep-stable NO-WORK line for one
// deferred candidate (SCHED-GAP-1655): operators watch "NO-WORK: <lane>"
// the way they watch "LOAD-GATE: <lane>" — the waste this row closes is
// only provably closed if the deferral is visible per tick. Callers reach
// here only when the gate BLOCKED, i.e. the board was read and empty, so
// the log's read is a cheap re-walk of a small file (the same file the
// decision read — one authority for the predicate).
func noteBuilderNoWorkDeferral(projectName, workdir string) {
	open, ok := boardOpenRows(workdir)
	if !ok {
		log.Printf("NO-WORK: deferring %s — board read failed at %s (gate blocked on the pre-log read)", projectName, workdir)
		return
	}
	log.Printf("NO-WORK: deferring %s — board holds 0 dispatchable rows (%d open rows counted)", projectName, open)
}

// tasksBuilderAdmissionBlocked is the SCHED-GAP-1656 admission gate for ONE
// candidate: should this pass refuse to dispatch a TASKS-mode BUILDER lane
// because its own board holds no dispatchable work?
//
// WHY THIS EXISTS. SCHED-GAP-1655 gave the no-work gate to COOLDOWN-mode
// builder lanes (builderAdmissionBlocked). A tasks-mode builder lane had no
// such gate: the SCHED-GAP-124 waiver only fires when work EXISTS, so a
// board with zero dispatchable rows simply fell through the tasks branch to
// the lane's wall-clock pin — and once the pin elapsed the packer dispatched
// the lane anyway, burning a slot, a cooldown and a full LLM session to
// discover (again) that there was nothing to dispatch. That is the tasks
// half of the measured no-op-tick waste the no-work policy closes.
//
// Deferring costs NO latency, which is what makes the gate safe: a tasks lane
// with work is admitted by the SCHED-GAP-124 waiver the moment the row
// exists (no pin to wait out), and a board write additionally forces an
// evaluation through the board-wake watcher (a tasks-mode privilege,
// SCHED-GAP-1695/1727). The lane also parks — SCHED-GAP-1660's parked-empty
// registry records it at the classification step, which now RUNS because the
// lane is deferred rather than admitted — so the flip-back path is armed.
//
// Gate order mirrors builderAdmissionBlocked: mode first, then class, then
// ownership, then the board. Only the conjunction "tasks-mode AND builder
// AND owns its board AND board read-and-empty" blocks; every other shape is
// transparent:
//
//   - COOLDOWN mode is builderAdmissionBlocked's jurisdiction (SCHED-GAP-1655),
//     untouched here — the two gates partition by admission mode.
//   - REPORTER-class lanes (-sync/-pm/-qa/-dogfood/-releng/-review/-docs/
//     -readme/-perf suffixes, or a namespace reporter_class="reporter" pin)
//     keep their timer cadence BY DESIGN — an empty board is not a fault for
//     a lane whose product is a periodic report.
//   - a lane that does NOT own the board it reads resolves through
//     boardOwnedByLane (SCHED-GAP-141): it is a time-based lane and must keep
//     its cooldown, exactly as the tasks waiver refuses it.
//   - a missing/unreadable board or an empty workdir is fail-open
//     (builderBoardHasWork) — an evidence gap must never starve a lane.
func tasksBuilderAdmissionBlocked(projectName, workdir, admissionMode, boardOwnership, reporterConfig string) bool {
	if admissionMode != database.AdmissionModeTasks {
		return false // cooldown mode: SCHED-GAP-1655's gate owns that shape
	}
	if !laneIsBuilder(projectName, reporterConfig) {
		return false // reporter lane: timer cadence BY DESIGN, untouched
	}
	if !boardOwnedByLane(workdir, boardOwnership) {
		return false // foreign/unowned board: time-based lane (SCHED-GAP-141)
	}
	return !builderBoardHasWork(workdir)
}

// noWorkTickLine is the grep-stable completion line for a tick recorded
// outcome=no_work (SCHED-GAP-1655 deliverable 2): lane, tick id and the
// verdict in one line, the same register as the LOAD-GATE / BUILDER-GUARD
// lines. A function (not an inline format string) so the shape contract is
// testable — operators and dashboards grep this prefix.
func noWorkTickLine(project, tickID string) string {
	return "NO-WORK: " + project + " tick=" + tickID + " — completed session ran 0 tool calls (outcome no_work)"
}
