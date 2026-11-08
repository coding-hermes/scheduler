package scheduler

// SCHED-GAP-16981: pick-time BOARD-STALE drift evidence.
//
// Measured 2026-11-03 21:52 in the registered code-factory workdir
// (/home/kara/code-factory, HEAD f26a608, 2 commits behind origin/main
// 9f014411): scripts/board_drift.py reported DRIFT — the checkout's
// tasks.jsonl and events.jsonl both differed from origin/main — while five
// code-factory lanes were running ticks against that same checkout. So a
// board-driven lane can be handed work that is already closed upstream, or
// miss work that was just filed, purely because its checkout's board lags
// origin/main — and the tick record showed nothing about it.
//
// This file closes the logging residual only: when a board-driven
// (tasks-mode) lane is PICKED and its checkout's board bytes differ from
// origin/main's copy, the tick record shows it — ONE grep-stable line per
// picked lane naming the workdir HEAD sha, the origin/main sha, and the ids
// whose status disagrees (comma-separated, bounded with a "+N more" cap).
//
// Scope discipline (the row is a LOGGING row): nothing here changes which
// board a lane picks from, adds a refusal/veto path, or starts enforcing
// canonical boards. The pick decision above this line is untouched; when
// the board bytes MATCH origin/main, no line is emitted.
//
// Cost: the check runs only for lanes actually packed this pass whose
// effective admission mode is tasks (the board-driven lanes), and only
// shells out to git when such a lane has a board (three rev-parse calls +
// one cat-file). Cooldown-paced lanes and boardless lanes never pay it.

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/coding-hermes/scheduler/internal/database"
)

// boardStaleMaxEntries bounds the disagreeing-id list in one BOARD-STALE
// line. A drifted foreman board can carry dozens of flipped rows; the line
// names the first entries in board order and sums the rest as "+N more"
// — the token, both shas and the drift fact stay on one greppable line.
const boardStaleMaxEntries = 8

// logBoardStaleAtPick is the loop-level pick hook (SCHED-GAP-16981). Both
// packer paths have returned and the selection is final, nothing has been
// spawned yet — the same moment the ADMIT pass speaks for. Every packed
// board-driven lane gets one drift check. Best-effort end to end: a check
// can never fail or slow the pick it reports on.
func logBoardStaleAtPick(db *sql.DB, packed []PackedProject) {
	if db == nil || len(packed) == 0 {
		return
	}
	for i := range packed {
		p := packed[i]
		if p.Workdir == "" {
			continue
		}
		// Board-driven lanes only (SCHED-GAP-124 vocabulary): the AC
		// covers lanes that pick from a board. Resolved from the DB the
		// same way the post-tick paths resolve it — durable config, no
		// race at eval granularity.
		if admissionModeForProject(db, p.Name) != database.AdmissionModeTasks {
			continue
		}
		logBoardStaleForLane(p.Name, p.Workdir)
	}
}

// boardStaleSkipState deduplicates the reason-coded skip lines per workdir
// per reason (the ownership-refusal pattern): a checkout whose shape cannot
// support the comparison — not a git repo, no origin/main — logs ONE skip
// instead of one per pick, while BOARD-STALE drift lines themselves stay
// per-pick (each picked tick must show its own drift).
var (
	boardStaleSkipMu   sync.Mutex
	boardStaleSkipSeen = map[string]bool{}
)

// noteBoardStaleSkip logs one reason-coded skip per workdir+reason. Never
// panics, never fails a pick (SCHED-GAP-16981 AC).
func noteBoardStaleSkip(name, workdir, reason string) {
	key := workdir + "\x1f" + reason
	boardStaleSkipMu.Lock()
	prev := boardStaleSkipSeen[key]
	boardStaleSkipSeen[key] = true
	boardStaleSkipMu.Unlock()
	if prev {
		return
	}
	log.Printf("BOARD-STALE-SKIP (SCHED-GAP-16981): %s workdir=%s reason=%s — drift comparison skipped", name, workdir, reason)
}

// boardStatusIndex is an ordered id→status view of one board copy: the ids
// in first-occurrence board order, each mapped to its LAST occurrence's
// status (the fleet board-scan law: a re-filed id's last incarnation is the
// live one). Statuses are normalized the way the freshness reader
// normalizes RawStatus — lowercased and trimmed — so the comparison is
// vocabulary-tolerant ("Complete" == "complete").
type boardStatusIndex struct {
	order  []string
	status map[string]string
}

// jsonlBoardStatusIndex indexes a JSONL board's rows through the shared
// freshness parser (parseBoardRowsBytes) — one reader, no second parser.
func jsonlBoardStatusIndex(data []byte) boardStatusIndex {
	idx := boardStatusIndex{status: make(map[string]string)}
	objs, _, _ := parseBoardRowsBytes(data)
	for _, obj := range objs {
		id := strings.TrimSpace(boardString(obj["id"]))
		if id == "" {
			continue // anonymous rows cannot be named in the line
		}
		if _, seen := idx.status[id]; !seen {
			idx.order = append(idx.order, id)
		}
		idx.status[id] = strings.ToLower(strings.TrimSpace(boardString(obj["status"])))
	}
	return idx
}

// markdownBoardStatusIndex indexes a markdown board (.coding-hermes/
// tasks.md) with the same header conventions readMarkdownBoardFreshness
// reads: "## [ ] ID" is open work, "## [x] ID" is a checked (closed) row.
func markdownBoardStatusIndex(data []byte) boardStatusIndex {
	idx := boardStatusIndex{status: make(map[string]string)}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		var status string
		switch {
		case strings.HasPrefix(line, "## [ ] "):
			status = "open"
		case strings.HasPrefix(line, "## [x] "):
			status = "closed"
		default:
			continue
		}
		id := strings.TrimSpace(markdownTaskID(line))
		if id == "" {
			continue
		}
		if _, seen := idx.status[id]; !seen {
			idx.order = append(idx.order, id)
		}
		idx.status[id] = status
	}
	return idx
}

// boardStaleDiffEntries renders the ids whose status disagrees between the
// workdir copy and the origin/main copy — local order first, then
// origin-only ids (rows published upstream but not present in the lagging
// checkout) — each as "id:workdirStatus->originStatus" ("absent" marks the
// side missing the id). The list is capped at boardStaleMaxEntries with a
// trailing "+N more".
func boardStaleDiffEntries(local, origin boardStatusIndex) []string {
	var out []string
	compared := make(map[string]bool, len(local.order))
	for _, id := range local.order {
		ls := local.status[id]
		os, ok := origin.status[id]
		if !ok {
			os = "absent"
		}
		compared[id] = true
		if os == ls {
			continue // status agrees — bytes may still differ (see caller)
		}
		out = append(out, id+":"+ls+"->"+os)
	}
	for _, id := range origin.order {
		if compared[id] {
			continue
		}
		out = append(out, id+":absent->"+origin.status[id])
	}
	if len(out) > boardStaleMaxEntries {
		more := len(out) - boardStaleMaxEntries
		out = append(out[:boardStaleMaxEntries], "+"+strconv.Itoa(more)+" more")
	}
	return out
}

// logBoardStaleForLane runs the SCHED-GAP-16981 drift comparison for one
// picked board-driven lane and emits the evidence line. Order of facts in
// the BOARD-STALE line: workdir sha, origin/main sha, disagreeing ids.
//
// Fail-safe contract: a checkout that is not a git repo, or one where
// origin/main is unresolvable, logs a single reason-coded skip (deduped)
// and returns — the pick proceeds exactly as before. When the board bytes
// match origin/main, nothing is logged at all.
func logBoardStaleForLane(name, workdir string) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return // no board in this checkout: nothing to compare, nothing to say
	}
	local, ok := resolveWorkdir(workdir)
	if !ok {
		return // unmounted remote target — findBoardFile only resolved via a mount
	}

	headSHA := gitHeadSHA(local)
	if headSHA == "" {
		noteBoardStaleSkip(name, local, "not-a-git-repo")
		return
	}
	originSHA, err := gitOut(local, "rev-parse", "--verify", "origin/main^{commit}")
	if err != nil {
		noteBoardStaleSkip(name, local, "no-origin-main")
		return
	}
	toplevel, err := gitOut(local, "rev-parse", "--show-toplevel")
	if err != nil {
		noteBoardStaleSkip(name, local, "not-a-git-repo")
		return
	}

	// The board path must live INSIDE the checkout's repo for a
	// `git cat-file origin/main:<path>` comparison to be meaningful.
	// Symlinks are resolved on both sides first: satellite lanes link
	// their board dir into another project's workdir, and that board
	// belongs to a different repo — compare nothing, say nothing (one
	// deduped skip), rather than produce a cross-repo nonsense diff.
	boardAbs := boardPath
	if resolved, rerr := filepath.EvalSymlinks(boardPath); rerr == nil {
		boardAbs = resolved
	}
	rel, rerr := filepath.Rel(filepath.Clean(toplevel), filepath.Clean(boardAbs))
	if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		noteBoardStaleSkip(name, local, "board-outside-repo")
		return
	}
	relGitPath := filepath.ToSlash(rel)

	localBytes, err := os.ReadFile(boardPath)
	if err != nil {
		noteBoardStaleSkip(name, local, "board-unreadable")
		return
	}
	// Byte-exact origin copy: cat-file blob prints the raw blob (git show
	// would be equivalent; cat-file cannot apply textconv). A path absent
	// from origin/main — a board never published — is drift by the AC's
	// definition and names every local id as absent-at-origin.
	originBytes := gitBlobBytes(local, "origin/main:"+relGitPath)

	logBoardStaleCompare(name, boardPath, relGitPath, headSHA, originSHA, localBytes, originBytes)
}

// gitBlobBytes fetches one raw blob (untrimmed — byte-exactness is the AC's
// drift test); nil when the spec is absent or git fails, which the byte
// comparison then reports as drift.
func gitBlobBytes(dir, spec string) []byte {
	out, err := exec.Command("git", "-C", dir, "cat-file", "blob", spec).Output()
	if err != nil {
		return nil
	}
	return out
}

// logBoardStaleCompare performs the byte comparison and emits the
// BOARD-STALE line. Split from logBoardStaleForLane so tests can drive the
// comparison directly with in-memory board copies.
func logBoardStaleCompare(name, boardPath, relGitPath, headSHA, originSHA string, localBytes, originBytes []byte) {
	if bytes.Equal(localBytes, originBytes) {
		return // AC: matching bytes → no BOARD-STALE line
	}
	var entries []string
	if strings.HasSuffix(boardPath, ".jsonl") {
		entries = boardStaleDiffEntries(jsonlBoardStatusIndex(localBytes), jsonlBoardStatusIndex(originBytes))
	} else {
		entries = boardStaleDiffEntries(markdownBoardStatusIndex(localBytes), markdownBoardStatusIndex(originBytes))
	}
	disagreeing := strings.Join(entries, ",")
	if len(entries) == 0 {
		// Bytes differ but every named id's status agrees — an
		// ordering/field-level edit. The line must still fire (the AC's
		// trigger is byte drift); say so instead of an empty list.
		disagreeing = "none-status-identical"
	}
	log.Printf("BOARD-STALE (SCHED-GAP-16981): %s workdir_sha=%s origin_main_sha=%s disagreeing=%s board=%s",
		name, headSHA, originSHA, disagreeing, relGitPath)
}
