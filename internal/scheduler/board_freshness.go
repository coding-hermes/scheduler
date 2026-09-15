package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Git-verified board freshness reader (ADV-R06, G11).
//
// Board rows self-report state that the A4 catalog proved unreliable:
// 95.7% of completion stamps are synthetic .000000 values, updated_at does
// not move on completion, 3 of 5 wave rows were stamped into the future at
// read time, 18 orphaned + 2 nonexistent commit hashes, 34 pointer-less
// closures, and the implementation→board flip lags 10–55 min (median 4.4
// over 116 completions). This reader answers, for every row and for the
// board as a whole, the two questions a board-driven wake (ADV-R07) and
// any future consumer need:
//
//   - does this row's CLAIMED work actually exist in the commit graph, and
//   - when did it really land (the pointed commit's committer date)?
//
// The six A4 §5 requirements, as implemented here:
//
//  1. Git-first: a row's claimed work is checked against the commit graph
//     before its status is trusted. A row whose work demonstrably resolves
//     in currently-reachable history is never classified as work-to-spawn —
//     including a row whose status has not flipped yet (flip-window /
//     flip-overdue below).
//  2. Freshness is the pointed commit's committer date. completed_at and
//     updated_at are parsed only to detect future-dated stamps (req 4);
//     they are never a freshness source.
//  3. Resolvable evidence or park: status complete + hash resolving in
//     reachable history + attribution ⇒ closed. Orphan / absent /
//     malformed-pointer / mis-attributed / unattributed ⇒
//     claimed-closed-unverified: never work-to-spawn, never idle-proof.
//  4. A parseable completed_at in the future of the read clock distrusts
//     the row immediately (reason future-stamp), regardless of git state.
//  5. Bounded flip window: a row whose status is still open while its work
//     demonstrably landed is flip-window, not work-to-spawn; re-read after
//     Freshness+FlipWindow. With a resolving pointer past the window it
//     becomes flip-overdue — still never work-to-spawn (the work exists),
//     but flagged as a board-integrity anomaly. Without a pointer, the
//     window-bounded recent-commit scan is the grace period, not an
//     amnesty: past the window the row returns to open.
//  6. The timestamp dialect zoo is tolerated: naive stamps read as UTC,
//     Z-suffixed ISO, space separators, numeric offsets, microseconds,
//     .000000 synthetics, empty/null.
//
// This is a LIBRARY only (ADV-R06): nothing in the live daemon calls it —
// not evaluate(), the packers, spawn.go, CountPending, or any
// admission/ordering path. ADV-R07 wires it.

// DefaultFlipWindow is the default bounded re-read interval for rows whose
// implementation has landed but whose board row has not flipped yet. The
// A4 catalog observed a 10–55 min implementation→board lag (median 4.4
// min over 116 completions); the upper bound is the safe interval: inside
// it a not-yet-flipped row is assumed mid-flip, not stale.
const DefaultFlipWindow = 55 * time.Minute

// RowStatus is the git-verified classification of one board row. Rows that
// cannot be verified are their own class — they are never rounded into
// either the work-to-spawn or the idle answer.
type RowStatus string

const (
	// RowClosed: status claims completion AND the pointed commit resolves
	// in currently-reachable history AND the commit names this row. The
	// only verdict that may support an idle claim.
	RowClosed RowStatus = "closed"
	// RowUnverified: status claims completion but the claim does not
	// verify (see the Reason* constants). Never work-to-spawn, never
	// idle-proof — the AC's "claimed-closed, unverified" class.
	RowUnverified RowStatus = "claimed-closed-unverified"
	// RowOpen: visible, unclaimed work. The only verdict that raises
	// WorkToSpawn.
	RowOpen RowStatus = "open"
	// RowFlipWindow: status still open but the work demonstrably landed
	// within one flip window (resolving attributed pointer, or a commit
	// inside the window names the row). Not work-to-spawn; re-read after
	// Freshness+FlipWindow.
	RowFlipWindow RowStatus = "flip-window"
	// RowFlipOverdue: status still open, pointer resolves and is
	// attributed, but the commit is older than one flip window — the flip
	// never happened. A board-integrity anomaly, but never work-to-spawn:
	// the work demonstrably exists.
	RowFlipOverdue RowStatus = "flip-overdue"
	// RowFixture: declared/perpetual fixture (ADV-R05 layers). Excluded
	// from every aggregate — a fixture is never work and never idle
	// evidence.
	RowFixture RowStatus = "fixture"
	// RowBlocked: user-gated (status=blocked with a blocked_reason). Not
	// work-to-spawn, but not idle-proof either — the work exists, it is
	// deferred, not done.
	RowBlocked RowStatus = "blocked"
	// RowOther: a status in no known vocabulary (e.g. wontfix). Neither
	// work nor idle evidence; visible in Rows for the consumer to decide.
	RowOther RowStatus = "other"
)

// Reason values carried by RowVerdict.Reason when Status == RowUnverified.
const (
	// ReasonFutureStamp: completed_at parses to a time after the read
	// clock (requirement 4 — immediate distrust, checked before any git
	// work).
	ReasonFutureStamp = "future-stamp"
	// ReasonNoPointer: commit_hash is absent, null, or empty.
	ReasonNoPointer = "no-pointer"
	// ReasonMalformedPointer: commit_hash is text that cannot name a
	// commit (e.g. "out-of-band (daemon)").
	ReasonMalformedPointer = "malformed-pointer"
	// ReasonAbsentHash: hash-shaped pointer that resolves to no object in
	// the repo (the A4 catalog's nonexistent-hash class).
	ReasonAbsentHash = "absent-hash"
	// ReasonOrphanHash: object exists but is not reachable from HEAD (the
	// A4 catalog's orphaned-hash class).
	ReasonOrphanHash = "orphan-hash"
	// ReasonMisAttributed: the commit resolves and is reachable but names
	// a DIFFERENT board row's id — the off-by-one pointer class.
	ReasonMisAttributed = "mis-attributed"
	// ReasonUnattributed: the commit names no board row id at all.
	ReasonUnattributed = "unattributed"
	// ReasonRepoUnreadable: the repo has no readable HEAD; nothing could
	// be verified at all.
	ReasonRepoUnreadable = "repo-unreadable"
)

// completeStatuses is the completion vocabulary this reader accepts
// (the live board mixes complete/completed/done).
var completeStatuses = map[string]bool{
	"complete": true, "completed": true, "done": true, "closed": true,
}

// openStatuses mirrors the open vocabulary of boardOpenRows
// (adaptive_cooldown.go): these statuses mean visible work. The empty
// status counts as open — never hide work behind a missing field.
var openStatuses = map[string]bool{
	"": true, "pending": true, "open": true, "in_progress": true,
	"in-progress": true, "claimed": true, "ready": true, "todo": true,
	"new": true, "rework": true,
}

// blockedStatusLiteral is the user-gated status (see board_awareness.go
// GAP-036 (d): blocked rows stay blocked until an operator unblocks).
const blockedStatusLiteral = "blocked"

// commitHashShape matches bare hexadecimal commit ids of 7..40 digits —
// the only pointer forms that can name a commit. Free-text pointers
// ("out-of-band (systemd restart ...)") fail this shape check and park as
// malformed-pointer rather than being probed against git.
var commitHashShape = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// boardStampLayouts are the timestamp dialects seen across fleet boards
// (requirement 6): naive stamps (read as UTC — time.Parse defaults an
// absent zone to UTC), Z-suffixed ISO, space separators, numeric offsets,
// microsecond fractions including the synthetic .000000 zeros, date-only.
var boardStampLayouts = []string{
	time.RFC3339Nano,                // 2026-09-08T07:30:00Z / +02:00, optional fraction
	"2006-01-02T15:04:05.999999999", // naive ISO with fraction (UTC)
	"2006-01-02T15:04:05",           // naive ISO (UTC)
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999", // space separator + synthetic .000000 (UTC)
	"2006-01-02 15:04:05Z07:00",     // space separator with offset
	"2006-01-02 15:04:05",           // space separator (UTC)
	"2006-01-02",                    // date only
}

// parseBoardStamp parses one board timestamp in any of the fleet's
// dialects. Naive stamps are read as UTC. ok is false for empty,
// null-equivalent, and unparseable values.
func parseBoardStamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range boardStampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// RowVerdict is the git-verified verdict for one board row.
type RowVerdict struct {
	// ID is the row id ("" for rows that parse but carry none).
	ID string
	// Status is the verified classification.
	Status RowStatus
	// Reason names the distrust cause when Status == RowUnverified
	// (one of the Reason* constants); "" otherwise.
	Reason string
	// Commit is the FULL sha of the pointed commit when one resolved
	// (set on closed, flip-window and flip-overdue verdicts, and kept on
	// parked verdicts whose hash resolved — evidence, not approval).
	Commit string
	// Freshness is the pointed commit's COMMITTER date (requirement 2) —
	// set whenever a commit resolved, never derived from completed_at or
	// updated_at. Zero when nothing resolved.
	Freshness time.Time
	// StampTime is the dialect-parsed completed_at, kept for diagnostics
	// (future-stamp rows carry it); zero when absent or unparseable.
	StampTime time.Time
}

// FreshnessOptions tunes ReadBoardFreshness. The zero value is usable:
// FlipWindow defaults to DefaultFlipWindow and Now to time.Now(). Tests
// inject both for determinism.
type FreshnessOptions struct {
	// FlipWindow is the bounded re-read interval (requirement 5). <= 0
	// means DefaultFlipWindow.
	FlipWindow time.Duration
	// Now is the read clock for future-stamp detection and flip-window
	// arithmetic. Zero means time.Now().
	Now time.Time
}

// FreshnessReport is the aggregate answer for one board read.
//
// WorkToSpawn is true iff at least one row is classified open (visible,
// unclaimed work) OR a board line was malformed / the read was truncated
// (unknown counts as work-visible — never hide work, the same law as
// boardOpenRows counting malformed lines as open). It is NEVER raised by
// a row whose claimed work resolves in git: closed, unverified, blocked,
// flip-window and flip-overdue rows are all "not work-to-spawn".
//
// VerifiablyIdle is true iff the board was fully read, the repo HEAD was
// readable, there is at least one row, and EVERY row is a verified
// closure or a fixture. Unverified closures, flip-window/flip-overdue,
// blocked, other, malformed lines and partial reads all break the idle
// claim — idle is never inferred from unverified closures (the AC).
//
// Both answers are fail-safe: whenever the reader could not verify
// (unreadable board, unreadable repo), CanVerify is false and neither
// claim is made beyond what the row statuses visibly support.
type FreshnessReport struct {
	// Rows holds one verdict per deduplicated row, in board order.
	Rows []RowVerdict
	// Counts tallies Rows by status.
	Counts map[RowStatus]int
	// TotalRows is len(Rows): the deduplicated row count.
	TotalRows int
	// MalformedLines counts JSONL lines that failed to parse (skipped,
	// never fatal) plus one for a truncated/failed board read.
	MalformedLines int
	// CanVerify is true iff the board was fully read AND the repo HEAD
	// was readable. When false, closure claims could not be verified.
	CanVerify bool
	// WorkToSpawn answers "is there work to spawn?" (see above).
	WorkToSpawn bool
	// VerifiablyIdle answers "is this board verifiably idle?" (see
	// above).
	VerifiablyIdle bool
	// LastVerifiedLanding is the newest committer date among CLOSED rows
	// — the honest "when did real work last land" (requirement 2). Zero
	// when nothing verified.
	LastVerifiedLanding time.Time
	// RereadAfter is the earliest time every flip-window row's window has
	// expired (max over rows of Freshness+FlipWindow); re-read then
	// (requirement 5). Zero when no row is mid-flip.
	RereadAfter time.Time
	// FlipWindow is the window in effect for this read.
	FlipWindow time.Duration
	// ReadAt is the read clock (options.Now or time.Now()).
	ReadAt time.Time
}

// recentCommit is one entry of the window-bounded recent-commit scan used
// for open rows without a pointer (the mid-flip case: the worker's commit
// landed, the foreman has not flipped the row yet, so the row carries no
// commit_hash at all).
type recentCommit struct {
	sha     string
	when    time.Time
	message string
}

// freshnessEntry is one deduplicated parsed board row.
type freshnessEntry struct {
	id      string
	obj     map[string]json.RawMessage
	fixture bool
}

// ReadBoardFreshness reads the board at boardPath and verifies every row's
// claimed work against the commit graph of the git repo at repoDir.
//
//	board.jsonl: {"id":"T-1","status":"complete","commit_hash":"089daee",...}
//	rep := ReadBoardFreshness(repoDir, boardPath, FreshnessOptions{})
//	if rep.WorkToSpawn { /* visible unclaimed work */ }
//	if rep.VerifiablyIdle { /* every row a verified closure or fixture */ }
//
// A missing/unreadable board yields a zero report (CanVerify=false, no
// rows, neither claim). Markdown boards (.md, no .jsonl suffix) have no
// evidence fields, so their closures are unverifiable by construction:
// open headers raise WorkToSpawn, checked headers park as unverified
// (no-pointer), and VerifiablyIdle is always false for them.
func ReadBoardFreshness(repoDir, boardPath string, opts FreshnessOptions) FreshnessReport {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	window := opts.FlipWindow
	if window <= 0 {
		window = DefaultFlipWindow
	}
	rep := FreshnessReport{
		Counts:     make(map[RowStatus]int),
		FlipWindow: window,
		ReadAt:     now,
	}

	if !strings.HasSuffix(boardPath, ".jsonl") {
		readMarkdownBoardFreshness(&rep, boardPath)
		return rep
	}
	if _, err := os.Stat(boardPath); err != nil {
		// No board at path: nothing visible, nothing provable.
		return rep
	}

	objs, malformed, scanErr := parseBoardRowsFreshness(boardPath)
	rep.MalformedLines = malformed
	if scanErr != nil {
		// Truncated/failed read: what we parsed may be partial — never
		// idle-proof, and count the failure as work-visible.
		rep.MalformedLines++
	}

	// Deduplicate by id, keeping the LAST occurrence: re-filed ids have
	// their last incarnation live (the fleet board-scan law). Rows with
	// no id are kept individually — they cannot be attributed anyway.
	fixtureIDs := loadFixtureRegistry(boardPath)
	var entries []freshnessEntry
	pos := make(map[string]int)
	for _, obj := range objs {
		id := boardString(obj["id"])
		e := freshnessEntry{
			id:      id,
			obj:     obj,
			fixture: boardRowIsFixture(obj, fixtureIDs),
		}
		if id != "" {
			if p, ok := pos[id]; ok {
				entries[p] = e
				continue
			}
			pos[id] = len(entries)
		}
		entries = append(entries, e)
	}
	ids := make([]string, 0, len(pos))
	for id := range pos {
		ids = append(ids, id)
	}

	// Git-first requires a repo: probe HEAD once. Without it no closure
	// can verify (every complete row parks as repo-unreadable) and no
	// idle claim can be made.
	repoOK := gitHeadReadable(repoDir)
	rep.CanVerify = repoOK && scanErr == nil

	// Window-bounded recent commits for the pointerless mid-flip case.
	var recent []recentCommit
	if repoOK {
		recent = gitRecentCommits(repoDir, now.Add(-window), 200)
	}

	matchers := make(map[string]*regexp.Regexp)
	matcher := func(id string) *regexp.Regexp {
		if id == "" {
			return nil
		}
		re, ok := matchers[id]
		if !ok {
			// Word-bounded exact id: ADV-R061 and ADV-R06X never match,
			// while dash/hash-suffixed aliases (ADV-R06-OLD, ADV-R06#3)
			// and trailing punctuation ("Addresses ADV-R06.") do — the
			// suffix sits behind a word boundary, so the base id is
			// still named. Case-sensitive: ids are identifiers.
			re = regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`)
			matchers[id] = re
		}
		return re
	}

	for _, e := range entries {
		var v RowVerdict
		v.ID = e.id
		switch {
		case e.fixture:
			// ADV-R05: fixtures are excluded by data, regardless of
			// status — never reclassified as work by this reader.
			v.Status = RowFixture
		default:
			status := strings.ToLower(strings.TrimSpace(boardString(e.obj["status"])))
			switch {
			case completeStatuses[status]:
				v = classifyCompleteRow(e, repoDir, repoOK, now, ids, matcher, v)
			case status == blockedStatusLiteral:
				v.Status = RowBlocked
			case openStatuses[status]:
				v = classifyOpenRow(e, repoDir, repoOK, now, window, recent, matcher, v)
			default:
				v.Status = RowOther
			}
		}
		if v.Status == RowClosed && v.Freshness.After(rep.LastVerifiedLanding) {
			rep.LastVerifiedLanding = v.Freshness
		}
		if v.Status == RowFlipWindow {
			if until := v.Freshness.Add(window); until.After(rep.RereadAfter) {
				rep.RereadAfter = until
			}
		}
		rep.Counts[v.Status]++
		rep.Rows = append(rep.Rows, v)
	}

	rep.TotalRows = len(rep.Rows)
	rep.WorkToSpawn = rep.Counts[RowOpen] > 0 || rep.MalformedLines > 0
	rep.VerifiablyIdle = rep.CanVerify && rep.TotalRows > 0 &&
		rep.MalformedLines == 0 &&
		rep.Counts[RowOpen] == 0 &&
		rep.Counts[RowUnverified] == 0 &&
		rep.Counts[RowFlipWindow] == 0 &&
		rep.Counts[RowFlipOverdue] == 0 &&
		rep.Counts[RowBlocked] == 0 &&
		rep.Counts[RowOther] == 0
	return rep
}

// classifyCompleteRow verifies a row whose status claims completion. This
// is requirements 1–4 in one pass: distrust the future-stamped row first
// (req 4), then demand a hash-shaped pointer that resolves as a commit
// (req 3), is reachable from HEAD (req 3), and names this row (req 1 —
// git-first: the status was only the CLAIM; the commit graph is the
// proof). Freshness on any resolving commit is its committer date (req 2).
//
// Note: unlike boardRowClosed (board_closure.go), which gates on the row
// SHAPE (worker_status, completed_at), this classifier is deliberately
// git-first — a row whose worker_status lags but whose commit verifiably
// landed and is attributed is CLOSED here, because the work demonstrably
// exists. The two definitions answer different questions.
func classifyCompleteRow(e freshnessEntry, repoDir string, repoOK bool, now time.Time, ids []string, matcher func(string) *regexp.Regexp, v RowVerdict) RowVerdict {
	v.Status = RowUnverified
	if !repoOK {
		v.Reason = ReasonRepoUnreadable
		return v
	}
	// Requirement 4 first: a parseable completion stamp in the future of
	// the read clock distrusts the row immediately — before any git work.
	// A row that lies about time is not trusted about work, even when its
	// hash would resolve.
	if stamp, ok := parseBoardStamp(boardString(e.obj["completed_at"])); ok && stamp.After(now) {
		v.Reason = ReasonFutureStamp
		v.StampTime = stamp
		return v
	}
	pointer := strings.TrimSpace(boardString(e.obj["commit_hash"]))
	if pointer == "" {
		v.Reason = ReasonNoPointer
		return v
	}
	if !commitHashShape.MatchString(pointer) {
		v.Reason = ReasonMalformedPointer
		return v
	}
	sha, err := gitRevVerifyCommit(repoDir, pointer)
	if err != nil {
		v.Reason = ReasonAbsentHash
		return v
	}
	v.Commit = sha
	reachable, rerr := gitReachableFromHEAD(repoDir, sha)
	if rerr != nil || !reachable {
		v.Reason = ReasonOrphanHash
		if when, _, gerr := gitCommitInfo(repoDir, sha); gerr == nil {
			v.Freshness = when // committer date is evidence even when parked
		}
		return v
	}
	when, msg, err := gitCommitInfo(repoDir, sha)
	if err != nil {
		// Resolves and reachable, but unreadable: a broken repo state —
		// park rather than guess.
		v.Reason = ReasonRepoUnreadable
		return v
	}
	v.Freshness = when
	if m := matcher(e.id); m != nil && m.MatchString(msg) {
		v.Status = RowClosed
		v.Reason = ""
		return v
	}
	// The commit does not name this row. If it names a different board
	// row, that is the off-by-one pointer class; naming no row at all is
	// unattributed. Both park as unverified — never work-to-spawn.
	for _, other := range ids {
		if other == e.id {
			continue
		}
		if m := matcher(other); m != nil && m.MatchString(msg) {
			v.Reason = ReasonMisAttributed
			return v
		}
	}
	v.Reason = ReasonUnattributed
	return v
}

// classifyOpenRow handles a row whose status claims open work, with the
// git-first override (requirement 1) and the bounded flip window
// (requirement 5): if the row's claimed work demonstrably exists in git,
// it is NOT work-to-spawn. Two detection paths:
//
//   - a resolving, attributed commit_hash pointer on the open row: the
//     implementation landed and only the board flip lags. Within one
//     window ⇒ flip-window; past it ⇒ flip-overdue (board-integrity
//     anomaly, still never work-to-spawn — the AC forbids reporting work
//     for a row whose hash resolves at HEAD).
//   - no usable pointer: the window-bounded recent-commit scan. A commit
//     inside the window that names this row is the mid-flip case (the
//     worker committed; the foreman has not flipped the row yet). Past
//     the window the scan sees nothing and the row stays OPEN — the
//     window is a grace period, not an amnesty.
//
// A pointer that does not resolve or does not attribute provides no
// evidence in either direction: the row stays open (never hide work).
func classifyOpenRow(e freshnessEntry, repoDir string, repoOK bool, now time.Time, window time.Duration, recent []recentCommit, matcher func(string) *regexp.Regexp, v RowVerdict) RowVerdict {
	v.Status = RowOpen
	if !repoOK {
		return v // open is open even without git; only flip protection is lost
	}
	m := matcher(e.id)
	pointer := strings.TrimSpace(boardString(e.obj["commit_hash"]))
	if commitHashShape.MatchString(pointer) {
		sha, err := gitRevVerifyCommit(repoDir, pointer)
		if err != nil {
			return v // unresolvable pointer on an open row: no evidence either way
		}
		reachable, rerr := gitReachableFromHEAD(repoDir, sha)
		if rerr != nil || !reachable {
			return v
		}
		when, msg, gerr := gitCommitInfo(repoDir, sha)
		if gerr != nil || m == nil || !m.MatchString(msg) {
			// Pointer resolves but does not attribute to this row: no
			// proof THIS row's work landed — stays open.
			return v
		}
		v.Commit = sha
		v.Freshness = when
		if now.Sub(when) <= window {
			v.Status = RowFlipWindow
		} else {
			v.Status = RowFlipOverdue
		}
		return v
	}
	if m == nil {
		return v // anonymous row: cannot be attributed
	}
	for _, rc := range recent {
		if m.MatchString(rc.message) {
			v.Commit = rc.sha
			v.Freshness = rc.when
			v.Status = RowFlipWindow // recent is window-bounded by construction
			return v
		}
	}
	return v
}

// readMarkdownBoardFreshness fills rep from a markdown board (the
// .coding-hermes/tasks.md legacy shape). Open unchecked headers raise
// RowOpen (fixtures excluded); checked headers are unverified closures —
// markdown rows carry no evidence fields, so no closure can verify and
// VerifiablyIdle is unreachable (CanVerify stays false).
func readMarkdownBoardFreshness(rep *FreshnessReport, boardPath string) {
	f, err := os.Open(boardPath)
	if err != nil {
		return // no/unreadable board — report stays the zero, fail-safe
	}
	defer f.Close()
	fixtureIDs := loadFixtureRegistry(boardPath)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		id := markdownTaskID(line)
		isFixt := isFixtureLine(line) || registryDeclares(id, fixtureIDs)
		var v RowVerdict
		v.ID = id
		switch {
		case strings.HasPrefix(line, "## [ ] "):
			if isFixt {
				v.Status = RowFixture
			} else {
				v.Status = RowOpen
			}
		case strings.HasPrefix(line, "## [x] "):
			if isFixt {
				v.Status = RowFixture
			} else {
				v.Status = RowUnverified
				v.Reason = ReasonNoPointer
			}
		default:
			continue
		}
		rep.Counts[v.Status]++
		rep.Rows = append(rep.Rows, v)
	}
	if err := sc.Err(); err != nil {
		rep.MalformedLines++
	}
	rep.TotalRows = len(rep.Rows)
	rep.WorkToSpawn = rep.Counts[RowOpen] > 0 || rep.MalformedLines > 0
}

// parseBoardRowsFreshness tolerantly parses a JSONL board: one JSON
// object per line, malformed lines counted and skipped (never fatal),
// 1MB scanner buffer — the same contract as board_closure.go. The
// returned error is a scanner failure (truncated read); an unopenable
// file is reported by the caller's os.Stat.
func parseBoardRowsFreshness(path string) (objs []map[string]json.RawMessage, malformed int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if jerr := json.Unmarshal([]byte(line), &obj); jerr != nil {
			malformed++
			continue
		}
		objs = append(objs, obj)
	}
	return objs, malformed, sc.Err()
}

// gitHeadReadable reports whether dir is a git repo with at least one
// readable commit at HEAD — the precondition for any git verification.
func gitHeadReadable(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD^{commit}").Run() == nil
}

// gitRevVerifyCommit resolves ref to a full commit sha, failing if ref
// names no object, is ambiguous, or is not a commit.
func gitRevVerifyCommit(dir, ref string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", ref+"^{commit}").Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("rev-parse produced no sha for %s", ref)
	}
	return sha, nil
}

// gitReachableFromHEAD reports whether sha is an ancestor of HEAD — i.e.
// the commit is in CURRENTLY reachable history (requirement 3), not merely
// an object that still exists in the object database. Exit code 1 is the
// ordinary "not an ancestor" answer; any other failure is an error.
func gitReachableFromHEAD(dir, sha string) (bool, error) {
	err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", sha, "HEAD").Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// gitCommitInfo returns the committer date (strict ISO from %cI —
// requirement 2's freshness source) and the raw message (subject + body,
// the attribution surface) of one commit.
func gitCommitInfo(dir, sha string) (time.Time, string, error) {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%cI%x1f%B", sha).Output()
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(out), "\x1f", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("unexpected git log output for %s", sha)
	}
	when, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("committer date for %s: %w", sha, err)
	}
	return when, parts[1], nil
}

// gitRecentCommits returns up to limit commits reachable from HEAD whose
// committer date is not older than since — the window-bounded scan behind
// the pointerless flip-window detection. Best-effort: nil on any git
// failure (the caller then simply sees no recent evidence).
func gitRecentCommits(dir string, since time.Time, limit int) []recentCommit {
	out, err := exec.Command("git", "-C", dir, "log", "-n", strconv.Itoa(limit),
		"--since="+since.UTC().Format(time.RFC3339),
		"--format=%H%x1f%cI%x1f%B%x1e", "HEAD").Output()
	if err != nil {
		return nil
	}
	var commits []recentCommit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimRight(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		when, werr := time.Parse(time.RFC3339, strings.TrimSpace(parts[1]))
		if werr != nil {
			continue
		}
		commits = append(commits, recentCommit{
			sha:     strings.TrimSpace(parts[0]),
			when:    when,
			message: parts[2],
		})
	}
	return commits
}
