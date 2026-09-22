// Command backfill-commit-signals re-classifies historical scheduler ticks
// whose commit anatomy was never stamped and writes the split back onto the
// tick row.
//
// ROW: SCHED-PERF-001-B (parent SCHED-PERF-001).
//
// Root cause being repaired (fixed in the daemon by 40ae3d3b, SCHED-GAP-202):
// before that commit the code_commits / board_commits stamp sat BELOW the
// adaptive-cooldown opt-in gate, so every project with adaptive_cooldown = 0
// — the fleet default — recorded 0/0 no matter how many commits its tick
// landed. The columns went flat fleet-wide while the legacy `commits` column
// kept working. This tool walks a closed time window of ticks that report
// commits > 0 but 0/0 anatomy, re-runs the SAME classifier the daemon now
// runs (scheduler.ClassifyGitCommits → classifyGitCommits in
// internal/scheduler/adaptive_cooldown.go, untouched), and UPDATEs only
// ticks.code_commits / ticks.board_commits.
//
// Window reconstruction. The canonical classifier's git window is OPEN-ENDED
// (`git log --since=<tick started>` up to HEAD *now*). Called live, right after
// the tick completes, that is exactly the tick's own commits. Called as a
// back-fill days later it also counts everything LATER ticks landed in the same
// workdir — on this fleet that inflated the 2026-09-20 window ~10x (23 commits
// per tick for ticks that claimed 1-4).
//
// So the tool reconstructs the tick's own window by differencing TWO canonical
// measurements:
//
//	since tick start   (spawned_at)      -> A = every commit from the tick onward
//	since tick end     (completed_at)    -> B = every commit after the tick
//	tick's own split   = A - B           (the same rule the daemon stamps live)
//
// B is obtained the same way a live stamp is: count the commits in the window
// (countCommitsWithPathsSince, whose counting rule mirrors the classifier's)
// and hand that count to the canonical classifier as `claimed`, which both
// satisfies its reconciliation guard and returns the canonical split. The call
// refuses (ok=false) rather than guessing when the count and the split
// disagree, and a tick with no usable completed_at falls back to the open-ended
// measurement — counted and reported as an upper bound, never silently.
//
// A tick whose workdir or git history is gone cannot be measured at all. It
// gets the explicit unmeasured marker (-1/-1) — SCHED-GAP-202's contract is
// that an unmeasured tick must never look like a zero-commit tick — and a
// review_notes entry on the owning board row naming the cause.
//
// Usage:
//
//	# dry-run (default): reports counts, writes nothing
//	backfill-commit-signals --db ~/.hermes/coding-hermes/scheduler.db \
//	  --since 2026-09-19T14:03:25-05:00 --until 2026-09-21T23:59:59-05:00
//
//	# apply: stamp the split, mark the unmeasurable, note the causes
//	backfill-commit-signals --db ~/.hermes/coding-hermes/scheduler.db \
//	  --since 2026-09-19T14:03:25-05:00 --until 2026-09-21T23:59:59-05:00 \
//	  --apply
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver — the scheduler's driver

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

const (
	// defaultSince / defaultUntil are the SCHED-PERF-001-B window: the first
	// tick after the 2026-09-19 zero-commit flake through the end of the day
	// before the daemon fix (40ae3d3b) started stamping again.
	defaultSince = "2026-09-19T14:03:25-05:00"
	defaultUntil = "2026-09-21T23:59:59-05:00"

	// defaultBoardRow is the board row whose review_notes carries the
	// unrecoverable-tick census.
	defaultBoardRow = "SCHED-PERF-001-B"

	// boardNoteMarker makes the board write idempotent: a row that already
	// carries this marker is never appended to twice.
	boardNoteMarker = "SCHED-PERF-001-B back-fill"
)

// defaultDBPath mirrors cmd/schedulerd/main.go's --db default.
func defaultDBPath() string {
	return os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db")
}

// affectedTick is one row selected by the window predicate: a tick that
// claimed commits but carries 0/0 commit anatomy.
type affectedTick struct {
	ID          string
	Project     string
	Workdir     string
	SpawnedAt   string
	CompletedAt string
	Claimed     int
	Started     time.Time
	Completed   time.Time
}

// disposition is the classifier's verdict for one affected tick.
type disposition struct {
	Tick     affectedTick
	Code     int
	Board    int
	OK       bool
	Bounded  bool // the split covers the tick's own window (completed_at known)
	Total    int  // commits the written split accounts for
	RawTotal int  // commits visible in the open-ended window (A); -1 when git never answered
	// CauseClass and Cause are set only when OK is false.
	CauseClass string
	Cause      string
}

// Why a tick could not be measured. These strings are the review_notes
// census's group keys, so they are stable identifiers, not prose.
const (
	causeSpawnedAtUnparseable = "spawned_at-unparseable"
	causeWorkdirUnset         = "workdir-unset"
	causeWorkdirMissing       = "workdir-missing"
	causeWorkdirNotGitRepo    = "workdir-not-a-git-repo"
	causeGitDirIsFile         = "workdir-git-is-a-worktree-file"
	causeGitUnavailable       = "git-unavailable"
	causeGitUnderReports      = "git-history-under-reports"
	causeClassifierRefused    = "classifier-refused"
)

// Result is the tool's whole outcome, kept as a value so the CLI test can
// assert on it without scraping stdout.
type Result struct {
	Affected      int
	Backfillable  int
	Unrecoverable int
	Bounded       int // back-fillable ticks measured inside their own window
	Unbounded     int // back-fillable ticks that fell back to the open-ended window
	OverClaim     int // written splits that still exceed the tick's own claim
	Applied       int
	Skipped       int
	DryRun        bool
	BoardWritten  bool
	BoardSkipped  string
	Dispositions  []disposition
}

// Unrecoverable returns the ticks that could not be measured, in tick order.
func (r Result) UnrecoverableTicks() []disposition {
	var out []disposition
	for _, d := range r.Dispositions {
		if !d.OK {
			out = append(out, d)
		}
	}
	return out
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole CLI: flag parsing, walk, report, optional write. It returns
// a process exit code and never calls os.Exit itself so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backfill-commit-signals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite scheduler database path")
	sinceRaw := fs.String("since", defaultSince, "Window start, inclusive (RFC3339, e.g. 2026-09-19T14:03:25-05:00)")
	untilRaw := fs.String("until", defaultUntil, "Window end, inclusive (RFC3339)")
	dryRun := fs.Bool("dry-run", true, "Report only, write nothing (default)")
	applyFlag := fs.Bool("apply", false, "Write the classification to ticks.code_commits / ticks.board_commits (explicit opt-in; without it the run is a dry-run)")
	boardPath := fs.String("board", "", "tasks.jsonl that receives the review_notes entry naming unrecoverable ticks; empty = auto-discover <repo root>/.coding-hermes/board/tasks.jsonl, and no git root means no board write")
	boardRow := fs.String("board-row", defaultBoardRow, "Board row id whose review_notes entry records the unrecoverable ticks")
	noBoard := fs.Bool("no-board", false, "Suppress the board review_notes write even when a board file is given or discoverable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	// --apply is the write opt-in; an EXPLICIT --dry-run=true always wins so
	// `--apply --dry-run` can never silently write.
	write := *applyFlag
	if visited(fs, "dry-run") && *dryRun {
		write = false
	}

	since, err := time.Parse(time.RFC3339, *sinceRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: --since %q is not RFC3339: %v\n", *sinceRaw, err)
		return 2
	}
	until, err := time.Parse(time.RFC3339, *untilRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: --until %q is not RFC3339: %v\n", *untilRaw, err)
		return 2
	}
	if until.Before(since) {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: --until %s precedes --since %s\n", *untilRaw, *sinceRaw)
		return 2
	}

	db, err := openDB(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	ticks, err := selectAffected(db, *sinceRaw, *untilRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: select ticks: %v\n", err)
		return 1
	}
	dispositions := make([]disposition, 0, len(ticks))
	for _, tk := range ticks {
		dispositions = append(dispositions, classifyTick(tk))
	}

	res := summarize(dispositions, !write)

	board := ""
	if !*noBoard {
		board = resolveBoardPath(*boardPath)
	}
	note := ""
	if len(res.UnrecoverableTicks()) > 0 {
		note = buildBoardNote(res, *sinceRaw, *untilRaw, clock.Real().Now())
	}

	report(stdout, *dbPath, *sinceRaw, *untilRaw, res, board, *boardRow, note)

	if !write {
		return 0
	}

	applied, skipped, err := applyDispositions(db, dispositions)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: apply: %v\n", err)
		return 1
	}
	res.Applied, res.Skipped = applied, skipped
	_, _ = fmt.Fprintf(stdout, "applied: %d tick row(s) updated (%d stamped, %d marked -1/-1), %d skipped (row changed under us)\n",
		applied+skipped, res.Backfillable, res.Unrecoverable, skipped)

	if note == "" {
		_, _ = fmt.Fprintln(stdout, "board: no review_notes entry needed (no unrecoverable ticks)")
		return 0
	}
	if board == "" {
		_, _ = fmt.Fprintln(stdout, "board: skipped — no review_notes write requested (--no-board, or no --board and no discoverable git root)")
		return 0
	}
	wrote, err := appendBoardReviewNote(board, *boardRow, note)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backfill-commit-signals: board review_notes: %v\n", err)
		return 1
	}
	if !wrote {
		_, _ = fmt.Fprintf(stdout, "board: %s already carries a %q entry — left unchanged\n", board, boardNoteMarker)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "board: appended review_notes entry to %s row %s\n", board, *boardRow)
	return 0
}

// visited reports whether a flag was explicitly passed on the command line
// (as opposed to sitting at its default). Needed because --dry-run defaults
// to true — "not passed" and "--dry-run=true" must be distinguishable.
func visited(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// openDB opens the scheduler SQLite database read-write with the daemon's
// single-connection discipline. Dry-runs never issue a write (asserted by
// test), so the two modes share one open path — a read-only open of the live
// WAL database is not reliably possible (SQLite needs write access to the
// shared-memory index), and refusing to open at all would be worse than not
// writing.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only=0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma query_only: %w", err)
	}
	return db, nil
}

// selectAffected walks the window predicate. The window is compared as TEXT —
// every row the fleet writes carries RFC3339 spawned_at, and the accepted
// reproduction query in the row reads the same way
// (spawned_at >= '2026-09-20'). The commit-anatomy predicate is the row's
// exact defect statement: commits recorded, anatomy missing.
func selectAffected(db *sql.DB, sinceRaw, untilRaw string) ([]affectedTick, error) {
	rows, err := db.Query(`
SELECT t.id, t.project_name, COALESCE(t.spawned_at,''), COALESCE(t.completed_at,''), COALESCE(t.commits,0), COALESCE(p.workdir,'')
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE t.spawned_at >= ? AND t.spawned_at <= ?
  AND COALESCE(t.code_commits, 0) = 0
  AND COALESCE(t.board_commits, 0) = 0
  AND COALESCE(t.commits, 0) > 0
ORDER BY t.spawned_at, t.id`, sinceRaw, untilRaw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []affectedTick
	for rows.Next() {
		var tk affectedTick
		if err := rows.Scan(&tk.ID, &tk.Project, &tk.SpawnedAt, &tk.CompletedAt, &tk.Claimed, &tk.Workdir); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, tk.SpawnedAt); err == nil {
			tk.Started = t
		}
		if t, err := time.Parse(time.RFC3339, tk.CompletedAt); err == nil {
			tk.Completed = t
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

// classifyTick runs the canonical classifier on one tick's workdir, with the
// pre-checks the classifier itself treats as a silent ok=false, so the
// unmeasured marker always comes with a specific, re-findable cause. A
// measurable tick is then bounded to its own window (see belowTickEnd).
func classifyTick(tk affectedTick) disposition {
	d := disposition{Tick: tk, RawTotal: -1}
	if tk.Started.IsZero() {
		d.CauseClass = causeSpawnedAtUnparseable
		d.Cause = fmt.Sprintf("ticks.spawned_at is not RFC3339 (%q) — the tick start instant is needed as the git window", tk.SpawnedAt)
		return d
	}
	if tk.Workdir == "" {
		d.CauseClass = causeWorkdirUnset
		d.Cause = fmt.Sprintf("projects.workdir is empty for project %s — no path recorded, nothing to measure", tk.Project)
		return d
	}
	gitDir := filepath.Join(tk.Workdir, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		d.CauseClass = causeWorkdirMissing
		d.Cause = fmt.Sprintf("workdir missing on disk: %s (%v)", tk.Workdir, err)
		return d
	}
	if !fi.IsDir() {
		d.CauseClass = causeGitDirIsFile
		d.Cause = fmt.Sprintf("workdir %s has a .git FILE, not a directory (linked worktree) — the canonical classifier only measures a full git dir", tk.Workdir)
		return d
	}

	// A: every commit from the tick's start onward (the canonical live call).
	code, board, ok := scheduler.ClassifyGitCommits(tk.Workdir, tk.Started, tk.Claimed)
	if !ok {
		// ok == false with the repo present means either git itself failed or
		// git saw fewer commits than the tick claimed. Ask git the same
		// question the classifier asks to name which one it was.
		total, gerr := countCommitsWithPathsSince(tk.Workdir, tk.Started)
		d.RawTotal = total
		switch {
		case gerr != nil:
			d.CauseClass = causeGitUnavailable
			d.Cause = fmt.Sprintf("git could not read %s: %v", tk.Workdir, gerr)
		case total < tk.Claimed:
			d.CauseClass = causeGitUnderReports
			d.Cause = fmt.Sprintf("git history under-reports the tick: git sees %d commit(s) touching files since %s but the tick claimed %d (empty/merge commits carry no paths, and a shallow clone or rewritten history loses commits — the classifier refuses to trust a split it cannot reconcile)",
				total, tk.SpawnedAt, tk.Claimed)
		default:
			d.CauseClass = causeClassifierRefused
			d.Cause = fmt.Sprintf("canonical classifier returned ok=false with %d commit(s) touching files since %s (tick claimed %d)", total, tk.SpawnedAt, tk.Claimed)
		}
		return d
	}
	d.Code, d.Board, d.OK, d.Total, d.RawTotal = code, board, true, code+board, code+board

	// B: subtract everything that landed after the tick finished, so the
	// written split covers the tick's own window instead of every commit the
	// workdir saw in the days since.
	if !tk.Completed.IsZero() && tk.Completed.After(tk.Started) {
		if bCode, bBoard, ok := belowTickEnd(tk); ok {
			if code-bCode < 0 || board-bBoard < 0 {
				// B cannot be a subset of A (clock skew / rewritten history):
				// keep the open-ended measurement rather than invent one.
				return d
			}
			d.Code, d.Board, d.Bounded = code-bCode, board-bBoard, true
			d.Total = d.Code + d.Board
		}
	}
	return d
}

// belowTickEnd measures the commits that landed AFTER the tick completed, using
// the same canonical classifier, so the tick's own window can be isolated by
// subtraction (A - B). It refuses (ok=false) when the count it hands the
// classifier and the split the classifier returns disagree — a mirrored counter
// that has drifted from the classifier must surface as an unbounded
// measurement, never as a wrong subtraction.
func belowTickEnd(tk affectedTick) (code, board int, ok bool) {
	total, err := countCommitsWithPathsSince(tk.Workdir, tk.Completed)
	if err != nil {
		return 0, 0, false
	}
	// claimed = total satisfies the classifier's reconciliation guard exactly;
	// total == 0 is the classifier's valid empty measurement.
	code, board, ok = scheduler.ClassifyGitCommits(tk.Workdir, tk.Completed, total)
	if !ok {
		return 0, 0, false
	}
	if code+board != total {
		return 0, 0, false
	}
	return code, board, true
}

// countCommitsWithPathsSince counts the commits in a window the way the
// classifier counts them: one commit per block that touched at least one path.
// An empty or merge commit contributes no paths and is therefore not counted —
// the classifier skips it, and counting raw `git log` lines instead would
// report more commits than the classifier reconciled. It is only ever used to
// satisfy/validate the canonical call's guard, NEVER to classify.
func countCommitsWithPathsSince(workdir string, since time.Time) (int, error) {
	out, err := exec.Command("git", "-C", workdir, "log",
		"--since="+since.Format(time.RFC3339), "--pretty=format:@@%H", "--name-only").Output()
	if err != nil {
		return -1, err
	}
	n := 0
	for _, blk := range strings.Split(string(out), "@@") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		for _, p := range strings.Split(blk, "\n")[1:] { // first line is the hash
			if strings.TrimSpace(p) != "" {
				n++
				break
			}
		}
	}
	return n, nil
}

// summarize folds the per-tick verdicts into the reported counts.
func summarize(ds []disposition, dryRun bool) Result {
	res := Result{Affected: len(ds), DryRun: dryRun, Dispositions: ds}
	for _, d := range ds {
		if !d.OK {
			res.Unrecoverable++
			continue
		}
		res.Backfillable++
		if d.Bounded {
			res.Bounded++
		} else {
			res.Unbounded++
		}
		if d.Total > d.Tick.Claimed {
			res.OverClaim++
		}
	}
	return res
}

// applyDispositions writes the split for every affected tick — code_commits
// and board_commits ONLY, both in one transaction. The WHERE clause repeats
// the selection predicate so a row the daemon stamped while this tool was
// walking is never clobbered; those rows are counted as skipped instead.
func applyDispositions(db *sql.DB, ds []disposition) (applied, skipped int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`UPDATE ticks SET code_commits = ?, board_commits = ?
WHERE id = ? AND COALESCE(code_commits, 0) = 0 AND COALESCE(board_commits, 0) = 0`)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = stmt.Close() }()

	for _, d := range ds {
		code, board := d.Code, d.Board
		if !d.OK {
			code, board = -1, -1 // explicit unmeasured marker (SCHED-GAP-202)
		}
		r, err := stmt.Exec(code, board, d.Tick.ID)
		if err != nil {
			return applied, skipped, fmt.Errorf("tick %s: %w", d.Tick.ID, err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return applied, skipped, err
		}
		if n == 0 {
			skipped++
			continue
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return applied, skipped, err
	}
	return applied, skipped, nil
}

// resolveBoardPath picks the board file: the explicit --board when given,
// otherwise <git toplevel>/.coding-hermes/board/tasks.jsonl discovered from
// the working directory (so a run from the repo needs no path), otherwise
// empty meaning "no board write".
func resolveBoardPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	p := filepath.Join(strings.TrimSpace(string(out)), ".coding-hermes", "board", "tasks.jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// buildBoardNote is the review_notes text: every unrecoverable tick, its
// project and its cause, so the census is re-findable from the board alone.
func buildBoardNote(res Result, sinceRaw, untilRaw string, now time.Time) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "%s (applied %s): %d of %d affected ticks in %s..%s are UNRECOVERABLE and carry the explicit unmeasured marker code_commits=-1/board_commits=-1 (the canonical classifier could not measure them; an unmeasured tick must never look like a zero-commit tick). Back-fillable %d/%d stamped from the same classifier, %d of them inside the tick's own window (spawned_at..completed_at) and %d on the open-ended window (no usable completed_at, so an upper bound); %d written splits still exceed the tick's own claimed count. Unmeasurable ticks: ",
		boardNoteMarker, now.UTC().Format(time.RFC3339), res.Unrecoverable, res.Affected, sinceRaw, untilRaw,
		res.Backfillable, res.Affected, res.Bounded, res.Unbounded, res.OverClaim)
	for i, d := range res.UnrecoverableTicks() {
		if i > 0 {
			b.WriteString("; ")
		}
		_, _ = fmt.Fprintf(&b, "%s (%s): %s", d.Tick.ID, d.Tick.Project, d.Cause)
	}
	return b.String()
}

// appendBoardReviewNote appends note to the review_notes value of the row with
// the given id, touching nothing else on the line. It returns false (no
// write) when the row already carries boardNoteMarker, which makes repeated
// applies idempotent. The rest of every line is preserved byte-for-byte: the
// value is spliced textually instead of re-marshalling the row, so the board's
// existing JSON escaping (and the git diff) stays exactly as the fleet wrote
// it.
func appendBoardReviewNote(path, rowID, note string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe map[string]interface{}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			continue // not a JSON row (header/comment) — never rewrite it
		}
		id, _ := probe["id"].(string)
		if id != rowID {
			continue
		}
		cur, _ := probe["review_notes"].(string)
		if strings.Contains(cur, boardNoteMarker) {
			return false, nil
		}
		next := note
		if cur != "" {
			next = cur + " || " + note
		}
		spliced, err := spliceReviewNotes(line, next)
		if err != nil {
			return false, fmt.Errorf("row %s: %w", rowID, err)
		}
		lines[i] = spliced
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("row %s not found in %s", rowID, path)
}

// spliceReviewNotes replaces the value of the review_notes key in one JSON
// object line, leaving every other byte of the line untouched.
func spliceReviewNotes(line, value string) (string, error) {
	key := `"review_notes":`
	idx := strings.Index(line, key)
	if idx < 0 {
		return "", errors.New("no review_notes key on the row")
	}
	start := idx + len(key)
	// Skip insignificant whitespace between the key and its value.
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	var end int
	switch {
	case strings.HasPrefix(line[start:], "null"):
		end = start + len("null")
	case start < len(line) && line[start] == '"':
		end = start + 1
		for end < len(line) {
			if line[end] == '\\' {
				end += 2
				continue
			}
			if line[end] == '"' {
				end++
				break
			}
			end++
		}
		if end > len(line) || line[end-1] != '"' {
			return "", errors.New("unterminated review_notes string")
		}
	default:
		return "", errors.New("review_notes value is neither null nor a JSON string")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return line[:start] + string(encoded) + line[end:], nil
}

// report prints the human summary. The count line is the row's acceptance
// format verbatim: "N affected / M back-fillable / K unrecoverable".
func report(w io.Writer, dbPath, sinceRaw, untilRaw string, res Result, board, boardRow, note string) {
	mode := "DRY-RUN (nothing written; pass --apply to write)"
	if !res.DryRun {
		mode = "APPLY (writes ticks.code_commits / ticks.board_commits)"
	}
	_, _ = fmt.Fprintln(w, "backfill-commit-signals — "+boardNoteMarker)
	_, _ = fmt.Fprintf(w, "db        : %s\n", dbPath)
	_, _ = fmt.Fprintf(w, "window    : %s .. %s (inclusive)\n", sinceRaw, untilRaw)
	_, _ = fmt.Fprintln(w, "predicate : code_commits = 0 AND board_commits = 0 AND commits > 0")
	_, _ = fmt.Fprintf(w, "mode      : %s\n", mode)
	_, _ = fmt.Fprintf(w, "%d affected / %d back-fillable / %d unrecoverable\n", res.Affected, res.Backfillable, res.Unrecoverable)
	_, _ = fmt.Fprintf(w, "window     : %d of %d back-fillable ticks measured inside their own window (spawned_at..completed_at); %d fell back to the open-ended window (no usable completed_at) and are upper bounds\n",
		res.Bounded, res.Backfillable, res.Unbounded)
	_, _ = fmt.Fprintf(w, "over-claim : %d of %d written splits still exceed the tick's own claimed commit count\n", res.OverClaim, res.Backfillable)

	if res.Unrecoverable > 0 {
		counts := map[string]int{}
		for _, d := range res.UnrecoverableTicks() {
			counts[d.CauseClass]++
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_, _ = fmt.Fprintln(w, "unrecoverable by cause class:")
		for _, k := range keys {
			_, _ = fmt.Fprintf(w, "  %4d  %s\n", counts[k], k)
		}
		_, _ = fmt.Fprintln(w, "unrecoverable ticks (tick id | project | cause):")
		for _, d := range res.UnrecoverableTicks() {
			_, _ = fmt.Fprintf(w, "  %s | %s | %s\n", d.Tick.ID, d.Tick.Project, d.Cause)
		}
	}
	if board != "" {
		_, _ = fmt.Fprintf(w, "board     : %s (row %s)\n", board, boardRow)
	} else {
		_, _ = fmt.Fprintln(w, "board     : none (no --board and no git root discovered) — the unrecoverable census stays in this output only")
	}
	if res.DryRun && note != "" {
		_, _ = fmt.Fprintf(w, "board note text that --apply would append:\n%s\n", note)
	}
}
