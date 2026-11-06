package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// PERF-005: the seven windowed GET /api/v1/metrics `ticks` blocks predicate on
// julianday(spawned_at) / julianday(completed_at). A function call on the
// column defeats every plain ticks index, so each block full-scanned ~101k
// production rows per render. Migration v83 adds the two expression indexes on
// the EXACT expressions the predicates use (the julianday() itself is
// load-bearing — timestamps are stored with mixed UTC/local-offset formats, so
// indexing the bare column and rewriting the predicates to string bounds would
// silently drop rows). These tests pin: the schema (idempotent), the flipped
// plans at production scale, byte-identical outputs before/after, and the
// untouched plans of the five OTHER hot julianday() call sites.

const (
	// perf005IndexSpawned / perf005IndexCompleted — the two v83 expression
	// indexes under test.
	perf005IndexSpawned   = "idx_ticks_spawned_jd"
	perf005IndexCompleted = "idx_ticks_completed_jd"

	// perf005TickCount / perf005ProjectCount — the production-scale fixture
	// shape (the live database measured 2026-11-05: 101,610 ticks / 879
	// projects with the production DDL).
	perf005TickCount    = 101610
	perf005ProjectCount = 879

	// perf005Cutoff is the 24h-window bound bound to every windowed
	// statement. The fixture generates spawns over 90 days ending
	// 2026-10-29T23:59:59, so this cutoff selects the last ~1.1k rows —
	// the same window selectivity the live endpoint sees.
	perf005Cutoff = "2026-10-29T00:00:00Z"
)

// The seven windowed statements, VERBATIM from internal/api/server_metrics.go
// (metricsSpawns, metricsTickDurations, metricsGateway, metricsOutcomes,
// metricsStalls). Byte-for-byte copies: the plan test's value is that THESE
// texts flip plans, and any drift here vs the handler is exactly what must
// fail loudly.
const (
	perf005SpawnsTotalSQL = `SELECT COUNT(*) FROM ticks WHERE julianday(spawned_at) >= julianday(?)`

	perf005SpawnsByNamespaceSQL = `SELECT COALESCE(NULLIF(p.namespace_id, ''), '-') AS ns, COUNT(*)
FROM ticks t
LEFT JOIN projects p ON p.name = t.project_name
WHERE julianday(t.spawned_at) >= julianday(?)
GROUP BY ns
ORDER BY ns`

	perf005SpawnsByOutcomeSQL = `SELECT outcome, COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?) AND outcome IS NOT NULL
GROUP BY outcome`

	perf005GatewayDrainSQL = `SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND error IS NOT NULL AND error <> ''
  AND (error LIKE '%503%' OR error LIKE '%draining%')`

	perf005ZeroOutputSQL = `SELECT COUNT(*)
FROM ticks
WHERE julianday(spawned_at) >= julianday(?)
  AND outcome = 'committed'
  AND COALESCE(commits, 0) = 0`

	perf005TickDurationSQL = `SELECT CAST(ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000.0) AS INTEGER)
FROM ticks
WHERE completed_at IS NOT NULL AND completed_at <> ''
  AND spawned_at IS NOT NULL AND spawned_at <> ''
  AND julianday(completed_at) >= julianday(?)
  AND julianday(completed_at) >= julianday(spawned_at)`

	perf005StallsSQL = `SELECT CAST(ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400.0 / 60.0) AS INTEGER) AS minutes
FROM ticks
WHERE completed_at IS NOT NULL AND completed_at <> ''
  AND spawned_at IS NOT NULL AND spawned_at <> ''
  AND julianday(completed_at) >= julianday(?)
  AND julianday(completed_at) >= julianday(spawned_at)`
)

// perf005Windowed describes one of the seven windowed metrics statements.
//
// WHY the two arms of the equivalence comparison: five of the seven return a
// deterministic row order (single-row aggregates, by_namespace's explicit
// ORDER BY, and by_outcome whose GROUP BY always sorts by the group key), so
// their raw output must be byte-identical before/after the index. The two
// plain row-returning statements (tick_duration_ms, stalls) carry no ORDER BY
// — a different scan legitimately reorders rows — so they are compared as
// sorted multisets plus an identical row count.
var perf005Windowed = []struct {
	name        string
	sql         string
	byteCompare bool // true = raw output byte-identical; false = sorted multiset + row count
}{
	{"spawns_total", perf005SpawnsTotalSQL, true},
	{"spawns_by_namespace", perf005SpawnsByNamespaceSQL, true},
	{"spawns_by_outcome", perf005SpawnsByOutcomeSQL, true},
	{"gateway_drain_503", perf005GatewayDrainSQL, true},
	{"outcomes_zero_output", perf005ZeroOutputSQL, true},
	{"tick_duration_ms", perf005TickDurationSQL, false},
	{"stalls", perf005StallsSQL, false},
}

// The five OTHER hot julianday() call sites, VERBATIM from their sources with
// the fmt %d placeholders resolved to the gatewayZombieMaxAge value (15
// minutes, internal/scheduler/tick_process.go). The no-regression test pins
// each plan to its pre-v83 index so the new expression indexes must not
// poach them.
const (
	// internal/scheduler/tick_process.go staleGatewayTicksSQL (%d = 15).
	perf005StaleGatewaySQL = `
SELECT id, project_name FROM ticks
WHERE status='running' AND pid = 0 AND (
    (heartbeat_at IS NOT NULL AND julianday(heartbeat_at) < julianday('now', '-15 minutes'))
 OR (heartbeat_at IS NULL     AND julianday(spawned_at)  < julianday('now', '-15 minutes'))
)`

	// internal/scheduler/tick_process.go liveGatewayTicksSQL (%d = 15).
	perf005LiveGatewaySQL = `
SELECT id, project_name FROM ticks
WHERE status='running' AND pid = 0 AND (
    (heartbeat_at IS NOT NULL AND heartbeat_at <> ''
        AND julianday(heartbeat_at) IS NOT NULL
        AND julianday(heartbeat_at) >= julianday('now', '-15 minutes'))
 OR ((heartbeat_at IS NULL OR heartbeat_at = '')
        AND spawned_at IS NOT NULL
        AND julianday(spawned_at) IS NOT NULL
        AND julianday(spawned_at) >= julianday('now', '-15 minutes'))
)`

	// internal/scheduler/loop.go allRunningRowsArePhantoms (%d = 15).
	perf005PhantomsSQL = `
SELECT COUNT(*),
       SUM(CASE WHEN pid = 0 AND (
            (heartbeat_at IS NOT NULL AND julianday(heartbeat_at) < julianday('now', '-15 minutes'))
         OR (heartbeat_at IS NULL     AND julianday(spawned_at)  < julianday('now', '-15 minutes')))
           THEN 1 ELSE 0 END)
FROM ticks WHERE status = 'running'`

	// internal/scheduler/budget.go LoadBudgetSpends (the plan is
	// param-independent, so the ? placeholders stay unbound — EXPLAIN QUERY
	// PLAN prepares without values).
	perf005BudgetSpendsSQL = `
SELECT project_name,
       COALESCE(SUM(CASE WHEN spawned_at >= ? AND julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(CASE WHEN spawned_at >= ? AND julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(cost_usd), 0.0)
FROM ticks
GROUP BY project_name`

	// internal/api/server_helpers.go stallCensus.
	perf005StallCensusSQL = `
SELECT p.namespace_id,
       COUNT(*) AS terminal,
       SUM(CASE WHEN t.error LIKE 'stalled: no progress%' THEN 1 ELSE 0 END) AS stalls
FROM ticks t JOIN projects p ON t.project_name = p.name
WHERE t.completed_at IS NOT NULL
  AND t.completed_at <> ''
  AND julianday(t.completed_at) >= julianday('now', '-1 day')
GROUP BY p.namespace_id`
)

// perf005ScanTicksRe matches a full scan of the ticks table in an
// EXPLAIN QUERY PLAN detail line, aliased ("SCAN t") or not ("SCAN ticks"),
// including the older "SCAN TABLE ticks" shape — any of which means the
// window predicate failed to seek.
var perf005ScanTicksRe = regexp.MustCompile(`SCAN (TABLE )?(ticks|t)([^a-zA-Z0-9_]|$)`)

// perf005FixtureOnce / perf005FixturePath — the production-scale fixture is
// built ONCE per test binary (same shape as gap060SchemaTemplate: the v1-v83
// ladder plus a 101k-row insert is seconds; three tests each re-inserting it
// would triple that for no new information). The template is the PRE-upgrade
// state: fully migrated, then BOTH v83 indexes dropped, then the fixture rows
// inserted — exactly what a live database looks like the moment before the
// migration would apply. Each test copies the template and performs the
// upgrade (CREATE INDEX, timed) itself, so before/after arms live in one
// database.
var (
	perf005FixtureOnce sync.Once
	perf005FixturePath string
	perf005FixtureErr  error
)

func perf005FixtureTemplate() (string, error) {
	perf005FixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "perf005-fixture-*")
		if err != nil {
			perf005FixtureErr = err
			return
		}
		perf005FixturePath = filepath.Join(dir, "fixture.db")
		perf005FixtureErr = buildPerf005Fixture(perf005FixturePath)
	})
	return perf005FixturePath, perf005FixtureErr
}

// buildPerf005Fixture creates a fully migrated database at dbPath, drops the
// two v83 expression indexes (pre-upgrade state), and inserts
// perf005ProjectCount projects + perf005TickCount ticks in ONE transaction
// (batched prepared INSERT — no fixture file is checked in). Spawn timestamps
// alternate RFC3339 UTC ("...Z") and local-offset ("...-05:00") to reproduce
// the mixed-offset storage that makes the julianday() predicates load-bearing.
func buildPerf005Fixture(dbPath string) (err error) {
	db, err := InitDB(dbPath)
	if err != nil {
		return fmt.Errorf("InitDB fixture: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close fixture db: %w", cerr)
		}
	}()

	for _, idx := range []string{perf005IndexSpawned, perf005IndexCompleted} {
		if _, err = db.Exec("DROP INDEX IF EXISTS " + idx); err != nil {
			return fmt.Errorf("drop %s: %w", idx, err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin fixture tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projStmt, err := tx.Prepare(`
INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES (?, ?, ?, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	if err != nil {
		return fmt.Errorf("prepare project insert: %w", err)
	}
	defer projStmt.Close()
	for j := 0; j < perf005ProjectCount; j++ {
		name := fmt.Sprintf("perf005-lane-%03d", j)
		if _, err = projStmt.Exec(name,
			"https://example.invalid/perf005/"+name,
			"/tmp/perf005/workdir/"+name); err != nil {
			return fmt.Errorf("insert project %s: %w", name, err)
		}
	}

	tickStmt, err := tx.Prepare(`
INSERT INTO ticks (
    id, project_name, session_id, pid, status, outcome,
    spawned_at, completed_at, exit_code, commits, files_changed,
    tokens_in, tokens_out, cost_usd, error, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare tick insert: %w", err)
	}
	defer tickStmt.Close()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	minusFive := time.FixedZone("perf005-05", -5*3600)
	for i := 0; i < perf005TickCount; i++ {
		project := fmt.Sprintf("perf005-lane-%03d", i%perf005ProjectCount)
		// ~90 days of spawns, mixed offset formats (even rows UTC "Z",
		// odd rows "-05:00") — the row shape a raw string bound
		// measurably drops rows for.
		spawnBase := base.
			Add(time.Duration(i%90) * 24 * time.Hour).
			Add(time.Duration(i%86400) * time.Second)
		var spawned time.Time
		if i%2 == 0 {
			spawned = spawnBase.UTC()
		} else {
			spawned = spawnBase.In(minusFive)
		}
		spawnedAt := spawned.Format(time.RFC3339)

		var status, outcome, errorTxt interface{}
		completedAt := ""
		switch {
		case i%10 == 0:
			// A slice stays live: running rows (outcome NULL, never completed).
			status, outcome = "running", nil
		case i%23 == 0:
			// Failures carry the drain-class error text.
			status, outcome = "failed", "failed"
			errorTxt = "gateway POST: HTTP 503: gateway is draining"
		default:
			status = "completed"
			if i%5 == 0 {
				outcome = "committed"
			} else {
				outcome = "dry_run"
			}
			// Terminal rows complete 5..300 minutes later, in the SAME
			// offset format as their spawn (the storage reality the
			// handler's julianday() comparisons exist for).
			completedAt = spawned.Add(time.Duration(i%60+1) * 5 * time.Minute).Format(time.RFC3339)
		}
		if i%97 == 0 && status != "failed" {
			errorTxt = "gateway POST: HTTP 503: gateway is draining"
		}

		if _, err = tickStmt.Exec(
			fmt.Sprintf("perf005-%06d", i), project,
			fmt.Sprintf("perf005-sess-%06d", i), 4242,
			status, outcome,
			spawnedAt, completedAt, 0,
			commitsFor(i, outcome), i%7,
			1000+i%5000, 100+i%900, 0.05,
			errorTxt, spawnedAt,
		); err != nil {
			return fmt.Errorf("insert tick %d: %w", i, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit fixture tx: %w", err)
	}
	return nil
}

// commitsFor resolves the ticks.commits value for row i so committed rows
// include a zero-output slice (outcome='committed' AND commits=0 — the
// outcomes.zero_output_committed arm's row shape).
func commitsFor(i int, outcome interface{}) int {
	if s, ok := outcome.(string); ok && s == "committed" && i%17 == 0 {
		return 0
	}
	return i%3 + 1
}

// perf005OpenFixtureCopy copies the shared pre-upgrade template into this
// test's own directory and opens it (copySQLiteShm + the GAP-060 pragma set,
// matching the package's established test-database posture).
func perf005OpenFixtureCopy(t *testing.T) *sql.DB {
	t.Helper()
	tmpl, err := perf005FixtureTemplate()
	if err != nil {
		t.Fatalf("build perf005 fixture template: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "perf005.db")
	copySQLiteShm(t, tmpl, dst)
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("open perf005 fixture copy: %v", err)
	}
	db.SetMaxOpenConns(1)
	applyGap060Pragmas(t, db)
	t.Cleanup(func() { db.Close() })
	return db
}

// perf005CreateIndexes applies the upgrade arm: both v83 expression indexes,
// timed (the reported build cost — the owner measured 1.14 s for both on the
// live-scale fixture).
func perf005CreateIndexes(t *testing.T, db *sql.DB) time.Duration {
	t.Helper()
	start := time.Now()
	for _, ddl := range []string{
		"CREATE INDEX IF NOT EXISTS " + perf005IndexSpawned + " ON ticks(julianday(spawned_at))",
		"CREATE INDEX IF NOT EXISTS " + perf005IndexCompleted + " ON ticks(julianday(completed_at))",
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	return time.Since(start)
}

// perf005Plan returns the concatenated EXPLAIN QUERY PLAN detail lines for a
// statement. The Go driver (unlike the sqlite3 CLI) rejects a prepared
// statement whose ? placeholders are left unbound, so a NULL is bound for
// every placeholder (counted outside single-quoted literals — planning needs
// no real values).
func perf005Plan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	args := make([]interface{}, perf005PlaceholderCount(query))
	for i := range args {
		args[i] = nil
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var sel, order, from int
		var detail string
		if err := rows.Scan(&sel, &order, &from, &detail); err != nil {
			t.Fatalf("scan explain row for %q: %v", query, err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows %q: %v", query, err)
	}
	return strings.Join(lines, "\n")
}

// perf005PlaceholderCount counts ? placeholders in a SQL text, skipping
// single-quoted literals ('503' contains none, but the guard is free).
func perf005PlaceholderCount(query string) int {
	n, inQuote := 0, false
	for i := 0; i < len(query); i++ {
		switch {
		case inQuote:
			if query[i] == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					i++ // escaped ''
				} else {
					inQuote = false
				}
			}
		case query[i] == '\'':
			inQuote = true
		case query[i] == '?':
			n++
		}
	}
	return n
}

// perf005BestRunMs executes a bound statement `runs` times and returns the
// best wall time in ms (best-of-N damps scheduler noise on a shared host).
func perf005BestRunMs(t *testing.T, db *sql.DB, query, cutoff string, runs int) float64 {
	t.Helper()
	var best float64
	for r := 0; r < runs; r++ {
		start := time.Now()
		rows, err := db.Query(query, cutoff)
		if err != nil {
			t.Fatalf("time %q: %v", query, err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("drain %q: %v", query, err)
		}
		rows.Close()
		ms := float64(time.Since(start).Microseconds()) / 1000.0
		if r == 0 || ms < best {
			best = ms
		}
	}
	return best
}

// perf005CaptureRows renders a statement's full result set as one string per
// row (columns joined with "|", NULL as empty) — the byte-level output the
// equivalence comparison needs.
func perf005CaptureRows(t *testing.T, db *sql.DB, query string, args ...interface{}) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("capture %q: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %q: %v", query, err)
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		fields := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				fields[i] = v.String
			}
		}
		out = append(out, strings.Join(fields, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return out
}

// TestPerf005_ExpressionIndexesSchemaAndIdempotent pins the v83 schema: both
// expression indexes exist after a fresh InitDB (and are the EXPRESSION form —
// a future edit flattening them to plain column indexes would pass the name
// count but serve none of the window predicates), the ledger records v83
// exactly once, and a SECOND full Migrate run adds no duplicates.
func TestPerf005_ExpressionIndexesSchemaAndIdempotent(t *testing.T) {
	db, err := InitDB(filepath.Join(t.TempDir(), "perf005-schema.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	countIndexes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'index' AND name IN (?, ?)`,
			perf005IndexSpawned, perf005IndexCompleted).Scan(&n); err != nil {
			t.Fatalf("count expression indexes: %v", err)
		}
		return n
	}

	if got := countIndexes(); got != 2 {
		t.Fatalf("expression indexes after fresh InitDB = %d, want 2", got)
	}
	// The creation SQL must be the expression form — the whole point of v83.
	for name, want := range map[string]string{
		perf005IndexSpawned:   "julianday(spawned_at)",
		perf005IndexCompleted: "julianday(completed_at)",
	} {
		var s sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&s); err != nil {
			t.Fatalf("read sqlite_master.sql for %s: %v", name, err)
		}
		if !s.Valid || !strings.Contains(s.String, want) {
			t.Errorf("index %s sql = %q, want the expression index form containing %q", name, s.String, want)
		}
	}

	// Second ladder run: idempotent, no duplicate index rows.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate run: %v", err)
	}
	if got := countIndexes(); got != 2 {
		t.Errorf("expression indexes after second Migrate = %d, want 2 (IF NOT EXISTS must not duplicate)", got)
	}
	for _, name := range []string{perf005IndexSpawned, perf005IndexCompleted} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("sqlite_master rows for %s = %d, want 1", name, n)
		}
	}

	var v int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE version = 83`).Scan(&v); err != nil || v != 1 {
		t.Errorf("migrations ledger rows for v83 = %d (err %v), want 1", v, err)
	}
	if got, err := MigrationVersion(ctx, db); err != nil || got != latestMigration {
		t.Errorf("MigrationVersion = %d (err %v), want latestMigration = %d", got, err, latestMigration)
	}
}

// TestPerf005_WindowedQueryPlansAtProductionScale builds the 101,610-row
// fixture, captures the PRE-index plans and timings of all seven windowed
// metrics statements, applies the v83 upgrade (timed), and asserts every plan
// now seeks via idx_ticks_spawned_jd / idx_ticks_completed_jd with NO full
// scan of ticks — plus logs the before/after timing table.
func TestPerf005_WindowedQueryPlansAtProductionScale(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (101,610 ticks) skipped in -short; run without -short for the plan evidence")
	}
	db := perf005OpenFixtureCopy(t)

	var rowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&rowCount); err != nil {
		t.Fatalf("count fixture ticks: %v", err)
	}
	if rowCount < 101000 {
		t.Fatalf("fixture ticks = %d, want >= 101000", rowCount)
	}

	type planRow struct {
		before   string
		beforeMs float64
	}
	plans := make(map[string]*planRow, len(perf005Windowed))
	for _, w := range perf005Windowed {
		plans[w.name] = &planRow{
			before:   perf005Plan(t, db, w.sql),
			beforeMs: perf005BestRunMs(t, db, w.sql, perf005Cutoff, 3),
		}
	}

	build := perf005CreateIndexes(t, db)
	t.Logf("v83 index build on %d-row fixture: %v (both indexes)", rowCount, build)

	for _, w := range perf005Windowed {
		after := perf005Plan(t, db, w.sql)
		afterMs := perf005BestRunMs(t, db, w.sql, perf005Cutoff, 3)
		p := plans[w.name]

		if !strings.Contains(after, perf005IndexSpawned) && !strings.Contains(after, perf005IndexCompleted) {
			t.Errorf("%s: plan after upgrade does not use either expression index:\nBEFORE:\n%s\nAFTER:\n%s",
				w.name, p.before, after)
		}
		if perf005ScanTicksRe.MatchString(after) {
			t.Errorf("%s: plan after upgrade still full-scans ticks:\n%s", w.name, after)
		}
		t.Logf("%-22s %8.1f ms -> %6.1f ms\n  before: %s\n  after:  %s",
			w.name, p.beforeMs, afterMs,
			strings.ReplaceAll(p.before, "\n", " | "),
			strings.ReplaceAll(after, "\n", " | "))
	}
}

// TestPerf005_OutputEquivalenceBeforeAfterIndex proves the upgrade is
// read-only: the same statements over the same fixture rows return identical
// output before and after the expression indexes. The five
// deterministic-order statements must be byte-identical; tick_duration_ms and
// stalls (no ORDER BY — scan order may legitimately change with the plan)
// must be identical sorted multisets with identical row counts.
func TestPerf005_OutputEquivalenceBeforeAfterIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (101,610 ticks) skipped in -short; run without -short for the equivalence evidence")
	}
	db := perf005OpenFixtureCopy(t)

	before := make(map[string][]string, len(perf005Windowed))
	for _, w := range perf005Windowed {
		before[w.name] = perf005CaptureRows(t, db, w.sql, perf005Cutoff)
	}

	perf005CreateIndexes(t, db)

	for _, w := range perf005Windowed {
		after := perf005CaptureRows(t, db, w.sql, perf005Cutoff)
		if len(after) != len(before[w.name]) {
			t.Errorf("%s: row count changed %d -> %d", w.name, len(before[w.name]), len(after))
			continue
		}
		if w.byteCompare {
			b, a := strings.Join(before[w.name], "\n"), strings.Join(after, "\n")
			if b != a {
				t.Errorf("%s: output not byte-identical across the index upgrade", w.name)
			}
		} else {
			sb, sa := append([]string(nil), before[w.name]...), append([]string(nil), after...)
			sort.Strings(sb)
			sort.Strings(sa)
			if strings.Join(sb, "\n") != strings.Join(sa, "\n") {
				t.Errorf("%s: sorted multiset of rows changed across the index upgrade", w.name)
			}
		}
		t.Logf("%-22s rows=%d byteCompare=%v identical", w.name, len(after), w.byteCompare)
	}
}

// TestPerf005_NoRegressionOtherJuliandayCallSites pins the plans of the five
// OTHER hot julianday() call sites: each must keep its pre-v83 index
// (idx_ticks_status_running for the three running-tick scans,
// idx_ticks_project_spawned_cost for the budget aggregate) and must not
// switch onto an expression index. stallCensus is compared as a whole-plan
// before/after: if the planner changes its plan at all, the upgrade must be
// justified by timing (after <= before + 10%) or the test fails with both
// plans quoted.
func TestPerf005_NoRegressionOtherJuliandayCallSites(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (101,610 ticks) skipped in -short; run without -short for the plan evidence")
	}
	db := perf005OpenFixtureCopy(t)

	pinned := []struct {
		name, sql, wantIndex string
	}{
		{"staleGatewayTicksSQL", perf005StaleGatewaySQL, "idx_ticks_status_running"},
		{"liveGatewayTicksSQL", perf005LiveGatewaySQL, "idx_ticks_status_running"},
		{"allRunningRowsArePhantoms", perf005PhantomsSQL, "idx_ticks_status_running"},
		{"LoadBudgetSpends", perf005BudgetSpendsSQL, "idx_ticks_project_spawned_cost"},
	}

	beforePlans := make(map[string]string, 5)
	for _, p := range pinned {
		beforePlans[p.name] = perf005Plan(t, db, p.sql)
	}
	beforePlans["stallCensus"] = perf005Plan(t, db, perf005StallCensusSQL)
	stallBeforeMs := perf005BestRunMs(t, db, perf005StallCensusSQL, perf005Cutoff, 3)

	build := perf005CreateIndexes(t, db)
	t.Logf("v83 index build: %v", build)

	for _, p := range pinned {
		after := perf005Plan(t, db, p.sql)
		if !strings.Contains(after, p.wantIndex) {
			t.Errorf("%s: plan lost its pre-v83 index %s:\nBEFORE:\n%s\nAFTER:\n%s",
				p.name, p.wantIndex, beforePlans[p.name], after)
		}
		for _, jdx := range []string{perf005IndexSpawned, perf005IndexCompleted} {
			if strings.Contains(after, jdx) {
				t.Errorf("%s: plan switched onto expression index %s:\n%s", p.name, jdx, after)
			}
		}
		t.Logf("%-26s kept %s\n  after: %s",
			p.name, p.wantIndex, strings.ReplaceAll(after, "\n", " | "))
	}

	afterStall := perf005Plan(t, db, perf005StallCensusSQL)
	stallAfterMs := perf005BestRunMs(t, db, perf005StallCensusSQL, perf005Cutoff, 3)
	t.Logf("stallCensus %8.1f ms -> %6.1f ms\n  before: %s\n  after:  %s",
		stallBeforeMs, stallAfterMs,
		strings.ReplaceAll(beforePlans["stallCensus"], "\n", " | "),
		strings.ReplaceAll(afterStall, "\n", " | "))
	if afterStall != beforePlans["stallCensus"] {
		// The plan moved. The upgrade may stand only if it is not slower —
		// the expression index turning this window predicate into a seek is
		// an improvement; anything slower is a regression to fail on.
		if stallAfterMs > stallBeforeMs*1.10 {
			t.Errorf("stallCensus plan changed AND regressed (%.1f ms -> %.1f ms > +10%%):\nBEFORE:\n%s\nAFTER:\n%s",
				stallBeforeMs, stallAfterMs, beforePlans["stallCensus"], afterStall)
		}
	}
}
