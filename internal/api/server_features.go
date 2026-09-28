package api

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-131 — feature-usage surface + dead-feature reaper.
//
// Two read-only endpoints:
//
//   - GET /api/v1/features — every tracked mechanism with use_count +
//     first/last-used timestamps, plus the live admission_mode row gauge.
//   - GET /api/v1/features/prune-candidates[?weeks=N] — the mechanisms whose
//     last proven use is older than N weeks (default 8) or that never fired.
//     FLAG ONLY: nothing here deletes a feature; it is the reaper's report,
//     not its scissors.
//
// Both flush the in-memory counters to the persisted feature_usage table
// first (best-effort), so a feature used THIS process can never be reported
// "never used".

// featurePruneWeeksDefault is the prune window when neither the daemon flag
// nor the ?weeks= query override set one (8 weeks, matching the task's
// "N=4 or 8, configurable" upper bound).
const featurePruneWeeksDefault = 8

// featureUsageEntry is one feature row on the wire, the persisted counters
// enriched with the canonical description from database.FeatureDefinitions.
type featureUsageEntry struct {
	Feature     string `json:"feature"`
	Description string `json:"description"`
	UseCount    int64  `json:"use_count"`
	FirstUsedAt string `json:"first_used_at"`
	LastUsedAt  string `json:"last_used_at"`
}

// SetFeaturePruneWeeks installs the daemon-level prune window (weeks). A
// non-positive value keeps the default (featurePruneWeeksDefault). The
// /api/v1/features/prune-candidates ?weeks= override still wins per request.
func (s *Server) SetFeaturePruneWeeks(weeks int) {
	if weeks > 0 {
		s.featurePruneWeeks = weeks
	}
}

// pruneWeeks returns the configured prune window, never zero.
func (s *Server) pruneWeeks() int {
	if s.featurePruneWeeks > 0 {
		return s.featurePruneWeeks
	}
	return featurePruneWeeksDefault
}

// features handles GET /api/v1/features. Read-only: it flushes the pending
// in-memory counters (a write, but the same best-effort observability write
// the daemon makes periodically) and reads the persisted table — it mutates
// no scheduling state.
func (s *Server) features(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := r.Context()
	// Flush first so a feature used this process is never reported 0. A
	// failed flush is logged and ignored — the persisted view is served
	// either way (a flush failure costs observability, never data).
	if err := database.FlushFeatureUsage(ctx, s.db); err != nil {
		log.Printf("features: flush feature usage: %v", err)
	}
	rows, err := database.LoadFeatureUsage(ctx, s.db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	projectsTasks, namespacesTasks, err := database.AdmissionModeRowCounts(ctx, s.db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	desc := featureDescriptions()
	entries := make([]featureUsageEntry, 0, len(rows))
	for _, fu := range rows {
		entries = append(entries, featureUsageEntry{
			Feature:     fu.Feature,
			Description: desc[fu.Feature],
			UseCount:    fu.UseCount,
			FirstUsedAt: fu.FirstUsedAt,
			LastUsedAt:  fu.LastUsedAt,
		})
	}
	writeJSON(w, 200, map[string]interface{}{
		"features": entries,
		"admission_mode_rows": map[string]int{
			"projects_tasks":   projectsTasks,
			"namespaces_tasks": namespacesTasks,
		},
		"prune_weeks": s.pruneWeeks(),
	})
}

// featurePruneCandidates handles GET /api/v1/features/prune-candidates. The
// prune window is the daemon default (--feature-prune-weeks) overridden by a
// ?weeks=N query (non-positive/unparseable values fall back to the default).
func (s *Server) featurePruneCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	ctx := r.Context()

	weeks := s.pruneWeeks()
	if v := r.URL.Query().Get("weeks"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			weeks = n
		}
	}
	cutoff := s.clock().Now().UTC().Add(-time.Duration(weeks) * 7 * 24 * time.Hour)
	cutoffStr := cutoff.Format(time.RFC3339)

	if err := database.FlushFeatureUsage(ctx, s.db); err != nil {
		log.Printf("features: flush feature usage: %v", err)
	}
	rows, err := database.LoadFeatureUsage(ctx, s.db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	candidates := database.FeaturePruneCandidates(rows, cutoffStr)
	// Never null — an empty prune list serializes as [] so a dashboard can
	// iterate without a nil check.
	if candidates == nil {
		candidates = []database.FeatureUsage{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"prune_weeks": weeks,
		"cutoff":      cutoffStr,
		"candidates":  candidates,
	})
}

// featureDescriptions builds the canonical name → description lookup from the
// database package's vocabulary.
func featureDescriptions() map[string]string {
	out := make(map[string]string, len(database.FeatureDefinitions))
	for _, def := range database.FeatureDefinitions {
		out[def.Name] = def.Description
	}
	return out
}
