package database

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// SCHED-GAP-131 — per-feature live usage counters + the dead-feature reaper.
//
// The 2026-09-16 PRD's honest caveat was that the scheduler is "sized for a
// fleet 10x larger" and polishing surfaces that may never have fired. This
// file turns "never once fired" from archaeology into a query:
//
//   - RecordFeatureUse increments an in-memory counter for a named mechanism
//     (the fire points live in the scheduler and here in BumpProject);
//   - FlushFeatureUsage merges the pending counters into the persisted
//     feature_usage table (migration v49);
//   - LoadFeatureUsage / FeaturePruneCandidates are the read surface the
//     /api/v1/features endpoint serves.
//
// The counters are IN-MEMORY with a periodic flush (called on every features
// read and from the daemon health ticker), matching the task's "prefer
// in-memory with periodic flush to DB": a fire is a cheap locked map update,
// never a synchronous SQLite write on the spawn/dedupe hot path.

// Feature vocabulary (exact strings — the keys persisted in feature_usage).
const (
	FeatureBumpArming        = "bump_arming"
	FeatureWaveTicks         = "wave_ticks"
	FeatureAdmissionMode     = "admission_mode"
	FeatureLoadGateDeferrals = "load_gate_deferrals"
	FeatureDedupeSuppress    = "dedupe_suppressions"
	// FeatureBuilderGuard (SCHED-GAP-1674): the builder no-artifact guard
	// fired (nudged or aborted) at least one running tick.
	FeatureBuilderGuard = "builder_guard"
	// FeatureNoopGuard (SCHED-GAP-1682): the no-op-by-lane-class guard
	// re-entered (or attempted to re-enter) at least one no-op tick.
	FeatureNoopGuard = "noop_guard"
)

// FeatureDefinition names one tracked mechanism and what "use" means for it.
type FeatureDefinition struct {
	Name        string
	Description string
}

// FeatureDefinitions is the canonical, ordered vocabulary. Every feature is
// always present in the /api/v1/features response (0 = never used), so a
// missing feature is never ambiguous to a dashboard.
var FeatureDefinitions = []FeatureDefinition{
	{FeatureBumpArming, "SCHED-GAP-107 task bump armed (BumpProject set bump_active=1)"},
	{FeatureWaveTicks, "a tick dispatched into a wave-enabled namespace (S12 wave scheduling)"},
	{FeatureAdmissionMode, "a tasks-mode lane admitted via the SCHED-GAP-124 board-work waiver"},
	{FeatureLoadGateDeferrals, "the SCHED-GAP-125 load-average gate deferred a spawn"},
	{FeatureDedupeSuppress, "a duplicate spawn suppressed (SCHED-GAP-030/103)"},
	{FeatureBuilderGuard, "the SCHED-GAP-1674 builder no-artifact guard nudged or aborted a running tick"},
	{FeatureNoopGuard, "the SCHED-GAP-1682 no-op guard re-entered (or attempted) a no-op tick's session"},
}

// FeatureUsage is one feature-usage row as served to the API. FirstUsedAt and
// LastUsedAt are RFC3339 UTC strings; "" means "never used".
type FeatureUsage struct {
	Feature     string `json:"feature"`
	UseCount    int64  `json:"use_count"`
	FirstUsedAt string `json:"first_used_at"`
	LastUsedAt  string `json:"last_used_at"`
}

// featureUsageDelta is the pending, unflushed counter for one feature.
type featureUsageDelta struct {
	count int64
	first time.Time // earliest record in this process
	last  time.Time // latest record in this process
}

var (
	featureUsageMu   sync.Mutex
	featureUsageSeam clock.Seam // zero value = wall clock
	featureUsageDel  = map[string]featureUsageDelta{}
)

// RecordFeatureUse increments the in-memory counter for name and stamps
// first/last used at the tracker's clock. Thread-safe; merged into the
// persisted feature_usage table by FlushFeatureUsage. Unknown names are
// recorded verbatim so a mis-typed call site stays visible instead of being
// silently dropped.
func RecordFeatureUse(name string) {
	if name == "" {
		return
	}
	now := featureUsageSeam.Get().Now().UTC()
	featureUsageMu.Lock()
	d := featureUsageDel[name]
	if d.count == 0 {
		d.first = now
	}
	d.count++
	d.last = now
	featureUsageDel[name] = d
	featureUsageMu.Unlock()
}

// SetFeatureUsageClock installs the clock used to stamp first/last used.
// nil keeps the wall clock. A test seam; production leaves it unset.
func SetFeatureUsageClock(c clock.Clock) { featureUsageSeam.Set(c) }

// ResetFeatureUsage clears all pending in-memory counters. A test seam so
// suites that share one process never leak a prior test's counts.
func ResetFeatureUsage() {
	featureUsageMu.Lock()
	featureUsageDel = map[string]featureUsageDelta{}
	featureUsageMu.Unlock()
}

// FlushFeatureUsage merges the pending in-memory counters into the persisted
// feature_usage table and clears them. Idempotent — each pending delta is
// added once — so it can run on every features read and periodically with no
// double count. Callers that only want the persisted view ignore the error
// and read LoadFeatureUsage (a failed flush costs observability, never data).
func FlushFeatureUsage(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	featureUsageMu.Lock()
	if len(featureUsageDel) == 0 {
		featureUsageMu.Unlock()
		return nil
	}
	deltas := featureUsageDel
	featureUsageDel = map[string]featureUsageDelta{}
	featureUsageMu.Unlock()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feature usage flush begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for name, d := range deltas {
		if err := upsertFeatureUsage(ctx, tx, name, d.count, d.first, d.last); err != nil {
			return fmt.Errorf("feature usage flush %s: %w", name, err)
		}
	}
	return tx.Commit()
}

// upsertFeatureUsage folds one delta into the persisted row. use_count
// accumulates; first_used_at keeps the earliest stamp; last_used_at keeps the
// latest. The RFC3339 UTC strings are fixed-width with no fractional seconds,
// so lexicographic comparison is chronological.
func upsertFeatureUsage(ctx context.Context, tx *sql.Tx, name string, count int64, first, last time.Time) error {
	var curCount int64
	var curFirst, curLast string
	err := tx.QueryRowContext(ctx,
		`SELECT use_count, first_used_at, last_used_at FROM feature_usage WHERE feature = ?`, name).
		Scan(&curCount, &curFirst, &curLast)
	switch {
	case err == sql.ErrNoRows:
		_, err = tx.ExecContext(ctx,
			`INSERT INTO feature_usage (feature, use_count, first_used_at, last_used_at) VALUES (?,?,?,?)`,
			name, count, first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
		return err
	case err != nil:
		return err
	}

	newFirst := curFirst
	if newFirst == "" {
		newFirst = first.UTC().Format(time.RFC3339)
	}
	newLast := curLast
	if newLast == "" || last.UTC().Format(time.RFC3339) > newLast {
		newLast = last.UTC().Format(time.RFC3339)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE feature_usage SET use_count = use_count + ?, first_used_at = ?, last_used_at = ? WHERE feature = ?`,
		count, newFirst, newLast, name)
	return err
}

// LoadFeatureUsage returns every tracked feature in canonical order, each with
// its persisted use_count (0 and "" timestamps for a feature that has no row).
// A nil db returns the full vocabulary zeroed rather than erroring.
func LoadFeatureUsage(ctx context.Context, db *sql.DB) ([]FeatureUsage, error) {
	persisted := map[string]FeatureUsage{}
	if db != nil {
		rows, err := db.QueryContext(ctx,
			`SELECT feature, use_count, first_used_at, last_used_at FROM feature_usage`)
		if err != nil {
			return nil, fmt.Errorf("load feature usage: %w", err)
		}
		for rows.Next() {
			var fu FeatureUsage
			if err := rows.Scan(&fu.Feature, &fu.UseCount, &fu.FirstUsedAt, &fu.LastUsedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("load feature usage scan: %w", err)
			}
			persisted[fu.Feature] = fu
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("load feature usage iterate: %w", err)
		}
		rows.Close()
	}

	out := make([]FeatureUsage, 0, len(FeatureDefinitions))
	for _, def := range FeatureDefinitions {
		fu := persisted[def.Name]
		fu.Feature = def.Name
		out = append(out, fu)
	}
	return out, nil
}

// FeaturePruneCandidates returns the subset of rows whose last use predates
// cutoff (an RFC3339 UTC string) or that were never used. Pure — no DB access —
// so the endpoint can flush first, then filter a fully-merged view. A feature
// is flagged, never deleted: this is the reaper's report, not its scissors.
func FeaturePruneCandidates(rows []FeatureUsage, cutoff string) []FeatureUsage {
	var out []FeatureUsage
	for _, r := range rows {
		if r.LastUsedAt == "" || r.LastUsedAt < cutoff {
			out = append(out, r)
		}
	}
	return out
}

// AdmissionModeRowCounts reports how many project and namespace rows are
// currently configured with the tasks admission mode (SCHED-GAP-124) — the
// "admission_mode rows" gauge the board row names alongside the event counter.
func AdmissionModeRowCounts(ctx context.Context, db *sql.DB) (projectsTasks, namespacesTasks int, err error) {
	if db == nil {
		return 0, 0, nil
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE admission_mode = ?`, AdmissionModeTasks).Scan(&projectsTasks); err != nil {
		return 0, 0, fmt.Errorf("count tasks-mode projects: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM namespaces WHERE admission_mode = ?`, AdmissionModeTasks).Scan(&namespacesTasks); err != nil {
		return 0, 0, fmt.Errorf("count tasks-mode namespaces: %w", err)
	}
	return projectsTasks, namespacesTasks, nil
}
