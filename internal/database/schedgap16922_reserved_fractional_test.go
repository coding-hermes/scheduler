package database

import (
	"context"
	"testing"
)

func TestMigrationV75FractionalReserved(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	var columnType string
	if err := db.QueryRowContext(ctx, `SELECT type FROM pragma_table_info('namespaces') WHERE name = 'reserved'`).Scan(&columnType); err != nil {
		t.Fatalf("reserved column: %v", err)
	}
	if columnType != "REAL" {
		t.Fatalf("reserved column type = %q, want REAL", columnType)
	}

	var description string
	if err := db.QueryRowContext(ctx, `SELECT desc FROM migrations WHERE version = 75`).Scan(&description); err != nil {
		t.Fatalf("migration 75 record: %v", err)
	}
	if description == "" {
		t.Fatal("migration 75 has an empty description")
	}

	if err := CreateNamespace(ctx, db, &Namespace{ID: "fractional", Weight: 1, Reserved: 0.5, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	var storageType string
	var reserved float64
	if err := db.QueryRowContext(ctx, `SELECT typeof(reserved), reserved FROM namespaces WHERE id = 'fractional'`).Scan(&storageType, &reserved); err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if storageType != "real" || reserved != 0.5 {
		t.Fatalf("stored reserved = %s|%g, want real|0.5", storageType, reserved)
	}
}
