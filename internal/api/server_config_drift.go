package api

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// SCHED-GAP-219 — the drift tripwire moves from the retired ops script
// (fleet-cooldown-policy.py --verify) into the API, so the surface is the
// daemon, not an ops script. The check is READ-ONLY: it compares every
// pinned project row (cooldown_pin_s IS NOT NULL) against the fleet.toml
// block and reports a mismatch when the toml's cooldown_s for a pinned
// project differs from the DB pin.
//
// What drifted in the old model is now impossible by construction (the
// loader imports pins, never applies toml values over DB state), so this
// block answers the operator question the old tripwire carried: "does the
// seed file still agree with the authority?" A toml value BELOW the DB pin
// is informational, not a violation — the pin is the floor and the file is
// a seed — but it is surfaced so a stale seed is visible before somebody
// treats its numbers as current policy.
//
// Fail-open everywhere: a missing/unreadable/unparseable toml yields a
// block with present=false and a reason — never an error response, never a
// fabricated "OK".

// fleetTomlSeed is the subset of the seed file the drift probe reads.
type fleetTomlSeed struct {
	Projects []struct {
		Name      string `toml:"name"`
		CooldownS int    `toml:"cooldown_s"`
	} `toml:"projects"`
}

// configDriftBlock builds the /api/v1/status config_drift block: the seeded
// fleet.toml path, whether it was readable, and the per-project comparison
// of DB operator pins against the toml cooldown values. db may be nil (the
// loop-less test servers) — the block then reports present=false.
func (s *Server) configDriftBlock(ctx context.Context, db *sql.DB) map[string]interface{} {
	block := map[string]interface{}{
		"present": false,
	}

	// Resolve the seed path the same way the daemon's --config resolves:
	// main.go wires the resolved path into SetFleetTomlPath when known; the
	// fallback is the historical default (~/.hermes/fleet.toml).
	path := s.fleetTomlPath
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = filepath.Join(home, ".hermes", "fleet.toml")
		}
	}
	block["path"] = path

	raw, err := os.ReadFile(path)
	if err != nil {
		block["reason"] = "seed fleet.toml not readable: " + err.Error()
		return block
	}
	var seed fleetTomlSeed
	if _, err := toml.Decode(string(raw), &seed); err != nil {
		block["reason"] = "seed fleet.toml unparseable: " + err.Error()
		return block
	}
	block["present"] = true
	block["projects_seeded"] = len(seed.Projects)

	tomlCD := map[string]int{}
	for _, p := range seed.Projects {
		if p.Name != "" && p.CooldownS > 0 {
			tomlCD[p.Name] = p.CooldownS
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT name, cooldown_s, cooldown_pin_s FROM projects WHERE cooldown_pin_s IS NOT NULL`)
	if err != nil {
		block["reason"] = "pin query failed: " + err.Error()
		return block
	}
	defer rows.Close()

	type driftEntry struct {
		DBPin int `json:"db_pin"`
		Toml  int `json:"toml_cooldown_s"`
	}
	mismatches := map[string]driftEntry{}
	pinnedCount := 0
	for rows.Next() {
		var name string
		var cd int
		var pin sql.NullInt64
		if err := rows.Scan(&name, &cd, &pin); err != nil {
			continue
		}
		if !pin.Valid {
			continue
		}
		pinnedCount++
		tc, seeded := tomlCD[name]
		if !seeded {
			// A DB pin with no toml entry: the seed predates the pin. Not
			// a violation (the DB is the authority) but visible drift.
			mismatches[name] = driftEntry{DBPin: int(pin.Int64), Toml: 0}
			continue
		}
		if tc != int(pin.Int64) {
			mismatches[name] = driftEntry{DBPin: int(pin.Int64), Toml: tc}
		}
	}
	block["pinned_projects"] = pinnedCount
	// "mismatches" keeps the old tripwire's vocabulary (MISMATCH <project>):
	// each entry is {db_pin, toml_cooldown_s}. Empty = every pin agrees with
	// the seed file (or the seed has no number for it and the pin is the
	// authority — reported for visibility, still not a violation).
	block["mismatches"] = mismatches
	return block
}

// SetFleetTomlPath records the resolved --config seed path so the config_drift
// block probes the file the daemon actually booted from. Called by main.go;
// tests construct the Server without it (the historical default applies).
func (s *Server) SetFleetTomlPath(path string) {
	s.fleetTomlPath = path
}
