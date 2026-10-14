// Command cooldown-residue is the READ-ONLY wake-residue detector
// (SCHED-GAP-1670).
//
// A "residue" lane is an enabled lane whose LIVE cooldown sits below its own
// operator pin — the lane runs hotter than the cadence recorded for it. The
// fleet's correction tool (~/.hermes/scripts/fleet-cooldown-policy.py) has
// always been able to revert the class, but nothing ran it on a schedule: its
// only useful path MUTATES fleet state, so it must stay an operator action.
// This command is the piece that can be scheduled instead — it reads the same
// two sources (the scheduler DB rows and the fleet.toml seed), classifies,
// prints, and exits 0 (clean) / 1 (residue found) / 2 (could not read). It
// never writes: the DB handle is opened mode=ro (database.OpenReadOnly), no
// migration runs, and no PUT is ever issued.
//
// Daily scheduling lives in deploy/cooldown-residue.{service,timer} plus the
// wrapper scripts/cooldown-residue-detector.sh.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/coding-hermes/scheduler/internal/cooldownaudit"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/version"
)

// Exit codes: the detector is a monitoring surface, so "found residue" must be
// distinguishable from "could not look".
const (
	exitClean   = 0
	exitResidue = 1
	exitError   = 2
)

// jsonReport is the machine-readable shape: what was read, and what was found.
type jsonReport struct {
	DB        string                  `json:"db"`
	FleetToml string                  `json:"fleet_toml"`
	Scanned   int                     `json:"enabled_lanes_scanned"`
	Residue   []cooldownaudit.Finding `json:"residue"`
	Clean     bool                    `json:"clean"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the detector and returns the process exit code. argv excludes
// the program name; stdout/stderr are injected so the exit-code policy and the
// report are testable without a subprocess.
func run(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cooldown-residue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"),
		"scheduler database to read (read-only: never written, never created)")
	tomlPath := fs.String("fleet-toml", os.ExpandEnv("$HOME/.hermes/fleet.toml"),
		"fleet.toml seed to read pins from; a missing file only disables the seed-pin fallback")
	asJSON := fs.Bool("json", false, "emit the findings as a JSON object instead of the text report")
	quiet := fs.Bool("quiet", false, "print nothing when the fleet is clean (the exit code still reports)")
	showVersion := fs.Bool("version", false, "print version/build info and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\n", "cooldown-residue")
		_, _ = fmt.Fprintln(stderr, "Read-only wake-residue detector (SCHED-GAP-1670): reports every enabled lane")
		_, _ = fmt.Fprintln(stderr, "whose live cooldown sits below its own operator pin. Exit 0 = clean, 1 =")
		_, _ = fmt.Fprintln(stderr, "residue found, 2 = could not read state. Mutates nothing.")
		_, _ = fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return exitError
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "cooldown-residue %s (commit: %s, built: %s)\n",
			version.Current(), version.CurrentCommit(), version.CurrentBuildDate())
		return exitClean
	}

	ctx := context.Background()
	db, err := database.OpenReadOnly(*dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cooldown-residue: %v\n", err)
		return exitError
	}
	defer func() { _ = db.Close() }()

	lanes, err := cooldownaudit.LoadLanes(ctx, db)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cooldown-residue: %s: %v\n", *dbPath, err)
		return exitError
	}
	if len(lanes) == 0 {
		// Fail closed: a clean report from an empty read is the worst possible
		// verdict — it is indistinguishable from a healthy fleet.
		_, _ = fmt.Fprintf(stderr, "cooldown-residue: refusing to report clean — 0 lanes read from %s (wrong database?)\n", *dbPath)
		return exitError
	}

	pins, pinErr := cooldownaudit.FleetPins(*tomlPath)
	if pinErr != nil {
		_, _ = fmt.Fprintf(stderr, "cooldown-residue: WARNING: %v — continuing with DB pins only\n", pinErr)
	}
	lanes = cooldownaudit.ApplyFleetPins(lanes, pins)
	findings := cooldownaudit.DetectResidue(lanes)

	enabled := 0
	for _, lane := range lanes {
		if lane.Enabled {
			enabled++
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jsonReport{
			DB:        *dbPath,
			FleetToml: *tomlPath,
			Scanned:   enabled,
			Residue:   findings,
			Clean:     len(findings) == 0,
		}); err != nil {
			_, _ = fmt.Fprintf(stderr, "cooldown-residue: encode report: %v\n", err)
			return exitError
		}
	} else if len(findings) > 0 || !*quiet {
		_, _ = fmt.Fprintf(stdout, "cooldown-residue: db=%s\ncooldown-residue: fleet.toml=%s\n", *dbPath, *tomlPath)
		_, _ = fmt.Fprint(stdout, cooldownaudit.Report(findings, enabled))
	}

	if len(findings) > 0 {
		return exitResidue
	}
	return exitClean
}
