package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// newTestDB returns an initialized in-memory SQLite database and fails the
// test if initialization errors. The caller is responsible for Close.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sampleProject(name string) *Project {
	return &Project{
		Name:      name,
		RepoURL:   "https://github.com/example/" + name,
		Workdir:   "/tmp/work/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "deepseek-v4-pro",
		Provider:  "deepseek-foreman",
		Enabled:   true,
	}
}

func sampleTick(projectName string) *Tick {
	return &Tick{
		ID:          NextTickID(projectName),
		ProjectName: projectName,
		Status:      StatusQueued,
	}
}

// --- Schema & migration tests --------------------------------------------

func TestInitDB_CreatesSchema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}

	for _, table := range []string{"projects", "ticks", "events", "migrations"} {
		var count int
		err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&count)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s not created (count=%d)", table, count)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Running migrate again should be a no-op.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate call: %v", err)
	}

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}
}

// TestMigrate_TicksHeartbeatColumn pins migration v10 (S-GAP-003): the ticks
// table must gain heartbeat_at so gateway-spawn (pid=0) rows have a liveness
// signal the zombie reapers can check.
func TestMigrate_TicksHeartbeatColumn(t *testing.T) {
	db := newTestDB(t)

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('ticks') WHERE name = 'heartbeat_at'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(ticks): %v", err)
	}
	if n != 1 {
		t.Errorf("ticks.heartbeat_at missing after Migrate (count=%d) — migration v10 not applied", n)
	}

	// The column must be usable for both write and julianday() comparison
	// (the reaper query shape).
	if _, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		 VALUES ('sgap003-mig', 'https://example.com/sgap003-mig', '/tmp/sgap003-mig', '2026-08-05T00:00:00Z', '2026-08-05T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, heartbeat_at)
		 VALUES ('sgap003-mig-t1', 'sgap003-mig', 'running', '2026-08-05T00:00:00Z', '2026-08-05T00:00:00Z', '2026-08-05T00:10:00Z')`,
	); err != nil {
		t.Fatalf("insert tick with heartbeat_at: %v", err)
	}
	var stale int
	if err := db.QueryRow(
		`SELECT count(*) FROM ticks WHERE id = 'sgap003-mig-t1'
		 AND julianday(heartbeat_at) < julianday('now', '-15 minutes')`,
	).Scan(&stale); err != nil {
		t.Fatalf("julianday heartbeat comparison: %v", err)
	}
	if stale != 1 {
		t.Errorf("julianday(heartbeat_at) comparison matched %d rows, want 1 (RFC3339 must parse)", stale)
	}
}

func TestInitDB_WALAndForeignKeys(t *testing.T) {
	// In-memory databases cannot use WAL mode — SQLite keeps "memory" mode
	// for them. To verify WAL is actually applied, test with a temp file.
	dir := t.TempDir()
	dbPath := dir + "/test.db"
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB(%s): %v", dbPath, err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var fkEnabled int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fkEnabled != 1 {
		t.Errorf("foreign_keys = %d, want 1", fkEnabled)
	}
}

// --- Project CRUD tests ---------------------------------------------------

func TestCreateProject_AndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("alpha")
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := GetProject(ctx, db, "alpha")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "alpha" || got.Weight != 10 || !got.Enabled {
		t.Errorf("GetProject returned %+v", got)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("timestamps not set: created=%q updated=%q", got.CreatedAt, got.UpdatedAt)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, err := GetProject(ctx, db, "nope")
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
}

func TestListProjects(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := CreateProject(ctx, db, sampleProject(name)); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	// Disable beta
	if err := DeleteProject(ctx, db, "beta"); err != nil {
		t.Fatalf("DeleteProject beta: %v", err)
	}

	all, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListProjects all returned %d, want 3", len(all))
	}

	enabled, err := ListProjects(ctx, db, true)
	if err != nil {
		t.Fatalf("ListProjects enabled: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("ListProjects enabled returned %d, want 2", len(enabled))
	}
	for _, p := range enabled {
		if !p.Enabled {
			t.Errorf("disabled project %q in enabled-only list", p.Name)
		}
	}

	if all[0].Name != "alpha" || all[1].Name != "beta" {
		t.Errorf("ListProjects order = %v,%v,%v; want alpha,beta,gamma",
			all[0].Name, all[1].Name, all[2].Name)
	}
}

func TestUpdateProject(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	weight := 50
	priority := 8
	if err := UpdateProject(ctx, db, "alpha", ProjectUpdates{
		Weight:   &weight,
		Priority: &priority,
	}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	got, err := GetProject(ctx, db, "alpha")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Weight != 50 || got.Priority != 8 {
		t.Errorf("after update: weight=%d priority=%d, want 50/8", got.Weight, got.Priority)
	}
}

func TestUpdateProject_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	w := 50
	err := UpdateProject(ctx, db, "nope", ProjectUpdates{Weight: &w})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("expected ErrProjectNotFound, got %v", err)
	}
}

func TestDeleteProject_SoftDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := DeleteProject(ctx, db, "alpha"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	got, err := GetProject(ctx, db, "alpha")
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if got.Enabled {
		t.Errorf("project still enabled after DeleteProject")
	}
}

func TestPurgeProject_HardDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// A historical tick referencing the project (ticks.project_name is a FK
	// with NO ACTION — purge must not be blocked by it, and the tick must
	// survive the purge).
	if err := CreateTick(ctx, db, sampleTick("alpha")); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	if err := PurgeProject(ctx, db, "alpha"); err != nil {
		t.Fatalf("PurgeProject: %v", err)
	}

	// Row is gone from the projects table.
	if _, err := GetProject(ctx, db, "alpha"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("GetProject after purge = %v, want ErrProjectNotFound", err)
	}
	// Historical tick retained (referenced by name string only).
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticks WHERE project_name = 'alpha'`).Scan(&n); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if n != 1 {
		t.Errorf("ticks for purged project = %d, want 1 (historical ticks must be retained)", n)
	}
	// FK enforcement restored: a new project can be created and a bogus
	// orphan tick insert is still refused.
	if err := CreateProject(ctx, db, sampleProject("beta")); err != nil {
		t.Fatalf("CreateProject after purge: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('orphan', 'nope', 'queued', '2026-08-15T00:00:00Z')`); err == nil {
		t.Error("orphan tick insert succeeded — foreign keys not restored after purge")
	}
}

func TestPurgeProject_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	err := PurgeProject(ctx, db, "nope")
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("PurgeProject(nope) = %v, want ErrProjectNotFound", err)
	}
}

func TestCreateProject_CheckConstraintWeight(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("bad")
	p.Weight = 200 // out of range [1,100]
	err := CreateProject(ctx, db, p)
	if err == nil {
		t.Fatal("expected CHECK constraint error for weight=200, got nil")
	}
}

func TestCreateProject_CheckConstraintPriority(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("bad")
	p.Priority = 11 // out of range [1,10]
	err := CreateProject(ctx, db, p)
	if err == nil {
		t.Fatal("expected CHECK constraint error for priority=11, got nil")
	}
}

// TestCreateProject_CaseInsensitiveNameDuplicate verifies that creating a
// project whose name differs only in case from an ENABLED project is rejected
// (SCHED-GAP-005). The two must not coexist or the daemon will split ticks
// between them unpredictably.
func TestCreateProject_CaseInsensitiveNameDuplicate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// "heading" is enabled (sampleProject sets Enabled: true).
	if err := CreateProject(ctx, db, sampleProject("heading")); err != nil {
		t.Fatalf("CreateProject heading: %v", err)
	}
	// Case-variant name, different workdir so only the name guard triggers.
	dup := sampleProject("HEADING")
	dup.Workdir = "/tmp/work/HEADING-other"
	err := CreateProject(ctx, db, dup)
	if err == nil {
		t.Fatal("expected case-insensitive duplicate name error, got nil")
	}
	if !strings.Contains(err.Error(), "already registered by enabled project") {
		t.Errorf("error = %q, want 'already registered by enabled project'", err.Error())
	}
}

// TestCreateProject_CaseInsensitiveNameDisabledOK verifies that a disabled
// (archived) project with the same lowercase name does NOT block creation —
// an archived entry is harmless, mirroring the workdir check's semantics.
func TestCreateProject_CaseInsensitiveNameDisabledOK(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	archived := sampleProject("speclang")
	archived.Enabled = false
	if err := CreateProject(ctx, db, archived); err != nil {
		t.Fatalf("CreateProject archived speclang: %v", err)
	}
	// Canonical enabled registration with a case-variant name + different workdir.
	canonical := sampleProject("SpecLang")
	canonical.Workdir = "/tmp/work/speclang-canonical"
	if err := CreateProject(ctx, db, canonical); err != nil {
		t.Fatalf("CreateProject canonical SpecLang (disabled dup exists): %v", err)
	}
}

// --- Tick lifecycle tests -------------------------------------------------

func TestCreateTick_AndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	tk := sampleTick("alpha")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	got, err := GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.ProjectName != "alpha" {
		t.Errorf("project_name = %q, want alpha", got.ProjectName)
	}
}

func TestTick_LifecycleTransitions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("alpha")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	// queued → running
	if err := UpdateTickStatus(ctx, db, tk.ID, StatusRunning, "sess-123"); err != nil {
		t.Fatalf("UpdateTickStatus running: %v", err)
	}
	got, err := GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.SessionID != "sess-123" {
		t.Errorf("session_id = %q, want sess-123", got.SessionID)
	}
	if got.SpawnedAt == "" {
		t.Error("spawned_at not set on running transition")
	}

	// running → completed (committed)
	if err := CompleteTick(ctx, db, tk.ID, OutcomeCommitted, 0, ""); err != nil {
		t.Fatalf("CompleteTick: %v", err)
	}
	got, err = GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.Outcome != OutcomeCommitted {
		t.Errorf("outcome = %q, want committed", got.Outcome)
	}
	if got.CompletedAt == "" {
		t.Error("completed_at not set")
	}
}

func TestTick_FailedTransition(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("alpha")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := CompleteTick(ctx, db, tk.ID, OutcomeFailed, 1, "boom"); err != nil {
		t.Fatalf("CompleteTick failed: %v", err)
	}
	got, err := GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want failed", got.Outcome)
	}
	if got.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", got.ExitCode)
	}
	if got.Error != "boom" {
		t.Errorf("error = %q, want boom", got.Error)
	}
}

func TestTick_TimeoutTransition(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("alpha")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := CompleteTick(ctx, db, tk.ID, OutcomeTimeout, -1, "timed out"); err != nil {
		t.Fatalf("CompleteTick timeout: %v", err)
	}
	got, err := GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Status != StatusTimeout {
		t.Errorf("status = %q, want timeout", got.Status)
	}
}

func TestCompleteTick_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	err := CompleteTick(ctx, db, "nonexistent", OutcomeCommitted, 0, "")
	if !errors.Is(err, ErrTickNotFound) {
		t.Fatalf("expected ErrTickNotFound, got %v", err)
	}
}

func TestRecordTickMetrics(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := sampleTick("alpha")
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := RecordTickMetrics(ctx, db, tk.ID, 3, 12, 10, 50000, 8000, 0.42, 7.5); err != nil {
		t.Fatalf("RecordTickMetrics: %v", err)
	}
	got, err := GetTick(ctx, db, tk.ID)
	if err != nil {
		t.Fatalf("GetTick: %v", err)
	}
	if got.Commits != 3 || got.FilesChanged != 12 {
		t.Errorf("commits=%d files=%d, want 3/12", got.Commits, got.FilesChanged)
	}
	if got.TokensIn != 50000 || got.TokensOut != 8000 {
		t.Errorf("tokens in=%d out=%d, want 50000/8000", got.TokensIn, got.TokensOut)
	}
	if got.CostUSD != 0.42 {
		t.Errorf("cost=%f, want 0.42", got.CostUSD)
	}
	if got.Urgency != 7.5 {
		t.Errorf("urgency=%f, want 7.5", got.Urgency)
	}
}

func TestListTicks_RecentFirst(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	ids := []string{"alpha-2026-01-01-00-00-01", "alpha-2026-01-01-00-00-02", "alpha-2026-01-01-00-00-03"}
	for _, id := range ids {
		tk := &Tick{ID: id, ProjectName: "alpha", Status: StatusQueued, CreatedAt: id[6:] + "Z"}
		if err := CreateTick(ctx, db, tk); err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}

	got, err := ListTicks(ctx, db, "alpha", 0)
	if err != nil {
		t.Fatalf("ListTicks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d ticks, want 3", len(got))
	}
	// newest first → last-created id should be first
	if got[0].ID != ids[2] {
		t.Errorf("first = %q, want %q (most recent)", got[0].ID, ids[2])
	}
}

func TestListTicks_Limit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Use explicit sequential IDs — NextTickID only has second resolution
	// and would collide when called multiple times within one second.
	for i := 0; i < 5; i++ {
		tk := &Tick{
			ID:          "alpha-tick-" + string(rune('0'+i)),
			ProjectName: "alpha",
			Status:      StatusQueued,
		}
		if err := CreateTick(ctx, db, tk); err != nil {
			t.Fatalf("CreateTick: %v", err)
		}
	}
	got, err := ListTicks(ctx, db, "alpha", 2)
	if err != nil {
		t.Fatalf("ListTicks: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestListAllTicks_PaginationAcrossProjects(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"alpha", "beta"} {
		if err := CreateProject(ctx, db, sampleProject(name)); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}

	fixtures := []Tick{
		{ID: "alpha-1", ProjectName: "alpha", Status: StatusCompleted, CreatedAt: "2026-07-20T10:00:00Z"},
		{ID: "beta-1", ProjectName: "beta", Status: StatusFailed, CreatedAt: "2026-07-20T11:00:00Z"},
		{ID: "alpha-2", ProjectName: "alpha", Status: StatusRunning, CreatedAt: "2026-07-20T12:00:00Z"},
		{ID: "beta-2", ProjectName: "beta", Status: StatusQueued, CreatedAt: "2026-07-20T13:00:00Z"},
	}
	for i := range fixtures {
		if err := CreateTick(ctx, db, &fixtures[i]); err != nil {
			t.Fatalf("CreateTick %s: %v", fixtures[i].ID, err)
		}
	}

	page1, err := ListAllTicks(ctx, db, 2, 0)
	if err != nil {
		t.Fatalf("ListAllTicks page 1: %v", err)
	}
	page2, err := ListAllTicks(ctx, db, 2, 2)
	if err != nil {
		t.Fatalf("ListAllTicks page 2: %v", err)
	}

	if len(page1) != 2 || page1[0].ID != "beta-2" || page1[1].ID != "alpha-2" {
		t.Errorf("page 1 = %#v, want beta-2 then alpha-2", page1)
	}
	if len(page2) != 2 || page2[0].ID != "beta-1" || page2[1].ID != "alpha-1" {
		t.Errorf("page 2 = %#v, want beta-1 then alpha-1", page2)
	}
}

func TestPruneOldTicks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Insert 5 ticks with explicit created_at to control ordering.
	for i, id := range []string{
		"alpha-2026-01-01-00-00-01",
		"alpha-2026-01-01-00-00-02",
		"alpha-2026-01-01-00-00-03",
		"alpha-2026-01-01-00-00-04",
		"alpha-2026-01-01-00-00-05",
	} {
		tk := &Tick{
			ID:          id,
			ProjectName: "alpha",
			Status:      StatusQueued,
			CreatedAt:   "2026-01-01T00:00:0" + string(rune('0'+i)) + "Z",
		}
		if err := CreateTick(ctx, db, tk); err != nil {
			t.Fatalf("CreateTick %s: %v", id, err)
		}
	}

	// Keep the 2 most recent.
	if err := PruneOldTicks(ctx, db, "alpha", 2); err != nil {
		t.Fatalf("PruneOldTicks: %v", err)
	}

	remaining, err := ListTicks(ctx, db, "alpha", 0)
	if err != nil {
		t.Fatalf("ListTicks after prune: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("after prune got %d, want 2", len(remaining))
	}
	// The two most recent by created_at are ...-04 and ...-05.
	want := map[string]bool{
		"alpha-2026-01-01-00-00-04": true,
		"alpha-2026-01-01-00-00-05": true,
	}
	for _, tk := range remaining {
		if !want[tk.ID] {
			t.Errorf("unexpected survivor %q", tk.ID)
		}
	}
}

func TestTick_ForeignKeyConstraint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// No project created — tick insert should fail with FK violation.
	tk := sampleTick("ghost")
	err := CreateTick(ctx, db, tk)
	if err == nil {
		t.Fatal("expected FK constraint error for tick on non-existent project, got nil")
	}
}

func TestTick_CheckConstraintStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("alpha")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := &Tick{
		ID:          "alpha-test",
		ProjectName: "alpha",
		Status:      TickStatus("bogus"),
	}
	err := CreateTick(ctx, db, tk)
	if err == nil {
		t.Fatal("expected CHECK constraint error for invalid status, got nil")
	}
}

func TestNextTickID_Format(t *testing.T) {
	id := NextTickID("myproj")
	// Format: myproj-YYYY-MM-DD-HH-mm-ss
	parts := strings.Split(id, "-")
	// Expect at least 7 parts: myproj, YYYY, MM, DD, HH, mm, ss.
	// (Project names may contain hyphens, so we check the tail.)
	if len(parts) < 7 {
		t.Fatalf("NextTickID %q has %d parts, want >= 7", id, len(parts))
	}
	tail := parts[len(parts)-6:]
	// Validate each tail part is a 2-digit (or 4-digit for year) numeric string.
	if len(tail[0]) != 4 {
		t.Errorf("year part = %q, want 4 digits", tail[0])
	}
	for _, p := range tail[1:] {
		if len(p) != 2 {
			t.Errorf("time part %q is not 2 digits", p)
		}
	}
	if !strings.HasPrefix(id, "myproj-") {
		t.Errorf("id %q does not start with 'myproj-'", id)
	}
}

// --- Event tests ----------------------------------------------------------

func TestLogEvent_AndList(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	e := &Event{
		Severity:  SeverityInfo,
		Component: "scheduler",
		Message:   "project registered",
		Details:   `{"project":"alpha"}`,
	}
	if err := LogEvent(ctx, db, e); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if e.ID == 0 {
		t.Error("event ID not set after insert")
	}

	got, err := ListEvents(ctx, db, "", "", 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Message != "project registered" {
		t.Errorf("message = %q", got[0].Message)
	}
}

func TestListEvents_FilterBySeverity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	events := []Event{
		{Severity: SeverityInfo, Component: "eval", Message: "info msg"},
		{Severity: SeverityMedium, Component: "eval", Message: "medium msg"},
		{Severity: SeverityHigh, Component: "eval", Message: "high msg"},
		{Severity: SeverityCritical, Component: "eval", Message: "critical msg"},
	}
	for i := range events {
		if err := LogEvent(ctx, db, &events[i]); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}

	tests := []struct {
		severity string
		want     int
	}{
		{string(SeverityInfo), 1},
		{string(SeverityHigh), 1},
		{"", 4}, // no filter
	}
	for _, tc := range tests {
		got, err := ListEvents(ctx, db, tc.severity, "", 0, 0)
		if err != nil {
			t.Fatalf("ListEvents severity=%q: %v", tc.severity, err)
		}
		if len(got) != tc.want {
			t.Errorf("severity=%q: got %d, want %d", tc.severity, len(got), tc.want)
		}
	}
}

func TestListEvents_FilterByComponent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for _, comp := range []string{"spawner", "packer", "spawner"} {
		if err := LogEvent(ctx, db, &Event{
			Severity: SeverityInfo, Component: comp, Message: "x",
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}

	got, err := ListEvents(ctx, db, "", "spawner", 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("spawner events = %d, want 2", len(got))
	}
}

func TestListEvents_Pagination(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := LogEvent(ctx, db, &Event{
			Severity: SeverityInfo, Component: "test", Message: "msg",
		}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
	}

	// limit=2 offset=0
	page1, err := ListEvents(ctx, db, "", "", 2, 0)
	if err != nil {
		t.Fatalf("ListEvents page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 = %d, want 2", len(page1))
	}

	// limit=2 offset=2
	page2, err := ListEvents(ctx, db, "", "", 2, 2)
	if err != nil {
		t.Fatalf("ListEvents page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 = %d, want 2", len(page2))
	}

	// page1 and page2 should not overlap
	if page1[0].ID == page2[0].ID {
		t.Errorf("pagination overlap: both pages start with id %d", page1[0].ID)
	}
}

func TestLogEvent_CheckConstraintSeverity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	e := &Event{
		Severity: EventSeverity("bogus"),
		Message:  "x",
	}
	err := LogEvent(ctx, db, e)
	if err == nil {
		t.Fatal("expected CHECK constraint error for invalid severity, got nil")
	}
}

// --- Namespace tests --------------------------------------------------------

func TestCreateNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns := &Namespace{
		ID:          "test-ns",
		Weight:      30,
		Reserved:    5,
		HardCap:     50,
		Enabled:     true,
		Description: "test namespace",
	}
	err := CreateNamespace(ctx, db, ns)
	if err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	// Verify it was created by reading it back.
	got, err := GetNamespace(ctx, db, "test-ns")
	if err != nil {
		t.Fatalf("GetNamespace after create: %v", err)
	}
	if got.Weight != 30 || got.Reserved != 5 || got.HardCap != 50 {
		t.Fatalf("expected weight=30 reserved=5 hard_cap=50, got weight=%d reserved=%d hard_cap=%d",
			got.Weight, got.Reserved, got.HardCap)
	}
	if !got.Enabled {
		t.Fatal("expected enabled=true")
	}
	if got.Description != "test namespace" {
		t.Fatalf("description = %q, want %q", got.Description, "test namespace")
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatal("CreatedAt/UpdatedAt should be auto-populated")
	}
}

func TestCreateNamespace_DuplicateID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns := &Namespace{ID: "dup-ns", Weight: 10}
	if err := CreateNamespace(ctx, db, ns); err != nil {
		t.Fatalf("first CreateNamespace: %v", err)
	}
	err := CreateNamespace(ctx, db, ns)
	if err == nil {
		t.Fatal("expected error on duplicate ID, got nil")
	}
}

func TestGetNamespace_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	_, err := GetNamespace(ctx, db, "no-such-ns")
	if !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("expected ErrNamespaceNotFound, got: %v", err)
	}
}

func TestGetNamespace_Found(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns := &Namespace{ID: "get-test", Weight: 50, Enabled: true}
	if err := CreateNamespace(ctx, db, ns); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	got, err := GetNamespace(ctx, db, "get-test")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if got.ID != "get-test" || got.Weight != 50 {
		t.Fatalf("id=%q weight=%d, want id=%q weight=50", got.ID, got.Weight, "get-test")
	}
	if !got.Enabled {
		t.Fatal("expected enabled=true")
	}
}

func TestListNamespaces_All(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Empty to start
	ns, err := ListNamespaces(ctx, db, false)
	if err != nil {
		t.Fatalf("ListNamespaces empty: %v", err)
	}
	if len(ns) != 0 {
		t.Fatalf("expected 0 namespaces, got %d", len(ns))
	}

	// Create two namespaces, one disabled.
	if err := CreateNamespace(ctx, db, &Namespace{ID: "a", Weight: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := CreateNamespace(ctx, db, &Namespace{ID: "b", Weight: 20, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	all, err := ListNamespaces(ctx, db, false)
	if err != nil {
		t.Fatalf("ListNamespaces all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 total, got %d", len(all))
	}
	// Should be ordered by id ASC.
	if all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("expected [a b] order, got [%s %s]", all[0].ID, all[1].ID)
	}
}

func TestListNamespaces_EnabledOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateNamespace(ctx, db, &Namespace{ID: "a", Weight: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := CreateNamespace(ctx, db, &Namespace{ID: "b", Weight: 20, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	enabled, err := ListNamespaces(ctx, db, true)
	if err != nil {
		t.Fatalf("ListNamespaces enabledOnly: %v", err)
	}
	if len(enabled) != 1 {
		t.Fatalf("expected 1 enabled namespace, got %d", len(enabled))
	}
	if enabled[0].ID != "a" {
		t.Fatalf("expected 'a', got %q", enabled[0].ID)
	}
}

func TestUpdateNamespace_AllFields(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateNamespace(ctx, db, &Namespace{ID: "upd", Weight: 10, Reserved: 2, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	w := 25
	r := 8
	c := 60
	e := false
	d := "updated desc"
	err := UpdateNamespace(ctx, db, "upd", NamespacePatch{
		Weight:      &w,
		Reserved:    &r,
		HardCap:     &c,
		Enabled:     &e,
		Description: &d,
	})
	if err != nil {
		t.Fatalf("UpdateNamespace: %v", err)
	}

	got, err := GetNamespace(ctx, db, "upd")
	if err != nil {
		t.Fatal(err)
	}
	if got.Weight != 25 || got.Reserved != 8 || got.HardCap != 60 {
		t.Fatalf("got weight=%d reserved=%d hard_cap=%d", got.Weight, got.Reserved, got.HardCap)
	}
	if got.Enabled {
		t.Fatal("expected enabled=false after update")
	}
	if got.Description != "updated desc" {
		t.Fatalf("description = %q", got.Description)
	}
}

func TestUpdateNamespace_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	w := 5
	err := UpdateNamespace(ctx, db, "no-such-ns", NamespacePatch{Weight: &w})
	if !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("expected ErrNamespaceNotFound, got: %v", err)
	}
}

func TestUpdateNamespace_Noop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateNamespace(ctx, db, &Namespace{ID: "noop", Weight: 10}); err != nil {
		t.Fatal(err)
	}

	// Empty patch should succeed (only UpdatedAt changes).
	if err := UpdateNamespace(ctx, db, "noop", NamespacePatch{}); err != nil {
		t.Fatalf("UpdateNamespace empty patch: %v", err)
	}

	got, err := GetNamespace(ctx, db, "noop")
	if err != nil {
		t.Fatal(err)
	}
	if got.Weight != 10 {
		t.Fatalf("weight changed to %d, want 10", got.Weight)
	}
}

func TestDeleteNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := CreateNamespace(ctx, db, &Namespace{ID: "to-delete", Weight: 5, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if err := DeleteNamespace(ctx, db, "to-delete"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	// Verify it's disabled.
	got, err := GetNamespace(ctx, db, "to-delete")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("expected enabled=false after soft delete")
	}
}

func TestDeleteNamespace_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	err := DeleteNamespace(ctx, db, "no-such-ns")
	if err == nil {
		t.Fatal("expected error on delete of missing namespace, got nil")
	}
}

// TestMigrate_FallbackChainColumns pins migration v14 (SCHED-GAP-064): the
// projects table must gain fallback_model/fallback_provider/no_global_fallback
// so fleet.toml fallback chains persist across restarts.
func TestMigrate_FallbackChainColumns(t *testing.T) {
	db := newTestDB(t)

	for _, col := range []string{"fallback_model", "fallback_provider", "no_global_fallback"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('projects') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(projects) for %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("projects.%s missing after Migrate (count=%d) — migration v14 not applied", col, n)
		}
	}
}

// TestProject_FallbackChainRoundTrip pins CreateProject/GetProject/UpdateProject
// round-tripping the SCHED-GAP-064 fields end-to-end.
func TestProject_FallbackChainRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := sampleProject("fallback-chain")
	p.FallbackModel = "fallback-m"
	p.FallbackProvider = "fallback-p"
	p.NoGlobalFallback = true
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := GetProject(ctx, db, "fallback-chain")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.FallbackModel != "fallback-m" || got.FallbackProvider != "fallback-p" || !got.NoGlobalFallback {
		t.Errorf("GetProject fallback fields = (%q, %q, %v), want (fallback-m, fallback-p, true)",
			got.FallbackModel, got.FallbackProvider, got.NoGlobalFallback)
	}

	// Partial update: clear the flag, keep the tiers.
	f := false
	if err := UpdateProject(ctx, db, "fallback-chain", ProjectUpdates{NoGlobalFallback: &f}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = GetProject(ctx, db, "fallback-chain")
	if err != nil {
		t.Fatalf("GetProject after update: %v", err)
	}
	if got.NoGlobalFallback {
		t.Error("NoGlobalFallback = true after clearing update, want false")
	}
	if got.FallbackProvider != "fallback-p" {
		t.Errorf("FallbackProvider = %q after unrelated update, want fallback-p (partial update must not clear it)", got.FallbackProvider)
	}

	// ListProjects must also surface the fields.
	all, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := false
	for _, pr := range all {
		if pr.Name == "fallback-chain" {
			found = true
			if pr.FallbackModel != "fallback-m" || pr.FallbackProvider != "fallback-p" {
				t.Errorf("ListProjects fallback fields = (%q, %q), want (fallback-m, fallback-p)",
					pr.FallbackModel, pr.FallbackProvider)
			}
		}
	}
	if !found {
		t.Error("ListProjects did not return fallback-chain")
	}
}
