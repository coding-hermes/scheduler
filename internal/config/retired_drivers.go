package config

import "strings"

// retiredDriverScripts are the dagger-era tick drivers the fleet no longer
// runs (SCHED-GAP-150). A project row whose `command` names one of these looks
// schedulable while driving a script that is gone — the 130 legacy command
// rows cleared 2026-09-17 (pm 42 / qa 31 / sync 26 / dogfood 31).
//
// This is the config-loader twin of internal/api/retired_drivers.go. The two
// cannot be one Go definition: internal/api imports internal/scheduler, which
// imports internal/config, so config -> api is an import cycle. The list is
// therefore mirrored here — same 5 names, same spelling, same order — and the
// mirror is pinned by TestSCHEDGAP150_RetiredDriverListParityWithAPIGate,
// which parses both sources and fails the moment either is edited alone.
// ops/check-fleet-invariants.py carries the third (fleet-wide) copy, pinned to
// the api list by TestCheckFleetInvariants_RetiredDriversMatchPythonGate.
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
