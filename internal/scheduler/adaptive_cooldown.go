package scheduler

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Adaptive cooldown (auto slow-down / speed-up) — opt-in per project.
//
// Problem: a project whose board has gone permanently quiet (or whose
// operator has nothing queued) keeps burning a full foreman session every
// cooldown period (fleet baselines 900s–21600s) forever, because neither the
// VERDICT-based autoSlowdown (caps at 24h and refuses to touch operator-set
// cooldowns >= 1h) nor the failure backoff ever escalates a HEALTHY-but-idle
// project. Parked projects must not be abandoned either — a vulnerability /
// update wave injected by the weekly UPD-* board task must re-accelerate the
// project instantly.
//
// Behavior (per project, when adaptive_cooldown is enabled):
//
//  1. A tick is NO-PROGRESS when it completes with no CODE commits AND no new
//     board rows since the previous tick. "New board rows" is measured as
//     growth in the total row count of the workdir's
//     .coding-hermes/board/tasks.jsonl (the canonical task board) versus the
//     count recorded when the previous tick completed (board_rows_seen). A
//     row-count signal — NOT an mtime signal — is deliberate: the foreman
//     itself rewrites tasks.jsonl in place while marking rows done, so an
//     mtime compare would let an idle tick's own bookkeeping self-report as
//     progress. Only a net increase in rows means NEW work appeared
//     (injected between ticks by a UPD-* board task, or appended by this
//     very tick — either way it is progress).
//     Commits get the same treatment (SCHED-GAP-104): the foreman's own
//     board bookkeeping (tasks/events/board jsonl rewrites) is committed
//     every tick by projects like boardctl, so a raw commits>0 test lets a
//     permanently idle project self-report progress forever. Commits are
//     classified by path via git: commits touching ONLY .coding-hermes/ are
//     bookkeeping (board_commits), anything touching files outside it is
//     real work (code_commits). Only code_commits count as progress. If git
//     is unavailable or the window yields nothing while the tick claimed
//     commits, the tick falls open to the legacy behavior (all commits are
//     progress) — the detector must never punish honest work over a
//     measurement gap.
//  2. After no_progress_threshold consecutive no-progress ticks (default
//     10), cooldown_s is multiplied by adaptiveCooldownFactor (2x) at each
//     further no-progress tick, capped at cooldown_ceiling_s (no explicit
//     ceiling → the derived default 8 × floor, ADV-R10). The project stays
//     in normal cooldown mechanics the whole time (the packer just reads
//     cooldown_s), so it keeps getting re-checked — it can never be
//     abandoned.
//  3. ANY progress — a code-commit tick OR a net DECREASE in open board rows
//     (the project closed work) — resets the streak to 0 and, when cooldown_s
//     is above the floor (cooldown_floor_s, defaulted to the cooldown in
//     force at enable time), drops it straight back to the floor. This is
//     the speed-up path: the moment the foreman completes backlog work or
//     lands code, the very next tick snaps the project back to its base
//     cadence. Row-count GROWTH is deliberately NOT progress (SCHED-GAP-105):
//     growth is injection (sibling crons filing findings), not output.
//
// Failed spawns never reach this code (the slot-pool spawn-error path
// completes TickFailed and returns early), so spawn-failure backoff
// (S-GAP-001, consecutive_failures) and adaptive no-progress escalation stay
// orthogonal. Timeout ticks DO reach here: a tick that burned its slot and
// produced nothing is exactly the hourly waste adaptive exists to stop.

// adaptiveCooldownFactor is the per-escalation multiplier applied to
// cooldown_s once the no-progress streak passes the threshold. 2x per
// no-progress tick is aggressive enough to reach the ceiling from any
// fleet base in three steps (8 × floor = factor³) while remaining
// monotonic and bounded.
const adaptiveCooldownFactor = 2

// adaptiveCooldown handles the opt-in per-project adaptive cooldown policy
// for one completed tick. Returns true when the project has adaptive_cooldown
// enabled (the outcome was accounted for — callers must skip the legacy
// autoSlowdown verdict logic); false when the feature is off and legacy
// behavior should run unchanged. All reads/writes are best-effort: a DB or
// board read error falls back to treating the tick as no-progress state
// untouched rather than failing the tick lifecycle.
func adaptiveCooldown(db *sql.DB, project, workdir string, outcome TickOutcome) bool {
	if db == nil {
		return false
	}

	// SCHED-GAP-202: commit-anatomy observability is UNCONDITIONAL. The
	// classification + tick-row stamp below runs exactly once per completed
	// tick, BEFORE any early return — including the adaptive == 0 opt-out and
	// the projects-row error return — so a project with adaptive cooldown off
	// (the fleet default) still gets its code/board commit split recorded.
	// measured is false when the measurement failed (git unavailable or
	// under-reporting); the tick row then carries the explicit unmeasured
	// marker (-1/-1) instead of silently looking like a zero-commit tick.
	codeCommits, boardCommits, measured := persistGitCommitSignals(db, outcome, workdir)

	// SCHED-GAP-203: a DEFERRED tick is a harness-side gateway blip, not a
	// project-side no-progress tick, so it must not extend the no-progress
	// streak or ratchet cooldown_s. Timeouts DO reach the escalation below
	// ("a tick that burned its slot and produced nothing is exactly the
	// waste adaptive exists to stop") — but a deferred tick never ran a
	// foreman turn at all, so charging the project for it would let one
	// gateway flap park a healthy lane: the same class of defect SCHED-GAP-134
	// (auto-disable on drain noise) and SCHED-GAP-143 (transport-class
	// failures never touch consecutive_failures) already closed elsewhere.
	// Returning true means "accounted for" — the caller then skips the legacy
	// autoSlowdown verdict logic too, which is correct here because a deferred
	// tick has no output to parse.
	// The commit-anatomy stamp ABOVE still ran (SCHED-GAP-202 observability is
	// unconditional): a mid-stream SSE drop can abort a turn that already
	// committed, and those commits stay recorded on the row.
	if outcome.Status == TickDeferred {
		return true
	}

	var (
		adaptive  int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
		openSeen  int
		currentCD int
	)
	err := db.QueryRow(`SELECT adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_threshold, no_progress_ticks, board_rows_seen, board_open_seen, cooldown_s
	FROM projects WHERE name = ?`, project).
		Scan(&adaptive, &floorS, &ceilingS, &threshold, &streak, &rowsSeen, &openSeen, &currentCD)
	if err != nil {
		return false // project gone or unreadable — let legacy autoSlowdown no-op too
	}
	if adaptive == 0 {
		return false // opt-in feature — default unchanged (observability already stamped above)
	}

	// SCHED-GAP-124: tasks-mode projects keep their admission governed by
	// the board, so the escalation half of adaptive cooldown must not
	// ratchet their cooldown_s (it would linger after the board drains).
	// The speed-up reset below still runs — it restores the floor pin
	// after progress, which is exactly the "auto kicks back in" baseline.
	if mode := admissionModeForProject(db, project); mode == database.AdmissionModeTasks {
		// The commit-anatomy split was already classified and persisted
		// above (SCHED-GAP-202) — tick reporting stays complete for every
		// mode without a duplicate classification.
		if openNow, openOK := boardOpenRows(workdir); openOK && openNow == 0 {
			// Board drained (never-done-only counts as drained): snap the
			// cooldown back to the floor pin so the cron gate resumes at
			// baseline the moment tasks-mode stops admitting.
			if floorS > 0 && currentCD > floorS {
				if _, err := db.Exec(`UPDATE projects SET cooldown_s = ?, no_progress_ticks = 0 WHERE name = ?`,
					floorS, project); err != nil {
					log.Printf("ADAPTIVE: %s tasks-mode floor restore failed: %v", project, err)
					return true
				}
				log.Printf("ADMISSION: %s board drained (tasks mode) → cooldown %ds → %ds (floor pin)", project, currentCD, floorS)
			}
		}
		return true
	}

	// Resolve built-in defaults for zero-valued policy columns (rows enabled
	// before the normalization existed, hand-edited SQL, etc.).
	if ceilingS <= 0 {
		// Derived default (ADV-R10): 8 × the floor (the fleet.toml pin
		// shape) — the ONLY stable base. Deriving from the live cooldown
		// would re-derive the cap each tick and ratchet it alongside the
		// escalation (never capping). A floorless row (floor 0, ceiling
		// 0 — hand-edited SQL / pre-normalization relic) stays at 0:
		// the streak is tracked, escalation disabled, matching the
		// documented dynamic-project semantic. The enable path always
		// writes a floor, so properly-enabled rows never land here.
		ceilingS = database.DefaultAdaptiveCooldownCeiling(floorS)
	}
	if threshold <= 0 {
		threshold = database.DefaultAdaptiveCooldownThreshold
	}

	// Board "new work" signal (SCHED-GAP-105): track OPEN rows, not total
	// rows. Total-row growth cannot distinguish "my foreman finished work"
	// from "a sibling cron injected findings onto my board" — injection
	// streams (qa-cron, error-scanner) would reset a no-output streak
	// forever. Direction is the signal:
	//   open rows DECREASED → this project closed net work  → progress
	//   open rows increased/equal → injections or churn, no output → no progress
	// Unknown baseline (-1) or a missing board file never reports progress.
	//
	// SCHED-GAP-207: the signal is counted over UNIQUE OPEN IDS, not open
	// rows. An id-as-SLOT writer (a satellite re-filing each cycle's finding
	// under the same id) grows the raw open-row count forever while the
	// unique-id count stays flat — exactly the churn the direction rule
	// exists to reject. The unique-id baseline lives in board_open_seen
	// (column repurposed: rows→unique ids); a re-file that does not close
	// the previous row leaves the count equal → no progress, which is the
	// closure protocol (ask 2) expressed as an incentive.
	netClosed := false
	rowsNow, hasBoard := countBoardRows(workdir)
	if hasBoard {
		// Record the total-row observation for observability (a board that
		// disappeared mid-observation keeps its old baseline — the rows did
		// not disappear in reality, the read failed).
		if _, err := db.Exec(`UPDATE projects SET board_rows_seen = ? WHERE name = ?`,
			rowsNow, project); err != nil {
			log.Printf("ADAPTIVE: %s board_rows_seen update failed: %v", project, err)
		}
	}
	if uniqueNow, uniqueOK := boardOpenUniqueIDs(workdir); uniqueOK {
		if openSeen >= 0 && uniqueNow < openSeen {
			netClosed = true // net completions — the board itself proves output
		}
		if _, err := db.Exec(`UPDATE projects SET board_open_seen = ? WHERE name = ?`,
			uniqueNow, project); err != nil {
			log.Printf("ADAPTIVE: %s board_open_seen update failed: %v", project, err)
		}
	}

	// Commit "real work" signal (SCHED-GAP-104): reuse the split classified
	// and persisted at the top of this function (SCHED-GAP-202 hoist — do
	// not classify twice). Falls open to legacy behavior (all commits are
	// progress) whenever git measurement failed.
	if !measured {
		codeCommits = outcome.Commits // legacy fallback: count them all
		boardCommits = 0
	}

	progress := codeCommits > 0 || netClosed

	if progress {
		// Speed-up path: reset the streak and drop any elevated cooldown back
		// to the configured floor immediately.
		resetCooldown := floorS > 0 && currentCD > floorS
		setSQL := "UPDATE projects SET no_progress_ticks = 0"
		args := []any{project}
		if resetCooldown {
			setSQL += ", cooldown_s = ?"
			args = append([]any{floorS}, args...)
		}
		if streak != 0 || resetCooldown {
			if _, err := db.Exec(setSQL+" WHERE name = ?", args...); err != nil {
				log.Printf("ADAPTIVE: %s reset write failed: %v", project, err)
				return true
			}
			log.Printf("ADAPTIVE: %s progress (code_commits=%d board_commits=%d net_board_closed=%v) → streak 0, cooldown %ds → %ds (floor)",
				project, codeCommits, boardCommits, netClosed, currentCD, floorS)
		}
		return true
	}

	// No-progress tick: extend the streak; once it reaches the threshold,
	// escalate cooldown progressively (2x per no-progress tick) to the
	// ceiling. Projects with no explicit cooldown (cooldown_s = 0, dynamic
	// priority-derived interval) have nothing to escalate — track the streak
	// for observability but never write a bogus cooldown.
	streak++
	newCD := currentCD
	if currentCD > 0 && currentCD < ceilingS && streak >= threshold {
		newCD = currentCD * adaptiveCooldownFactor
		if newCD <= currentCD || newCD > ceilingS {
			newCD = ceilingS // overflow guard or cap
		}
	}
	if _, err := db.Exec(`UPDATE projects SET no_progress_ticks = ?, cooldown_s = ? WHERE name = ?`,
		streak, newCD, project); err != nil {
		log.Printf("ADAPTIVE: %s streak write failed: %v", project, err)
		return true
	}
	if newCD != currentCD {
		log.Printf("ADAPTIVE: %s no-progress tick #%d (threshold %d) → cooldown %ds → %ds (ceiling %ds)",
			project, streak, threshold, currentCD, newCD, ceilingS)
	} else if streak == threshold {
		log.Printf("ADAPTIVE: %s reached no-progress threshold %d (cooldown stays %ds — no explicit base or already at ceiling)",
			project, threshold, currentCD)
	}
	return true
}

// countBoardRows returns the total number of task rows on the project board
// in workdir, plus whether a board file exists. JSONL boards count non-empty
// lines (one row per line); markdown boards count task headers ("## [ ] " /
// "## [x] "). Malformed/unreadable boards yield (0, false) — never an error.
func countBoardRows(workdir string) (int, bool) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return 0, false
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	count := 0
	isJSONL := strings.HasSuffix(boardPath, ".jsonl")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if isJSONL {
			count++
		} else if strings.HasPrefix(line, "## [ ] ") || strings.HasPrefix(line, "## [x] ") {
			count++
		}
	}
	return count, true
}

// boardPrefix is the path prefix that marks a file as fleet bookkeeping
// (board tasks/events, fixture state) rather than product code.
const boardPrefix = ".coding-hermes/"

// boardOpenRows counts rows whose status is open/pending/in-progress etc. in
// the project board in workdir. ok is false when the board is missing or
// unreadable — callers must treat the observation as absent, not zero.
// Statuses follow the fleet-wide open vocabulary (pending/open/in_progress/
// claimed/ready/todo/new/rework); unknown or missing statuses default to
// OPEN so a malformed row never hides open work.
//
// Perpetual fixture rows (SCHED-GAP-106) are EXCLUDED: a NEVER-DONE audit
// fixture is a permanent board resident, not work. Counting it would (a)
// keep the open-row baseline forever non-zero and (b) mask real net-closed
// signal. ADV-R05 canonicalized the exclusion: a row is a fixture when its
// id is declared active in the board's fixtures.jsonl registry (the
// canonical layer), carries "perpetual": true, or is in the NEVER-DONE id
// family — see boardRowIsFixture (fixture_registry.go), which this
// function shares with countPendingBoard so both consumers exclude
// identically.
func boardOpenRows(workdir string) (int, bool) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return 0, false
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	count := 0
	isJSONL := strings.HasSuffix(boardPath, ".jsonl")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !isJSONL {
			// Markdown boards: unchecked headers are open, checked are done.
			// Fixture rows (registry-declared, NEVER-DONE) are excluded here too.
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) &&
				!registryDeclares(markdownTaskID(line), fixtureIDs) {
				count++
			}
			continue
		}
		var row struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Perpetual bool   `json:"perpetual"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			count++ // malformed row — count as open, never hide work
			continue
		}
		if isFixtureRow(row.ID, row.Perpetual) || registryDeclares(row.ID, fixtureIDs) {
			continue // declared/perpetual fixture — not work (SCHED-GAP-106, ADV-R05)
		}
		s := strings.ToLower(row.Status)
		switch s {
		case "", "pending", "open", "in_progress", "in-progress", "claimed", "ready", "todo", "new", "rework":
			count++
		}
	}
	return count, true
}

// boardOpenUniqueIDs is the SCHED-GAP-207 metric half: the open-work signal
// counted the way a human counts it — DISTINCT open row ids, not open rows.
// A satellite that re-files each cycle's finding under the same id (an
// id-as-SLOT) produces an open-row count that grows forever without any new
// work existing; on the fleet's 2026-09-22 measurement 29% of open rows were
// such id-collisions. The unique-id count is the number a burndown or an
// "is this project done" caller actually wants.
//
// Same contract as boardOpenRows: ok is false when the board is missing or
// unreadable; the same open vocabulary applies (unknown/missing statuses
// count as open); perpetual fixture rows are excluded identically. A
// malformed row contributes ONE synthetic id ("" — it is open work whose id
// cannot be read, never hidden). Markdown boards have no ids by
// construction: their unchecked headers each count as one unique id (the
// header text IS the id).
func boardOpenUniqueIDs(workdir string) (int, bool) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return 0, false
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	ids := make(map[string]bool)
	isJSONL := strings.HasSuffix(boardPath, ".jsonl")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !isJSONL {
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) &&
				!registryDeclares(markdownTaskID(line), fixtureIDs) {
				ids[markdownTaskID(line)] = true
			}
			continue
		}
		var row struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Perpetual bool   `json:"perpetual"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			ids[""] = true // malformed row — open work, id unreadable
			continue
		}
		if isFixtureRow(row.ID, row.Perpetual) || registryDeclares(row.ID, fixtureIDs) {
			continue // declared/perpetual fixture — not work (SCHED-GAP-106, ADV-R05)
		}
		s := strings.ToLower(row.Status)
		switch s {
		case "", "pending", "open", "in_progress", "in-progress", "claimed", "ready", "todo", "new", "rework":
			ids[row.ID] = true
		}
	}
	return len(ids), true
}

// neverDonePrefix marks the fleet-standard perpetual audit fixture row
// (SCHED-GAP-106). Boards carry exactly one; its id may carry a suffix
// (NEVER-DONE-2, NEVERDONE...). Case-insensitive.
const neverDonePrefix = "never-done"

// isFixtureRow reports whether a board row is a perpetual fixture under the
// FALLBACK layers (ADV-R05): an explicit perpetual flag or an id in the
// NEVER-DONE family. The canonical layer — declaration in the board's
// fixtures.jsonl registry — is applied by boardRowIsFixture
// (fixture_registry.go), which both countPendingBoard and boardOpenRows
// consult; this predicate covers rows that carry neither flag nor
// declaration. Fixtures are invisible to adaptive cooldown and the
// pending-work boost — they are always on the board by design and must
// never keep a finished project fast (or wake one).
func isFixtureRow(id string, perpetual bool) bool {
	if perpetual {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), neverDonePrefix)
}

// isFixtureLine is the markdown-board variant of isFixtureRow: the whole
// line is available, so the fixture test is a contains on the marker.
func isFixtureLine(line string) bool {
	return strings.Contains(strings.ToUpper(line), "NEVER-DONE") ||
		strings.Contains(strings.ToLower(line), "\"perpetual\": true")
}

// isBoardPath reports whether a repo-relative path is fleet bookkeeping.
func isBoardPath(p string) bool {
	return strings.HasPrefix(filepath.ToSlash(p), boardPrefix)
}

// classifyGitCommits inspects the commits a tick produced in workdir and
// splits them into code vs board-bookkeeping by touched path. ok is false
// whenever the measurement is impossible (no git repo, git failure) or
// untrustworthy (git shows fewer commits than the tick claimed — shallow
// clones, clock skew) so callers can fall open to legacy behavior instead of
// punishing honest work over a measurement gap.
func classifyGitCommits(workdir string, since time.Time, claimed int) (code, board int, ok bool) {
	if claimed <= 0 {
		return 0, 0, true // nothing to classify — a valid empty measurement
	}
	if workdir == "" {
		return 0, 0, false
	}
	if fi, err := os.Stat(filepath.Join(workdir, ".git")); err != nil || !fi.IsDir() {
		return 0, 0, false
	}
	cmd := exec.Command("git", "-C", workdir, "log",
		"--since="+since.Format(time.RFC3339),
		"--pretty=format:@@%H", "--name-only")
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, false
	}
	total := 0
	for _, blk := range strings.Split(string(out), "@@") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		paths := strings.Split(blk, "\n")[1:] // first line is the hash
		nonEmpty := make([]string, 0, len(paths))
		for _, p := range paths {
			p = strings.TrimSpace(p)
			if p != "" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		if len(nonEmpty) == 0 {
			continue
		}
		total++
		isBoard := true
		for _, p := range nonEmpty {
			if !isBoardPath(p) {
				isBoard = false
				break
			}
		}
		if isBoard {
			board++
		} else {
			code++
		}
	}
	if total < claimed {
		// Git under-reports what the tick claimed — do not trust the split.
		return 0, 0, false
	}
	return code, board, true
}

// persistGitCommitSignals classifies the outcome's commits (classifyGitCommits)
// and stamps the split onto the tick row for fleet-wide observability.
// SCHED-GAP-202: the stamp is UNCONDITIONAL — on a failed measurement the
// tick row carries the explicit unmeasured marker (code_commits = -1,
// board_commits = -1) instead of being left at the schema default 0, so an
// unmeasured tick is never indistinguishable from a zero-commit tick.
// Returns (code, board, measured); measured=false means the caller must fall
// back to legacy behavior (all commits count as progress).
func persistGitCommitSignals(db *sql.DB, outcome TickOutcome, workdir string) (code, board int, measured bool) {
	code, board, measured = classifyGitCommits(workdir, outcome.Started, outcome.Commits)
	if !measured {
		code, board = -1, -1 // explicit unmeasured marker — a legal sentinel (columns are NOT NULL DEFAULT 0)
	}
	if outcome.TickID != "" {
		if _, err := db.Exec(`UPDATE ticks SET code_commits = ?, board_commits = ? WHERE id = ?`,
			code, board, outcome.TickID); err != nil {
			// Observability write only — never fail the lifecycle over it.
			log.Printf("ADAPTIVE: %s tick %s commit-signal write failed: %v",
				outcome.Project, outcome.TickID, err)
		}
	}
	return code, board, measured
}
