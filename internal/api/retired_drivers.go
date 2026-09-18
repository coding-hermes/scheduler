package api

import (
	"fmt"
	"strings"
)

// retiredDriverScripts are the dagger-era tick drivers the fleet no longer
// runs: every lane executes its own foreman skill, so a project row whose
// `command` names one of these looks schedulable while driving a script that
// is gone (the 130 legacy command rows cleared 2026-09-17).
//
// RETIRED_DRIVERS: this list MUST stay identical — same 5 names, same
// spelling, same order — to the RETIRED_DRIVERS tuple in
// ops/check-fleet-invariants.py (check #4, class "executors"). The Python
// tuple is the fleet-wide backstop that scans every ENABLED row; this list is
// the enable-path gate that refuses to write such a command in the first
// place. Parity is pinned by
// TestCheckFleetInvariants_RetiredDriversMatchPythonGate.
var retiredDriverScripts = []string{
	"pm-standin-tick.sh",
	"qa-scheduler-tick.sh",
	"sync-scheduler-tick.sh",
	"dogfood-scheduler-tick.sh",
	"dagger-role-tick.sh",
}

// retiredDriverInCommand returns the first retired driver named inside cmd, or
// "" when the command drives no retired driver.
func retiredDriverInCommand(cmd string) string {
	for _, name := range retiredDriverScripts {
		if strings.Contains(cmd, name) {
			return name
		}
	}
	return ""
}

// isRetiredCommand reports whether cmd drives a retired driver script.
//
// Deliberately stricter than the Python gate in exactly one way: that gate
// tolerates a row whose command/prompt carries a "RETIRED" or "do NOT run"
// marker (a documentation relic it must not flag), while an enable-path write
// has no reason to name a retired driver at all — here every mention is a
// reject, so this gate can never be talked out of its verdict by wording.
func isRetiredCommand(cmd string) bool {
	return retiredDriverInCommand(cmd) != ""
}

// retiredCommandError is the actionable 400 body for a rejected command. It
// names the offending driver so an operator can fix it from the response
// alone.
func retiredCommandError(cmd string) string {
	return fmt.Sprintf("command contains retired driver '%s'; remove the custom command or use a supported executor",
		retiredDriverInCommand(cmd))
}
