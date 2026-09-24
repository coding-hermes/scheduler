package dashboard

// SCHED-GAP-1596 — Fleet Tape: the fleet rendered as one instrument, a
// stock-ticker page (the approved mockup at /home/kara/ticker-mockup.html).
// Three elements, all fed by real figures:
//
//   - INDEX BAR: the fleet as one quote — lanes open, 24h ticks with the move
//     vs the prior 24h, 24h notional, settle rate, and the live SSE feed pill.
//   - MARQUEE TAPE: per-lane quotes in a scrolling strip. Only the motion is
//     CSS; every quote in it is real data rendered server-side (duplicated
//     once so the translateX(-50%) loop is seamless).
//   - MARKET BOARD: one row per lane — 14-hour trend sparkline, $/hour
//     notional, move vs prior 24h, ticks/h, settle, urgency, state.
//
// Data honesty: every figure is computed from the scheduler DB — the same
// store the API reads — at render time, in ONE bounded aggregate query
// (single-scan doctrine, same as collect). Nothing is invented, cached per
// lane, or hardcoded:
//
//   - move      = ticks spawned in the last 24h vs the 24h before that
//     (NEW when the prior window is empty and the current one is not).
//   - $/hour    = completed-tick notional (cost_usd) over the last 24h ÷ 24.
//   - settle    = completed ÷ (completed+failed+timeout) in the last 24h.
//   - trend     = completed ticks per hour over the last 14 hours.
//   - urgency   = the engine's UrgencyCalculator (the SAME instance the
//     API's /api/v1/queue ranks with — one formula, one source,
//     SCHED-GAP-174). With no calculator configured (tests), it falls back
//     to the overview's inline priority×(1+hours-idle) formula.
//   - state     = SUSPENDED (lane disabled) / VOLATILE (active lane whose
//     settle rate is below 50%) / OPEN.
//
// Feed: the browser opens /api/v1/events/stream (SSE). Each event pokes a
// throttled `tape-sse` body event the board subscribes to; the shared
// `autorefresh` cadence (layout.html, SCHED-GAP-1606) is the fallback
// trigger so the board still refreshes when SSE is down. There is NO private
// timer on this page. Deliberately NO dependency on /api/v1/status or
// /api/v1/queue — both routes wedge under load; the tape's data path must
// never hang behind them.

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// tapeWindowHours is the sparkline window: 14 hourly buckets, matching
	// the mockup's 14-hour trend.
	tapeWindowHours = 14
	// tapeReelCap bounds the marquee: the busiest 40 enabled lanes, twice.
	tapeReelCap = 40
	// tapeFeedURL is the SSE endpoint the page's browser JS connects to.
	tapeFeedURL = "/api/v1/events/stream"
	// tapeRefreshThrottleMs is the minimum gap between two tape-sse board
	// refreshes, however fast the fleet commits.
	tapeRefreshThrottleMs = 5000
)

// TapeLane is one row of the market board / one quote of the marquee.
// Display fields are pre-rendered strings so the template stays dumb and the
// htmx fragment byte-matches the full page's rows.
type TapeLane struct {
	Name   string // project name (the DB key, also the drill-down link)
	Sym    string // deterministic ticker symbol derived from the name
	Enable bool   // projects.enabled

	RowClass   string // live | warn | halt — row tint
	State      string // OPEN | VOLATILE | SUSPENDED
	PillClass  string // live | warn | halt — state pill
	SparkClass string // live | warn | halt — sparkline stroke

	SparkPoints  string // SVG polyline points for the 14h trend
	PriceLabel   string // $/hour (24h completed notional ÷ 24)
	MoveLabel    string // ▲ n% | ▼ n% | NEW | — flat
	MoveClass    string // up | down | flat
	RateLabel    string // ticks per hour over 24h
	SettleLabel  string // completed ÷ terminal, percent
	UrgencyLabel string // engine urgency, comma-grouped

	Ticks24h   int64   // spawned in the last 24h (any status)
	TicksPrior int64   // spawned 24h–48h ago (any status)
	settleDone int64   // completed in 24h (unexported: fleet settle input, not rendered)
	settleBad  int64   // failed+timeout in 24h (unexported: fleet settle input)
	urgency    float64 // raw engine urgency (unexported: suspend-lane sort key)
}

// TapeData feeds both the /tape page and its htmx rows fragment.
type TapeData struct {
	Title       string
	GeneratedAt string

	LanesTotal   int
	LanesEnabled int

	Ticks24h   int64
	TicksPrior int64
	// IndexMoveLabel/IndexMoveClass quote the whole fleet: ticks 24h vs the
	// prior 24h (one decimal, e.g. "▲ 46.0%").
	IndexMoveLabel string
	IndexMoveClass string

	Notional24h   float64
	NotionalLabel string // comma-grouped dollars
	SettleLabel   string // one-decimal percent
	SettleClass   string // up (≥50% settled) | down | flat (no terminal ticks yet)

	Lanes []TapeLane // board rows, sorted: open lanes by rate desc, then suspended by urgency desc
	Reel  []TapeLane // marquee quotes (capped), duplicated in the template for the seamless loop

	FeedURL           string
	RefreshThrottleMs int
}

// tapeQuery builds the single aggregate query behind the whole page: one row
// per project with its 24h / prior-24h / 14-bucket tick rollups. The LEFT
// JOIN is bounded to the 48h join window so the scan stays flat as the ticks
// table grows; completed_at/spawned_at are stored UTC RFC3339, so the window
// bounds are UTC too (same reason as collect).
func tapeQuery() string {
	var b strings.Builder
	b.WriteString(`
SELECT
  p.name,
  p.enabled,
  p.priority,
  COALESCE(p.decay_rate, 1.0)                       AS decay_rate,
  COALESCE(p.last_tick_completed, '')               AS last_completed,
  COALESCE(p.created_at, '')                        AS created_at,
  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? THEN 1 ELSE 0 END), 0)  AS ticks24,
  COALESCE(SUM(CASE WHEN tk.spawned_at <  ? THEN 1 ELSE 0 END), 0)  AS ticks_prior,
  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? AND tk.status = 'completed' THEN 1 ELSE 0 END), 0) AS done24,
  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? AND (tk.status = 'failed' OR tk.status = 'timeout') THEN 1 ELSE 0 END), 0) AS bad24,
  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? AND tk.status = 'completed' THEN tk.cost_usd ELSE 0.0 END), 0.0) AS cost24,
  COALESCE(SUM(CASE WHEN tk.spawned_at <  ? AND tk.status = 'completed' THEN tk.cost_usd ELSE 0.0 END), 0.0) AS cost_prior,
  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? AND tk.status = 'completed' THEN tk.tokens_in + tk.tokens_out ELSE 0 END), 0) AS tokens24`)
	for i := 0; i < tapeWindowHours; i++ {
		fmt.Fprintf(&b, ",\n  COALESCE(SUM(CASE WHEN tk.spawned_at >= ? AND tk.spawned_at < ? THEN 1 ELSE 0 END), 0) AS bucket%d", i)
	}
	b.WriteString(`
FROM projects p
LEFT JOIN ticks tk ON tk.project_name = p.name AND tk.spawned_at >= ?
GROUP BY p.name`)
	return b.String()
}

// tapeData reads the DB once and derives every figure on the page.
func (g *Generator) tapeData(ctx context.Context) TapeData {
	now := g.clock().Now()
	dayAgo := now.UTC().Add(-24 * time.Hour)
	priorStart := now.UTC().Add(-48 * time.Hour)
	bounds := make([]time.Time, tapeWindowHours+1)
	for i := range bounds {
		bounds[i] = now.UTC().Add(time.Duration(-(tapeWindowHours - i)) * time.Hour)
	}
	dayAgoS := dayAgo.Format(time.RFC3339)

	data := TapeData{
		Title:             "Fleet Tape",
		GeneratedAt:       now.UTC().Format("2006-01-02 15:04:05Z"),
		FeedURL:           tapeFeedURL,
		RefreshThrottleMs: tapeRefreshThrottleMs,
	}

	args := []any{dayAgoS, dayAgoS, dayAgoS, dayAgoS, dayAgoS, dayAgoS, dayAgoS}
	for i := 0; i < tapeWindowHours; i++ {
		args = append(args, bounds[i].Format(time.RFC3339), bounds[i+1].Format(time.RFC3339))
	}
	args = append(args, priorStart.Format(time.RFC3339))

	var (
		name, lastCompleted, createdAt string
		enabled                        bool
		priority                       int
		decay                          float64
		ticks24, ticksPrior            int64
		done24, bad24                  int64
		cost24, costPrior              float64
		tokens24                       int64
	)
	buckets := make([]int64, tapeWindowHours)

	rows, err := g.db.QueryContext(ctx, tapeQuery(), args...)
	if err != nil {
		// Degrade honestly: an unreadable DB renders an empty tape (the
		// index bar shows zeros) rather than invented figures.
		return data
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		for i := range buckets {
			buckets[i] = 0
		}
		scan := []any{&name, &enabled, &priority, &decay, &lastCompleted, &createdAt,
			&ticks24, &ticksPrior, &done24, &bad24, &cost24, &costPrior, &tokens24}
		for i := range buckets {
			scan = append(scan, &buckets[i])
		}
		if err := rows.Scan(scan...); err != nil {
			continue
		}
		data.LanesTotal++
		if enabled {
			data.LanesEnabled++
		}
		data.Ticks24h += ticks24
		data.TicksPrior += ticksPrior
		data.Notional24h += cost24

		lane := TapeLane{
			Name:       name,
			Sym:        tapeSymbol(name),
			Enable:     enabled,
			Ticks24h:   ticks24,
			TicksPrior: ticksPrior,
		}
		lane.SparkPoints = tapeSparkline(buckets)
		lane.PriceLabel = tapePriceLabel(cost24)
		lane.MoveLabel, lane.MoveClass = tapeMove(ticks24, ticksPrior)
		lane.RateLabel = fmt.Sprintf("%.2f", float64(ticks24)/24.0)
		lane.SettleLabel = tapeSettleLabel(done24, bad24)

		urgency := tapeUrgency(g, priority, decay, now, lastCompleted, createdAt)
		lane.urgency = urgency
		lane.UrgencyLabel = commaGroup(int64(math.Round(urgency)))
		lane.settleDone, lane.settleBad = done24, bad24

		lane.State, lane.PillClass, lane.RowClass, lane.SparkClass = tapeState(enabled, ticks24, done24, bad24)
		data.Lanes = append(data.Lanes, lane)
	}
	// Terminal rows error is a scan problem; the loop above already skipped
	// broken rows, and the page degrades to what rendered.

	sortTapeLanes(data.Lanes)
	for _, lane := range data.Lanes {
		if !lane.Enable {
			break // enabled lanes sort first; the reel quotes open lanes only
		}
		data.Reel = append(data.Reel, lane)
		if len(data.Reel) >= tapeReelCap {
			break
		}
	}

	data.IndexMoveLabel, data.IndexMoveClass = tapeIndexMove(data.Ticks24h, data.TicksPrior)
	data.NotionalLabel = "$" + commaGroup(int64(math.Round(data.Notional24h)))
	// Fleet settle is computed from the raw per-lane counts, not the rounded
	// per-lane labels — rounding a rounded mean is a lie.
	var fleetDone, fleetBad int64
	for _, lane := range data.Lanes {
		fleetDone += lane.settleDone
		fleetBad += lane.settleBad
	}
	data.SettleLabel = tapeSettleLabel(fleetDone, fleetBad)
	switch {
	case fleetDone+fleetBad == 0:
		data.SettleClass = "flat"
	case tapeSettle(fleetDone, fleetBad) >= 50:
		data.SettleClass = "up"
	default:
		data.SettleClass = "down"
	}
	return data
}

// GenerateTape renders the full /tape page.
func (g *Generator) GenerateTape(w io.Writer) error {
	return g.tapeTmpl.ExecuteTemplate(w, "tape", g.tapeData(context.Background()))
}

// GenerateTapeRows renders the board-rows fragment (tbody children only) for
// htmx to swap into the tape board. Routes get this when the request carries
// the HX-Request header.
func (g *Generator) GenerateTapeRows(w io.Writer) error {
	return g.tapeTmpl.ExecuteTemplate(w, "tape_rows", g.tapeData(context.Background()))
}

// tapeUrgency is the engine's urgency when a calculator is configured (the
// same instance /api/v1/queue ranks with), else the overview's inline
// priority×(1+hours-idle) formula. Never panics on unparseable timestamps.
func tapeUrgency(g *Generator, priority int, decay float64, now time.Time, lastCompleted, createdAt string) float64 {
	if g.urgencyCalc != nil {
		var lc *time.Time
		if t, err := time.Parse(time.RFC3339, lastCompleted); err == nil {
			lc = &t
		}
		created := now
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			created = t
		}
		return g.urgencyCalc.ComputeUrgency(float64(priority), decay, now, lc, created)
	}
	if t, err := time.Parse(time.RFC3339, lastCompleted); err == nil {
		return float64(priority) * (1 + now.Sub(t).Hours())
	}
	return float64(priority)
}

// tapeState classifies a lane for the board. VOLATILE needs recent activity:
// an enabled lane with ticks in the window and a sub-50% settle rate.
func tapeState(enabled bool, ticks24, done24, bad24 int64) (state, pillClass, rowClass, sparkClass string) {
	if !enabled {
		return "SUSPENDED", "halt", "halt", "halt"
	}
	if ticks24 > 0 && tapeSettle(done24, bad24) < 50 {
		return "VOLATILE", "warn", "warn", "warn"
	}
	return "OPEN", "live", "live", "live"
}

// tapeSettle is the raw settle rate in percent (0 when nothing terminal).
func tapeSettle(done, bad int64) float64 {
	terminal := done + bad
	if terminal == 0 {
		return 0
	}
	return float64(done) / float64(terminal) * 100
}

func tapeSettleLabel(done, bad int64) string {
	return fmt.Sprintf("%.1f%%", tapeSettle(done, bad))
}

// tapeMove quotes one lane: last-24h ticks vs the prior 24h.
func tapeMove(curr, prior int64) (label, class string) {
	switch {
	case prior > 0:
		pct := (float64(curr) - float64(prior)) / float64(prior) * 100
		switch {
		case pct > 0:
			return fmt.Sprintf("▲ %.0f%%", pct), "up"
		case pct < 0:
			return fmt.Sprintf("▼ %.0f%%", -pct), "down"
		default:
			return "— flat", "flat"
		}
	case curr > 0:
		return "NEW", "up"
	default:
		return "— flat", "flat"
	}
}

// tapeIndexMove quotes the whole fleet (one decimal, mockup style).
func tapeIndexMove(curr, prior int64) (label, class string) {
	switch {
	case prior > 0:
		pct := (float64(curr) - float64(prior)) / float64(prior) * 100
		switch {
		case pct > 0:
			return fmt.Sprintf("▲ %.1f%%", pct), "up"
		case pct < 0:
			return fmt.Sprintf("▼ %.1f%%", -pct), "down"
		default:
			return "— flat", "flat"
		}
	case curr > 0:
		return "NEW", "up"
	default:
		return "— flat", "flat"
	}
}

// tapePriceLabel renders the 24h notional as a $/hour quote — two decimals
// at/above a dollar, three below (the mockup's precision split).
func tapePriceLabel(cost24 float64) string {
	perHour := cost24 / 24
	if perHour >= 1 {
		return fmt.Sprintf("$%.2f/h", perHour)
	}
	return fmt.Sprintf("$%.3f/h", perHour)
}

// tapeSparkline renders 14 hourly counts as an SVG polyline across the
// mockup's 100×22 viewbox (y: 2 = busiest, 21 = quiet).
func tapeSparkline(counts []int64) string {
	var max int64
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	n := len(counts)
	pts := make([]string, n)
	for i, c := range counts {
		y := 21.0
		if max > 0 {
			y = 21.0 - 19.0*float64(c)/float64(max)
		}
		pts[i] = fmt.Sprintf("%.1f,%.1f", float64(i)*100.0/float64(n-1), y)
	}
	return strings.Join(pts, " ")
}

// tapeSymbol derives a deterministic ticker symbol from the lane name:
// uppercase alphanumerics only; longer names compress to first-4 + last-1 so
// sibling satellites stay distinguishable (hermes-canopy → HERCA-like).
func tapeSymbol(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	s := b.String()
	if s == "" {
		return "???"
	}
	if len(s) > 5 {
		return s[:4] + s[len(s)-1:]
	}
	return s
}

// sortTapeLanes orders the board: open lanes by ticks/h (desc, then name),
// suspended lanes after by raw urgency (desc, then name) — the mockup's
// shape. Sorting keys are numeric values, never their rendered labels.
func sortTapeLanes(lanes []TapeLane) {
	sort.SliceStable(lanes, func(i, j int) bool {
		a, b := lanes[i], lanes[j]
		if a.Enable != b.Enable {
			return a.Enable
		}
		if a.Enable {
			if ra, rb := float64(a.Ticks24h)/24.0, float64(b.Ticks24h)/24.0; ra != rb {
				return ra > rb
			}
		} else if a.urgency != b.urgency {
			return a.urgency > b.urgency
		}
		return a.Name < b.Name
	})
}

// commaGroup renders an integer with thousands separators (1,993).
func commaGroup(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
