package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Non-code lane family output metrics (SCHED-GAP-177).
//
// Problem: 32% of all ticks (836/week measured 2026-09-12..19, ~$2,030
// sticker) come from the four non-code satellite families — qa (13
// lanes/139 ticks/$552), pm (15/54/$122), sync (23/581/$1,245), dogfood
// (13/62/$111) — and their output was recorded NOWHERE: those ticks write
// no product code, so code_commits read 0 forever and nothing could
// distinguish "does real work our columns don't capture" (PM ticks DO
// commit board rows; sync lanes write DuckBrain keys) from
// "idle-spinning on empty input" (the QA family had no output of ANY
// kind in the DB).
//
// Metric: output for a completed tick = code_commits > 0 OR
// board_commits > 0, the split persistGitCommitSignals already stamps on
// every tick row (SCHED-GAP-202 made that stamp unconditional). When the
// split is unmeasured (-1/-1 sentinel), the tick falls open to the raw
// commit claim — the same fall-open rule adaptive cooldown uses, so a
// measurement gap can never manufacture a zero-output streak and a
// false HIGH event. Counters live on the projects row (one output_count
// + one zero_output_streak pair per family); a lane whose streak reaches
// laneOutputHighThreshold consecutive zero-output ticks raises a HIGH
// lane-output event naming it.
//
// Scope: only ticks of projects whose namespace_id maps to a family
// (see laneFamily). Coding lanes skip this entirely.

// laneOutputHighThreshold is the consecutive zero-output tick count at
// which a lane raises a HIGH event. 8 matches the board row's acceptance
// criterion ("zero recorded output across >= 8 consecutive ticks raises a
// HIGH event naming it") and sits deliberately below the adaptive default
// threshold of 10 — the alert is the observability surface, not a
// cooldown action.
const laneOutputHighThreshold = 8

// laneFamily maps a namespace id to its non-code lane family, or "" when
// the namespace is not one of the four families. The sync family lives
// under the "duckbrain-sync" namespace slug in the live fleet (no bare
// "sync" namespace exists); both spellings resolve so a renamed or
// re-created namespace keeps counting. Matching is exact — "qa-extra"
// or "sync2" are different namespaces, not prefix matches.
func laneFamily(namespaceID string) string {
	switch namespaceID {
	case "qa":
		return "qa"
	case "pm":
		return "pm"
	case "dogfood":
		return "dogfood"
	case "sync", "duckbrain-sync":
		return "sync"
	default:
		return ""
	}
}

// recordLaneFamilyOutput accounts one terminal tick against the owning
// project's family counters: an output tick increments the family's
// output count and resets its zero-output streak; a zero-output tick
// extends the streak and, at laneOutputHighThreshold, emits a HIGH
// lane-output event naming the lane. Deferred ticks never ran a foreman
// turn and are skipped (the SCHED-GAP-203 rule: one gateway flap must
// not charge the lane). Best-effort like every other post-tick hook —
// a DB error is logged and never fails the tick lifecycle. db may be
// nil (standalone-pool tests); tickID is used only for the event
// details.
func recordLaneFamilyOutput(db *sql.DB, project, tickID string) {
	if db == nil {
		return
	}

	// The commit anatomy this decision reads was stamped on the tick row
	// by persistGitCommitSignals before any early return (SCHED-GAP-202),
	// so the authoritative values live in the DB, not in the caller's
	// outcome snapshot — reading them here keeps one writer per column.
	var (
		status       string
		codeCommits  sql.NullInt64
		boardCommits sql.NullInt64
		commits      sql.NullInt64
	)
	err := db.QueryRow(`SELECT status, code_commits, board_commits, commits
FROM ticks WHERE id = ?`, tickID).Scan(&status, &codeCommits, &boardCommits, &commits)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("LANE-OUTPUT: %s tick %s read failed: %v", project, tickID, err)
		}
		return
	}
	if status == string(TickDeferred) {
		return
	}

	// Unmeasured sentinel (-1/-1): fall open to the raw claim so a
	// measurement gap never reads as zero output.
	code := int(codeCommits.Int64)
	board := int(boardCommits.Int64)
	if code < 0 || board < 0 {
		code = int(commits.Int64)
		board = 0
	}
	output := code > 0 || board > 0

	var nsID sql.NullString
	if err := db.QueryRow(`SELECT namespace_id FROM projects WHERE name = ?`, project).Scan(&nsID); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("LANE-OUTPUT: %s namespace read failed: %v", project, err)
		}
		return
	}
	if !nsID.Valid {
		return // unscheduled lane — no family to account
	}
	fam := laneFamily(nsID.String)
	if fam == "" {
		return
	}

	if output {
		if _, err := db.Exec(laneOutputSetSQL(fam, "count"), project); err != nil {
			log.Printf("LANE-OUTPUT: %s %s output count write failed: %v", project, fam, err)
		}
		return
	}

	// Zero-output tick: extend the streak, then alert at the crossing.
	// Throttle is crossing-detection, not an events-table window: the
	// streak climbs by exactly one per zero-output tick (one tick per
	// project at a time — the slot pool's running map guarantees it), so
	// "previous streak < threshold <= new streak" identifies the single
	// crossing tick per streak generation. A lane stuck at streak 20
	// emits ONE event, not thirteen — the same once-per-crossing doctrine
	// as the consecutive-failures throttle (SCHED-GAP-137c). The window
	// between the streak UPDATE and the event INSERT is two adjacent
	// best-effort statements; a crash there delays the alert to the next
	// crossing and never duplicates it — the accepted trade for not
	// adding a fourth alert-state column per family.
	var streak int
	if err := db.QueryRow(laneOutputStreakReadSQL(fam), project).Scan(&streak); err != nil {
		log.Printf("LANE-OUTPUT: %s %s streak read failed: %v", project, fam, err)
		return
	}
	streak++
	if _, err := db.Exec(laneOutputSetSQL(fam, "streak"), streak, project); err != nil {
		log.Printf("LANE-OUTPUT: %s %s streak write failed: %v", project, fam, err)
		return
	}
	if streak >= laneOutputHighThreshold && streak-1 < laneOutputHighThreshold {
		emitLaneOutputHighEvent(db, project, fam, streak)
	}
}

// laneOutputColumns names the (count, streak) column pair per family —
// the single table every SQL helper below is built from, so adding a
// family is one entry here plus the struct/migration pair.
var laneOutputColumns = map[string][2]string{
	"qa":      {"qa_output_count", "qa_zero_output_streak"},
	"pm":      {"pm_output_count", "pm_zero_output_streak"},
	"sync":    {"sync_output_count", "sync_zero_output_streak"},
	"dogfood": {"dogfood_output_count", "dogfood_zero_output_streak"},
}

// laneOutputSetSQL builds the UPDATE for a family's counter. kind
// "count" bumps the output count and resets the streak (args: project);
// kind "streak" writes the streak (args: streak, project).
func laneOutputSetSQL(family, kind string) string {
	cols := laneOutputColumns[family]
	if kind == "count" {
		return "UPDATE projects SET " + cols[0] + " = " + cols[0] + " + 1, " + cols[1] + " = 0 WHERE name = ?"
	}
	return "UPDATE projects SET " + cols[1] + " = ? WHERE name = ?"
}

// laneOutputStreakReadSQL builds the streak SELECT for a family.
func laneOutputStreakReadSQL(family string) string {
	return "SELECT " + laneOutputColumns[family][1] + " FROM projects WHERE name = ?"
}

// emitLaneOutputHighEvent writes the HIGH event naming the lane, through
// the database package's single write path so it also publishes to the
// live event stream (CTL-002), like every other scheduler event.
func emitLaneOutputHighEvent(db *sql.DB, project, family string, streak int) {
	details, _ := json.Marshal(map[string]any{
		"project":     project,
		"lane_family": family,
		"streak":      streak,
		"threshold":   laneOutputHighThreshold,
	})
	_ = database.LogEvent(context.Background(), db, &database.Event{
		Severity:  database.SeverityHigh,
		Component: "lane-output",
		Message:   "non-code lane " + project + " (" + family + ") has zero recorded output for " + itoa(streak) + " consecutive ticks",
		Details:   string(details),
	})
	log.Printf("LANE-OUTPUT: %s (%s) zero-output streak reached %d — HIGH event emitted", project, family, streak)
}
