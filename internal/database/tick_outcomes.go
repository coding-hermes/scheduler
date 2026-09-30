package database

import (
	"context"
	"database/sql"
	"fmt"
)

// SCHED-GAP-1655 no-work accounting — the read side.
//
// The outcome column is the one place a tick's terminal verdict lives, and
// migration v55 widened its CHECK with 'no_work' so a zero-tool-call tick
// records its own verdict instead of collapsing into 'dry_run'. This file
// aggregates that column per lane so the waste is measurable per project
// (the row's deliverable 4), following the CountProjectDispatchReasons
// pattern exactly: every vocabulary bucket present and zero-filled, the
// query through the (project_name, spawned_at) covering-index prefix,
// status-filtered to terminal ticks.

// ProjectOutcomeVocabulary is the frozen ticks.outcome vocabulary in
// reporting order — the schema CHECK's values (the same set the metrics
// aggregator's by_outcome map seeds). CountProjectOutcomes zero-fills it so
// a consumer can render the full split without merging its own keys.
var ProjectOutcomeVocabulary = []string{
	string(OutcomeCommitted),
	string(OutcomeDryRun),
	string(OutcomeFailed),
	string(OutcomeTimeout),
	"deferred",
	string(OutcomeAbortedNoArtifact),
	string(OutcomeNoWork),
}

// OutcomeSplit is one lane's terminal ticks split by outcome
// (SCHED-GAP-1655). Total counts every terminal tick with an outcome
// recorded; Unset counts terminal ticks whose outcome is still NULL —
// rows that never reached the finalization stamp, i.e. absence of
// MEASUREMENT, never absence of work (the DispatchCount.Coverage
// convention). NoWork is the zero-tool-call verdict the row's waste
// metric reads; Buckets carries the full zero-filled vocabulary.
type OutcomeSplit struct {
	Total   int            `json:"total"`   // terminal ticks with an outcome recorded
	NoWork  int            `json:"no_work"` // the zero-tool-call verdict
	Unset   int            `json:"unset"`   // terminal ticks with no outcome (unmeasured)
	Buckets map[string]int `json:"buckets"` // every outcome vocabulary value, zero-filled
}

// CountProjectOutcomes aggregates one project's TERMINAL ticks by outcome
// (SCHED-GAP-1655). Every vocabulary bucket is present — zero-filled — so
// a consumer can render the full split without merging its own keys;
// Unset counts terminal rows whose outcome is still NULL. Best-effort by
// contract: a failed aggregate is returned as an error the caller logs,
// never a lifecycle gate.
func CountProjectOutcomes(ctx context.Context, db *sql.DB, project string) (OutcomeSplit, error) {
	split := OutcomeSplit{Buckets: make(map[string]int, len(ProjectOutcomeVocabulary))}
	for _, o := range ProjectOutcomeVocabulary {
		split.Buckets[o] = 0
	}
	rows, err := db.QueryContext(ctx, `
		SELECT outcome, COUNT(*)
		FROM ticks
		WHERE project_name = ? AND status IN ('completed', 'failed', 'timeout', 'deferred')
		GROUP BY outcome
	`, project)
	if err != nil {
		return split, fmt.Errorf("count outcomes for %q: %w", project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			outcome sql.NullString
			n       int
		)
		if err := rows.Scan(&outcome, &n); err != nil {
			return split, fmt.Errorf("scan outcome row for %q: %w", project, err)
		}
		if !outcome.Valid {
			// Terminal tick with no outcome: never reached the
			// finalization stamp. Unmeasured, not "no work".
			split.Unset += n
			continue
		}
		split.Total += n
		if _, known := split.Buckets[outcome.String]; known {
			split.Buckets[outcome.String] += n
		} else {
			// A legacy vocabulary value outside the current CHECK
			// (pre-rebuild rows). Total stays honest; the unknown
			// bucket is surfaced rather than silently dropped.
			split.Buckets[outcome.String] += n
		}
	}
	if err := rows.Err(); err != nil {
		return split, fmt.Errorf("iterate outcome rows for %q: %w", project, err)
	}
	split.NoWork = split.Buckets[string(OutcomeNoWork)]
	return split, nil
}
