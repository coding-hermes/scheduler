package dashboard_test

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// ch:trace row=PERF-008 spec=none:board-acceptance wave=.coding-hermes/waves/coding-hermes-scheduler-foreman-2026-11-13-17-16-14.json#PERF-008 test=internal/dashboard/perf008_index_test.go::TestPERF008ObservatoryIndex doc=none:reason evidence=internal/dashboard/perf008_index_test.go witness=none:live-deployment-unavailable verdict=none:foreman-judge commit=none:foreman-merge memory=none:foreman-closeout
func TestPERF008ObservatoryIndex(t *testing.T) {
	ctx := context.Background()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var version int
	if err := db.QueryRow(`SELECT count(*) FROM migrations WHERE version=89`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration 89 count=%d err=%v", version, err)
	}
	var column string
	if err := db.QueryRow(`SELECT name FROM pragma_index_info('idx_ticks_created') WHERE seqno=0`).Scan(&column); err != nil || column != "created_at" {
		t.Fatalf("index first column=%q err=%v", column, err)
	}
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at) VALUES ('a','','','2026-01-01','2026-01-01'), ('b','','','2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	for _, tick := range []struct {
		id, project, status, created, spawned, completed string
		cost                                             float64
	}{
		{"old", "a", "completed", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z", 9},
		{"boundary", "a", "completed", "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z", "2026-01-02T00:01:00Z", 1},
		{"new", "b", "failed", "2026-01-02T02:00:00Z", "2026-01-02T02:00:00Z", "", 2},
	} {
		x := &database.Tick{ID: tick.id, ProjectName: tick.project, Status: database.TickStatus(tick.status), CreatedAt: tick.created, SpawnedAt: tick.spawned, CompletedAt: tick.completed, CostUSD: tick.cost, TokensIn: 3, TokensOut: 4}
		if err := database.CreateTick(ctx, db, x); err != nil {
			t.Fatal(err)
		}
	}
	query := perf008ObservatoryQuery(t)
	const cutoff = "2026-01-02T00:00:00Z"
	plan := perf008Plan(t, db, query, cutoff)
	if !strings.Contains(plan, "USING INDEX idx_ticks_created (created_at>?)") {
		t.Fatalf("window predicate did not seek idx_ticks_created: %s", plan)
	}
	indexed := perf008Rows(t, db, query, cutoff)
	if len(indexed) != 2 {
		t.Fatalf("indexed rows=%v, want boundary and new", indexed)
	}
	g := dashboard.NewGenerator(db, nil)
	g.SetClock(clock.NewFixed(time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)))
	indexedSnapshot, err := g.CollectObservatory("6h", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_ticks_created`); err != nil {
		t.Fatal(err)
	}
	unindexed := perf008Rows(t, db, query, cutoff)
	if !reflect.DeepEqual(indexed, unindexed) {
		t.Fatalf("indexed rows %v differ from unindexed %v", indexed, unindexed)
	}
	unindexedSnapshot, err := g.CollectObservatory("6h", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexedSnapshot, unindexedSnapshot) {
		t.Fatalf("snapshot changed: indexed=%+v unindexed=%+v", indexedSnapshot, unindexedSnapshot)
	}
}

// Extract the actual SQL literal from the collector so the plan test cannot
// pass against a stale copy after the production query changes.
func perf008ObservatoryQuery(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(filename), "observatory.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var queries []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && strings.Contains(value, "FROM ticks t") && strings.Contains(value, "t.created_at >= ?") {
			queries = append(queries, value)
		}
		return true
	})
	if len(queries) != 1 {
		t.Fatalf("found %d matching production queries, want one", len(queries))
	}
	return queries[0]
}

func perf008Plan(t *testing.T, db *sql.DB, query, cutoff string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "; ")
}

func perf008Rows(t *testing.T, db *sql.DB, query, cutoff string) []string {
	t.Helper()
	rows, err := db.Query(query, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var ns, spawned, completed sql.NullString
		var status string
		var cost float64
		var tokensIn, tokensOut int64
		if err := rows.Scan(&ns, &status, &cost, &tokensIn, &tokensOut, &spawned, &completed); err != nil {
			t.Fatal(err)
		}
		result = append(result, fmt.Sprintf("%#v|%s|%g|%d|%d|%#v|%#v", ns, status, cost, tokensIn, tokensOut, spawned, completed))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}
