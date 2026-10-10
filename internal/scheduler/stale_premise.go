package scheduler

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1657 — the stale-premise pick gate.
//
// Measured problem: ~98 zero-commit ticks/week were spent discovering, at
// PICK time, that the row the foreman was about to work had already landed
// — a board row whose status is still "pending" but whose commit_hash is
// already set (the worker/foreman wrote the commit and forgot to flip the
// row). The classic stale row: status pending + commit_hash present. The
// scheduler can detect that shape from the board alone, before dispatching
// a full LLM session to re-discover it.
//
// This gate closes the gap at ADMISSION time: when a BUILDER lane's board
// holds pending rows and EVERY one of them is stale (non-empty commit_hash
// while status is still pending), the packers defer the lane with reason
// stale_premise and name the stale row(s). The lane is not dispatched, no
// cooldown is consumed, and the deferral is queryable in the SCHED-GAP-157
// deferrals table exactly like no_work.
//
// SCOPE — the same conjunction as the SCHED-GAP-1655/1656 no-work gate:
//   - BUILDER-class lanes only (cooldown-mode AND tasks-mode). REPORTER-class
//     lanes keep their timer cadence BY DESIGN (SCHED-GAP-1655 deliverable 3),
//     and a tasks-mode lane that does not own its board (SCHED-GAP-141) is
//     time-based and stays transparent.
//   - the gate sits strictly downstream of the existing no-work gate. A stale
//     pending row IS an open row, so no_work — which fires on an EMPTY board
//     (boardOpenRows == 0) — never sees it; the two reasons partition
//     "nothing to do" into empty-board and all-stale.
//
// THE SIGNAL (cheap, a board read only — no git operations inside the
// daemon): a row is stale when its status is still "pending" AND its
// commit_hash is non-empty. We deliberately do NOT run the git battery
// (board_freshness.go is a library, never called from the live daemon) and
// do NOT content-sniff files_changed — per the row's "keep it SIMPLE and
// cheap" contract the commit_hash signal is the whole check.
//
// LIMITATION (documented per the brief): because reachability is NOT
// checked, a pending row whose commit_hash names a commit that has not
// actually landed in the workdir would still read as stale here. That is
// accepted: the fallback signal the brief names (commit_hash present on a
// still-pending row) is the classic stale shape, and a hash on a pending
// row that never landed is a board-integrity anomaly the foreman fixes on
// its next real tick — not a reason to keep burning sessions on it.

// AdmissionReasonStalePremise is the SCHED-GAP-1657 vocabulary entry: a
// BUILDER lane whose board holds pending rows and every one of them is
// stale (status still pending but commit_hash already present) was
// excluded from selection because the candidate row's premise is already
// satisfied. Carries no cooldown_remaining_s (board-caused, not
// pin-caused) and is distinct from no_work (an EMPTY board): the board
// HAS rows, they are just already done.
const AdmissionReasonStalePremise = "stale_premise"

// boardStalePendingRows scans the lane's board and reports its stale
// pending rows. A row is STALE when its status is still "pending" and its
// commit_hash is non-empty — the "work landed, row never flipped" shape.
// It returns:
//
//   - staleIDs: the ids of every stale pending row, in board order,
//   - pendingCount: the total number of non-fixture, non-deferred pending
//     rows (the dispatchable vocabulary the foreman picks),
//   - ok: false when the board is missing or unreadable (fail-open — an
//     evidence gap never defers a lane).
//
// Fixture rows (ADV-R05) and deferred rows (SCHED-GAP-1727) are excluded
// by boardRowIsFixture, the same layers boardOpenRows applies, so a
// perpetual fixture with a stale-looking shape never counts here. Markdown
// boards carry no commit_hash field by construction, so they return ok
// with zero pending rows (never stale, never deferred by this gate).
func boardStalePendingRows(workdir string) (staleIDs []string, pendingCount int, ok bool) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return nil, 0, false
	}
	if !strings.HasSuffix(boardPath, ".jsonl") {
		return nil, 0, true
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue // malformed line: open by boardOpenRows' law, not stale evidence
		}
		if boardRowIsFixture(obj, fixtureIDs) {
			continue
		}
		if strings.ToLower(strings.TrimSpace(boardString(obj["status"]))) != "pending" {
			continue
		}
		pendingCount++
		if strings.TrimSpace(boardString(obj["commit_hash"])) != "" {
			staleIDs = append(staleIDs, boardRowID(obj))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, false
	}
	return staleIDs, pendingCount, true
}

// stalePremiseBlockedQuiet is the gate decision for ONE candidate, without
// the skip log: has this BUILDER lane's board been read and found to hold
// pending rows that are ALL stale (premise already met)?
//
// blocked is false — spawn — unless every leg of the conjunction holds: the
// lane is builder-class, its admission mode is one the gate governs, and
// (for tasks mode) it owns the board it reads, AND the board was read with
// at least one pending row, all of which are stale. rowID is the joined
// stale row ids (comma-separated) for the deferral record; "" whenever
// blocked is false.
func stalePremiseBlockedQuiet(projectName, workdir, admissionMode, boardOwnership, reporterConfig string) (blocked bool, rowID string) {
	if !laneIsBuilder(projectName, reporterConfig) {
		return false, "" // reporter lane: timer cadence BY DESIGN (SCHED-GAP-1655 d3)
	}
	switch admissionMode {
	case database.AdmissionModeCooldown:
		// cooldown-mode builder: the board gate applies; ownership is a
		// waiver concept (SCHED-GAP-141), so it has no role here.
	case database.AdmissionModeTasks:
		if !boardOwnedByLane(workdir, boardOwnership) {
			return false, "" // foreign/unowned board: time-based lane (SCHED-GAP-141)
		}
	default:
		return false, "" // unknown mode: never widen the gate
	}
	staleIDs, pendingCount, ok := boardStalePendingRows(workdir)
	if !ok || pendingCount == 0 {
		return false, "" // unreadable board or no pending rows: fail-open / not this shape
	}
	if len(staleIDs) != pendingCount {
		return false, "" // at least one non-stale pending row: real dispatchable work
	}
	return true, strings.Join(staleIDs, ",")
}

// stalePremiseBlocks is the packer-side noting wrapper: the gate's own
// verdict plus the grep-stable skip log. Called from the packers'
// selection sites (one log per skip). The queued/borrowing mirrors and the
// admission classifier use stalePremiseBlockedQuiet so the skip is counted
// exactly once per pass.
func stalePremiseBlocks(projectName, workdir, admissionMode, boardOwnership, reporterConfig string) (blocked bool, rowID string) {
	blocked, rowID = stalePremiseBlockedQuiet(projectName, workdir, admissionMode, boardOwnership, reporterConfig)
	if blocked {
		noteStalePremiseDeferral(projectName, rowID)
	}
	return blocked, rowID
}

// noteStalePremiseDeferral logs the grep-stable STALE-PREMISE line for one
// deferred candidate (SCHED-GAP-1657): operators watch "STALE-PREMISE:
// <lane>" the way they watch NO-WORK / LOAD-GATE. rowID names the stale
// row(s) whose premise is already met — the blocker the dashboard/deferrals
// output must surface.
func noteStalePremiseDeferral(projectName, rowID string) {
	log.Printf("STALE-PREMISE: deferring %s — pending row(s) already satisfied (commit_hash present on %s; SCHED-GAP-1657)", projectName, rowID)
}
