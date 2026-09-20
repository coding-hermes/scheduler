package database

import (
	"context"
	"database/sql"
	"fmt"
)

// ADV-R13 — host load/memory telemetry (measurement only).
//
// The scheduler persists one host_samples row per evaluation cycle so any
// future load threshold is derived from recorded history instead of
// fabricated on zero measurements. These helpers are the only read/write
// path for that table; NO admission-path code (packers, slot pool, load
// gate, spawner) consumes it — see load_telemetry.go for the scope
// boundary.

// HostSample is one persisted host load/memory reading. SampledAt is an
// RFC3339 UTC string (the canonical timestamp format for TEXT timestamp
// columns); Source records where the reading came from (e.g. "proc").
type HostSample struct {
	ID                int64
	SampledAt         string
	Load1             float64
	Load5             float64
	Load15            float64
	MemTotalBytes     int64
	MemAvailableBytes int64
	Source            string
}

// InsertHostSample persists one load/memory reading.
func InsertHostSample(ctx context.Context, db *sql.DB, s *HostSample) error {
	const q = `INSERT INTO host_samples
    (sampled_at, load1, load5, load15, mem_total_bytes, mem_available_bytes, source)
    VALUES (?,?,?,?,?,?,?)`
	res, err := db.ExecContext(ctx, q,
		s.SampledAt, s.Load1, s.Load5, s.Load15,
		s.MemTotalBytes, s.MemAvailableBytes, s.Source)
	if err != nil {
		return fmt.Errorf("insert host sample: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("host sample last insert id: %w", err)
	}
	s.ID = id
	return nil
}

// LatestHostSample returns the newest persisted sample. It returns
// sql.ErrNoRows when no sample has been recorded yet — the caller must
// surface the gap honestly (dashboard shows "unavailable"), never
// synthesize a zero row.
func LatestHostSample(ctx context.Context, db *sql.DB) (*HostSample, error) {
	const q = `SELECT id, sampled_at, load1, load5, load15,
    mem_total_bytes, mem_available_bytes, source
    FROM host_samples ORDER BY id DESC LIMIT 1`
	var s HostSample
	err := db.QueryRowContext(ctx, q).Scan(
		&s.ID, &s.SampledAt, &s.Load1, &s.Load5, &s.Load15,
		&s.MemTotalBytes, &s.MemAvailableBytes, &s.Source)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("latest host sample: %w", err)
	}
	return &s, nil
}
