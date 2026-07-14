package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNamespaceNotFound is returned when a namespace lookup or update targets
// an id that does not exist in the namespaces table.
var ErrNamespaceNotFound = errors.New("namespace not found")

// CreateNamespace inserts a new namespace row. CreatedAt and UpdatedAt are
// set automatically if empty.
func CreateNamespace(ctx context.Context, db *sql.DB, ns *Namespace) error {
	if ns.CreatedAt == "" {
		ns.CreatedAt = nowUTC()
	}
	if ns.UpdatedAt == "" {
		ns.UpdatedAt = ns.CreatedAt
	}
	const q = `INSERT INTO namespaces
(id, weight, reserved, hard_cap, enabled, description, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?)`
	_, err := db.ExecContext(ctx, q,
		ns.ID, ns.Weight, ns.Reserved, ns.HardCap, boolToInt(ns.Enabled),
		nullableString(ns.Description), ns.CreatedAt, ns.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create namespace %q: %w", ns.ID, err)
	}
	return nil
}

// GetNamespace loads a single namespace by id. Returns ErrNamespaceNotFound
// if no row matches.
func GetNamespace(ctx context.Context, db *sql.DB, id string) (*Namespace, error) {
	const q = `SELECT id, weight, reserved, hard_cap, enabled, COALESCE(description,''), created_at, updated_at
FROM namespaces WHERE id = ?`
	var ns Namespace
	var enabled int
	err := db.QueryRowContext(ctx, q, id).Scan(
		&ns.ID, &ns.Weight, &ns.Reserved, &ns.HardCap, &enabled,
		&ns.Description, &ns.CreatedAt, &ns.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrNamespaceNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace %q: %w", id, err)
	}
	ns.Enabled = enabled != 0
	return &ns, nil
}

// ListNamespaces returns all namespaces, ordered by id. If enabledOnly is
// true, only enabled=1 rows are returned.
func ListNamespaces(ctx context.Context, db *sql.DB, enabledOnly bool) ([]Namespace, error) {
	q := `SELECT id, weight, reserved, hard_cap, enabled, COALESCE(description,''), created_at, updated_at
FROM namespaces`
	if enabledOnly {
		q += " WHERE enabled = 1"
	}
	q += " ORDER BY id ASC"

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()

	var out []Namespace
	for rows.Next() {
		var ns Namespace
		var enabled int
		if err := rows.Scan(
			&ns.ID, &ns.Weight, &ns.Reserved, &ns.HardCap, &enabled,
			&ns.Description, &ns.CreatedAt, &ns.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan namespace row: %w", err)
		}
		ns.Enabled = enabled != 0
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate namespace rows: %w", err)
	}
	return out, nil
}

// UpdateNamespace applies the given patch to the namespace named id. Only
// non-nil fields in patch are written; UpdatedAt is always refreshed.
func UpdateNamespace(ctx context.Context, db *sql.DB, id string, patch NamespacePatch) error {
	setClauses := []string{"updated_at = ?"}
	args := []any{nowUTC()}

	if patch.Weight != nil {
		setClauses = append(setClauses, "weight = ?")
		args = append(args, *patch.Weight)
	}
	if patch.Reserved != nil {
		setClauses = append(setClauses, "reserved = ?")
		args = append(args, *patch.Reserved)
	}
	if patch.HardCap != nil {
		setClauses = append(setClauses, "hard_cap = ?")
		args = append(args, *patch.HardCap)
	}
	if patch.Enabled != nil {
		setClauses = append(setClauses, "enabled = ?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	if patch.Description != nil {
		setClauses = append(setClauses, "description = ?")
		args = append(args, *patch.Description)
	}

	args = append(args, id)
	q := "UPDATE namespaces SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update namespace %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for namespace %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNamespaceNotFound, id)
	}
	return nil
}

// DeleteNamespace soft-deletes a namespace by setting enabled=0. The row
// is retained so historical namespace_ticks remain referentially valid.
func DeleteNamespace(ctx context.Context, db *sql.DB, id string) error {
	return UpdateNamespace(ctx, db, id, NamespacePatch{Enabled: BoolPtr(false)})
}
