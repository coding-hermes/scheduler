package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

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
// THE SIGNAL (cheap — one bounded subprocess per stale-shaped row): a row
// is stale when its status is still "pending", its commit_hash is
// non-empty, AND that hash names a commit object in the lane's workdir
// repo — the criterion "the declared artifact is ALREADY PRESENT IN THE
// WORKDIR", verified with a single bounded `git rev-parse --verify
// <hash>^{commit}` (see commitPresentInWorkdir). Exit 0 means the commit
// landed; ANY error — a missing repo, a fabricated/never-landed hash, a
// hung git — means NOT stale (fail-open: a wrong deferral skips real work
// and costs more than a spent tick). We deliberately do NOT run the full
// git battery (board_freshness.go is a library, never called from the live
// daemon) and do NOT content-sniff files_changed — per the row's "keep it
// SIMPLE and cheap" contract the commit_hash reachability is the whole
// check.
//
// LIMITATION (documented): the probe verifies object existence, not
// full reachability-from-HEAD — a hash that names an orphaned object
// (present in the object DB but not an ancestor of HEAD) reads as stale.
// That is accepted: the false-positive class this gate must fix is the
// fabricated/never-landed hash (object absent entirely), and an orphaned
// object still proves the artifact was created in this workdir at some
// point. Free-text commit_hash values ("out-of-band (daemon)") fail the
// probe and read as NOT stale — the safe side.

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
// commit_hash is non-empty AND resolves to a commit object in the lane's
// workdir repo — the "work landed, row never flipped" shape, now
// git-verified (SCHED-GAP-1657 rework: a fabricated/never-landed hash must
// NOT read as stale). It returns:
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
		hash := strings.TrimSpace(boardString(obj["commit_hash"]))
		if hash != "" && commitPresentInWorkdir(workdir, hash) {
			staleIDs = append(staleIDs, boardRowID(obj))
		}
		// else: empty hash (fresh work) or a hash that does not resolve in
		// the workdir (fabricated/never-landed, or a non-git workdir) — NOT
		// stale. Fail-open: a wrong deferral skips real work.
	}
	if err := sc.Err(); err != nil {
		return nil, 0, false
	}
	return staleIDs, pendingCount, true
}

// stalePremiseGitTimeout bounds one reachability probe so a hung git can
// never stall the picker (SCHED-GAP-1657 rework): the probe must be cheap
// and must fail-open — on timeout or any other error the row reads as NOT
// stale and the lane dispatches.
const stalePremiseGitTimeout = 2 * time.Second

// commitPresentInWorkdir reports whether hash names a commit object in the
// git repo rooted at workdir — the premise check "the declared artifact is
// already present in the workdir". The probe is exactly
// `git rev-parse --verify <hash>^{commit}` (object existence; the cheapest
// correct check per the brief), run with a bounded context so a hung git
// can never block the picker. It is fail-open: a missing repo, a
// fabricated/never-landed hash, or a timeout all return false. The hash is
// passed as an argv element (never shell-interpolated) — it is hex by
// construction, and even an attacker-shaped value stays inert to git.
func commitPresentInWorkdir(workdir, hash string) bool {
	if strings.TrimSpace(workdir) == "" || strings.TrimSpace(hash) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), stalePremiseGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--verify", hash+"^{commit}")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
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
