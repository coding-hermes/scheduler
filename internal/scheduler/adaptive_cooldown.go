package scheduler

import (
	"bufio"
	"database/sql"
	"log"
	"os"
	"strings"

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
//  1. A tick is NO-PROGRESS when it completes with 0 commits AND no new
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
//  2. After no_progress_threshold consecutive no-progress ticks (default
//     10), cooldown_s is multiplied by adaptiveCooldownFactor (2x) at each
//     further no-progress tick, capped at cooldown_ceiling_s (default
//     604800 = weekly). The project stays in normal cooldown mechanics the
//     whole time (the packer just reads cooldown_s), so it keeps getting
//     re-checked — it can never be abandoned.
//  3. ANY progress — a non-zero-commit tick OR a board row-count increase —
//     resets the streak to 0 and, when cooldown_s is above the floor
//     (cooldown_floor_s, defaulted to the cooldown in force at enable time),
//     drops it straight back to the floor. This is the speed-up path: the
//     moment UPD-* work lands on the board, the very next tick snaps the
//     project back to its base cadence.
//
// Failed spawns never reach this code (the slot-pool spawn-error path
// completes TickFailed and returns early), so spawn-failure backoff
// (S-GAP-001, consecutive_failures) and adaptive no-progress escalation stay
// orthogonal. Timeout ticks DO reach here: a tick that burned its slot and
// produced nothing is exactly the hourly waste adaptive exists to stop.

// adaptiveCooldownFactor is the per-escalation multiplier applied to
// cooldown_s once the no-progress streak passes the threshold. 2x per
// no-progress tick is aggressive enough to reach the weekly ceiling from any
// fleet base in a handful of ticks while remaining monotonic and bounded.
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

	var (
		adaptive  int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
		currentCD int
	)
	err := db.QueryRow(`SELECT adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_threshold, no_progress_ticks, board_rows_seen, cooldown_s
	FROM projects WHERE name = ?`, project).
		Scan(&adaptive, &floorS, &ceilingS, &threshold, &streak, &rowsSeen, &currentCD)
	if err != nil {
		return false // project gone or unreadable — let legacy autoSlowdown no-op too
	}
	if adaptive == 0 {
		return false // opt-in feature — default unchanged
	}

	// Resolve built-in defaults for zero-valued policy columns (rows enabled
	// before the normalization existed, hand-edited SQL, etc.).
	if ceilingS <= 0 {
		ceilingS = database.DefaultAdaptiveCooldownCeilingS
	}
	if threshold <= 0 {
		threshold = database.DefaultAdaptiveCooldownThreshold
	}

	// Board "new work" signal: did tasks.jsonl gain rows since the previous
	// tick's observation? Unknown baseline (-1) or a missing board file never
	// reports progress; only a net row-count increase does.
	newRows := false
	rowsNow, hasBoard := countBoardRows(workdir)
	if hasBoard {
		if rowsSeen >= 0 && rowsNow > rowsSeen {
			newRows = true
		}
		// Record the observation for the next tick regardless (a board that
		// disappeared mid-observation keeps its old baseline — the rows did
		// not disappear in reality, the read failed).
		if _, err := db.Exec(`UPDATE projects SET board_rows_seen = ? WHERE name = ?`,
			rowsNow, project); err != nil {
			log.Printf("ADAPTIVE: %s board_rows_seen update failed: %v", project, err)
		}
	}

	progress := outcome.Commits > 0 || newRows

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
			log.Printf("ADAPTIVE: %s progress (commits=%d new_board_rows=%v) → streak 0, cooldown %ds → %ds (floor)",
				project, outcome.Commits, newRows, currentCD, floorS)
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
