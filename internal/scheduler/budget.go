package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
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

var meteredBudgetSettings struct {
	sync.RWMutex
	enabled bool
	stateDB string
}

// SetMeteredBudgetEnabled configures the opt-in SCHED-GAP-127 budget meter.
// The daemon points stateDB at the dedicated foreman HERMES_HOME/state.db.
// Default false preserves the historical ticks.cost_usd gate exactly.
func SetMeteredBudgetEnabled(enabled bool, stateDB string) {
	meteredBudgetSettings.Lock()
	meteredBudgetSettings.enabled = enabled
	meteredBudgetSettings.stateDB = stateDB
	meteredBudgetSettings.Unlock()
}

func meteredBudgetConfig() (enabled bool, stateDB string) {
	meteredBudgetSettings.RLock()
	defer meteredBudgetSettings.RUnlock()
	return meteredBudgetSettings.enabled, meteredBudgetSettings.stateDB
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

// budgetSpendWindowSlack is the safety margin subtracted from a window
// boundary to build the cheap string prefilter bound in LoadBudgetSpends.
// Real-world RFC3339 offsets span -12:00 … +14:00, so a row's local
// wall-clock stamp can sit at most 14h ahead of its UTC instant; 24h leaves
// 10h of headroom and still narrows the candidate set to a couple of days of
// rows instead of the whole table.
const budgetSpendWindowSlack = 24 * time.Hour

// budgetSpendPrefilterBound renders the index-friendly lower bound for a
// window boundary: the boundary minus budgetSpendWindowSlack, formatted as a
// bare space-separated local date-time with NO zone suffix.
//
// This bound is a SUPERSET test — it can only ever admit extra candidate rows,
// never drop one the exact julianday() predicate keeps, so it is safe to AND
// it in front of that predicate (and unsafe to use on its own):
//
//   - A row whose UTC instant is at/after the boundary has a wall-clock stamp
//     at/after boundary-14h, i.e. strictly after boundary-24h.
//   - Positions 0-9 hold the date and 11-18 the time, so the comparison is
//     decided before either stamp's zone suffix is reached; a space separator
//     also sorts below RFC3339's 'T' (and below no separator at all), and the
//     bound's missing suffix makes a row sharing its first 19 characters
//     compare GREATER (the longer string wins).
//
// The win is that the string compare is ~an order of magnitude cheaper than
// julianday() per row, so the window predicates stop paying a date parser on
// every tick row in the table — only the rows near the boundary reach it.
func budgetSpendPrefilterBound(boundary time.Time) string {
	return boundary.UTC().Add(-budgetSpendWindowSlack).Format("2006-01-02 15:04:05")
}

// LoadBudgetSpends computes per-project spend in all three windows with a
// single GROUP BY over the ticks table. spawned_at is RFC3339 text; the
// window predicates use julianday() so rows with non-UTC offsets compare
// correctly (raw string comparison would be wrong — same rationale as the
// stale-gateway SQL in tick_process.go). Queued rows with NULL spawned_at
// fall into neither window predicate but DO count toward Total, matching
// "spent = cost of every tick the project was charged for".
//
// SCHED-GAP-1636: each window predicate is a cheap string prefilter AND the
// exact julianday() compare, in that order (see budgetSpendPrefilterBound).
// The aggregate still visits every tick row — Total is all-time and cannot be
// windowed — but it no longer parses two dates per row on every call, which
// is where most of the per-request cost went. The covering index
// idx_ticks_project_spawned_cost (migration 45) lets the whole scan run from
// the index instead of chasing fat tick rows.
func LoadBudgetSpends(ctx context.Context, db *sql.DB, now time.Time) (map[string]BudgetSpend, error) {
	dayStart := UTCDayStart(now)
	weekStart := UTCWeekStart(now)
	rows, err := db.QueryContext(ctx, `
SELECT project_name,
       COALESCE(SUM(CASE WHEN spawned_at >= ? AND julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(CASE WHEN spawned_at >= ? AND julianday(spawned_at) >= julianday(?) THEN cost_usd ELSE 0 END), 0.0),
       COALESCE(SUM(cost_usd), 0.0)
FROM ticks
GROUP BY project_name`,
		budgetSpendPrefilterBound(dayStart), dayStart.Format(time.RFC3339),
		budgetSpendPrefilterBound(weekStart), weekStart.Format(time.RFC3339))
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

// loadMeteredBudgetSpends reads the dedicated Hermes state.db instead of tick
// stickers. The meter is deliberately FLEET-WIDE: the state DB is the cash
// ledger for every foreman/worker session, so each configured USD cap compares
// against the same daily/weekly/lifetime truth rather than pretending an
// overlapping session can be attributed to one project from timestamps alone.
// The next-tick lane marginal is handled by the normal tick cost path; this
// function only reports spend already present in the ledger.
func loadMeteredBudgetSpends(ctx context.Context, schedulerDB *sql.DB, stateDB string, now time.Time) (map[string]BudgetSpend, error) {
	daily, err := sumSessionMeteredUSDInWindow(ctx, stateDB, UTCDayStart(now), now)
	if err != nil {
		return nil, fmt.Errorf("load metered daily spend: %w", err)
	}
	weekly, err := sumSessionMeteredUSDInWindow(ctx, stateDB, UTCWeekStart(now), now)
	if err != nil {
		return nil, fmt.Errorf("load metered weekly spend: %w", err)
	}
	total, err := sumSessionMeteredUSDInWindow(ctx, stateDB, time.Unix(0, 0), now)
	if err != nil {
		return nil, fmt.Errorf("load metered total spend: %w", err)
	}

	rows, err := schedulerDB.QueryContext(ctx, `SELECT name FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("load projects for metered budget: %w", err)
	}
	defer rows.Close()
	spend := BudgetSpend{Daily: daily, Weekly: weekly, Total: total}
	out := make(map[string]BudgetSpend)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan project for metered budget: %w", err)
		}
		out[name] = spend
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
	enabled, stateDB := meteredBudgetConfig()
	var (
		spends map[string]BudgetSpend
		err    error
	)
	if enabled {
		spends, err = loadMeteredBudgetSpends(ctx, db, stateDB, now)
	} else {
		spends, err = LoadBudgetSpends(ctx, db, now)
	}
	if err != nil {
		mode := "tick"
		if enabled {
			mode = "metered"
		}
		log.Printf("BUDGET: %s spend query failed (%v) — budget enforcement OFF this cycle", mode, err)
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
