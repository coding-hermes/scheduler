package main

import (
	"context"
	"database/sql"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
)

func TestPrintStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	if err := database.CreateProject(ctx, db, &database.Project{Name: "alpha", RepoURL: "local:/tmp", Workdir: "/tmp", Enabled: true, Weight: 10, Priority: 5}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := database.CreateProject(ctx, db, &database.Project{Name: "beta", RepoURL: "local:/tmp", Workdir: "/tmp", Enabled: false, Weight: 10, Priority: 5}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var fleetCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&fleetCount); err != nil {
		t.Fatalf("query projects: %v", err)
	}
	var enabledCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE enabled=1`).Scan(&enabledCount); err != nil {
		t.Fatalf("query enabled: %v", err)
	}
	if fleetCount != 2 || enabledCount != 1 {
		t.Fatalf("expected 2 projects with 1 enabled, got %d/%d", fleetCount, enabledCount)
	}

	var runningCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&runningCount); err != nil {
		t.Fatalf("query running ticks: %v", err)
	}
	if runningCount != 0 {
		t.Fatalf("expected 0 active ticks, got %d", runningCount)
	}
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := database.InitDB(dir + "/test.db")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return db
}
