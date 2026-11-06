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

// PERF-004: the lane_churn enable-per-day count of GET /api/v1/metrics
// (internal/api/server_metrics.go metricsLaneChurn) predicates on
// component = 'api' AND message LIKE 'project enabled: % (cascade resume of %)'
// AND julianday(created_at) >= julianday(?). The julianday() call defeats any
// created_at range and no pre-v84 events index leads with component, so the
// statement full-scanned the ~1M-row events table on every render. Migration
// v84 adds idx_events_component_created — the component equality alone
// restricts the scan to the ~1% api rows (no ANALYZE statistics required).
// These tests pin: the schema (idempotent), the flipped plan at production
// scale, the paired A/B speedup (medians of alternating arms), and
// byte-identical result sets across 24h / 7d / all-time windows.

const (
	// perf004IndexName — the v84 index under test.
	perf004IndexName = "idx_events_component_created"

	// perf004EnableSQL is the lane_churn enable-per-day statement, VERBATIM
	// from internal/api/server_metrics.go (metricsLaneChurn's enable query).
	// Byte-for-byte copy: the plan/timing/equivalence tests are evidence
	// about THIS text, and any drift here vs the handler must fail loudly.
	perf004EnableSQL = `
SELECT date(created_at) AS day, COUNT(*)
FROM events
WHERE component = 'api'
  AND message LIKE 'project enabled: % (cascade resume of %)'
  AND julianday(created_at) >= julianday(?)
GROUP BY day`

	// perf004CreateIndexDDL is the migration's statement, verbatim (the
	// upgrade arm of the A/B pairs and the equivalence test apply exactly
	// what v84 runs).
	perf004CreateIndexDDL = `CREATE INDEX IF NOT EXISTS ` + perf004IndexName + ` ON events(component, created_at)`

	// perf004EventCount / perf004APIEvery / perf004SpreadDays — the
	// production-scale fixture shape. The live events table measured
	// 2026-11-05 holds 1,007,110 rows of which 9,896 (~1%) are
	// component='api'; the fixture generates 1,000,000 generic rows (every
	// 100th one api) spread over 90 days, plus the 9 enable events below.
	perf004EventCount = 1000000
	perf004APIEvery   = 100
	perf004SpreadDays = 90

	// The window bounds bound to perf004EnableSQL in the equivalence and
	// timing tests. The fixture spreads events over 2026-08-02..2026-10-30.
	perf004Cutoff24h = "2026-10-30T00:00:00Z"
	perf004Cutoff7d  = "2026-10-24T00:00:00Z"
	perf004CutoffAll = "2020-01-01T00:00:00Z"
)

// perf004EnableEvents — the fixture's 9 enable events (the ONLY rows the
// statement ever returns), at live-like created_at instants. The expected
// GROUP BY output is fully determined by this slice:
//
//	all-time: 8 rows (7 days x1, 2026-10-30 x2)
//	7d:       3 rows (2026-10-26|1, 2026-10-27|1, 2026-10-30|2)
//	24h:      1 row  (2026-10-30|2)
var perf004EnableEvents = []string{
	"2026-09-15T09:14:00Z",
	"2026-09-28T11:02:00Z",
	"2026-10-05T08:45:00Z",
	"2026-10-12T14:30:00Z",
	"2026-10-19T10:05:00Z",
	"2026-10-26T09:00:00Z",
	"2026-10-27T16:20:00Z",
	"2026-10-30T07:55:00Z",
	"2026-10-30T19:40:00Z",
}

// perf004ScanEventsRe matches a full scan of the events table in an
// EXPLAIN QUERY PLAN detail line ("SCAN events", or the older
// "SCAN TABLE events" shape) — any of which means the component equality
// failed to seek.
var perf004ScanEventsRe = regexp.MustCompile(`SCAN (TABLE )?events([^a-zA-Z0-9_]|$)`)

// perf004FixtureOnce / perf004FixturePath — the production-scale fixture is
// built ONCE per test binary (same shape as the PERF-005 fixture). The
// template is the PRE-upgrade state: fully migrated, then the v84 index
// dropped, then the fixture rows inserted — exactly what a live database
// looks like the moment before the migration would apply. Each test copies
// the template and performs the upgrade itself, so before/after arms live in
// one database.
var (
	perf004FixtureOnce sync.Once
	perf004FixturePath string
	perf004FixtureErr  error
)

func perf004FixtureTemplate() (string, error) {
	perf004FixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "perf004-fixture-*")
		if err != nil {
			perf004FixtureErr = err
			return
		}
		perf004FixturePath = filepath.Join(dir, "fixture.db")
		perf004FixtureErr = buildPerf004Fixture(perf004FixturePath)
	})
	return perf004FixturePath, perf004FixtureErr
}

// perf004DetailsPayload — production events average ~1.07 KB/row (the live
// table measured 1.08 GB over 1,007,110 rows, dominated by the details JSON),
// and that row width IS the unindexed scan cost. The fixture stamps a ~1 KB
// deterministic details payload on every row so the A/B arms measure the
// production scan, not a slim synthetic one (slim rows measured a misleading
// ~42x on this host: the scan ran 3x cheaper than production while the seek
// arm was unchanged).
var perf004DetailsPayload = `{"lane":"perf004","source":"fixture","payload":"` + strings.Repeat("p", 960) + `"}`

// buildPerf004Fixture creates a fully migrated database at dbPath, drops the
// v84 index (pre-upgrade state), and inserts perf004EventCount generic event
// rows (every perf004APIEvery-th one component='api') in ONE transaction with
// a prepared statement, followed by the perf004EnableEvents rows. No fixture
// file is checked in; the generated rows are deterministic, so the expected
// result sets are pinned constants.
func buildPerf004Fixture(dbPath string) (err error) {
	db, err := InitDB(dbPath)
	if err != nil {
		return fmt.Errorf("InitDB fixture: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close fixture db: %w", cerr)
		}
	}()

	if _, err = db.Exec("DROP INDEX IF EXISTS " + perf004IndexName); err != nil {
		return fmt.Errorf("drop %s: %w", perf004IndexName, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin fixture tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
INSERT INTO events (severity, component, message, details, created_at)
VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare event insert: %w", err)
	}
	defer stmt.Close()

	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < perf004EventCount; i++ {
		component := ""
		if i%perf004APIEvery == 0 {
			component = "api"
		}
		created := base.
			Add(time.Duration(i%perf004SpreadDays) * 24 * time.Hour).
			Add(time.Duration(i%86400) * time.Second).
			Format(time.RFC3339)
		message := fmt.Sprintf("lane lifecycle probe %06d", i)
		if component == "api" {
			message = fmt.Sprintf("api diagnostic probe %06d", i/perf004APIEvery)
		}
		if _, err = stmt.Exec("INFO", component, message, perf004DetailsPayload, created); err != nil {
			return fmt.Errorf("insert event %d: %w", i, err)
		}
	}
	for k, at := range perf004EnableEvents {
		message := fmt.Sprintf("project enabled: perf004-sat-%02d (cascade resume of perf004-primary)", k)
		if _, err = stmt.Exec("INFO", "api", message, perf004DetailsPayload, at); err != nil {
			return fmt.Errorf("insert enable event %d: %w", k, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit fixture tx: %w", err)
	}
	return nil
}

// perf004OpenFixtureCopy copies the shared pre-upgrade template into this
// test's own directory and opens it (copySQLiteShm + the GAP-060 pragma set,
// matching the package's established test-database posture).
func perf004OpenFixtureCopy(t *testing.T) *sql.DB {
	t.Helper()
	tmpl, err := perf004FixtureTemplate()
	if err != nil {
		t.Fatalf("build perf004 fixture template: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "perf004.db")
	copySQLiteShm(t, tmpl, dst)
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("open perf004 fixture copy: %v", err)
	}
	db.SetMaxOpenConns(1)
	applyGap060Pragmas(t, db)
	t.Cleanup(func() { db.Close() })
	return db
}

// perf004Plan returns the concatenated EXPLAIN QUERY PLAN detail lines for
// perf004EnableSQL with the cutoff bound (planning is value-independent for
// this statement, but the placeholder must be bound).
func perf004Plan(t *testing.T, db *sql.DB, cutoff string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+perf004EnableSQL, cutoff)
	if err != nil {
		t.Fatalf("explain enable statement: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var sel, order, from int
		var detail string
		if err := rows.Scan(&sel, &order, &from, &detail); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// perf004RunMs executes the enable statement once (fully drained) and returns
// the wall time in ms.
func perf004RunMs(t *testing.T, db *sql.DB, cutoff string) float64 {
	t.Helper()
	start := time.Now()
	rows, err := db.Query(perf004EnableSQL, cutoff)
	if err != nil {
		t.Fatalf("time enable statement: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain enable statement: %v", err)
	}
	rows.Close()
	return float64(time.Since(start).Microseconds()) / 1000.0
}

// perf004CaptureRows renders the statement's full result set as one string
// per row ("day|count") — the byte-level output the equivalence comparison
// needs.
func perf004CaptureRows(t *testing.T, db *sql.DB, cutoff string) []string {
	t.Helper()
	rows, err := db.Query(perf004EnableSQL, cutoff)
	if err != nil {
		t.Fatalf("capture enable rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			t.Fatalf("scan enable row: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%d", day, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enable rows: %v", err)
	}
	return out
}

// perf004Median returns the median of the samples (the sorted middle, or the
// mean of the two middles for an even count).
func perf004Median(samples []float64) float64 {
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// perf004Loadavg reads the host's 1/5/15-minute load averages for the test
// log — a marginal arm on a loaded host is attributable, not mysterious.
func perf004Loadavg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}

// TestPerf004_EventsComponentIndexSchemaAndIdempotent pins the v84 schema:
// the index exists after a fresh InitDB in the (component, created_at) form,
// the ledger records v84 exactly once, and a SECOND full Migrate run adds no
// duplicate and no error.
func TestPerf004_EventsComponentIndexSchemaAndIdempotent(t *testing.T) {
	db, err := InitDB(filepath.Join(t.TempDir(), "perf004-schema.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	countIndex := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'index' AND name = ?`, perf004IndexName).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", perf004IndexName, err)
		}
		return n
	}

	if got := countIndex(); got != 1 {
		t.Fatalf("%s rows after fresh InitDB = %d, want 1", perf004IndexName, got)
	}
	// The creation SQL must carry the (component, created_at) column list —
	// the whole point of v84 (a flattened single-column form would still
	// pass the name count but would not serve the plan).
	var s sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, perf004IndexName).Scan(&s); err != nil {
		t.Fatalf("read sqlite_master.sql for %s: %v", perf004IndexName, err)
	}
	if !s.Valid || !strings.Contains(s.String, "component, created_at") {
		t.Errorf("%s sql = %q, want the (component, created_at) form", perf004IndexName, s.String)
	}

	// Second ladder run: idempotent, no duplicate index rows, no error.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate run: %v", err)
	}
	if got := countIndex(); got != 1 {
		t.Errorf("%s rows after second Migrate = %d, want 1 (IF NOT EXISTS must not duplicate)", perf004IndexName, got)
	}

	var v int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE version = 84`).Scan(&v); err != nil || v != 1 {
		t.Errorf("migrations ledger rows for v84 = %d (err %v), want 1", v, err)
	}
	if got, err := MigrationVersion(ctx, db); err != nil || got != latestMigration {
		t.Errorf("MigrationVersion = %d (err %v), want latestMigration = %d", got, err, latestMigration)
	}
}

// TestPerf004_EventsComponentQueryPlanAtProductionScale builds the 1M-row
// fixture (production distribution: ~1% component='api', 9 enable events),
// captures the PRE-index plan (a full events scan — the red arm), applies the
// v84 upgrade (timed), and asserts the plan now seeks via
// idx_events_component_created with NO full scan of events.
func TestPerf004_EventsComponentQueryPlanAtProductionScale(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (1,000,009 events) skipped in -short; run without -short for the plan evidence")
	}
	db := perf004OpenFixtureCopy(t)

	var total, api, matches int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
		t.Fatalf("count fixture events: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'api'`).Scan(&api); err != nil {
		t.Fatalf("count api events: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'api'
  AND message LIKE 'project enabled: % (cascade resume of %)'`).Scan(&matches); err != nil {
		t.Fatalf("count enable events: %v", err)
	}
	if total != perf004EventCount+len(perf004EnableEvents) {
		t.Fatalf("fixture events = %d, want %d", total, perf004EventCount+len(perf004EnableEvents))
	}
	if api != perf004EventCount/perf004APIEvery+len(perf004EnableEvents) {
		t.Fatalf("fixture api events = %d, want ~1%% of the table (%d)", api, perf004EventCount/perf004APIEvery+len(perf004EnableEvents))
	}
	if matches != len(perf004EnableEvents) {
		t.Fatalf("fixture enable events = %d, want %d", matches, len(perf004EnableEvents))
	}

	before := perf004Plan(t, db, perf004CutoffAll)
	if !perf004ScanEventsRe.MatchString(before) {
		t.Fatalf("plan BEFORE the upgrade should full-scan events (the red arm), got:\n%s", before)
	}

	start := time.Now()
	if _, err := db.Exec(perf004CreateIndexDDL); err != nil {
		t.Fatalf("create %s: %v", perf004IndexName, err)
	}
	t.Logf("v84 index build on %d-row events fixture: %v", total, time.Since(start))

	after := perf004Plan(t, db, perf004CutoffAll)
	if !strings.Contains(after, perf004IndexName) {
		t.Errorf("plan after upgrade does not use %s:\nBEFORE:\n%s\nAFTER:\n%s", perf004IndexName, before, after)
	}
	if perf004ScanEventsRe.MatchString(after) {
		t.Errorf("plan after upgrade still full-scans events:\n%s", after)
	}
	t.Logf("BEFORE: %s\nAFTER:  %s",
		strings.ReplaceAll(before, "\n", " | "),
		strings.ReplaceAll(after, "\n", " | "))
}

// TestPerf004_EventsComponentPairedABTiming alternates the two arms —
// DROP INDEX / CREATE INDEX — inside ONE invocation over the same fixture
// copy and the same statement text, taking one sample per arm per pair. The
// indexed arm must be >= 50x faster by per-arm median (the owner measured
// ~95x: 0.286 s -> 0.003 s). Both arms' numbers and the host loadavg are
// quoted in the log so a marginal run on a loaded host is attributable.
func TestPerf004_EventsComponentPairedABTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (1,000,009 events) skipped in -short; run without -short for the A/B timing evidence")
	}
	db := perf004OpenFixtureCopy(t)

	const pairs = 6
	loadavg := perf004Loadavg()

	var without, with []float64
	for p := 0; p < pairs; p++ {
		if _, err := db.Exec("DROP INDEX IF EXISTS " + perf004IndexName); err != nil {
			t.Fatalf("drop %s (pair %d): %v", perf004IndexName, p, err)
		}
		without = append(without, perf004RunMs(t, db, perf004Cutoff24h))
		if _, err := db.Exec(perf004CreateIndexDDL); err != nil {
			t.Fatalf("create %s (pair %d): %v", perf004IndexName, p, err)
		}
		with = append(with, perf004RunMs(t, db, perf004Cutoff24h))
	}

	medWithout, medWith := perf004Median(without), perf004Median(with)
	if medWith <= 0 {
		t.Fatalf("indexed arm median = %v ms, want > 0", medWith)
	}
	speedup := medWithout / medWith
	t.Logf("paired A/B over %d pairs (cutoff %s), host loadavg %q:\n  without index: median %.3f ms  samples %v\n  with    index: median %.3f ms  samples %v\n  speedup: %.1fx",
		pairs, perf004Cutoff24h, loadavg, medWithout, without, medWith, with, speedup)
	if speedup < 50 {
		t.Errorf("indexed arm speedup = %.1fx (median %.3f ms vs %.3f ms), want >= 50x", speedup, medWith, medWithout)
	}
}

// TestPerf004_EventsComponentResultSetEquivalence proves the upgrade is
// read-only: for a 24h, a 7d and an all-time window the statement returns
// byte-identical rows with and without the index over the same fixture. The
// expected row sets are pinned too — they follow from perf004EnableEvents
// (the fixture's only matching rows), so a fixture regression cannot
// masquerade as equivalence.
func TestPerf004_EventsComponentResultSetEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("production-scale fixture (1,000,009 events) skipped in -short; run without -short for the equivalence evidence")
	}
	db := perf004OpenFixtureCopy(t)

	windows := []struct {
		name   string
		cutoff string
		want   []string
	}{
		{"24h", perf004Cutoff24h, []string{"2026-10-30|2"}},
		{"7d", perf004Cutoff7d, []string{"2026-10-26|1", "2026-10-27|1", "2026-10-30|2"}},
		{"all-time", perf004CutoffAll, []string{
			"2026-09-15|1", "2026-09-28|1", "2026-10-05|1", "2026-10-12|1",
			"2026-10-19|1", "2026-10-26|1", "2026-10-27|1", "2026-10-30|2",
		}},
	}

	before := make(map[string][]string, len(windows))
	for _, w := range windows {
		before[w.name] = perf004CaptureRows(t, db, w.cutoff)
	}

	if _, err := db.Exec(perf004CreateIndexDDL); err != nil {
		t.Fatalf("create %s: %v", perf004IndexName, err)
	}

	for _, w := range windows {
		after := perf004CaptureRows(t, db, w.cutoff)
		b, a := strings.Join(before[w.name], "\n"), strings.Join(after, "\n")
		if b != a {
			t.Errorf("%s window: output not byte-identical across the index upgrade\nBEFORE:\n%s\nAFTER:\n%s", w.name, b, a)
			continue
		}
		if a != strings.Join(w.want, "\n") {
			t.Errorf("%s window: rows %q, want %q (the fixture's pinned truth)", w.name, a, strings.Join(w.want, "\n"))
		}
		t.Logf("%-9s rows=%d byte-identical across the upgrade", w.name, len(after))
	}
}
