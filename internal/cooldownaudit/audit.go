// Package cooldownaudit implements the READ-ONLY wake-residue detector
// (SCHED-GAP-1670).
//
// Wake residue is a lane whose LIVE cooldown sits BELOW the lane's own
// operator pin: the lane runs hotter than the cadence the operator recorded
// for it. The fleet's policy script names the class itself — "any live
// cooldown BELOW the fast tier is wake residue, NOT intent: REVERT to the
// operator pin" — and it is expensive: the 2026-09-28 measurement found
// python-audit-{lint,typing,security,complexity} at 900s against an 86400s pin
// burning ~25 zero-commit ticks each per day, and fleet-wide 78% of completed
// ticks produced no commit.
//
// Two things were missing, and this package is the first:
//
//  1. NOTHING ran the correction on a schedule. The policy tool's --apply path
//     MUTATES fleet state, so it must stay an operator action; what a cron can
//     own is a READ-ONLY detector that reports residue daily and exits
//     nonzero, so the drift is visible within a day instead of accumulating
//     until somebody looks.
//  2. The loader's pin-snap did not hold when the pin arrived via a fleet.toml
//     import (see SetCooldownPin in internal/database) — a lane whose file
//     carries a weekly cadence could stay live at a daily one.
//
// This package NEVER writes: it reads rows, classifies them, and reports. The
// mutating remedy remains ~/.hermes/scripts/fleet-cooldown-policy.py --apply,
// which is deliberately not scheduled.
//
// The two pin sources are the ones the policy tool uses: the live DB column
// `projects.cooldown_pin_s` (the authority, and where a fleet.toml import
// lands) and the `cooldown_s` of a `[[projects]]` block in fleet.toml (the
// seed). The DB pin wins when both are present; the file pin fills a lane
// whose row carries no pin, so a lane whose file declares a weekly cadence is
// reported even when the import never happened.
package cooldownaudit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Pin provenance for a Finding. The DB pin is the cooldown authority; the
// fleet.toml value is the seed a boot imports (or would import).
const (
	PinSourceDB        = "db"
	PinSourceFleetToml = "fleet-toml"
)

// Lane is one scheduling lane as the detector sees it: the live cooldown the
// packer honours, and the operator pin (nil = the lane carries no pin, so it
// cannot be in the residue class).
type Lane struct {
	Name string
	// CooldownS is the live cooldown — what the lane actually runs at.
	CooldownS int
	// PinS is the operator pin, nil when no pin is set on the row.
	PinS *int
	// Enabled is the row's enabled flag. A disabled lane is not scheduled, so
	// it is never residue.
	Enabled bool
	// PinSource records where PinS came from (PinSourceDB / PinSourceFleetToml
	// after ApplyFleetPins). Empty is reported as PinSourceDB.
	PinSource string
}

// Finding is one residue lane: the live cooldown, the pin it sits below, and
// how far below.
type Finding struct {
	Name string `json:"name"`
	// LiveS is the live cooldown_s the lane runs at today.
	LiveS int `json:"live_s"`
	// PinS is the operator pin the live value should never sit below.
	PinS int `json:"pin_s"`
	// DeltaS is pin_s - live_s, always > 0.
	DeltaS int `json:"delta_s"`
	// PinSource is PinSourceDB or PinSourceFleetToml.
	PinSource string `json:"pin_source"`
}

// DetectResidue returns the residue class: one Finding per ENABLED lane whose
// live cooldown sits below its own operator pin, sorted by lane name so two
// runs over the same state produce byte-identical output.
//
// A live cooldown ABOVE the pin is not residue — the pin is a floor, not a
// cap (a slower lane spends less, and the policy only ever promotes an idle
// lane to the completed tier with an explicit pin update).
func DetectResidue(lanes []Lane) []Finding {
	findings := make([]Finding, 0)
	for _, lane := range lanes {
		if !lane.Enabled || lane.PinS == nil {
			continue
		}
		pin := *lane.PinS
		if pin <= 0 || lane.CooldownS >= pin {
			continue
		}
		source := lane.PinSource
		if source == "" {
			source = PinSourceDB
		}
		findings = append(findings, Finding{
			Name:      lane.Name,
			LiveS:     lane.CooldownS,
			PinS:      pin,
			DeltaS:    pin - lane.CooldownS,
			PinSource: source,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Name < findings[j].Name })
	return findings
}

// LoadLanes reads every lane row from the scheduler database. The query is a
// single SELECT over `projects` — the same table the policy tool reads through
// /api/v1/projects, minus that endpoint's pagination trap (SCHED-GAP-1622: a
// bare GET hides ~60% of a 500-lane fleet behind a default limit). A read
// through this function is always the whole fleet.
func LoadLanes(ctx context.Context, db *sql.DB) ([]Lane, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name, cooldown_s, cooldown_pin_s, enabled FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("read lanes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	lanes := make([]Lane, 0, 512)
	for rows.Next() {
		var lane Lane
		var pin sql.NullInt64
		if err := rows.Scan(&lane.Name, &lane.CooldownS, &pin, &lane.Enabled); err != nil {
			return nil, fmt.Errorf("scan lane: %w", err)
		}
		if pin.Valid {
			p := int(pin.Int64)
			lane.PinS = &p
			lane.PinSource = PinSourceDB
		}
		lanes = append(lanes, lane)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lanes: %w", err)
	}
	return lanes, nil
}

// ApplyFleetPins returns a copy of lanes with the fleet.toml seed pins filling
// any lane whose row carries no pin. The DB pin always wins — the seed is the
// file a boot imports, and the import never lowers a pin, so a DB pin is at
// least the file value. The input slice is not modified.
func ApplyFleetPins(lanes []Lane, pins map[string]int) []Lane {
	out := make([]Lane, len(lanes))
	copy(out, lanes)
	if len(pins) == 0 {
		return out
	}
	for i := range out {
		if out[i].PinS != nil {
			continue
		}
		seed, ok := pins[out[i].Name]
		if !ok || seed <= 0 {
			continue
		}
		p := seed
		out[i].PinS = &p
		out[i].PinSource = PinSourceFleetToml
	}
	return out
}

// fleetSeed is the subset of the seed file the detector reads. Namespace
// blocks and every other project key are ignored.
type fleetSeed struct {
	Projects []struct {
		Name      string `toml:"name"`
		CooldownS int    `toml:"cooldown_s"`
	} `toml:"projects"`
}

// FleetPins reads {lane name: cooldown_s} from a fleet.toml seed file. Only
// explicit positive cooldown_s values are returned: a keyless project entry
// carries no cadence intent, and 0 is not a value a pin can hold. A missing or
// unparseable file is an error (never a silently empty pin set).
func FleetPins(path string) (map[string]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet.toml %s: %w", path, err)
	}
	var seed fleetSeed
	if _, err := toml.Decode(string(raw), &seed); err != nil {
		return nil, fmt.Errorf("parse fleet.toml %s: %w", path, err)
	}
	pins := make(map[string]int, len(seed.Projects))
	for _, p := range seed.Projects {
		if p.Name == "" || p.CooldownS <= 0 {
			continue
		}
		pins[p.Name] = p.CooldownS
	}
	return pins, nil
}

// Report renders the human-readable report for findings and the number of
// enabled lanes scanned. It always states the scanned count, so "0 residue"
// is never confused with "nothing was read".
func Report(findings []Finding, enabledScanned int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "scanned %d enabled lane(s)\n", enabledScanned)
	if len(findings) == 0 {
		fmt.Fprintf(&b, "OK: no enabled lane sits below its own cooldown pin\n")
		return b.String()
	}
	fmt.Fprintf(&b, "ALERT: %d enabled lane(s) live BELOW their own cooldown pin (wake residue)\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "  %-40s live %8ds  pin %8ds  delta %8ds  (%s)\n",
			f.Name, f.LiveS, f.PinS, f.DeltaS, f.PinSource)
	}
	b.WriteString("read-only report: no state was changed; the mutating remedy is\n" +
		"  ~/.hermes/scripts/fleet-cooldown-policy.py --apply (operator action, never scheduled)\n")
	return b.String()
}
