package database

import (
	"context"
	"strings"
	"testing"
)

// SCHED-GAP-1622 acceptance test 1: the busy_timeout pragma must be non-zero
// (>= 5000 ms) after opening the daemon DB path through the database package's
// own setup function (InitDB). Non-zero busy_timeout is what removes the
// pool's immediate-fail mode — any non-first pooled connection meeting a write
// lock waits instead of surfacing SQLITE_BUSY.
//
// A temp-FILE database is used (mirroring TestInitDB_WALAndForeignKeys, which
// explains why in-memory databases cannot verify file-format pragmas).

func TestInitDB_BusyTimeoutAtLeast5000(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB(%s): %v", dbPath, err)
	}
	defer db.Close()

	var busy_timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy_timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busy_timeout < 5000 {
		t.Errorf("busy_timeout = %d, want >= 5000 ms (SCHED-GAP-1622 fix A: reads must wait out a write lock, not fail immediately)", busy_timeout)
	}
	if busy_timeout != BusyTimeoutMS {
		t.Errorf("busy_timeout = %d, InitDB armed a value different from the documented BusyTimeoutMS = %d", busy_timeout, BusyTimeoutMS)
	}
}

// TestInitDB_BusyTimeoutOnSecondConnection pins the pragma on a POOLED
// (non-first) connection: PRAGMAs are connection-scoped, and database/sql
// opens additional connections under concurrency — a busy_timeout armed only
// on the first connection would not remove the immediate-fail mode.
func TestInitDB_BusyTimeoutOnSecondConnection(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB(%s): %v", dbPath, err)
	}
	defer db.Close()

	// Open a second explicit connection (the pool's SetMaxOpenConns(1) allows
	// replacing — close is deferred; Grab Conn then close forces the pool to
	// open a fresh one afterwards) and read the pragma through it.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("grab conn: %v", err)
	}
	var first int
	if err := conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&first); err != nil {
		t.Fatalf("busy_timeout on grabbed conn: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("release conn: %v", err)
	}
	if first < 5000 {
		t.Errorf("busy_timeout = %d on a pooled connection, want >= 5000", first)
	}
}

// TestLatestMigration_SchedGap1622RunningIndex pins the migration ladder for
// the SCHED-GAP-1622 partial running-ticks index.
func TestLatestMigration_SchedGap1622RunningIndex(t *testing.T) {
	if latestMigration != 46 {
		t.Fatalf("latestMigration = %d, want 46 (SCHED-GAP-1622 partial index idx_ticks_status_running)", latestMigration)
	}
	m := migrations[len(migrations)-1]
	if m.version != 46 {
		t.Fatalf("last migration version = %d, want 46", m.version)
	}
	if !strings.Contains(m.stmt, "idx_ticks_status_running") {
		t.Errorf("v46 stmt does not create idx_ticks_status_running: %s", m.stmt)
	}
	if !strings.Contains(m.stmt, "WHERE status = 'running'") {
		t.Errorf("v46 index is not the PARTIAL (status='running') form: %s", m.stmt)
	}
}

// TestListProjectsPage_Basics covers the pagination contract the API handler
// serves (SCHED-GAP-1622 fix B / SCHED-GAP-1624 core): defaults applied, the
// right slice for limit+offset, and a Total that counts the WHOLE match set —
// not the page.
func TestListProjectsPage_Basics(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	names := []string{"p-alpha", "p-beta", "p-delta", "p-epsilon", "p-gamma"} // already in ASC order
	for _, n := range names {
		p := sampleProject(n)
		p.Enabled = false // keep the enabledOnly arm meaningful
		if err := CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}

	// Default: no bounds given → first DefaultListProjectsLimit rows, full total.
	page, err := ListProjectsPage(ctx, db, false, ListProjectsPageOpts(0, 0))
	if err != nil {
		t.Fatalf("ListProjectsPage default: %v", err)
	}
	if page.Limit != DefaultListProjectsLimit || page.Offset != 0 {
		t.Errorf("default page limit/offset = %d/%d, want %d/0", page.Limit, page.Offset, DefaultListProjectsLimit)
	}
	if page.Total != len(names) {
		t.Errorf("default total = %d, want %d (the full match count, not the page length)", page.Total, len(names))
	}
	if len(page.Projects) != len(names) {
		t.Errorf("default page rows = %d, want %d (fewer than the default cap)", len(page.Projects), len(names))
	}

	// limit+offset returns exactly the middle slice, in the same name order
	// as ListProjects.
	page, err = ListProjectsPage(ctx, db, false, ListProjectsPageOpts(2, 1))
	if err != nil {
		t.Fatalf("ListProjectsPage limit=2 offset=1: %v", err)
	}
	if len(page.Projects) != 2 {
		t.Fatalf("limit=2 offset=1 rows = %d, want 2", len(page.Projects))
	}
	if page.Projects[0].Name != names[1] || page.Projects[1].Name != names[2] {
		t.Errorf("offset slice = [%s, %s], want [%s, %s]", page.Projects[0].Name, page.Projects[1].Name, names[1], names[2])
	}
	if page.Total != len(names) {
		t.Errorf("paged total = %d, want %d", page.Total, len(names))
	}
	if page.Limit != 2 || page.Offset != 1 {
		t.Errorf("echoed limit/offset = %d/%d, want 2/1", page.Limit, page.Offset)
	}

	// enabledOnly counts only enabled rows in Total.
	page, err = ListProjectsPage(ctx, db, true, ListProjectsPageOpts(0, 0))
	if err != nil {
		t.Fatalf("ListProjectsPage enabledOnly: %v", err)
	}
	if page.Total != 0 {
		t.Errorf("enabledOnly total = %d, want 0 (no project was enabled)", page.Total)
	}
	if len(page.Projects) != 0 {
		t.Errorf("enabledOnly rows = %d, want 0", len(page.Projects))
	}
}

// TestListProjectsPage_Clamps proves normalization: negative/zero limit falls
// back to the default, an over-max limit is capped, a negative offset floors
// at 0 — an adversarial query string can neither inflate the read nor 500.
func TestListProjectsPage_Clamps(t *testing.T) {
	cases := []struct {
		limit, offset   int
		wantLim, wantOf int
	}{
		{-5, -1, DefaultListProjectsLimit, 0},
		{0, 0, DefaultListProjectsLimit, 0},
		{MaxListProjectsLimit * 10, 7, MaxListProjectsLimit, 7},
		{3, 2, 3, 2},
	}
	for _, c := range cases {
		got := ListProjectsPageOpts(c.limit, c.offset)
		if got.Limit != c.wantLim || got.Offset != c.wantOf {
			t.Errorf("Opts(%d, %d) = %d/%d, want %d/%d", c.limit, c.offset, got.Limit, got.Offset, c.wantLim, c.wantOf)
		}
	}
}

// TestListProjectsPage_MatchesListProjects pins surface parity: paging through
// with LIMIT = total reproduces exactly the full ListProjects result set.
func TestListProjectsPage_MatchesListProjects(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, n := range []string{"q-alpha", "q-beta"} {
		if err := CreateProject(ctx, db, sampleProject(n)); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	full, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	page, err := ListProjectsPage(ctx, db, false, ListProjectsPageOpts(len(full)+10, 0))
	if err != nil {
		t.Fatalf("ListProjectsPage: %v", err)
	}
	if len(page.Projects) != len(full) {
		t.Fatalf("page rows = %d, full rows = %d — the surfaces disagree", len(page.Projects), len(full))
	}
	for i := range full {
		if page.Projects[i].Name != full[i].Name {
			t.Errorf("row %d: page=%s full=%s — name ordering drifted between the surfaces", i, page.Projects[i].Name, full[i].Name)
		}
	}
}
