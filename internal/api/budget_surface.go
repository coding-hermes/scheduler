package api

import (
	"context"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── ADV-R09/G8: budget authority chain + spend reality surface ──────────
//
// Three lies this file retires:
//
//  1. budget_total was a literal 100 in the status handler (coincidentally
//     equal to the --budget flag default). It now reads the Loop — the single
//     object built from the resolved flag — so every surface reports the
//     EFFECTIVE budget, and budget_source says which config layer owned it.
//  2. Recorded spend blended measured money with the flat token estimate.
//     ticks.cost_source (migration v29) now stamps each row, and the status
//     spend block splits by it.
//  3. The price map carried no as-of date or unknown-model policy in code.
//     scheduler.PriceMapAsOf/PriceMapSource surface the sticker vintage and
//     provenance next to the spend figures.

// defaultWeightBudget mirrors the --budget flag default (cmd/schedulerd) and
// config.defaultWeightBudget. It is the DOCUMENTED unset behavior: when no
// config layer sets a budget, the fleet schedules against 100 weight units
// (scheduling admission currency — NOT dollars; money caps are the per-project
// daily/weekly/final_budget_usd fields).
const defaultWeightBudget = 100

// effectiveBudget returns the loop's budget when a loop is attached, else the
// documented default. A nil loop (tests without one) previously reported the
// literal; now it reports the same documented default, labeled as such.
func (s *Server) effectiveBudget() int {
	if s.loop != nil {
		return s.loop.WeightBudget()
	}
	return defaultWeightBudget
}

// budgetSource reports which config layer owns the effective budget:
//
//	toml         — [scheduler] weight_budget in the fleet config file
//	env          — SCHEDULER_BUDGET
//	flag         — an explicit --budget on the command line
//	flag-default — NO layer set it; the documented unset behavior applied
//
// main.go computes the provenance at resolution time and passes it via
// SetResolvedConfig (BudgetSource); when absent (tests, nil loop) the source
// is reported as "flag-default" — the honest label for "nobody configured
// this", never a guess.
func (s *Server) budgetSource() string {
	if s.resolvedConfig.BudgetSource != "" {
		return s.resolvedConfig.BudgetSource
	}
	if s.loop != nil && s.loop.WeightBudget() != defaultWeightBudget {
		// A non-default value on the loop proves SOME layer set it, but the
		// provenance snapshot is absent — say so rather than inventing one.
		return "unattributed"
	}
	return "flag-default"
}

// spendByCostSource aggregates ticks.cost_usd + token sums by cost_source in
// one GROUP BY. Rows written before migration v29 carry cost_source=” —
// classified as "legacy" in the surface (never rewritten; history stays
// intact). Token averages skip zero-token rows so the legacy 0/0-token exec
// rows do not drag measured averages toward zero. The returned block also
// carries the price vintage (as-of date + map provenance) the USD figures
// were priced at.
func (s *Server) spendByCostSource(ctx context.Context) map[string]interface{} {
	type spendTier struct {
		CostUSD      float64 `json:"cost_usd"`
		TokensIn     int64   `json:"tokens_in"`
		TokensOut    int64   `json:"tokens_out"`
		Ticks        int     `json:"ticks"`
		TokensInAvg  float64 `json:"tokens_in_avg"`
		TokensOutAvg float64 `json:"tokens_out_avg"`
	}
	tiers := map[string]*spendTier{}
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(cost_source,''),'legacy') AS src,
       COALESCE(SUM(cost_usd),0),
       COALESCE(SUM(tokens_in),0),
       COALESCE(SUM(tokens_out),0),
       COUNT(*),
       COALESCE(AVG(NULLIF(tokens_in,0)),0),
       COALESCE(AVG(NULLIF(tokens_out,0)),0)
  FROM ticks
 GROUP BY src`)
	if err != nil {
		// Fail-open: a broken spend query must not break /api/v1/status.
		return map[string]interface{}{"error": "spend query failed"}
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var t spendTier
		if err := rows.Scan(&src, &t.CostUSD, &t.TokensIn, &t.TokensOut, &t.Ticks, &t.TokensInAvg, &t.TokensOutAvg); err != nil {
			return map[string]interface{}{"error": "spend scan failed"}
		}
		tiers[src] = &t
	}
	return map[string]interface{}{
		"by_cost_source": tiers,
		"price_as_of":    scheduler.PriceMapAsOf(),
		"price_source":   scheduler.PriceMapSource(),
	}
}
