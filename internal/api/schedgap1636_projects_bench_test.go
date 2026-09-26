package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1636 benchmark harness.
//
// GET /api/v1/projects (listProjects) runs two DB steps behind a 5s
// request-scoped deadline: database.ListProjects (one SELECT over the
// projects table) and the per-project budget-spend aggregate, then a
// per-project in-memory merge and one big writeJSON. On the live fleet (496
// lanes, 75k ticks) every call was a DB round trip, and behind
// SetMaxOpenConns(1) it competes with the eval loop for the single serialized
// SQLite connection — the row's failure mode is a 504 naming ListProjects.
//
// The seed below is a fleet-shaped dataset: benchProjects lanes with
// production-sized text columns and benchTickRows ticks spread over ~6 weeks
// with the local (UTC-05:00) RFC3339 offset the live daemon writes.
//
// The arms are measured in ONE process on ONE seeded database and, for the
// spend aggregate, against a frozen copy of the pre-fix SQL — so the ratios
// below stay meaningful even when the host is loaded (absolute times are not
// portable, the same-process A/B is).
//
// Run with:
//
//	go test -run '^$' -bench SCHEDGAP1636 -benchtime 5x -p 1 ./internal/api/
const (
	benchProjects = 500
	benchTickRows = 120000
)

// coveringSpendIndex is the index migration 45 adds (SCHED-GAP-1636). The
// benchmark creates/drops it explicitly so the arms can isolate its effect
// instead of inheriting whatever the migrations left behind.
const coveringSpendIndex = "idx_ticks_project_spawned_cost"

// legacySpendAggregateSQL is the PRE-FIX LoadBudgetSpends statement, frozen
// verbatim. It exists so the benchmark can compare the old shape against the
// shipped implementation on the same data in the same run; it is deliberately
// not used by production code.
const legacySpendAggregateSQL = `
SELECT project_name,
       COALESCE(SUM(CASE WHEN julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(CASE WHEN julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(cost_usd), 0.0)
FROM ticks
GROUP BY project_name`

// seedProjectsBench builds a temp-file SQLite database, seeds projects and
// ticks, and returns a Server whose loop is inert (budget=0) so no spawn can
// fire. The returned cleanup closes the database.
func seedProjectsBench(b *testing.B, projects, ticks int) (*Server, func()) {
	b.Helper()
	dir := b.TempDir()
	db, err := database.InitDB(filepath.Join(dir, "scheduler.db"))
	if err != nil {
		b.Fatalf("InitDB: %v", err)
	}
	cleanup := func() { _ = db.Close() }

	ctx := context.Background()
	// ~1.9 KB of per-lane text (prompt + command): production averages
	// ~2.3 KB of JSON per lane row, and the response for 496 lanes is
	// ~1.15 MB (SCHED-GAP-1624). The text columns dominate both the row
	// read and the JSON encode.
	pad := strings.Repeat("lane instruction text ", 80)
	for i := 0; i < projects; i++ {
		name := fmt.Sprintf("bench-lane-%04d", i)
		p := &database.Project{
			Name:      name,
			RepoURL:   "https://github.com/coding-hermes/" + name,
			Workdir:   filepath.Join(dir, "wd", name),
			Weight:    10,
			Priority:  5,
			CooldownS: 900,
			DecayRate: 1.0,
			Model:     "deepseek-v4-flash",
			Provider:  "custom",
			Prompt:    pad,
			Command:   "scheduler-foreman-tick.sh --mode full --lane " + name,
			Deliver:   "telegram:-1003310984808:1",
		}
		if err := database.CreateProject(ctx, db, p); err != nil {
			cleanup()
			b.Fatalf("CreateProject %s: %v", name, err)
		}
	}

	// Ticks: one transaction, prepared statement. spawned_at is written the
	// way the daemon writes it — RFC3339 in the host's local zone (live rows
	// carry a -05:00 offset, which is exactly why the window predicates cannot
	// be a plain string comparison).
	loc := time.FixedZone("local", -5*60*60)
	now := time.Now().In(loc)
	tx, err := db.Begin()
	if err != nil {
		cleanup()
		b.Fatalf("begin seed tx: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, cost_usd, created_at)
VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		cleanup()
		b.Fatalf("prepare tick insert: %v", err)
	}
	for i := 0; i < ticks; i++ {
		spawned := now.Add(-time.Duration(i) * 30 * time.Second)
		completed := spawned.Add(95 * time.Second)
		name := fmt.Sprintf("bench-lane-%04d", i%projects)
		if _, err := stmt.Exec(
			fmt.Sprintf("bench-tick-%08d", i), name, "completed", "committed",
			spawned.Format(time.RFC3339), completed.Format(time.RFC3339),
			0.0042, spawned.Format(time.RFC3339),
		); err != nil {
			cleanup()
			b.Fatalf("seed tick %d: %v", i, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		cleanup()
		b.Fatalf("commit seed: %v", err)
	}

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	return NewServer(db, loop), cleanup
}

// spendQueryArgs returns the four bind values the spend aggregate takes, in
// the order the shipped statement binds them (prefilter bound + exact boundary
// per window).
func spendQueryArgs(now time.Time) []any {
	return []any{
		budgetSpendPrefilterArg(scheduler.UTCDayStart(now)),
		scheduler.UTCDayStart(now).Format(time.RFC3339),
		budgetSpendPrefilterArg(scheduler.UTCWeekStart(now)),
		scheduler.UTCWeekStart(now).Format(time.RFC3339),
	}
}

// budgetSpendPrefilterArg mirrors the bound the shipped predicate computes.
// The bench cannot call the unexported helper directly, and it must bind the
// SAME bound so the arms differ only by predicate shape.
func budgetSpendPrefilterArg(boundary time.Time) string {
	return boundary.UTC().Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
}

// runSpendSQL times one spend-aggregate statement and fails on an empty result
// so a silently non-matching query cannot read as "fast".
func runSpendSQL(b *testing.B, db *sql.DB, q string, args ...any) {
	b.Helper()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.Query(q, args...)
		if err != nil {
			b.Fatalf("spend query: %v", err)
		}
		n := 0
		for rows.Next() {
			var name string
			var daily, weekly, total float64
			if err := rows.Scan(&name, &daily, &weekly, &total); err != nil {
				rows.Close()
				b.Fatalf("scan spend row: %v", err)
			}
			n++
		}
		rows.Close()
		if n != benchProjects {
			b.Fatalf("spend query returned %d project rows, want %d", n, benchProjects)
		}
	}
}

// BenchmarkSCHEDGAP1636ListProjectsOnly times the projects SELECT alone.
func BenchmarkSCHEDGAP1636ListProjectsOnly(b *testing.B) {
	srv, cleanup := seedProjectsBench(b, benchProjects, benchTickRows)
	defer cleanup()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		projects, err := database.ListProjects(ctx, srv.db, false)
		if err != nil {
			b.Fatalf("ListProjects: %v", err)
		}
		if len(projects) != benchProjects {
			b.Fatalf("ListProjects returned %d rows, want %d", len(projects), benchProjects)
		}
	}
}

// BenchmarkSCHEDGAP1636SpendAggregate decomposes the cost this row attacked:
// the predicate rewrite (julianday per row vs string prefilter + julianday)
// and the covering index that lets the scan avoid the tick rows entirely.
func BenchmarkSCHEDGAP1636SpendAggregate(b *testing.B) {
	srv, cleanup := seedProjectsBench(b, benchProjects, benchTickRows)
	defer cleanup()
	now := time.Now()
	args := spendQueryArgs(now)
	ctx := context.Background()

	createIndex := func() {
		if _, err := srv.db.Exec(`CREATE INDEX IF NOT EXISTS ` + coveringSpendIndex + ` ON ticks(project_name, spawned_at, cost_usd)`); err != nil {
			b.Fatalf("create covering index: %v", err)
		}
	}
	dropIndex := func() {
		if _, err := srv.db.Exec(`DROP INDEX IF EXISTS ` + coveringSpendIndex); err != nil {
			b.Fatalf("drop covering index: %v", err)
		}
	}
	plan := func(label string) {
		// EXPLAIN QUERY PLAN through this driver returns no rows when the
		// statement still carries bind placeholders, so inline the two window
		// boundaries as literals (the plan is decided before any parameter is
		// bound, so this reports the same plan the query uses).
		sqlText := legacySpendAggregateSQL
		for _, bound := range []string{args[1].(string), args[3].(string)} {
			sqlText = strings.Replace(sqlText, "?", "'"+bound+"'", 1)
		}
		rows, err := srv.db.Query(`EXPLAIN QUERY PLAN ` + sqlText)
		if err != nil {
			b.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				b.Fatalf("scan plan: %v", err)
			}
			b.Logf("plan %s: %s", label, detail)
			seen++
		}
		if seen == 0 {
			b.Fatalf("EXPLAIN QUERY PLAN %s returned no rows — no plan evidence", label)
		}
	}

	b.Run("legacy_predicates", func(b *testing.B) {
		dropIndex()
		createIndex() // the shipped state in production: index present
		plan("legacy, covering index present")
		runSpendSQL(b, srv.db, legacySpendAggregateSQL, args[1], args[3])
	})

	b.Run("shipped_loadbudgetspends", func(b *testing.B) {
		createIndex()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			spends, err := scheduler.LoadBudgetSpends(ctx, srv.db, now)
			if err != nil {
				b.Fatalf("LoadBudgetSpends: %v", err)
			}
			if len(spends) == 0 {
				b.Fatal("LoadBudgetSpends returned no projects")
			}
		}
	})

	b.Run("legacy_no_covering_index", func(b *testing.B) {
		dropIndex()
		plan("legacy, index dropped")
		runSpendSQL(b, srv.db, legacySpendAggregateSQL, args[1], args[3])
	})

	b.Run("shipped_no_covering_index", func(b *testing.B) {
		dropIndex()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := scheduler.LoadBudgetSpends(ctx, srv.db, now); err != nil {
				b.Fatalf("LoadBudgetSpends: %v", err)
			}
		}
	})
	createIndex() // leave the database in the state the arms that follow expect
}

// benchProjectsRequest issues one GET /api/v1/projects through the real
// handler and returns the response size.
func benchProjectsRequest(b *testing.B, srv *Server) int {
	b.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	srv.listProjects(rec, req)
	if rec.Code != http.StatusOK {
		b.Fatalf("listProjects = %d, body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return rec.Body.Len()
}

// BenchmarkSCHEDGAP1636ProjectsHandler times the whole handler path —
// ListProjects + spend aggregate + merge + JSON encode. The two arms are the
// endpoint's two steady states: every request paying the aggregate, and every
// request after the first being served from the short-TTL snapshot.
func BenchmarkSCHEDGAP1636ProjectsHandler(b *testing.B) {
	srv, cleanup := seedProjectsBench(b, benchProjects, benchTickRows)
	defer cleanup()

	b.Run("cold_uncached", func(b *testing.B) {
		srv.SetBudgetSpendCacheTTL(0)
		benchProjectsRequest(b, srv) // warm the code paths, not the snapshot
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchProjectsRequest(b, srv)
		}
	})

	b.Run("warm_snapshot_cache", func(b *testing.B) {
		srv.SetBudgetSpendCacheTTL(15 * time.Second)
		size := benchProjectsRequest(b, srv) // loads + stores the snapshot
		b.Logf("response size: %d bytes for %d lanes", size, benchProjects)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchProjectsRequest(b, srv)
		}
	})
}
