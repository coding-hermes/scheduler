package database

import (
	"context"
	"database/sql"
	"fmt"
)

// CooldownResidue is one ENABLED lane whose live cooldown_s sits strictly
// BELOW its own stored operator pin (cooldown_pin_s) — the "wake residue,
// NOT intent" class of SCHED-GAP-1670. While the pin outranks the live
// value inside effectiveCooldown (SCHED-GAP-1661), a residue row is still a
// defect: every surface that reads cooldown_s directly (dashboards, ops
// tooling, the API payload) shows a hotter lane than the operator pinned,
// and the drift is durable because the loader's equal-or-higher guard skips
// the re-import on later boots.
type CooldownResidue struct {
	Name      string `json:"name"`
	CooldownS int    `json:"cooldown_s"`
	PinS      int    `json:"pin_s"`
	PinBy     string `json:"pin_by"`
}

// DetectCooldownResidue reports whether an ENABLED lane's live cooldown_s is
// strictly below its own stored pin (cooldown_pin_s IS NOT NULL and > the
// live value). A NULL pin, a disabled row, and live >= pin are all clean.
//
// Read-only by contract: this is the residue DETECTOR — it never mutates.
// The reconciliation write is the pin-snap inside SetCooldownPin / the API
// PUT path, never this function.
func DetectCooldownResidue(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, cooldownResidueCountQuery).Scan(&n); err != nil {
		return false, fmt.Errorf("cooldown residue detection: %w", err)
	}
	return n > 0, nil
}

// ListCooldownResidue returns every ENABLED lane whose live cooldown_s sits
// strictly below its own stored pin, ordered by name for stable output.
// Read-only; pairs with DetectCooldownResidue for reporting surfaces that
// need the lanes themselves.
func ListCooldownResidue(ctx context.Context, db *sql.DB) ([]CooldownResidue, error) {
	rows, err := db.QueryContext(ctx, `
SELECT name, cooldown_s, cooldown_pin_s, COALESCE(cooldown_pin_by, '')
FROM projects
WHERE enabled = 1
  AND cooldown_pin_s IS NOT NULL
  AND cooldown_pin_s > cooldown_s
ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("cooldown residue list: %w", err)
	}
	defer rows.Close()

	var out []CooldownResidue
	for rows.Next() {
		var r CooldownResidue
		if err := rows.Scan(&r.Name, &r.CooldownS, &r.PinS, &r.PinBy); err != nil {
			return nil, fmt.Errorf("cooldown residue list: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cooldown residue list: rows: %w", err)
	}
	return out, nil
}

// cooldownResidueCountQuery is the single-row residue census shared by the
// detector and the daily-report surface. One subquery — the daemon's
// zero-select monitoring law (GAP-043) keeps per-request scans to a minimum.
const cooldownResidueCountQuery = `
SELECT COUNT(1)
FROM projects
WHERE enabled = 1
  AND cooldown_pin_s IS NOT NULL
  AND cooldown_pin_s > cooldown_s`
