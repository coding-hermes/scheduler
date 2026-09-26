package dashboard

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/agentlog"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// QueueEntry is one project in the evaluation queue view.
type QueueEntry struct {
	Name      string
	Weight    int
	Priority  int
	CooldownS int
	Enabled   bool
	Urgency   float64
	// SCHED-GAP-1589 display-only context. IntervalText is the lane's own
	// tick interval derived from its priority (ComputeInterval); WaitedText
	// is how long since last completion (else creation, the engine's elapsed
	// input); Band is the urgency band relative to this page's distribution.
	IntervalText string
	WaitedText   string
	Band         string
	// CooldownActive/CooldownText: the lane is still inside its cooldown
	// window (a pacing floor, not the queue order).
	CooldownActive bool
	CooldownText   string
	// WhyWaiting evidence: running lanes show their SCHED-GAP-157 admission
	// stamp; eligible-but-passed-over lanes show the latest deferrals
	// record. WhyRunning distinguishes the two shapes. WhyAt/WhyWaitMs are
	// the raw scanned stamps (RFC3339 / ms) the cell renders from.
	WhyRunning bool
	WhyReason  string
	WhyDetail  string
	WhyTitle   string
	WhyAt      string
	WhyWaitMs  int64
	// Nesting (SCHED-GAP-1590): the lane's position in the fleet hierarchy —
	// depth, its primary's name, and which parenthood source produced the
	// relation. Resolved by generator_lane_nesting.go over the SAME snapshot
	// this entry came from.
	Nesting laneNesting
}

// QueueData holds all data for the queue page.
type QueueData struct {
	Title       string
	Count       int
	TotalWeight int
	Entries     []QueueEntry
	// SCHED-GAP-1589: the page's own urgency scale, for the header sentence.
	MedianUrgencyText string
	P90UrgencyText    string
}

// FleetRow is one project in the fleet overview table.
type FleetRow struct {
	Name        string
	Weight      int
	Priority    int
	Enabled     bool
	LastTick    string
	LastOutcome string
	SessionID   string
	Urgency     float64
	RunningNow  int // 0 or 1; int avoids modernc.org/sqlite int→bool scan bug
	Completed   int
	Failed      int
	Timeout     int
	CostToday   float64
	CostWeek    float64
	// Board progress (parsed from <workdir>/.coding-hermes/tasks.md).
	Workdir           string
	CooldownS         int
	LastTickCompleted string
	BoardDone         int
	BoardTotal        int
	NextTickIn        string // human-readable "in Xm Ys", "running", "due now", "due — N board rows open", "idle — board drained", "tasks admission", or "—"
	// SCHED-GAP-1603: raw admission-mode inputs so the Next Tick cell can
	// resolve the lane's EFFECTIVE admission mode with the scheduler's rule
	// (project override → namespace default → cooldown). AdmissionMode "" =
	// inherit the namespace; NamespaceID "" = unscheduled (cooldown rule).
	AdmissionMode          string
	NamespaceID            string
	NamespaceAdmissionMode string
	// Recent cost series (last up-to-N completed ticks, oldest→newest) for the
	// cost sparkline, plus the count of recent failed/timeout ticks (failure flag).
	CostSeries     []float64
	RecentFailures int
	RecentTicks    int
	// Observability: average tick duration (seconds), success rate (0-100),
	// and estimated time-to-completion (from avg duration × steps left).
	AvgTickSecs int
	SuccessRate int // percent
	ETA         string
	// CompletionAt is the projected wall-clock completion as RFC3339 (UTC);
	// the dashboard renders it in the viewer's local timezone via JS.
	CompletionAt string
	// ProjectedCost is the estimated remaining cost to finish the board
	// (avg cost per completed tick × steps remaining).
	ProjectedCost float64
	// AvgCost is the mean cost per completed tick, used for live-cost estimate
	// of running ticks.
	AvgCost float64
	// EtaBreakdown is the learning-predictor per-type estimate, e.g.
	// "code ×2 40m + test ×5 25m" (empty when no signal).
	EtaBreakdown string
	// GitReins LLM-judge verdict pass rate (0-100) over the project history.
	GitReinsPass int // percent; -1 = no verdicts
	// CIConclusion is the latest GitHub Actions run conclusion (success/failure/
	// "" ) for the project's repo — an INDEPENDENT cross-check on GitReins. A
	// GitReins 100% is only trustworthy when CI is also green; a red CI flags
	// that the LLM-judge gate may be passing a suite that is actually failing
	// (e.g. cached test results). "" = unknown (no CI workflow or query failed).
	CIConclusion string

	// Nesting (SCHED-GAP-1590): the lane's position in the fleet hierarchy —
	// depth, its primary's name, and which parenthood source produced the
	// relation. Resolved by generator_lane_nesting.go over a full projects
	// snapshot (the fleet query itself does not select parent).
	Nesting laneNesting
}

// TickRow is one tick in the history table.
type TickRow struct {
	ID, Project, Status, Outcome, SessionID, SpawnedAt, CompletedAt string
	Commits, FilesChanged                                           int
	Duration                                                        string // human-readable elapsed time between spawned and completed
}

// NamespaceRow is one namespace in the allocation overview table.
type NamespaceRow struct {
	ID           string
	Weight       int
	Reserved     int
	HardCap      int
	Allocated    int
	Used         int
	Borrowed     int
	Lent         int
	ProjectCount int
	Utilization  float64
	// Demand (SCHED-GAP-1582): the enabled-weight the namespace carried
	// into its latest pack; Overcommitted the surplus HELD when demand
	// exceeded the allocation. 0 = not oversubscribed / no tick data.
	Demand        int
	Overcommitted int
}

// NamespaceTickRow is one namespace_tick in the utilization history table.
type NamespaceTickRow struct {
	TickGroup   string
	NamespaceID string
	Allocated   int
	Used        int
	Borrowed    int
	Lent        int
	CreatedAt   string
}

// FleetTableParams is the server-side table state (SCHED-GAP-1598): the
// search term, active sort, and page/size the overview tables were rendered
// with. The overview validates raw query params into this shape and both the
// full page and the htmx refresh render through it, so an autorefresh carries
// the operator's current view instead of resetting it.
type FleetTableParams struct {
	// Table is the owning table key ("projects", "ticks", "namespaces",
	// "nsticks") — it selects which columns are sortable.
	Table   string
	Q       string // substring match against lane name (projects) / namespace id
	Sort    string // column key; "" = the table's existing default order
	Dir     string // "asc" | "desc"; "" = asc
	Page    int    // 1-based
	PerPage int    // rows per page (bounded into pageSizeOptions); 0 = all
	// Counts are filled by fleetTableSlice once the filtered set is known.
	FilteredTotal int // rows matching search/sort (pre-pagination)
	TotalPages    int // ceil(FilteredTotal / PerPage), minimum 1
	// Pagination nav, filled by fleetNav after the page math.
	HasPrevious  bool
	PreviousPage int
	HasNext      bool
	NextPage     int
	// Filters are the params echoed back into the form inputs (validated:
	// values outside the vocabularies are dropped so a stray query param
	// renders the unfiltered view rather than a guaranteed-empty one).
	FilterP         string // projects: exact lane name (from DistinctTickProjects)
	FilterS         string // projects: exact last outcome (from outcomeVocabulary)
	PageSizeOptions []int  // selectable page sizes
}

// Query parameter keys, shared by the handler and the template so a rename
// cannot split the form from the parser.
const (
	fleetQKey    = "q"
	fleetSortKey = "sort"
	fleetDirKey  = "dir"
	fleetPageKey = "page"
	fleetSizeKey = "size"
)

// defaultFleetPageSize matches the tick-history page (tickHistoryPageSize,
// generator.go) so the two pages paginate identically.
const defaultFleetPageSize = 50

// pageSizeOptions are the per-table page-size choices. "all" is encoded as 0.
var pageSizeOptions = []int{25, 50, 100, 0}

// normalizeFleetTableParams validates raw form/query values into render-ready
// params. Unknown sort keys and directions fall back to the default order, a
// page beyond the result set is clamped by the caller once the total is known,
// and an unlisted page size falls back to the tick-history default of 50.
func normalizeFleetTableParams(raw FleetTableParams) FleetTableParams {
	p := raw
	p.Q = strings.TrimSpace(p.Q)
	p.FilterP = strings.TrimSpace(p.FilterP)
	p.FilterS = strings.TrimSpace(p.FilterS)
	if p.Sort != "" && !fleetSortValid(p.Table, p.Sort) {
		p.Sort = ""
	}
	if p.Dir != "asc" && p.Dir != "desc" {
		p.Dir = ""
	}
	if p.Dir == "" {
		p.Dir = "asc" // every sortable column defaults to ascending
	}
	known := false
	for _, s := range pageSizeOptions {
		if p.PerPage == s {
			known = true
			break
		}
	}
	if !known {
		p.PerPage = defaultFleetPageSize
	}
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSizeOptions == nil {
		p.PageSizeOptions = pageSizeOptions
	}
	return p
}

// fleetSortValid reports whether key is a sortable column of the named table.
func fleetSortValid(table, key string) bool {
	for _, k := range fleetSortable[table] {
		if k == key {
			return true
		}
	}
	return false
}

// fleetSortable lists the sortable column keys per overview table
// (SCHED-GAP-1598). The other tables keep their existing default order
// only — every listed key has a sort comparator in fleetTableSlice.
var fleetSortable = map[string][]string{
	"projects":   {"name", "weight", "priority", "last_tick", "outcome", "progress", "next", "cost_today"},
	"ticks":      {"project", "spawned"},
	"namespaces": {"id", "weight", "allocated", "used", "utilization", "projects"},
	"nsticks":    {"namespace", "created"},
}

// fleetTableSlice filters, sorts and slices one overview table
// (SCHED-GAP-1598). All of it is server-side on the full in-memory set — the
// page is already heavy and shipping 485 rows to the browser to filter there
// is exactly what this must not do. Pagination is the output of the math
// (page clamped into range; PerPage 0 = "all"); FilteredTotal carries the
// pre-pagination row count for the "showing N of M" line.
func fleetTableSlice[T any](rows []T, p *FleetTableParams, key func(T) string, less func(T, T) bool) []T {
	// 1. Search: case-insensitive substring on the row's key text.
	q := strings.ToLower(p.Q)
	filtered := rows
	if q != "" {
		filtered = make([]T, 0, len(rows))
		for _, r := range rows {
			if strings.Contains(strings.ToLower(key(r)), q) {
				filtered = append(filtered, r)
			}
		}
	}
	// 2. Sort. Stable so equal rows keep the query's default order — the
	// tables keep their existing default sequence as the unsorted baseline.
	if p.Sort != "" && less != nil {
		desc := p.Dir == "desc"
		sort.SliceStable(filtered, func(i, j int) bool {
			if desc {
				return less(filtered[j], filtered[i])
			}
			return less(filtered[i], filtered[j])
		})
	}
	// 3. Page math on the filtered (and sorted) set.
	p.FilteredTotal = len(filtered)
	if p.PerPage > 0 {
		p.TotalPages = (p.FilteredTotal + p.PerPage - 1) / p.PerPage
		if p.TotalPages == 0 {
			p.TotalPages = 1
		}
		if p.Page > p.TotalPages {
			p.Page = p.TotalPages
		}
		start := (p.Page - 1) * p.PerPage
		if start > p.FilteredTotal {
			start = p.FilteredTotal
		}
		end := start + p.PerPage
		if end > p.FilteredTotal {
			end = p.FilteredTotal
		}
		return filtered[start:end]
	}
	// "All" page size: one page, everything.
	p.TotalPages = 1
	p.Page = 1
	return filtered
}

// fleetPrefix maps each table to its query-parameter prefix. The projects
// table (the htmx-refreshed one) keeps the unprefixed family for backward
// compatibility; the other three are prefixed so one URL can carry all four
// states at once.
var fleetPrefix = map[string]string{
	"projects":   "",
	"ticks":      "t",
	"namespaces": "n",
	"nsticks":    "h",
}

// fleetQueryParams returns the non-empty table-state values as URL-encoded
// query-string fragments — used by pagination links and the htmx poll URL so
// search/page/sort survive a refresh (SCHED-GAP-1598). Keys carry the
// table's prefix so each table's links only ever touch its own params.
func fleetQueryParams(p FleetTableParams) []string {
	pref := fleetPrefix[p.Table]
	var parts []string
	if p.Q != "" {
		parts = append(parts, pref+fleetQKey+"="+url.QueryEscape(p.Q))
	}
	if p.Sort != "" {
		parts = append(parts, pref+fleetSortKey+"="+url.QueryEscape(p.Sort))
		parts = append(parts, pref+fleetDirKey+"="+url.QueryEscape(p.Dir))
	}
	if p.PerPage != defaultFleetPageSize {
		parts = append(parts, pref+fleetSizeKey+"="+url.QueryEscape(strconv.Itoa(p.PerPage)))
	}
	if p.FilterP != "" {
		parts = append(parts, "project="+url.QueryEscape(p.FilterP))
	}
	if p.FilterS != "" {
		parts = append(parts, "outcome="+url.QueryEscape(p.FilterS))
	}
	return parts
}

// fleetPageLink renders "key=value" page link parameters with the current
// table state minus the page parameter itself (the caller appends its own),
// mirroring tickHistoryData's BaseQS so filtering survives paging.
func (p FleetTableParams) BaseQS() string {
	return strings.Join(fleetQueryParams(p), "&")
}

type FleetData struct {
	Title       string
	GeneratedAt string
	BudgetTotal int
	BudgetUsed  int
	// SCHED-GAP-1582: oversubscription is an explicit, configured state —
	// enabled namespaces' latest measured demand vs the PER-TICK budget.
	// Shown as a labelled note, never as a fraction of the fleet-wide sum
	// (the SCHED-GAP-1583 lesson: a fleet sum and a per-tick budget are
	// not comparable quantities, so the note names both sides verbatim).
	BudgetOversubscribed bool
	NamespaceDemandTotal int
	ActiveTicks          int
	TotalProjects        int
	EnabledProjects      int
	Projects             []FleetRow
	RecentTicks          []TickRow
	Namespaces           []NamespaceRow
	NamespaceTicks       []NamespaceTickRow
	CostTodayTotal       float64
	CostWeekTotal        float64
	// SCHED-GAP-1601: the console control surface. The generator proxies
	// every control through the in-process API handler (same auth gate, same
	// audit); FleetPaused is the loop's authoritative paused flag rendered on
	// the console strip.
	Control     ControlData
	FleetPaused bool
	// FleetPausedKnown renders as the "unknown" state (API read failed) —
	// never a fabricated paused/resumed badge.
	FleetPausedKnown bool
	// TableState carries the operator's per-table server-side controls
	// (SCHED-GAP-1598): search / sort / page / size for each of the four
	// stacked tables. The full page renders from these; the htmx autorefresh
	// handler parses the same query params and re-renders through them, so a
	// refresh preserves the current view instead of resetting it.
	TableState FleetTables
	// OutcomeOptions / PageSizeOptions are the dropdown vocabularies for the
	// projects filter (outcome) and every table's page-size control.
	OutcomeOptions  []string
	PageSizeOptions []int
}

// FleetTables is the per-table parameter set for the overview page.
type FleetTables struct {
	Projects   FleetTableParams
	Ticks      FleetTableParams
	Namespaces FleetTableParams
	NSHistory  FleetTableParams
	// ProjectOptions is the lane-name vocabulary for the projects-table
	// filter dropdown (lanes present in the ticks table).
	ProjectOptions []string
}

// ProjectDetailData holds all data for the /projects/{name} page.
type ProjectDetailData struct {
	Title         string
	Project       *database.Project
	LatestTick    *database.Tick
	RecentTicks   []database.Tick
	BoardDone     int
	BoardTotal    int
	NextTickIn    string
	AvgTickSecs   int
	SuccessRate   int
	ETA           string
	BoardSteps    []BoardStep
	TickWork      map[string]string // tick id → what it worked on (commit subjects)
	GitReins      GitReinsSummary
	CompletionAt  string
	ProjectedCost float64
	AvgCost       float64          // mean cost per completed tick (for live-cost estimate)
	EtaBreakdown  string           // per-type estimate, e.g. "code ×2 40m + test ×5 25m"
	SpeedCost     []SpeedCostPoint // for the speed/cost-over-time charts
	// SCHED-GAP-1601: the lane's control strip (pause/resume/spawn/bump/
	// unbump/update/delete — every action the API offers on this lane) and
	// the global paused state so the pause/resume pair reads honestly.
	Control     ControlData
	FleetPaused bool
	// FleetPausedKnown renders as the "unknown" state (API read failed) —
	// never a fabricated badge.
	FleetPausedKnown bool
}

// BoardStep is one task row from the board, for the roadmap visualization.
type BoardStep struct {
	ID     string
	Title  string
	Status string // "done" | "active" | "pending"
	Commit string
}

// GitReinsVerdict is one LLM-judge verdict from .gitreins/history.
type GitReinsVerdict struct {
	TaskID      string
	TaskTitle   string
	Passed      bool
	Tier1Passed bool
	Tier2Passed bool
	HasTier2    bool
	EvaluatedAt string
}

// GitReinsSummary is the aggregate pass rate + latest verdicts for a project.
type GitReinsSummary struct {
	Total   int
	Passed  int
	Failed  int
	RatePct int
	Latest  []GitReinsVerdict // newest first, capped
}

// TickHistoryData holds one page of the global tick history.
type TickHistoryData struct {
	Title        string
	GeneratedAt  string
	Ticks        []database.Tick
	Page         int
	PageSize     int
	TotalTicks   int
	TotalPages   int
	HasPrevious  bool
	PreviousPage int
	HasNext      bool
	NextPage     int
	// Search/filter state (SCHED-GAP-1593). Filters are server-side; the
	// empty strings mean "no filter". Filtered=false renders the plain
	// history heading.
	Filtered       bool
	FilterQ        string   // substring match against tick id / project name
	FilterP        string   // exact project (lane) name
	FilterS        string   // exact status
	FilterO        string   // exact outcome
	StatusOptions  []string // the tick status vocabulary, for the dropdown
	OutcomeOptions []string // the tick outcome vocabulary, for the dropdown
	ProjectOptions []string // lanes seen in the tick table, for the dropdown
	// BaseQS is the filter query string WITHOUT the page parameter —
	// pagination links append their own ?page=N so filtering survives
	// paging.
	BaseQS string
}

// TickEventRow is one scheduler log event on the tick detail page.
type TickEventRow struct {
	ID        int64
	Severity  string
	Component string
	Message   string
	CreatedAt string
	Matched   bool // event message explicitly names this tick's id
}

// TickDetailData holds everything /ticks/{id} renders. The agent-side
// fields carry the honest-degradation contract: AgentStatus says exactly
// why the transcript is absent when it is (SCHED-GAP-1593 AC 4 — an
// operator must never mistake "could not fetch" for "nothing generated").
type TickDetailData struct {
	Title       string
	GeneratedAt string
	Tick        *database.Tick

	// GatewayTrace state.
	HasTrace      bool
	TraceModel    string
	TraceProvider string
	TraceSession  string
	TraceElapsedS int
	TraceEvents   int
	TraceAttempts int
	TraceClass    string

	// Agent transcript state (from the trace's session_id → state.db).
	AgentStatus  string // "resolved" | "session-not-found" | "unavailable" | "no-trace"
	AgentDetail  string // explicit human explanation when status != resolved
	AgentSession *agentlog.SessionInfo
	AgentTurns   []agentlog.Turn
	AgentCapped  bool

	// Scheduler log events around the tick window.
	Events      []TickEventRow
	EventsTotal int
	HasMore     bool
}

// NamespaceViewData holds namespace configuration, projects, and recent
// allocation history for /namespaces/{id}.
type NamespaceViewData struct {
	Title           string
	Namespace       *database.Namespace
	Projects        []database.Project
	RecentTicks     []database.NamespaceTick
	LatestTick      *database.NamespaceTick
	EnabledProjects int
	TotalWeight     int
	Utilization     float64
	// SCHED-GAP-1601: the namespace's control strip (update/move/delete)
	// plus the lane-name options the move control offers.
	Control     ControlData
	FleetPaused bool
	// FleetPausedKnown renders as the "unknown" state (API read failed) —
	// never a fabricated badge.
	FleetPausedKnown bool
	LaneOptions      []string
}

// HealthData holds daemon, database, gateway, and DuckBrain liveness info.
type HealthData struct {
	Title            string
	GeneratedAt      string
	DaemonStatus     string
	DatabaseStatus   string
	GatewayStatus    string
	GatewayURL       string
	DuckBrainStatus  string
	DuckBrainBaseURL string
	DuckBrainSpooled int
	Uptime           string
	ActiveTicks      int
	TotalTicks       int
	Goroutines       int
	MemoryMB         float64
	// HostSample* (ADV-R13): the latest PERSISTED host load/memory sample
	// (host_samples table) — not the daemon's own runtime.MemStats above,
	// which only ever measured this process. HostSampleAvailable=false
	// means no sample has been recorded yet; the template renders an
	// honest "unavailable" instead of a fabricated 0.00.
	HostSampleAvailable bool
	HostLoad1           float64
	HostLoad5           float64
	HostLoad15          float64
	HostMemTotalMB      float64
	HostMemAvailMB      float64
	HostSampleAt        string
	HostSampleSource    string
}

func (g *Generator) collect(ctx context.Context) FleetData {
	// ADV-R09/G8: the effective budget from the resolution chain; 0 (tests,
	// SetWeightBudget never called) renders the documented default of 100.
	budgetTotal := g.weightBudget
	if budgetTotal <= 0 {
		budgetTotal = 100
	}
	data := FleetData{
		Title:       "Fleet Overview",
		GeneratedAt: g.clock().Now().Format(time.RFC3339),
		BudgetTotal: budgetTotal,
	}

	// ── Projects: batch query with per-project stats via LEFT JOINs ──
	// Single query replaces 7 per-project queries (N+1 → 1).
	// Note: outcome and session_id are fetched via a SECOND LEFT JOIN to ticks
	// (t2) rather than correlated subqueries — SQLite's modernc driver rejects
	// MAX() references inside correlated subqueries ("misuse of aggregate").
	projectQuery := `
		SELECT
			p.name, p.weight, p.priority, p.enabled,
			COALESCE(p.workdir, '')            AS workdir,
			COALESCE(p.cooldown_s, 900)        AS cooldown_s,
			COALESCE(p.last_tick_completed, '') AS last_tick_completed,
			COALESCE(t.spawned_at, '')            AS last_tick,
			COALESCE(t2.outcome, '')               AS last_outcome,
			COALESCE(t2.session_id, '')            AS session_id,
			COALESCE(t.running, 0) > 0             AS running_now,
			COALESCE(t.completed, 0)               AS completed,
			COALESCE(t.failed, 0)                  AS failed,
			COALESCE(t.timed_out, 0)              AS timed_out,
			COALESCE(t.cost_today, 0.0)            AS cost_today,
			COALESCE(t.cost_week, 0.0)             AS cost_week,
			COALESCE(p.admission_mode, '')          AS admission_mode,
			COALESCE(p.namespace_id, '')            AS namespace_id,
			COALESCE(ns.admission_mode, '')         AS ns_admission_mode
		FROM projects p
		LEFT JOIN namespaces ns ON ns.id = p.namespace_id
		LEFT JOIN (
			SELECT
				tk.project_name,
				MAX(tk.spawned_at) AS spawned_at,
				SUM(CASE WHEN tk.status = 'running'   THEN 1 ELSE 0 END) AS running,
				SUM(CASE WHEN tk.status = 'completed' THEN 1 ELSE 0 END) AS completed,
				SUM(CASE WHEN tk.status = 'failed'    THEN 1 ELSE 0 END) AS failed,
				SUM(CASE WHEN tk.status = 'timeout'   THEN 1 ELSE 0 END) AS timed_out,
				COALESCE(SUM(CASE WHEN tk.status = 'completed' AND tk.completed_at >= ? THEN tk.cost_usd ELSE 0 END), 0.0) AS cost_today,
				COALESCE(SUM(CASE WHEN tk.status = 'completed' AND tk.completed_at >= ? THEN tk.cost_usd ELSE 0 END), 0.0) AS cost_week
			FROM ticks tk
			GROUP BY tk.project_name
		) t ON t.project_name = p.name
		LEFT JOIN ticks t2 ON t2.project_name = t.project_name AND t2.spawned_at = t.spawned_at
		ORDER BY p.name
	`
	// completed_at is stored as UTC RFC3339 (nowUTC in database/ticks.go), so
	// the window bounds must be UTC too — comparing local-offset strings
	// lexicographically against UTC strings mis-counts ticks near the boundary
	// by the server's UTC offset.
	dayAgo := g.clock().Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	weekAgo := g.clock().Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	rows, err := g.db.QueryContext(ctx, projectQuery, dayAgo, weekAgo)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r FleetRow
			if err := rows.Scan(&r.Name, &r.Weight, &r.Priority, &r.Enabled,
				&r.Workdir, &r.CooldownS, &r.LastTickCompleted,
				&r.LastTick, &r.LastOutcome, &r.SessionID,
				&r.RunningNow, &r.Completed, &r.Failed, &r.Timeout,
				&r.CostToday, &r.CostWeek,
				&r.AdmissionMode, &r.NamespaceID, &r.NamespaceAdmissionMode); err != nil {
				continue
			}
			data.TotalProjects++
			if r.Enabled {
				data.EnabledProjects++
				data.BudgetUsed += r.Weight
			}
			// Urgency: priority * (1 + hours since last tick)
			if r.LastTick != "" {
				if t, err := time.Parse(time.RFC3339, r.LastTick); err == nil {
					r.Urgency = float64(r.Priority) * (1 + g.clock().Since(t).Hours())
				}
			}
			// Board progress (done/total) from the project's tasks.md, plus the
			// human-readable countdown to the next tick.
			// SCHED-GAP-1603: the countdown only applies to cooldown-admission
			// lanes. A tasks-admission lane admits on board work, so its cell
			// renders board-driven state from the SAME readBoardProgress pass
			// (no second board reader); a lane with no readable board renders
			// the honest "tasks admission" label. The mode resolves with the
			// scheduler's own rule (project → namespace → cooldown).
			if r.Workdir != "" {
				r.BoardDone, r.BoardTotal = readBoardProgress(filepath.Join(r.Workdir, ".coding-hermes", "tasks.md"))
			}
			if effectiveAdmissionModeFor(r.AdmissionMode, r.NamespaceID, map[string]string{r.NamespaceID: r.NamespaceAdmissionMode}) == database.AdmissionModeTasks {
				r.NextTickIn = tasksAdmissionLabel(r.RunningNow == 1, r.BoardDone, r.BoardTotal)
			} else {
				r.NextTickIn = nextTickInAt(g.clock(), r.RunningNow == 1, r.LastTickCompleted, r.CooldownS)
			}
			data.CostTodayTotal += r.CostToday
			data.CostWeekTotal += r.CostWeek
			data.Projects = append(data.Projects, r)
		}
	}

	// SCHED-GAP-1590: the overview's Projects table renders lane nesting —
	// each row is annotated with its depth + primary (↳ rail) resolved from
	// the authoritative parent column via database.BuildLaneTree. The full
	// lane list (including disabled lanes — they keep their tree position) is
	// fetched in ONE extra query; the fleet table has no parity constraint
	// with an API ordering, so this is purely additive markup.
	if lanes, laneErr := database.ListProjects(ctx, g.db, false); laneErr == nil {
		annotateFleetNesting(data.Projects, lanes)
	}

	// Second pass for cost sparklines + recent failure flags + observability.
	// Done AFTER the project rows cursor is fully closed — the modernc.org/sqlite
	// driver deadlocks if we open nested queries on the same connection while a
	// rows cursor is still open (the collect() N+1 warning).
	// The fleet-wide learned prior is built ONCE and shared across all projects.
	fleet := g.fleetLearned(ctx)
	// CI conclusions (DASH-PERF-001): warm the TTL cache once, with bounded
	// concurrency, before the per-project loop — the ciConclusion reads below
	// then hit the cache and no render ever serializes N gh subprocesses again.
	workdirs := make([]string, 0, len(data.Projects))
	for i := range data.Projects {
		if wd := data.Projects[i].Workdir; wd != "" {
			workdirs = append(workdirs, wd)
		}
	}
	g.warmCIConclusions(workdirs)
	// DASH-PERF-003: the per-project stats below previously cost ~4-5 serial
	// DB round-trips per project (~176 total on the single shared connection —
	// the warm-render bottleneck). Two window-function queries now fetch every
	// project's recent ticks in one pass each, and the enrichment loop is pure
	// computation + file/subprocess work running with bounded concurrency
	// (projMaxConcurrent). Parallel DB queries would serialize on the
	// SetMaxOpenConns(1) pool anyway; batching is the only lever that cuts
	// serial DB time.
	samplesByProject := g.batchCompletedSamples(ctx)
	healthByProject := g.batchTickHealth(ctx)
	g.enrichProjects(data.Projects, samplesByProject, healthByProject, fleet)

	// Active ticks count.
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&data.ActiveTicks)

	// Recent ticks.
	tickRows, _ := g.db.QueryContext(ctx, `SELECT id, project_name, status, COALESCE(outcome,''), COALESCE(session_id,''), spawned_at, COALESCE(completed_at,''), commits, files_changed FROM ticks ORDER BY spawned_at DESC LIMIT 20`)
	if tickRows != nil {
		defer tickRows.Close()
		for tickRows.Next() {
			var t TickRow
			_ = tickRows.Scan(&t.ID, &t.Project, &t.Status, &t.Outcome, &t.SessionID, &t.SpawnedAt, &t.CompletedAt, &t.Commits, &t.FilesChanged)
			t.Duration = tickDuration(t.SpawnedAt, t.CompletedAt)
			data.RecentTicks = append(data.RecentTicks, t)
		}
	}

	// Namespaces — batch latest ticks + project counts to avoid N+1.
	namespaces, err := database.ListNamespaces(ctx, g.db, false)
	if err == nil && len(namespaces) > 0 {
		// Batch 1: latest namespace_tick per namespace (1 query, not N).
		// SCHED-GAP-1582: demand/overcommitted ride the same row — they are
		// what makes an oversubscribed namespace explicit instead of a bar
		// that overflows by construction.
		type nsTickVal struct {
			allocated, used, borrowed, lent int
			demand, overcommitted           int
		}
		latestTicks := make(map[string]nsTickVal)
		tickRows, terr := g.db.QueryContext(ctx, `
			SELECT nt.namespace_id, nt.allocated, nt.used, nt.borrowed, nt.lent, nt.demand, nt.overcommitted
			FROM namespace_ticks nt
			INNER JOIN (
				SELECT namespace_id, MAX(created_at) AS max_created
				FROM namespace_ticks
				GROUP BY namespace_id
			) latest ON nt.namespace_id = latest.namespace_id AND nt.created_at = latest.max_created
		`)
		if terr == nil {
			defer tickRows.Close()
			for tickRows.Next() {
				var nsID string
				var v nsTickVal
				if tickRows.Scan(&nsID, &v.allocated, &v.used, &v.borrowed, &v.lent, &v.demand, &v.overcommitted) == nil {
					latestTicks[nsID] = v
				}
			}
		}

		// Batch 2: enabled project count per namespace (1 query, not N).
		projectCounts := make(map[string]int)
		countRows, cerr := g.db.QueryContext(ctx, `
			SELECT namespace_id, COUNT(*) FROM projects WHERE enabled=1 GROUP BY namespace_id
		`)
		if cerr == nil {
			defer countRows.Close()
			for countRows.Next() {
				var nsID string
				var cnt int
				if countRows.Scan(&nsID, &cnt) == nil {
					projectCounts[nsID] = cnt
				}
			}
		}

		for _, ns := range namespaces {
			row := NamespaceRow{
				ID:       ns.ID,
				Weight:   ns.Weight,
				Reserved: ns.Reserved,
				HardCap:  ns.HardCap,
			}
			if v, ok := latestTicks[ns.ID]; ok {
				row.Allocated = v.allocated
				row.Used = v.used
				row.Borrowed = v.borrowed
				row.Lent = v.lent
				row.Demand = v.demand
				row.Overcommitted = v.overcommitted
			}
			if row.Allocated > 0 {
				row.Utilization = float64(row.Used) / float64(row.Allocated) * 100
			}
			row.ProjectCount = projectCounts[ns.ID]
			data.Namespaces = append(data.Namespaces, row)
		}

		// SCHED-GAP-1582: the oversubscription note — SUM of the latest
		// measured per-namespace DEMAND of ENABLED namespaces, compared
		// against the per-tick budget. This is a deliberate, labelled
		// comparison of demand-vs-budget (not the fleet weight sum vs
		// budget), and it never renders as a percentage.
		for _, ns := range namespaces {
			if !ns.Enabled {
				continue
			}
			data.NamespaceDemandTotal += latestTicks[ns.ID].demand
		}
		data.BudgetOversubscribed = data.NamespaceDemandTotal > budgetTotal
	}

	// Recent namespace ticks for the utilization chart.
	nsTickRows, _ := g.db.QueryContext(ctx, `SELECT tick_group, namespace_id, allocated, used, borrowed, lent, created_at FROM namespace_ticks ORDER BY created_at DESC LIMIT 100`)
	if nsTickRows != nil {
		defer nsTickRows.Close()
		for nsTickRows.Next() {
			var nt NamespaceTickRow
			_ = nsTickRows.Scan(&nt.TickGroup, &nt.NamespaceID, &nt.Allocated, &nt.Used, &nt.Borrowed, &nt.Lent, &nt.CreatedAt)
			data.NamespaceTicks = append(data.NamespaceTicks, nt)
		}
	}

	return data
}

// ── Batched per-project stats (DASH-PERF-003) ──────────────────────────────
//
// The fleet overview previously ran ~4-5 serial DB queries per project
// (recentCostSeries, recentTickHealth, observabilityStats, learnedETA) —
// ~176 round-trips on the single shared connection (SetMaxOpenConns(1)), the
// warm-render bottleneck. Two window-function queries now fetch every
// project's recent ticks in one pass each, and the per-project enrichment
// below is pure computation + file/subprocess work.

// completedSample is one completed tick's timing/cost in the batched stats
// path.
type completedSample struct {
	spawnedAt   string
	completedAt string
	costUSD     float64
}

// tickHealth is the (total, failed) recent-tick summary for one project.
type tickHealth struct {
	total  int
	failed int
}

// batchCompletedSamples returns, per project, the last up-to-20 completed
// ticks (newest first) in ONE query — previously three serial queries per
// project (recentCostSeries ×12, observabilityStats ×10, learnedETA ×20).
// One window covers all three consumers: the cost sparkline takes the first
// 12, observability the first 10 with a non-empty completed_at, and the
// learning predictor the first 20 with a parseable duration. Completed ticks
// with an empty completed_at are vanishingly rare (none exist in the fleet
// DB); they land in the cost series and are skipped by the duration-filtered
// consumers, which matches the old per-query behavior within the top-20
// window.
func (g *Generator) batchCompletedSamples(ctx context.Context) map[string][]completedSample {
	out := map[string][]completedSample{}
	rows, err := g.db.QueryContext(ctx, `
		SELECT project_name, spawned_at, completed_at, cost_usd
		FROM (
			SELECT project_name, spawned_at, completed_at, cost_usd,
			       ROW_NUMBER() OVER (PARTITION BY project_name ORDER BY spawned_at DESC) AS rn
			FROM ticks
			WHERE status = 'completed'
		) WHERE rn <= 20
	`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name, sp, co string
		var cost float64
		if rows.Scan(&name, &sp, &co, &cost) == nil {
			out[name] = append(out[name], completedSample{spawnedAt: sp, completedAt: co, costUSD: cost})
		}
	}
	// ROW_NUMBER guarantees the rank within each partition, but the emitted
	// row order is planner-dependent — sort each project's slice by
	// spawned_at desc (RFC3339 UTC strings sort lexicographically) so the
	// caps below are deterministic.
	for name := range out {
		sort.Slice(out[name], func(i, j int) bool {
			return out[name][i].spawnedAt > out[name][j].spawnedAt
		})
	}
	return out
}

// batchTickHealth returns, per project, (total, failed) over the last
// up-to-10 ticks (any status) in ONE query — previously one query per project
// (recentTickHealth).
func (g *Generator) batchTickHealth(ctx context.Context) map[string]tickHealth {
	out := map[string]tickHealth{}
	rows, err := g.db.QueryContext(ctx, `
		SELECT project_name, status
		FROM (
			SELECT project_name, status,
			       ROW_NUMBER() OVER (PARTITION BY project_name ORDER BY spawned_at DESC) AS rn
			FROM ticks
		) WHERE rn <= 10
	`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name, status string
		if rows.Scan(&name, &status) == nil {
			h := out[name]
			h.total++
			if status == "failed" || status == "timeout" {
				h.failed++
			}
			out[name] = h
		}
	}
	return out
}

// costSeriesFromSamples extracts the last up-to-n completed-tick costs,
// oldest→newest, from a newest-first sample slice — the batched equivalent of
// recentCostSeries. Returns an empty (non-nil) slice on empty, matching
// recentCostSeries' empty result (renders as the em-dash sparkline).
func costSeriesFromSamples(samples []completedSample, n int) []float64 {
	if len(samples) == 0 {
		return []float64{} // non-nil empty, matching recentCostSeries' empty result
	}
	if n > len(samples) {
		n = len(samples)
	}
	out := make([]float64, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, samples[i].costUSD)
	}
	return out
}

// observabilityFromSamples computes the observabilityStats tuple from a
// newest-first completed-sample slice — the batched equivalent of
// observabilityStats. The duration/cost averages count only the first 10
// samples with a non-empty completed_at (matching the old per-project SQL
// LIMIT 10 filter); samples with unparseable windows are skipped.
func observabilityFromSamples(clk clock.Clock, samples []completedSample, boardDone, boardTotal, recentTicks, recentFailures int) (avgSecs int, avgCost float64, successPct int, eta, completionAt string, projectedCost float64) {
	// Average duration + cost over up-to-10 completed ticks.
	var total time.Duration
	var totalCost float64
	var count int
	seen := 0 // samples with non-empty completed_at, mirroring the old LIMIT 10
	for _, s := range samples {
		if seen >= 10 {
			break
		}
		if s.completedAt == "" {
			continue
		}
		seen++
		if d := parseDuration(s.spawnedAt, s.completedAt); d > 0 {
			total += d
			totalCost += s.costUSD
			count++
		}
	}
	if count > 0 {
		avgSecs = int(total.Seconds() / float64(count))
		if avgSecs < 60 {
			avgSecs = 60 // floor so ETA isn't absurdly short
		}
		avgCost = totalCost / float64(count)
	}

	// Success rate over the last N ticks (recentTicks = total, recentFailures = bad).
	if recentTicks > 0 {
		successPct = (recentTicks - recentFailures) * 100 / recentTicks
	}

	// ETA + completion timestamp + projected cost from steps remaining.
	remaining := 0
	if boardTotal > 0 {
		remaining = boardTotal - boardDone
	}
	if remaining > 0 {
		if avgSecs > 0 {
			// avgSecs is in seconds; convert to a Duration properly.
			d := time.Duration(avgSecs) * time.Second * time.Duration(remaining)
			eta = formatETA(d)
			completionAt = clk.Now().UTC().Add(d).Format(time.RFC3339)
		}
		if avgCost > 0 {
			projectedCost = avgCost * float64(remaining)
		}
	}
	return avgSecs, avgCost, successPct, eta, completionAt, projectedCost
}

// tickSamplesFromCompleted converts batched completed samples into the
// tickSample form the learning predictor consumes, classifying each tick's
// work via git log. Only samples with a parseable duration are kept —
// matching learnedETA's own history query semantics (completed_at != ”,
// then duration-filtered).
func tickSamplesFromCompleted(clk clock.Clock, workdir string, samples []completedSample) []tickSample {
	var out []tickSample
	for _, s := range samples {
		d := parseDuration(s.spawnedAt, s.completedAt)
		if d <= 0 {
			continue
		}
		out = append(out, tickSample{dur: d, cost: s.costUSD, work: tickWork(clk, workdir, s.spawnedAt, s.completedAt, 4)})
	}
	return out
}

// enrichProjects fills the per-project dashboard fields (cost sparkline,
// recent health, observability, learned ETA, GitReins pass rate, CI
// conclusion) from the batched stats maps. All DB reads happened up front;
// the rest is file reads + git log subprocesses + cache lookups, so projects
// run with bounded concurrency (projMaxConcurrent). The fleet prior is
// read-only here, and each project row is touched by exactly one goroutine.
func (g *Generator) enrichProjects(projects []FleetRow, samplesByProject map[string][]completedSample, healthByProject map[string]tickHealth, fleet *fleetModel) {
	sem := make(chan struct{}, projMaxConcurrent)
	var wg sync.WaitGroup
	for i := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			r := &projects[i]
			samples := samplesByProject[r.Name]
			r.CostSeries = costSeriesFromSamples(samples, 12)
			if h, ok := healthByProject[r.Name]; ok {
				r.RecentTicks, r.RecentFailures = h.total, h.failed
			}
			r.AvgTickSecs, r.AvgCost, r.SuccessRate, r.ETA, r.CompletionAt, r.ProjectedCost = observabilityFromSamples(g.clock(), samples, r.BoardDone, r.BoardTotal, r.RecentTicks, r.RecentFailures)
			// Learning ETA: predict remaining time + cost from per-task-type
			// estimates learned from tick history + the fleet-wide prior.
			if r.Workdir != "" {
				steps := readBoardSteps(filepath.Join(r.Workdir, ".coding-hermes", "tasks.md"))
				if learned, learnedAt, breakdown, projCost := learnedETAFromSamples(g.clock(), steps, tickSamplesFromCompleted(g.clock(), r.Workdir, samples), fleet); learned > 0 {
					r.ETA = formatETA(learned)
					r.CompletionAt = learnedAt
					r.EtaBreakdown = breakdown
					if projCost > 0 {
						r.ProjectedCost = projCost
					}
				}
			}
			r.GitReinsPass = -1
			if r.Workdir != "" {
				if gr := cachedReadGitReins(r.Workdir, 0); gr.Total > 0 {
					r.GitReinsPass = gr.RatePct
				}
				r.CIConclusion = g.ciConclusion(r.Workdir)
			}
		}(i)
	}
	wg.Wait()
}

// latestTickForProject returns the most recently spawned tick for the project,
// or nil if the project has never been scheduled. Implementation lives here
// (not in the database package) to avoid widening the db API for a single
// dashboard caller; the SQL is a single indexed row lookup.
func latestTickForProject(ctx context.Context, db *sql.DB, projectName string) (*database.Tick, error) {
	const q = `SELECT id, project_name, COALESCE(session_id,''), status, COALESCE(outcome,''), COALESCE(spawned_at,''), COALESCE(completed_at,''), COALESCE(exit_code, 0), commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, COALESCE(error,''), created_at
FROM ticks WHERE project_name = ?
ORDER BY spawned_at DESC LIMIT 1`
	var t database.Tick
	var status, outcome string
	err := db.QueryRowContext(ctx, q, projectName).Scan(
		&t.ID, &t.ProjectName, &t.SessionID, &status, &outcome,
		&t.SpawnedAt, &t.CompletedAt, &t.ExitCode, &t.Commits, &t.FilesChanged,
		&t.TokensIn, &t.TokensOut, &t.CostUSD, &t.Urgency, &t.WeightUsed,
		&t.Error, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // no ticks yet — not an error for the dashboard
	}
	if err != nil {
		return nil, fmt.Errorf("latest tick for %q: %w", projectName, err)
	}
	t.Status = database.TickStatus(status)
	t.Outcome = database.TickOutcome(outcome)
	return &t, nil
}

// readBoardProgress counts task rows in a coding-hermes board file. It
// returns (done, total). Board format (model-router matrix):
//
//	## Active          → table rows "| T06 | ..." are PENDING tasks
//	## Completed       → table rows "| T05 | ..." are DONE tasks
//	## [ ] NEVER-DONE  → perpetual audit; NOT counted (never completes)
//
// A task row is any line starting with "| T" (or "| T00"-style task id) inside
// the Active or Completed section. Returns (0,0) if the board is missing or
// unreadable, so the dashboard degrades gracefully.
func readBoardProgress(path string) (done, total int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	section := "" // "active" | "completed" | other
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "## "):
			low := strings.ToLower(line)
			switch {
			case strings.Contains(low, "active"):
				section = "active"
			case strings.Contains(low, "completed"):
				section = "completed"
			default:
				// NEVER-DONE or any other section — not counted.
				section = "other"
			}
		case isTaskRow(line):
			// Task row. The NEVER-DONE line ("## [ ] NEVER-DONE") is a heading,
			// not a task row, so it never reaches here.
			switch section {
			case "active":
				total++
				// Markdown checklist item "- [x]" in an active section is done.
				if strings.HasPrefix(line, "- [x] ") {
					done++
				}
			case "completed":
				done++
				total++
			}
		}
	}
	return done, total
}

// trailingCommitRe matches a trailing "(<commit_hash>[, ...])" reference in a
// markdown checklist title, e.g. "(b0420b9, 2026-08-08)" or "(3cd9b0e)". Used
// by readBoardSteps to extract the commit for done steps (gitreins2 boards).
var trailingCommitRe = regexp.MustCompile(`\(([0-9a-f]{7}|[0-9a-f]{40})[,\)]`)

// isTaskRow reports whether a trimmed line is a task row — either a table row
// ("| T05 | ...") or a markdown task-list item ("- [ ] T05 ..." / "- [x] T05 ...").
// This lets the board parser count any task-prefix (T##, V01, DOCS-###, R2-##)
// in either board format. (Markdown checklist support added 2026-08-08 — some
// boards, e.g. gitreins2, track tasks as "- [x] R2-1 ..." rather than table rows.)
func isTaskRow(line string) bool {
	// Markdown task-list item: "- [ ] <ID>" or "- [x] <ID>".
	if strings.HasPrefix(line, "- [ ] ") || strings.HasPrefix(line, "- [x] ") {
		rest := line[6:] // strip "- [ ] " (6 chars) — covers both "[ ]" and "[x]"
		// ID is everything up to the first space or tab.
		end := strings.IndexAny(rest, " 	")
		if end <= 0 {
			return false
		}
		return isTaskID(rest[:end])
	}
	// Table row: "| <ID> |".
	if !strings.HasPrefix(line, "| ") {
		return false
	}
	rest := strings.TrimPrefix(line, "| ")
	idx := strings.Index(rest, " |")
	if idx <= 0 {
		return false
	}
	return isTaskID(rest[:idx])
}

// isTaskID reports whether s looks like a task identifier: 2+ chars of
// uppercase letters, digits, hyphens (e.g. T05, V01, DOCS-000, E2E-001).
// The first char must be a letter so the "---" table separator is excluded,
// and the literal header id "ID" is rejected so the table header row is not
// counted as a task.
func isTaskID(s string) bool {
	if len(s) < 2 || s == "ID" || !isUpperLetter(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isUpperLetter(c) && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isUpperLetter(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

// SCHED-GAP-1603: admission-mode-aware Next Tick rendering.
//
// The scheduler admits a lane either on its cooldown timer ("cooldown", the
// historical cron gate) or on board work ("tasks" — non-perpetual pending
// rows admit immediately; the cooldown pin resumes only once the board is
// drained). The fleet table's Next Tick column used to render a cooldown
// countdown for EVERY lane, so a tasks-mode foreman (e.g. a 6h-pin lane in
// a tasks namespace) showed a bogus "in 5h 59m" while its cooldown-paced
// satellites showed "due now" — the table read inverted from the actual
// admission model. These helpers render the cell from the lane's EFFECTIVE
// admission mode instead.

// effectiveAdmissionModeFor replicates the scheduler's resolution rule
// (internal/scheduler/admission_mode.go, admissionModeFor): the per-project
// admission_mode override wins, otherwise the namespace's default applies,
// otherwise cooldown semantics. It is deliberately NOT imported from the
// scheduler package because admissionModeFor is unexported and scheduler
// production code is not part of this change; the replication is pinned to
// the real resolver by TestAdmissionModeReplica_MatchesSchedulerResolver,
// which checks this precedence against the exported packer's observable
// admission behavior for a table of cases (a lane inside its cooldown pin
// with pending work is admitted iff its effective mode is tasks).
func effectiveAdmissionModeFor(projectMode, namespaceID string, nsModes map[string]string) string {
	switch projectMode {
	case database.AdmissionModeCooldown, database.AdmissionModeTasks:
		return projectMode
	}
	if m, ok := nsModes[namespaceID]; ok && m != "" {
		return m
	}
	return database.AdmissionModeCooldown
}

// nsKey dereferences an optional namespace pointer into the resolver's
// namespace key ("" when unset).
func nsKey(nsID *string) string {
	if nsID == nil {
		return ""
	}
	return *nsID
}

// tasksAdmissionLabel renders the board-driven Next Tick state for a
// tasks-admission lane — never a cooldown countdown. boardTotal comes from
// the SAME readBoardProgress pass that feeds the Progress column (no second
// board reader); boardTotal == 0 means the board could not be read (no
// workdir, no tasks.md, or an empty board), which renders the honest
// "tasks admission" label instead of guessing a countdown.
func tasksAdmissionLabel(running bool, boardDone, boardTotal int) string {
	if running {
		return "running"
	}
	if boardTotal <= 0 {
		return "tasks admission"
	}
	if open := boardTotal - boardDone; open > 0 {
		return fmt.Sprintf("due — %d board rows open", open)
	}
	return "idle — board drained"
}

// nextTickIn returns a human-readable countdown to the next tick, or a
// status string. running=true means a tick is in flight now. Otherwise the
// next tick is due cooldownS after the last tick completed.
func nextTickIn(running bool, lastTickCompleted string, cooldownS int) string {
	return nextTickInAt(clock.Real(), running, lastTickCompleted, cooldownS)
}

// nextTickInAt is nextTickIn on an explicit clock (SCHED-GAP-169): the
// remaining-cooldown projection is measured against clk, so a simulated run
// reports simulated countdowns instead of wall-clock ones.
func nextTickInAt(clk clock.Clock, running bool, lastTickCompleted string, cooldownS int) string {
	if running {
		return "running"
	}
	if cooldownS <= 0 {
		cooldownS = 900
	}
	if lastTickCompleted == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, lastTickCompleted)
	if err != nil {
		return "—"
	}
	due := t.Add(time.Duration(cooldownS) * time.Second)
	wait := clk.Until(due)
	if wait <= 0 {
		return "due now"
	}
	m := int(wait.Minutes())
	s := int(wait.Seconds()) % 60
	return fmt.Sprintf("in %dm %ds", m, s)
}

// SpeedCostPoint is one completed tick's (time, speed, cost, output) data point
// for the per-project speed/cost/commits/files charts.
type SpeedCostPoint struct {
	Label    string // "14:12", "16:44", ...
	Duration int    // tick duration in seconds (speed)
	Cost     float64
	Commits  int
	Files    int
}

// speedCostSeries returns the last up-to-n completed ticks for a project,
// oldest→newest, as (label, durationSecs, cost, commits, files) points. Used to
// draw the speed/cost/commits/files-over-time charts. Returns nil on error /
// empty. Only ticks that produced output (commits>0 || files>0 || cost>0) are
// included — pure timeouts/failures (0/0/0) are omitted since they output
// nothing meaningful.
func (g *Generator) speedCostSeries(ctx context.Context, project string, n int) []SpeedCostPoint {
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at, cost_usd, commits, files_changed FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type rev struct {
		label    string
		duration int
		cost     float64
		commits  int
		files    int
	}
	var reversed []rev
	for rows.Next() {
		var sp, co string
		var cost float64
		var commits, files int
		if rows.Scan(&sp, &co, &cost, &commits, &files) != nil {
			continue
		}
		// Skip zero-output ticks (nothing committed / no files changed / no cost).
		if commits <= 0 && files <= 0 && cost <= 0 {
			continue
		}
		d := parseDuration(sp, co)
		if d <= 0 {
			continue
		}
		label := ""
		if t, err := time.Parse(time.RFC3339, sp); err == nil {
			label = t.Local().Format("15:04")
		}
		reversed = append(reversed, rev{label: label, duration: int(d.Seconds()), cost: cost, commits: commits, files: files})
	}
	if len(reversed) == 0 {
		return nil
	}
	// Reverse to oldest→newest.
	out := make([]SpeedCostPoint, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, SpeedCostPoint{Label: reversed[i].label, Duration: reversed[i].duration, Cost: reversed[i].cost, Commits: reversed[i].commits, Files: reversed[i].files})
	}
	return out
}

// recentCostSeries returns the cost_usd of the last up-to-n completed ticks
// for a project, oldest→newest, for the cost sparkline. Returns nil on error.
func (g *Generator) recentCostSeries(ctx context.Context, project string, n int) []float64 {
	rows, err := g.db.QueryContext(ctx, `
		SELECT cost_usd FROM ticks
		WHERE project_name = ? AND status = 'completed'
		ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	// Collect newest→oldest, then reverse.
	rev := []float64{}
	for rows.Next() {
		var c float64
		if rows.Scan(&c) == nil {
			rev = append(rev, c)
		}
	}
	out := make([]float64, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// recentTickHealth returns (totalRecent, failedRecent) for a project over the
// last n ticks (any status), used for the failure flag. A failedRecent > 0
// lets the dashboard highlight a project with recent failed/timeout ticks.
func (g *Generator) recentTickHealth(ctx context.Context, project string, n int) (total, failed int) {
	rows, err := g.db.QueryContext(ctx, `
		SELECT status FROM ticks
		WHERE project_name = ? ORDER BY spawned_at DESC LIMIT ?
	`, project, n)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		if rows.Scan(&status) != nil {
			continue
		}
		total++
		if status == "failed" || status == "timeout" {
			failed++
		}
	}
	return total, failed
}

// tickDuration returns the human-readable elapsed time between spawned_at and
// completed_at, or "" when either is missing (still running / not finished).
func tickDuration(spawned, completed string) string {
	if spawned == "" || completed == "" {
		return ""
	}
	s, err1 := time.Parse(time.RFC3339, spawned)
	c, err2 := time.Parse(time.RFC3339, completed)
	if err1 != nil || err2 != nil {
		return ""
	}
	d := c.Sub(s)
	if d < 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// observabilityStats returns (avgSecs, avgCost, successPct, eta, completionAt, projectedCost)
// for a project over its recent completed ticks.
//   - avgSecs: mean duration of the last N completed ticks (floored at 60s)
//   - avgCost: mean cost of the last N completed ticks
//   - successPct: % of last N ticks that completed (vs failed/timeout)
//   - eta: avg duration × remaining board steps ("" when no signal)
//   - completionAt: UTC RFC3339 of now + eta ("" when no eta)
//   - projectedCost: avg cost per completed tick × remaining steps
func (g *Generator) observabilityStats(ctx context.Context, project string, boardDone, boardTotal int, recentTicks, recentFailures int) (avgSecs int, avgCost float64, successPct int, eta, completionAt string, projectedCost float64) {
	// Average duration + cost over up-to-10 completed ticks.
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at, cost_usd FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 10
	`, project)
	var total time.Duration
	var totalCost float64
	var count int
	if err == nil {
		for rows.Next() {
			var sp, co string
			var cost float64
			if rows.Scan(&sp, &co, &cost) == nil {
				if d := parseDuration(sp, co); d > 0 {
					total += d
					totalCost += cost
					count++
				}
			}
		}
		_ = rows.Close()
	}
	if count > 0 {
		avgSecs = int(total.Seconds() / float64(count))
		if avgSecs < 60 {
			avgSecs = 60 // floor so ETA isn't absurdly short
		}
		avgCost = totalCost / float64(count)
	}

	// Success rate over the last N ticks (recentTicks = total, recentFailures = bad).
	if recentTicks > 0 {
		successPct = (recentTicks - recentFailures) * 100 / recentTicks
	}

	// ETA + completion timestamp + projected cost from steps remaining.
	remaining := 0
	if boardTotal > 0 {
		remaining = boardTotal - boardDone
	}
	if remaining > 0 {
		if avgSecs > 0 {
			// avgSecs is in seconds; convert to a Duration properly.
			d := time.Duration(avgSecs) * time.Second * time.Duration(remaining)
			eta = formatETA(d)
			completionAt = g.clock().Now().UTC().Add(d).Format(time.RFC3339)
		}
		if avgCost > 0 {
			projectedCost = avgCost * float64(remaining)
		}
	}
	return avgSecs, avgCost, successPct, eta, completionAt, projectedCost
}

func parseDuration(spawned, completed string) time.Duration {
	s, err1 := time.Parse(time.RFC3339, spawned)
	c, err2 := time.Parse(time.RFC3339, completed)
	if err1 != nil || err2 != nil {
		return 0
	}
	d := c.Sub(s)
	if d < 0 {
		return 0
	}
	return d
}

// formatETA renders a duration as a compact human string, e.g. "1h 24m",
// "2d 3h", "3w 2d". Falls back to "—" for zero/negative.
func formatETA(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	const (
		day  = 24 * time.Hour
		week = 7 * day
	)
	switch {
	case d >= week:
		return fmt.Sprintf("%dw %dd", int(d/week), int(d%week/day))
	case d >= day:
		return fmt.Sprintf("%dd %dh", int(d/day), int(d%day/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// CI conclusion caching (DASH-PERF-001). `gh run list` is a ~0.7s
// subprocess; running it once per project on every fleet render serialized
// to ~30s per page. Conclusions are cached per workdir for ciCacheDefaultTTL,
// fetched with bounded concurrency on a cold cache, and hard-capped per
// fetch so a wedged gh can never block a render.
const (
	// ciCacheDefaultTTL is how long a fetched CI conclusion is reused before
	// the dashboard refetches it. 300s (DASH-PERF-003) cuts the cold-cache gh
	// warm pass (~4-8s with 44 projects at ciMaxConcurrent) from once per
	// minute to once per 5 minutes — CI conclusions are stable over minutes,
	// and the fleet overview polls every 10s, so a 5-minute staleness window
	// is invisible while removing a regular render spike.
	ciCacheDefaultTTL = 300 * time.Second
	// ciMaxConcurrent bounds in-flight gh subprocesses during the cache
	// warm pass: 44 projects × ~0.7s serialized was the ~30s stall; at
	// 8-wide a cold-cache render completes in ~4s.
	ciMaxConcurrent = 8
	// ciFetchTimeout is the hard per-fetch deadline. A hung gh (no network,
	// auth prompt, wedged pager) can never hold up a render longer than this.
	ciFetchTimeout = 2 * time.Second
	// projMaxConcurrent bounds the per-project enrichment pool in collect().
	// Enrichment does file reads, git log subprocesses, and cache lookups —
	// no DB access (all DB reads are batched up front) — so parallel projects
	// cannot deadlock the single sqlite connection (SetMaxOpenConns(1)).
	projMaxConcurrent = 8
)

// ciCacheEntry is one cached CI conclusion and when it was fetched.
type ciCacheEntry struct {
	conclusion string
	fetchedAt  time.Time
}

// ciConclusion returns the latest GitHub Actions run conclusion for the repo
// at workdir, consulting the TTL cache first. Empty string = unknown (CI pill
// hidden); "success"/"failure" drive the ci✓/ci✗ pills and the GitReins-vs-CI
// warning pill in fleet_table.html. A cache miss runs the gh subprocess
// (bounded by ciFetchTimeout) and stores the result, so the fleet overview
// pays the subprocess cost at most once per TTL window per workdir.
func (g *Generator) ciConclusion(workdir string) string {
	if workdir == "" {
		return ""
	}
	g.ciMu.Lock()
	if e, ok := g.ciCache[workdir]; ok && g.clock().Since(e.fetchedAt) < g.ciTTLValue() {
		g.ciMu.Unlock()
		return e.conclusion
	}
	g.ciMu.Unlock()
	conclusion := g.ciRunnerFunc()(workdir)
	g.ciMu.Lock()
	g.ciCache[workdir] = ciCacheEntry{conclusion: conclusion, fetchedAt: g.clock().Now()}
	g.ciMu.Unlock()
	return conclusion
}

// warmCIConclusions prefetches CI conclusions for any workdirs not already
// fresh in the TTL cache, with at most ciMaxConcurrent gh subprocesses in
// flight. collect() calls this once before its per-project loop so the loop
// reads are cache hits.
func (g *Generator) warmCIConclusions(workdirs []string) {
	need := make([]string, 0, len(workdirs))
	g.ciMu.Lock()
	for _, wd := range workdirs {
		if wd == "" {
			continue
		}
		if e, ok := g.ciCache[wd]; !ok || g.clock().Since(e.fetchedAt) >= g.ciTTLValue() {
			need = append(need, wd)
		}
	}
	g.ciMu.Unlock()
	if len(need) == 0 {
		return
	}
	runner := g.ciRunnerFunc()
	sem := make(chan struct{}, ciMaxConcurrent)
	var wg sync.WaitGroup
	for _, wd := range need {
		wg.Add(1)
		sem <- struct{}{}
		go func(wd string) {
			defer wg.Done()
			defer func() { <-sem }()
			conclusion := runner(wd)
			g.ciMu.Lock()
			g.ciCache[wd] = ciCacheEntry{conclusion: conclusion, fetchedAt: g.clock().Now()}
			g.ciMu.Unlock()
		}(wd)
	}
	wg.Wait()
}

// ciTTLValue returns the effective cache TTL, defaulting when unset so a
// zero-value Generator still behaves.
func (g *Generator) ciTTLValue() time.Duration {
	if g.ciTTL <= 0 {
		return ciCacheDefaultTTL
	}
	return g.ciTTL
}

// ciRunnerFunc returns the configured CI fetcher, defaulting to the real gh
// subprocess when unset (tests inject a counting runner).
func (g *Generator) ciRunnerFunc() func(workdir string) string {
	if g.ciRunner != nil {
		return g.ciRunner
	}
	return runCIConclusion
}

// runCIConclusion returns the latest GitHub Actions run conclusion for the
// repo at workdir (success / failure / "" when unknown). It is an
// independent cross-check on the GitReins LLM-judge pass rate: the judge can
// report a cached or LLM-asserted "green" that does not match a genuinely
// failing suite, and a red CI is the ground truth that unmasks it.
// Best-effort — on any error (no gh, no workflow, timeout) it returns "" so
// the dashboard degrades gracefully.
func runCIConclusion(workdir string) string {
	if workdir == "" {
		return ""
	}
	// gh has no -C dir flag (that's git); set the subprocess working dir.
	// A hard deadline (ciFetchTimeout) caps how long a wedged gh can hold
	// up a dashboard render.
	ctx, cancel := context.WithTimeout(context.Background(), ciFetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "run", "list", "--limit", "1",
		"--json", "conclusion,status,headBranch",
		"--jq", `.[0].conclusion`)
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "null" {
		return ""
	}
	// In-progress runs have conclusion=null; treat as unknown rather than
	// green/red so we don't mislabel a running CI.
	return s
}

// gitReinsReadFile reads one verdict file. It is a var so a test can count the
// walk's file reads and prove a warm cache hit re-walks nothing
// (SCHED-GAP-1576); production never assigns it.
var gitReinsReadFile = os.ReadFile

// readGitReins walks a project's .gitreins/history and returns the aggregate
// LLM-judge verdict summary (pass rate + latest verdicts). Each verdict is a
// .gitreins/history/<YYYY-MM-DD>/<sha>/verdict.json. Best-effort: malformed
// files are skipped; a missing/empty history yields a zero summary.
//
// It is the underlying walker: render call sites go through
// cachedReadGitReins (git_reins_cache.go), which wraps this for the TTL window.
func readGitReins(workdir string, maxLatest int) GitReinsSummary {
	root := filepath.Join(workdir, ".gitreins", "history")
	var sum GitReinsSummary
	var all []GitReinsVerdict

	// dateDir/verdictDir/verdict.json
	dateDirs, err := os.ReadDir(root)
	if err != nil {
		return sum
	}
	for _, dd := range dateDirs {
		if !dd.IsDir() {
			continue
		}
		verdictDirs, err := os.ReadDir(filepath.Join(root, dd.Name()))
		if err != nil {
			continue
		}
		for _, vd := range verdictDirs {
			if !vd.IsDir() {
				continue
			}
			p := filepath.Join(root, dd.Name(), vd.Name(), "verdict.json")
			data, err := gitReinsReadFile(p)
			if err != nil {
				continue
			}
			var raw struct {
				TaskID    string `json:"task_id"`
				TaskTitle string `json:"task_title"`
				Passed    bool   `json:"passed"`
				Evaluated string `json:"evaluated_at"`
				Stages    struct {
					Tier1 *struct {
						Passed bool `json:"passed"`
					} `json:"tier1"`
					Tier2 *struct {
						Passed bool `json:"passed"`
					} `json:"tier2"`
				} `json:"stages"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}
			v := GitReinsVerdict{
				TaskID:    raw.TaskID,
				TaskTitle: raw.TaskTitle,
				Passed:    raw.Passed,
			}
			if raw.Stages.Tier1 != nil {
				v.Tier1Passed = raw.Stages.Tier1.Passed
			}
			if raw.Stages.Tier2 != nil {
				v.Tier2Passed = raw.Stages.Tier2.Passed
				v.HasTier2 = true
			}
			if raw.Evaluated != "" {
				v.EvaluatedAt = raw.Evaluated
			} else {
				v.EvaluatedAt = dd.Name()
			}
			sum.Total++
			if v.Passed {
				sum.Passed++
			} else {
				sum.Failed++
			}
			all = append(all, v)
		}
	}
	if sum.Total > 0 {
		sum.RatePct = sum.Passed * 100 / sum.Total
	}
	// Newest first: sort by evaluatedAt desc (string compare works for ISO).
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[j].EvaluatedAt > all[i].EvaluatedAt {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if maxLatest > 0 && len(all) > maxLatest {
		all = all[:maxLatest]
	}
	sum.Latest = all
	return sum
}

// tickWork returns the commit subject lines that landed between spawned and
// completed for a project, by scanning the workdir git log. It's the
// observability answer to "what did this tick actually work on?" Best-effort:
// on any git error it returns "". commitCount caps how many messages we fetch.
func tickWork(clk clock.Clock, workdir, spawned, completed string, commitCount int) string {
	if workdir == "" || spawned == "" {
		return ""
	}
	// If completed is empty, only show commits strictly after spawned.
	since, err1 := time.Parse(time.RFC3339, spawned)
	if err1 != nil {
		return ""
	}
	var until time.Time
	if completed != "" {
		until, err1 = time.Parse(time.RFC3339, completed)
		if err1 != nil {
			until = clk.Now()
		}
	} else {
		until = clk.Now()
	}
	if until.Before(since) {
		until = clk.Now()
	}

	// git log --pretty=%s (subject only) with `--since`/`--until` in ISO.
	args := []string{
		"-C", workdir, "log",
		"--since=" + since.Add(-2*time.Second).Format(time.RFC3339),
		"--until=" + until.Add(2*time.Second).Format(time.RFC3339),
		"--pretty=%s", "-n", fmt.Sprintf("%d", commitCount),
	}
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, " · ")
}

// readBoardSteps parses a board into an ordered roadmap of steps (completed
// first, then pending). The first pending task is marked "active" (next up).
// The NEVER-DONE perpetual audit is excluded. Returns nil on missing/unreadable.
func readBoardSteps(path string) []BoardStep {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	type row struct {
		id, title, commit string
	}
	var doneRows, pendingRows []row
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "## "):
			low := strings.ToLower(line)
			switch {
			case strings.Contains(low, "active"):
				section = "active"
			case strings.Contains(low, "completed"):
				section = "completed"
			default:
				section = "other"
			}
		case isTaskRow(line):
			// Two formats:
			//  Table row:       | T05 | Title | ... | commit |
			//  Markdown checklist: - [x] R2-1 Title ... (commit)
			// (markdown checklist support added 2026-08-08 — gitreins2 boards)
			if strings.HasPrefix(line, "- [ ] ") || strings.HasPrefix(line, "- [x] ") {
				isDone := strings.HasPrefix(line, "- [x] ")
				rest := line[6:]
				// id = first token, title = remainder, commit = 7/40-hex in parens.
				end := strings.IndexAny(rest, " 	")
				if end <= 0 {
					continue
				}
				id := rest[:end]
				title := strings.TrimSpace(rest[end:])
				// Strip a trailing "(<hex>[, ...])" commit reference.
				commit := ""
				if cm := trailingCommitRe.FindStringSubmatch(title); cm != nil {
					commit = cm[1]
					title = strings.TrimSpace(strings.TrimSuffix(title, cm[0]))
				}
				if id == "" {
					continue
				}
				switch section {
				case "active":
					if isDone {
						doneRows = append(doneRows, row{id: id, title: title, commit: commit})
					} else {
						pendingRows = append(pendingRows, row{id: id, title: title, commit: commit})
					}
				case "completed":
					doneRows = append(doneRows, row{id: id, title: title, commit: commit})
				}
				continue
			}
			// Table row: | T05 | Title | ... |
			// cols[1]=ID, cols[2]=title. Only COMPLETED rows carry a commit
			// hash in a trailing cell; Active/pending rows have deps + model
			// names (e.g. "GLM-5.2") that look hash-like, so don't guess there.
			cols := strings.Split(line, "|")
			var id, title, commit string
			if len(cols) > 1 {
				id = strings.TrimSpace(cols[1])
			}
			if len(cols) > 2 {
				title = strings.TrimSpace(cols[2])
			}
			if section == "completed" {
				for i := len(cols) - 1; i >= 3; i-- {
					c := strings.TrimSpace(cols[i])
					if c != "" && (len(c) == 7 || len(c) == 40) {
						commit = c
						break
					}
				}
			}
			if id == "" {
				continue
			}
			r := row{id: id, title: title, commit: commit}
			switch section {
			case "active":
				pendingRows = append(pendingRows, r)
			case "completed":
				doneRows = append(doneRows, r)
			}
		}
	}
	if len(doneRows) == 0 && len(pendingRows) == 0 {
		return nil
	}
	// Order: completed first (in board order), then pending.
	out := make([]BoardStep, 0, len(doneRows)+len(pendingRows))
	for _, r := range doneRows {
		out = append(out, BoardStep{ID: r.id, Title: r.title, Status: "done", Commit: r.commit})
	}
	for i, r := range pendingRows {
		status := "pending"
		if i == 0 {
			status = "active" // next up
		}
		out = append(out, BoardStep{ID: r.id, Title: r.title, Status: status, Commit: r.commit})
	}
	return out
}

// ── Overview table controls (SCHED-GAP-1598) ───────────────────────────────
//
// The overview page stacks four tables (projects, recent ticks, namespaces,
// namespace utilization history). Search, sort, page and page-size run
// SERVER-SIDE in Go on the already-loaded rows — never in the browser, which
// would require shipping all 485 project rows to filter there. Each table's
// params are validated by normalizeFleetTableParams and applied by
// fleetTableSlice; the same params ride the htmx autorefresh request so a
// refresh preserves the operator's current view.

// fleetProjectSortable returns the comparator for a projects-table sort key.
// Sortable columns mirror the rendered headers: name, weight (W), priority
// (P), last tick time, last outcome, board progress percent, next-tick
// countdown and today's cost. Unparseable timestamps sort as zero.
func fleetProjectSortable(key string) func(a, b FleetRow) bool {
	switch key {
	case "name":
		return func(a, b FleetRow) bool { return a.Name < b.Name }
	case "weight":
		return func(a, b FleetRow) bool { return a.Weight < b.Weight }
	case "priority":
		return func(a, b FleetRow) bool { return a.Priority < b.Priority }
	case "last_tick":
		return func(a, b FleetRow) bool { return a.LastTick < b.LastTick }
	case "outcome":
		return func(a, b FleetRow) bool { return a.LastOutcome < b.LastOutcome }
	case "progress":
		// Progress percent is pct(done,total) clamped to 100 (SCHED-GAP-1583);
		// compare the clamped value so the sort matches what is rendered.
		return func(a, b FleetRow) bool {
			return pct(a.BoardDone, a.BoardTotal) < pct(b.BoardDone, b.BoardTotal)
		}
	case "next":
		// "running" < "due now" < "in Nm NS" < "—" — a rough urgency order:
		// running lanes first, then due-now, then by remaining wait.
		// SCHED-GAP-1603: tasks-admission states share the ranks — "due —
		// N board rows open" ranks WITH "due now" (work is admitting now),
		// "idle — board drained" and "tasks admission" sink LAST (nothing
		// to admit on the board signal), so mixed fleets stay sensible.
		rank := func(s string) int {
			switch {
			case s == "running":
				return 0
			case s == "due now", strings.HasPrefix(s, "due — "):
				return 1
			case strings.HasPrefix(s, "in "):
				return 2
			case s == "idle — board drained", s == "tasks admission":
				return 4
			default:
				return 3
			}
		}
		return func(a, b FleetRow) bool {
			ra, rb := rank(a.NextTickIn), rank(b.NextTickIn)
			if ra != rb {
				return ra < rb
			}
			return a.NextTickIn < b.NextTickIn
		}
	case "cost_today":
		return func(a, b FleetRow) bool { return a.CostToday < b.CostToday }
	default:
		return nil
	}
}

// fleetTickSortable returns the comparator for a recent-ticks sort key.
// spawned compares raw RFC3339 strings (lexicographic = chronological).
func fleetTickSortable(key string) func(a, b TickRow) bool {
	switch key {
	case "project":
		return func(a, b TickRow) bool { return a.Project < b.Project }
	case "spawned":
		return func(a, b TickRow) bool { return a.SpawnedAt < b.SpawnedAt }
	default:
		return nil
	}
}

// fleetNamespaceSortable returns the comparator for a namespaces sort key.
func fleetNamespaceSortable(key string) func(a, b NamespaceRow) bool {
	switch key {
	case "id":
		return func(a, b NamespaceRow) bool { return a.ID < b.ID }
	case "weight":
		return func(a, b NamespaceRow) bool { return a.Weight < b.Weight }
	case "allocated":
		return func(a, b NamespaceRow) bool { return a.Allocated < b.Allocated }
	case "used":
		return func(a, b NamespaceRow) bool { return a.Used < b.Used }
	case "utilization":
		return func(a, b NamespaceRow) bool { return a.Utilization < b.Utilization }
	case "projects":
		return func(a, b NamespaceRow) bool { return a.ProjectCount < b.ProjectCount }
	default:
		return nil
	}
}

// fleetNSTickSortable returns the comparator for a namespace-history sort key.
func fleetNSTickSortable(key string) func(a, b NamespaceTickRow) bool {
	switch key {
	case "namespace":
		return func(a, b NamespaceTickRow) bool { return a.NamespaceID < b.NamespaceID }
	case "created":
		return func(a, b NamespaceTickRow) bool { return a.CreatedAt < b.CreatedAt }
	default:
		return nil
	}
}

// applyFleetTables filters/sorts/slices all four overview tables in place,
// leaving data.Projects / RecentTicks / Namespaces / NamespaceTicks holding
// exactly the page the operator asked for. The collect query totals
// (TotalProjects, EnabledProjects, BudgetUsed, cost totals, oversubscription)
// are computed BEFORE this runs, so the stat cards keep describing the whole
// fleet regardless of the active page.
func applyFleetTables(data *FleetData) {
	ts := &data.TableState

	data.Projects = fleetTableSlice(data.Projects, &ts.Projects,
		func(r FleetRow) string { return r.Name },
		fleetProjectSortable(ts.Projects.Sort))

	data.RecentTicks = fleetTableSlice(data.RecentTicks, &ts.Ticks,
		func(r TickRow) string { return r.Project },
		fleetTickSortable(ts.Ticks.Sort))

	data.Namespaces = fleetTableSlice(data.Namespaces, &ts.Namespaces,
		func(r NamespaceRow) string { return r.ID },
		fleetNamespaceSortable(ts.Namespaces.Sort))

	data.NamespaceTicks = fleetTableSlice(data.NamespaceTicks, &ts.NSHistory,
		func(r NamespaceTickRow) string { return r.NamespaceID },
		fleetNSTickSortable(ts.NSHistory.Sort))

	fleetNav(&ts.Projects)
	fleetNav(&ts.Ticks)
	fleetNav(&ts.Namespaces)
	fleetNav(&ts.NSHistory)
}

// fleetNav fills the prev/next fields from the page math fleetTableSlice
// already did, so templates never re-derive pagination.
func fleetNav(p *FleetTableParams) {
	p.HasPrevious = p.Page > 1
	p.PreviousPage = p.Page - 1
	p.HasNext = p.Page < p.TotalPages
	p.NextPage = p.Page + 1
}

// fleetProjectFilters narrows the project rows by the validated dropdown
// selections (lane name + last outcome). Returns the input slice when both
// filters are empty.
func fleetProjectFilters(rows []FleetRow, p FleetTableParams) []FleetRow {
	if p.FilterP == "" && p.FilterS == "" {
		return rows
	}
	out := make([]FleetRow, 0, len(rows))
	for _, r := range rows {
		if p.FilterP != "" && r.Name != p.FilterP {
			continue
		}
		if p.FilterS != "" && r.LastOutcome != p.FilterS {
			continue
		}
		out = append(out, r)
	}
	return out
}

// parseFleetTableParams validates raw query values into render-ready params
// for the named table (SCHED-GAP-1598), validating the dropdown selections
// against their vocabularies.
func parseFleetTableParams(table string, q url.Values, projectOptions []string) FleetTableParams {
	pref := fleetPrefix[table]
	// An ABSENT size param means the documented default (50, matching the
	// tick history); an EXPLICIT size=0 means "all". Atoi cannot tell the
	// two apart (both give 0), so the presence check comes first.
	size := defaultFleetPageSize
	if q.Has(pref + fleetSizeKey) {
		size, _ = strconv.Atoi(q.Get(pref + fleetSizeKey))
	}
	page, _ := strconv.Atoi(q.Get(pref + fleetPageKey))
	p := normalizeFleetTableParams(FleetTableParams{
		Table:   table,
		Q:       q.Get(pref + fleetQKey),
		Sort:    q.Get(pref + fleetSortKey),
		Dir:     q.Get(pref + fleetDirKey),
		Page:    page,
		PerPage: size,
		FilterP: q.Get("project"),
		FilterS: q.Get("outcome"),
	})
	// Validate the dropdown selections against their vocabularies (mirrors
	// tickHistoryFilter: an unknown value renders unfiltered, not empty).
	if len(projectOptions) > 0 {
		known := false
		for _, name := range projectOptions {
			if p.FilterP == name {
				known = true
				break
			}
		}
		if !known {
			p.FilterP = ""
		}
	}
	if p.FilterS != "" {
		valid := false
		for _, s := range outcomeVocabulary {
			if p.FilterS == s {
				valid = true
				break
			}
		}
		if !valid {
			p.FilterS = ""
		}
	}
	return p
}

// ParseFleetTableQuery builds the four tables' params from the overview
// page's raw query values (SCHED-GAP-1598). Each table owns a prefixed
// parameter family so one URL carries all four states at once:
//
//	projects:   q, project, outcome, sort, dir, size, page
//	recent ticks:        tq, tsort, tsize, tpage
//	namespaces:          nq, nsort, nsize, npage
//	utilization history: hq, hsort, hsize, hpage
//
// Unknown values are dropped by normalizeFleetTableParams (unknown sort keys
// fall back to the table's default order, unlisted sizes to the tick-history
// default), so a stray or hand-edited query string renders the documented
// default view rather than an error.
func ParseFleetTableQuery(q url.Values) *FleetTables {
	ts := &FleetTables{}
	ts.Projects = parseFleetTableParams("projects", q, nil)
	ts.Ticks = parseFleetTableParams("ticks", q, nil)
	ts.Namespaces = parseFleetTableParams("namespaces", q, nil)
	ts.NSHistory = parseFleetTableParams("nsticks", q, nil)
	return ts
}
