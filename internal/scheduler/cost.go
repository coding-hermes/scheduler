package scheduler

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// modernc.org/sqlite is the pure-Go driver (no CGO); registered under the
	// driver name "sqlite". Same as internal/database/schema.go.
	_ "modernc.org/sqlite"
)

// Real per-tick cost from Hermes' own telemetry.
//
// The scheduler runs each foreman with an isolated HERMES_HOME (see spawn.go:
// "HERMES_HOME="+s.foremanHome). Hermes writes real token/cost usage to that
// home's state.db (session_model_usage.estimated_cost_usd / actual_cost_usd).
// Instead of the hardcoded $0.032 flat estimate, we look up the real
// estimated_cost_usd for sessions that overlap the tick's [start, finish]
// window. This replaces the "0.032 or 0" dashboard cost with genuine numbers.
//
// The cost lookup is best-effort: on any error (state.db missing, sqlite not
// available, empty result) we return 0 and the caller falls back to the flat
// estimate so aggregation still works.

// sumSessionCostInWindow queries a Hermes state.db for the total estimated
// cost of sessions whose activity window overlaps [start, end].
func sumSessionCostInWindow(stateDB string, start, end time.Time) (float64, int, error) {
	if stateDB == "" || start.IsZero() {
		return 0, 0, fmt.Errorf("no state db / start time")
	}
	if _, err := os.Stat(stateDB); err != nil {
		return 0, 0, fmt.Errorf("state db %s: %w", stateDB, err)
	}
	if end.IsZero() {
		end = time.Now()
	}

	// session_model_usage.first_seen / last_seen are unix-epoch floats.
	// Match sessions whose activity overlaps the tick window:
	//   first_seen <= end  AND  last_seen >= start
	db, err := sql.Open("sqlite", stateDB)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	// Prefer real actual cost when present (actual_cost_usd > 0); otherwise
	// fall back to estimated. Sum over overlapping sessions.
	q := `
SELECT COALESCE(SUM(CASE WHEN actual_cost_usd > 0 THEN actual_cost_usd ELSE estimated_cost_usd END), 0),
       COUNT(*)
  FROM session_model_usage
 WHERE first_seen <= ? AND last_seen >= ?`
	var cost float64
	var n int
	err = db.QueryRow(q, end.Unix(), start.Unix()).Scan(&cost, &n)
	if err != nil {
		return 0, 0, err
	}
	return cost, n, nil
}

// resolveRealTickCost returns the real cost of a tick, falling back to the flat
// estimate when real telemetry is unavailable. It sums:
//   - foreman + worker sessions in the foreman's Hermes state.db overlapping
//     the tick window (the dominant cost), and
//   - GitReins judge usage recorded in <workdir>/.gitreins/usage.jsonl within
//     the same window (gitreins uses its own LLM client, so it never appears
//     in Hermes telemetry).
//
// Returns (cost, isReal).
func resolveRealTickCost(foremanHome, workdir, project string, start, end time.Time) (float64, bool) {
	total := 0.0
	real := false

	// 1) Foreman + worker cost from Hermes telemetry.
	stateDB := filepath.Join(foremanHome, "state.db")
	if sessionCost, n, err := sumSessionCostInWindow(stateDB, start.Add(-2*time.Minute), end); err == nil {
		total += sessionCost
		if n > 0 && sessionCost > 0 {
			real = true
		}
	}

	// 2) GitReins judge cost from usage.jsonl (per-project).
	if workdir != "" {
		if jCost, n, err := sumGitreinsUsageInWindow(filepath.Join(workdir, ".gitreins", "usage.jsonl"), start, end); err == nil {
			total += jCost
			if n > 0 && jCost > 0 {
				real = true
			}
		}
	}

	if !real || total <= 0 {
		_, _, est := estimateTickCost()
		return est, false
	}
	return total, true
}

// sumGitreinsUsageInWindow reads .gitreins/usage.jsonl lines whose ts falls in
// [start, end] and converts token counts to USD using the same per-token rates
// as the foreman estimate. Returns (costUSD, lineCount).
func sumGitreinsUsageInWindow(path string, start, end time.Time) (float64, int, error) {
	if path == "" {
		return 0, 0, fmt.Errorf("no gitreins usage path")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	total := 0.0
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Ts        float64 `json:"ts"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		t := time.Unix(int64(rec.Ts), 0)
		if t.Before(start) || t.After(end) {
			continue
		}
		total += float64(rec.TokensIn)*estCostPerIn + float64(rec.TokensOut)*estCostPerOut
		count++
	}
	return total, count, sc.Err()
}
