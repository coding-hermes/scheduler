package scheduler

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1658 — the blocked-head pick gate (dependency_blocked and
// sibling_held), the third member of the pick-time premise gate family
// after the SCHED-GAP-1655/1656 no-work gate and the SCHED-GAP-1657
// stale-premise gate.
//
// Measured problem: a tick can be spent discovering, at PICK time, that it
// cannot proceed — the board shows pending rows, but every one of them is
// a head the foreman cannot actually take:
//
//   - DEPENDENCY-BLOCKED: the row's depends_on references at least one row
//     that is NOT complete (status != "complete"/"complete-vocabulary").
//     The wave planner skips such a row, so dispatching a session to a
//     board whose every pending row is blocked is a zero-work tick.
//   - SIBLING-HELD: the row is already being worked by a sibling lane.
//     The foreman's pick step skips it, with the same zero-work result.
//
// The gate sits in the same fail-open style as its siblings: a BUILDER
// lane whose board was read OK, holds pending rows, and EVERY one of them
// is dependency-blocked or sibling-held is deferred instead of dispatched;
// any one free pending row is dispatchable work and the lane proceeds.
// Unreadable board, missing workdir, malformed rows: dispatch — a wrong
// deferral skips real work and costs more than a spent tick.
//
// SCOPE — the same conjunction as stalePremiseBlockedQuiet (SCHED-GAP-1657):
// builder lanes only (laneIsBuilder); admission mode gated identically
// (cooldown always; tasks only when the lane owns its board,
// boardOwnedByLane — SCHED-GAP-141). Reporter lanes keep their timer
// cadence BY DESIGN.
//
// COMPOSITION with the stale-premise gate: a pending row may be BOTH stale
// (commit_hash present and landed) and blocked/held. Every call site
// consults stalePremise* FIRST and this gate second, so the stale reason
// keeps priority and a pass never double-defers the same lane. (Within
// this gate the two new reasons are disjoint candidates: a row the scan
// counted sibling-held was not also counted dependency-blocked.)
//
// THE SIGNAL (cheap — one pass over the board file, no subprocesses):
// premiseBoardShapeOf reuses the SCHED-GAP-1657 scan skeleton — the same
// findBoardFile resolution, the same fixture/deferred exclusions — and
// collects each pending row in that single pass; the classification runs
// AFTER the scan against the fully-built complete-id set, so a depends_on
// edge pointing at a complete row LATER in the file reads free, not
// blocked. No git, no exec, nothing beyond reads of the board the lane
// already owns.

// AdmissionReasonDependencyBlocked is the SCHED-GAP-1658 vocabulary entry:
// a BUILDER lane whose board holds pending rows and every one of them is
// blocked on an incomplete depends_on was excluded from selection because
// no candidate row can be taken. Carries no cooldown_remaining_s
// (board-caused, not pin-caused) and is distinct from no_work (an EMPTY
// board): the board HAS rows, the foreman just cannot take any of them.
const AdmissionReasonDependencyBlocked = "dependency_blocked"

// AdmissionReasonSiblingHeld is the SCHED-GAP-1658 vocabulary entry: a
// BUILDER lane whose board holds pending rows and every one of them is
// already claimed/in_progress by a sibling was excluded from selection.
// Same contract as dependency_blocked: board-caused, no cooldown remaining.
const AdmissionReasonSiblingHeld = "sibling_held"

// premiseBoardShape is the one-pass scan's verdict on a board: which
// pending rows are dependency-blocked, which are sibling-held, and how
// many pending rows exist in total (the dispatchable vocabulary the
// foreman picks from).
type premiseBoardShape struct {
	blockedIDs   []string // pending rows whose depends_on names a non-complete row
	heldIDs      []string // pending rows explicitly claimed/in_progress by a sibling
	pendingCount int      // every pending candidate (blocked + held + free)
}

// premiseCompleteStatuses is the status vocabulary that satisfies a
// depends_on edge (SCHED-GAP-1658). Named per the row's contract, exactly:
// "complete" (the fleet boards' only completion spelling — the live board
// census shows statuses are complete/pending and nothing else) and
// "complete-vocabulary" (the boardctl contract spelling). This is
// deliberately NOT board_freshness.go's completeStatuses legacy-spelling
// set (completed/done/closed): a dependency edge is satisfied by an
// EXPLICIT complete, and a rarely-spelled legacy value on a dep row must
// read as NOT complete here — the gate's deferral line names the blocker
// row, so the failure mode is a visible name, not a silent skip.
var premiseCompleteStatuses = map[string]bool{
	"complete":            true,
	"complete-vocabulary": true,
}

// siblingHeldWorkerStatuses is the worker_status vocabulary read as
// "another lane is working this row right now". These are the EXPLICIT
// sibling-claim spellings the wave planner / boardctl write when a lane
// takes a row.
var siblingHeldWorkerStatuses = map[string]bool{
	"claimed":     true,
	"in_progress": true,
	"in-progress": true,
}

// SIBLING-HELD DETECTION — exactly what this gate looks at (the brief's
// documentation contract): a pending row is sibling-held ONLY when the row
// carries an explicit worker_status field whose value is claimed /
// in_progress / in-progress (case-insensitive). That is the explicit
// sibling pattern — the board itself names the row as taken — so no
// second signal is inferred. A row whose worker_status is absent (the
// legacy board shape), "pending", "completed", or any other value is NOT
// sibling-held: it stays ordinary open work and can never sibling-block a
// lane (fail-open — a wrong deferral skips real work). Note the status vs
// worker_status split: a row whose STATUS is in_progress is not a pending
// row at all in this gate (it is open work, outside the pending
// vocabulary this gate reasons over); only status=="pending" rows with a
// claimed/in_progress worker_status field count as held. A claimed_by-style
// field does not exist on fleet boards today; if one lands, extend the
// resolution here — do not infer sibling-ness from status alone.

// premiseBoardShapeOf scans the lane's board and returns its shape. The
// scan mirrors boardStalePendingRows' skeleton exactly (SCHED-GAP-1657):
// findBoardFile resolution, JSONL boards only (markdown carries no
// depends_on/worker_status evidence by construction → ok=true, zero
// rows), fixture exclusion (boardRowIsFixture + loadFixtureRegistry,
// ADV-R05), deferred-row exclusion (SCHED-GAP-1727: a deferred row is
// visible but not actionable, so it is neither a candidate nor evidence),
// and a 1 MiB line cap.
//
// The scan collects pending candidates (id, depends_on, worker_status) and
// every complete row id; the classification runs after the loop against
// the FINAL complete set, so row order on the board never changes a
// verdict. A candidate is DEPENDENCY-BLOCKED when it has a non-empty,
// parseable depends_on and at least one referenced id is not complete
// (including references to rows that do not exist on the board at all).
// Malformed depends_on values (present but not a JSON string array) are
// treated as NO deps — fail-open per row; a wrong block skips real work.
//
// ok is false when the board is missing or unreadable (fail-open — an
// evidence gap never defers a lane).
func premiseBoardShapeOf(workdir string) (premiseBoardShape, bool) {
	var shape premiseBoardShape
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return shape, false
	}
	if !strings.HasSuffix(boardPath, ".jsonl") {
		return shape, true
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return shape, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)

	type candidate struct {
		id   string
		deps []string
		held bool
	}
	var candidates []candidate
	completeIDs := make(map[string]bool)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue // malformed line: open by boardOpenRows' law, not evidence
		}
		if boardRowIsFixture(obj, fixtureIDs) {
			continue
		}
		var deferred bool
		if raw, ok := obj["deferred"]; ok {
			_ = json.Unmarshal(raw, &deferred)
		}
		if deferred {
			continue // deferred:true — visible but not actionable (SCHED-GAP-1727)
		}
		status := strings.ToLower(strings.TrimSpace(boardString(obj["status"])))
		if premiseCompleteStatuses[status] {
			if id := boardRowID(obj); id != "<unknown>" {
				completeIDs[id] = true
			}
			continue
		}
		if status != "pending" {
			continue // this gate reasons over the pending vocabulary only
		}
		shape.pendingCount++
		id := boardRowID(obj)
		// Sibling-held: the EXPLICIT worker_status claim marker (see the
		// detection comment above). Absent/other values leave the row
		// ordinary open work.
		ws := strings.ToLower(strings.TrimSpace(boardString(obj["worker_status"])))
		if siblingHeldWorkerStatuses[ws] {
			candidates = append(candidates, candidate{id: id, held: true})
			continue // counted held; not also a dependency candidate
		}
		var deps []string
		if raw, ok := obj["depends_on"]; ok {
			if err := json.Unmarshal(raw, &deps); err != nil {
				deps = nil // malformed deps: not usable as evidence — fail-open
			}
		}
		candidates = append(candidates, candidate{id: id, deps: deps})
	}
	if err := sc.Err(); err != nil {
		return premiseBoardShape{}, false
	}

	// Classification pass: against the COMPLETE id set (all rows seen),
	// so a dep pointing at a complete row later in the file reads free.
	shape.blockedIDs = make([]string, 0, len(candidates))
	shape.heldIDs = make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.held {
			shape.heldIDs = append(shape.heldIDs, c.id)
			continue
		}
		blocked := false
		for _, dep := range c.deps {
			if strings.TrimSpace(dep) == "" {
				continue
			}
			if !completeIDs[dep] {
				blocked = true
				break
			}
		}
		if blocked {
			shape.blockedIDs = append(shape.blockedIDs, c.id)
		}
		// else: no deps (or only empty-string entries) or every dep
		// complete — a FREE pending row, real dispatchable work.
	}
	return shape, true
}

// premiseBlockedQuiet is the gate decision for ONE candidate, without the
// skip log: has this BUILDER lane's board been read and found to hold
// pending rows that are ALL dependency-blocked or sibling-held?
//
// blocked is false — spawn — unless every leg of the conjunction holds:
// the lane is builder-class, its admission mode is one the gate governs,
// and (for tasks mode) it owns the board it reads, AND the board was read
// with at least one pending row, NONE of which is free (every one is
// dependency-blocked or sibling-held). reason is the winning deferral
// vocabulary entry ("dependency_blocked" wins ties and majorities — the
// structural cause; "sibling_held" when held rows dominate); detail names
// the blocked/held row ids (comma-separated, board order) for the
// deferral record. Both are "" whenever blocked is false.
func premiseBlockedQuiet(projectName, workdir, admissionMode, boardOwnership, reporterConfig string) (blocked bool, reason, detail string) {
	if !laneIsBuilder(projectName, reporterConfig) {
		return false, "", "" // reporter lane: timer cadence BY DESIGN (SCHED-GAP-1655 d3)
	}
	switch admissionMode {
	case database.AdmissionModeCooldown:
		// cooldown-mode builder: the board gate applies; ownership is a
		// waiver concept (SCHED-GAP-141), so it has no role here.
	case database.AdmissionModeTasks:
		if !boardOwnedByLane(workdir, boardOwnership) {
			return false, "", "" // foreign/unowned board: time-based lane (SCHED-GAP-141)
		}
	default:
		return false, "", "" // unknown mode: never widen the gate
	}
	shape, ok := premiseBoardShapeOf(workdir)
	if !ok || shape.pendingCount == 0 {
		return false, "", "" // unreadable board or no pending rows: fail-open / not this shape
	}
	unfree := len(shape.blockedIDs) + len(shape.heldIDs)
	if unfree != shape.pendingCount {
		return false, "", "" // at least one free pending row: real dispatchable work
	}
	ids := make([]string, 0, unfree)
	ids = append(ids, shape.blockedIDs...)
	ids = append(ids, shape.heldIDs...)
	if len(shape.blockedIDs) >= len(shape.heldIDs) {
		return true, AdmissionReasonDependencyBlocked, strings.Join(ids, ",")
	}
	return true, AdmissionReasonSiblingHeld, strings.Join(ids, ",")
}

// premiseBlocks is the packer-side noting wrapper, the exact twin of
// stalePremiseBlocks (SCHED-GAP-1657): the gate's own verdict plus the
// grep-stable skip log, one log per skip. The queued/borrowing mirrors and
// the admission classifier use premiseBlockedQuiet so the skip is counted
// exactly once per pass.
func premiseBlocks(projectName, workdir, admissionMode, boardOwnership, reporterConfig string) (blocked bool, reason string) {
	blocked, reason, _ = premiseBlockedQuiet(projectName, workdir, admissionMode, boardOwnership, reporterConfig)
	if blocked {
		if reason == AdmissionReasonSiblingHeld {
			noteSiblingHeldDeferral(projectName)
		} else {
			noteDependencyBlockedDeferral(projectName)
		}
	}
	return blocked, reason
}

// noteDependencyBlockedDeferral logs the grep-stable DEPENDENCY-BLOCKED
// line for one deferred candidate (SCHED-GAP-1658): operators watch
// "DEPENDENCY-BLOCKED: <lane>" the way they watch STALE-PREMISE /
// NO-WORK / LOAD-GATE. The deferral names the cause — every pending row's
// depends_on edges cannot be satisfied yet.
func noteDependencyBlockedDeferral(projectName string) {
	log.Printf("DEPENDENCY-BLOCKED: deferring %s — pending row(s) blocked on incomplete depends_on (SCHED-GAP-1658)", projectName)
}

// noteSiblingHeldDeferral logs the grep-stable SIBLING-HELD line for one
// deferred candidate (SCHED-GAP-1658): every pending row on the board is
// explicitly claimed/in_progress by a sibling lane.
func noteSiblingHeldDeferral(projectName string) {
	log.Printf("SIBLING-HELD: deferring %s — pending row(s) already claimed/in_progress by a sibling (SCHED-GAP-1658)", projectName)
}
