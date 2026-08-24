package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Per-project budget enforcement (SCHED-GAP-066).
//
// Budgets are OPT-IN: a cap of 0 (or unset) means unlimited. Three windows,
// any combination per project:
//
//	daily_budget_usd  — resets at the UTC day boundary (00:00 UTC)
//	weekly_budget_usd — resets at the UTC week boundary (Monday 00:00 UTC)
//	final_budget_usd  — one-time lifetime cap; never resets. When exhausted
//	                    the project stops scheduling for good (e.g.
//	                    inference-estimator: fixed-budget one-time project).
//
// Enforcement gates SELECTION ONLY. A running tick is never killed mid-run:
// once a project's spend in a window reaches its cap, the packers exclude it
// from follow-up spawns and /api/v1/projects surfaces blocked_reason=budget
// with the spent/remaining numbers. Spend is summed from ticks.cost_usd
// (already recorded per tick from real Hermes telemetry — see cost.go).

// Budget window names returned by BudgetBlockReason.
const (
	BudgetWindowDaily  = "daily"
	BudgetWindowWeekly = "weekly"
	BudgetWindowFinal  = "final"
)

// BudgetSpend is a project's spend (USD) in the three enforcement windows.
type BudgetSpend struct {
	Daily  float64 `json:"daily"`  // since UTC midnight today
	Weekly float64 `json:"weekly"` // since Monday 00:00 UTC of the current week
	Total  float64 `json:"total"`  // all time
}

// UTCDayStart returns 00:00:00 UTC on the calendar day containing t.
func UTCDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// UTCWeekStart returns Monday 00:00:00 UTC of the ISO week containing t.
func UTCWeekStart(t time.Time) time.Time {
	d := UTCDayStart(t)
	// Go Weekday: Sunday=0 … Saturday=6; shift so Monday is offset 0.
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}

// budgetWindowForCaps returns the exhausted budget window for the given caps
// and spend ("final" / "weekly" / "daily"), or "" when no configured cap is
// reached. A cap <= 0 means unlimited for that window. "Reached" is >= : a
// project whose spend exactly equals the cap has no budget left for another
// tick. Final is checked first — it is the permanent stop.
func budgetWindowForCaps(dailyCapUSD, weeklyCapUSD, finalCapUSD float64, spend BudgetSpend) string {
	if finalCapUSD > 0 && spend.Total >= finalCapUSD {
		return BudgetWindowFinal
	}
	if weeklyCapUSD > 0 && spend.Weekly >= weeklyCapUSD {
		return BudgetWindowWeekly
	}
	if dailyCapUSD > 0 && spend.Daily >= dailyCapUSD {
		return BudgetWindowDaily
	}
	return ""
}

// budgetDetailForCaps renders the log detail for an exhausted window,
// e.g. "daily spent $6.25/$5.00". Returns "" when window is "".
func budgetDetailForCaps(window string, dailyCapUSD, weeklyCapUSD, finalCapUSD float64, spend BudgetSpend) string {
	switch window {
	case BudgetWindowFinal:
		return fmt.Sprintf("final spent $%.2f/$%.2f", spend.Total, finalCapUSD)
	case BudgetWindowWeekly:
		return fmt.Sprintf("weekly spent $%.2f/$%.2f", spend.Weekly, weeklyCapUSD)
	case BudgetWindowDaily:
		return fmt.Sprintf("daily spent $%.2f/$%.2f", spend.Daily, dailyCapUSD)
	}
	return ""
}

// BudgetBlockReason returns the exhausted budget window for a project, or ""
// when no configured cap is reached.
func BudgetBlockReason(p *database.Project, spend BudgetSpend) string {
	return budgetWindowForCaps(p.DailyBudgetUSD, p.WeeklyBudgetUSD, p.FinalBudgetUSD, spend)
}

// BudgetBlockDetail renders the log/API detail for a project's exhausted
// window, e.g. "daily spent $6.25/$5.00". Returns "" when not blocked.
func BudgetBlockDetail(p *database.Project, spend BudgetSpend) string {
	return budgetDetailForCaps(BudgetBlockReason(p, spend),
		p.DailyBudgetUSD, p.WeeklyBudgetUSD, p.FinalBudgetUSD, spend)
}

// LoadBudgetSpends computes per-project spend in all three windows with a
// single GROUP BY over the ticks table. spawned_at is RFC3339 text; the
// window predicates use julianday() so rows with non-UTC offsets compare
// correctly (raw string comparison would be wrong — same rationale as the
// stale-gateway SQL in tick_process.go). Queued rows with NULL spawned_at
// fall into neither window predicate but DO count toward Total, matching
// "spent = cost of every tick the project was charged for".
func LoadBudgetSpends(ctx context.Context, db *sql.DB, now time.Time) (map[string]BudgetSpend, error) {
	dayStart := UTCDayStart(now).Format(time.RFC3339)
	weekStart := UTCWeekStart(now).Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
SELECT project_name,
       COALESCE(SUM(CASE WHEN julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(CASE WHEN julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(cost_usd), 0.0)
FROM ticks
GROUP BY project_name`, dayStart, weekStart)
	if err != nil {
		return nil, fmt.Errorf("load budget spends: %w", err)
	}
	defer rows.Close()

	out := make(map[string]BudgetSpend)
	for rows.Next() {
		var name string
		var s BudgetSpend
		if err := rows.Scan(&name, &s.Daily, &s.Weekly, &s.Total); err != nil {
			return nil, fmt.Errorf("scan budget spend row: %w", err)
		}
		out[name] = s
	}
	return out, rows.Err()
}

// BudgetGate reports whether a project is budget-blocked at selection time:
// given the project's name and configured caps it returns a human-readable
// detail ("daily spent $6.25/$5.00") and true when any cap is reached, or
// ("", false) otherwise. A nil gate disables enforcement.
type BudgetGate func(name string, dailyCapUSD, weeklyCapUSD, finalCapUSD float64) (detail string, blocked bool)

// NewBudgetGate precomputes per-project spends for the windows anchored at
// now (one query per evaluation cycle, not per project) and returns the gate
// the packers consult during selection. On query error it logs and returns
// nil — fail-open: a broken spend query must never halt fleet scheduling.
func NewBudgetGate(ctx context.Context, db *sql.DB, now time.Time) BudgetGate {
	spends, err := LoadBudgetSpends(ctx, db, now)
	if err != nil {
		log.Printf("BUDGET: spend query failed (%v) — budget enforcement OFF this cycle", err)
		return nil
	}
	return func(name string, dailyCapUSD, weeklyCapUSD, finalCapUSD float64) (string, bool) {
		window := budgetWindowForCaps(dailyCapUSD, weeklyCapUSD, finalCapUSD, spends[name])
		if window == "" {
			return "", false
		}
		return budgetDetailForCaps(window, dailyCapUSD, weeklyCapUSD, finalCapUSD, spends[name]), true
	}
}
