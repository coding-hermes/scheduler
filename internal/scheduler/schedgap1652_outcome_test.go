package scheduler

import "testing"

// TestTerminalOutcomeFromArtifacts pins the SCHED-GAP-1652 contract (Bane's fix
// order, item 1): the outcome column is derived from OBSERVED ARTIFACTS, never
// restated from the fact that the process exited cleanly.
//
// Measured 2026-09-27 over 3,717 ticks: 2,533 carried outcome='committed' with
// commits=0, and 27% of those had in fact committed. The old mapping returned the
// literal "committed" for every TickCompleted, so an empty tick and a tick that
// shipped a feature were recorded identically.
func TestTerminalOutcomeFromArtifacts(t *testing.T) {
	cases := []struct {
		name string
		in   TickOutcome
		want string
	}{
		{
			"completed with no commit and no changed file is NOT a commit",
			TickOutcome{Status: TickCompleted, Commits: 0, FilesChanged: 0},
			"dry_run",
		},
		{
			"completed with a worker commit is committed",
			TickOutcome{Status: TickCompleted, Commits: 1, FilesChanged: 2},
			"committed",
		},
		{
			"completed with changed files but no commit counts as an artifact",
			TickOutcome{Status: TickCompleted, Commits: 0, FilesChanged: 3},
			"committed",
		},
		{
			// The -1 sentinel means "could not measure". It must never be read as
			// an artifact, or an unmeasurable tick would claim a commit it may not
			// have made (and a measured zero would be indistinguishable again).
			"the -1 not-measured sentinel is NOT an artifact",
			TickOutcome{Status: TickCompleted, Commits: -1, FilesChanged: -1},
			"dry_run",
		},
		{"failed stays failed", TickOutcome{Status: TickFailed}, "failed"},
		{"timeout stays timeout", TickOutcome{Status: TickTimeout}, "timeout"},
		{"deferred stays deferred", TickOutcome{Status: TickDeferred}, "deferred"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalOutcome(tc.in); got != tc.want {
				t.Fatalf("terminalOutcome(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTerminalOutcomeIsInSchemaVocabulary guards the constraint that made the -1
// sentinel necessary: the schema CHECK (and the metrics aggregator) only accept
// ('committed','dry_run','failed','timeout','deferred'), so no outcome value may
// ever be invented without a migration.
func TestTerminalOutcomeIsInSchemaVocabulary(t *testing.T) {
	allowed := map[string]bool{
		"committed": true, "dry_run": true, "failed": true, "timeout": true, "deferred": true,
	}
	for _, st := range []TickStatus{TickCompleted, TickFailed, TickTimeout, TickDeferred} {
		for _, o := range []TickOutcome{
			{Status: st, Commits: 0, FilesChanged: 0},
			{Status: st, Commits: 2, FilesChanged: 4},
			{Status: st, Commits: -1, FilesChanged: -1},
		} {
			got := terminalOutcome(o)
			if !allowed[got] {
				t.Fatalf("terminalOutcome(%+v) = %q, which the schema CHECK would reject", o, got)
			}
		}
	}
}
