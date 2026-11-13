package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

const perf009ListTicksSQL = "SELECT id, project_name, COALESCE(session_id,'') as session_id, status, COALESCE(outcome,'') as outcome, COALESCE(spawned_at,'') as spawned_at, COALESCE(completed_at,'') as completed_at, COALESCE(exit_code,0) as exit_code, COALESCE(commits,0) as commits, COALESCE(files_changed,0) as files_changed, COALESCE(tokens_in,0) as tokens_in, COALESCE(tokens_out,0) as tokens_out, COALESCE(cost_usd,0.0) as cost_usd, COALESCE(error,'') as error, created_at, COALESCE(dispatch_outcome,'') as dispatch_outcome, COALESCE(dispatch_reason,'') as dispatch_reason FROM ticks WHERE 1=1 ORDER BY ticks.spawned_at DESC LIMIT ?"

// ch:trace row=PERF-009 spec=none:board-acceptance wave=none:serial test=internal/api/perf009_ticks_index_test.go::TestPERF009_ListTicksUsesSpawnedIndex doc=none:reason evidence=internal/api/perf009_ticks_index_test.go witness=none:live-deployment-unavailable verdict=none:foreman-judge commit=none:foreman-merge memory=none:foreman-closeout
func TestPERF009_ListTicksUsesSpawnedIndex(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("server_helpers.go"))
	if err != nil {
		t.Fatalf("read listTicks source: %v", err)
	}
	sourceText := string(source)
	start := strings.Index(sourceText, "func listTicks(")
	end := strings.Index(sourceText, "func listEvents(")
	if start < 0 || end <= start {
		t.Fatal("cannot locate listTicks")
	}
	function := sourceText[start:end]
	parts := strings.SplitN(perf009ListTicksSQL, " ORDER BY ", 2)
	if !strings.Contains(function, `q := "`+parts[0]+`"`) || !strings.Contains(function, `q += " ORDER BY `+parts[1]+`"`) {
		t.Fatal("explained SQL differs from actual listTicks SQL")
	}

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var indexCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_ticks_spawned'`).Scan(&indexCount); err != nil {
		t.Fatalf("check idx_ticks_spawned: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("idx_ticks_spawned count = %d, want 1", indexCount)
	}

	const rowCount = 100001
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at) VALUES ('perf009', '', '', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, 'perf009', 'completed', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare ticks: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < rowCount; i++ {
		stamp := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		if _, err := stmt.Exec(fmt.Sprintf("perf009-%06d", i), stamp, stamp); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatalf("seed tick %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close tick insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit ticks: %v", err)
	}

	rows, err := db.Query(`EXPLAIN QUERY PLAN `+perf009ListTicksSQL, 50)
	if err != nil {
		t.Fatalf("explain listTicks: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	planText := strings.Join(plan, "\n")
	t.Logf("indexed plan: %s", planText)
	if !strings.Contains(planText, "idx_ticks_spawned") {
		t.Fatalf("listTicks plan does not use idx_ticks_spawned: %s", planText)
	}
	if strings.Contains(planText, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("listTicks plan still sorts into a temporary B-tree: %s", planText)
	}

	got, err := listTicks(context.Background(), db, "", "", 50)
	if err != nil {
		t.Fatalf("listTicks: %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("listTicks returned %d rows, want 50", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].SpawnedAt < got[i].SpawnedAt {
			t.Fatalf("listTicks ordering at row %d: %q before %q", i, got[i-1].SpawnedAt, got[i].SpawnedAt)
		}
	}
	if _, err := db.Exec(`DROP INDEX idx_ticks_spawned`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	withoutIndex, err := listTicks(context.Background(), db, "", "", 50)
	if err != nil {
		t.Fatalf("listTicks without index: %v", err)
	}
	if !reflect.DeepEqual(got, withoutIndex) {
		t.Fatal("listTicks results changed after dropping index")
	}
}
