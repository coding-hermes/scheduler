package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/agentlog"
	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

//go:embed static/htmx.min.js
var staticFS embed.FS

//go:embed templates/*.html
var templatesFS embed.FS

// htmxJS is the bundled htmx library, loaded via Go embed so the dashboard
// works offline (no CDN dependency at runtime).
var htmxJS = mustReadStatic("static/htmx.min.js")

// Generator produces the fleet dashboard as a single-file HTML page.
type Generator struct {
	// clk is the generator's time seam (SCHED-GAP-169). The zero value reads
	// as the wall clock, so rendering is unchanged until SetClock installs a
	// simulator (which also re-anchors the uptime origin).
	clk clock.Seam
	db  *sql.DB
	// urgencyCalc (SCHED-GAP-174) is the SAME engine calculator the API's
	// /api/v1/queue ranks with (built by the API from the resolved interval
	// range: api.SetResolvedConfig → newUrgencyCalculatorFromConfig). It is
	// supplied by the constructor from the one resolved config the daemon
	// already has, so /queue and /api/v1/queue answer one question with one
	// formula from one source. Nil = the API's documented fallback (a Server
	// built without resolved config scores priority-only in listQueue); the
	// dashboard mirrors that fallback so the two surfaces still agree.
	urgencyCalc       *scheduler.UrgencyCalculator
	tmpl              *template.Template // parsed once, reused
	fleetTmpl         *template.Template // partial: project table body only
	projectTmpl       *template.Template // full page: /projects/{name}
	queueTmpl         *template.Template // full page: /queue
	tickHistoryTmpl   *template.Template // full page: /ticks
	tickDetailTmpl    *template.Template // full page: /ticks/{id}
	namespaceViewTmpl *template.Template // full page: /namespaces/{id}
	healthTmpl        *template.Template // full page: /health
	tapeTmpl          *template.Template // full page: /tape (+ its rows fragment)
	gatewayURL        string
	duckbrainURL      string // optional; health panel probes its /health
	healthClient      *http.Client
	started           time.Time
	// weightBudget (ADV-R09/G8): the effective weight budget from the
	// --budget/SCHEDULER_BUDGET/TOML resolution, set by main.go via
	// SetWeightBudget. Zero (tests, unset) renders the documented default
	// of 100 — never a bare literal at the render site.
	weightBudget int
	spawnCounts  func() (httpCount, execCount int64) // optional; /health panel
	// agentLog (SCHED-GAP-1593): read-only, lazily-opened reader on the
	// agent's own state database; the tick drill-down resolves
	// gateway_trace.session_id through it. Nil = the drill-down renders the
	// explicit "agent state database not configured" notice.
	agentLog *agentlog.Reader
	// CI conclusion cache (DASH-PERF-001): `gh run list` is a ~0.7s
	// subprocess; running it once per project on EVERY fleet render cost
	// ~30s. Conclusions are cached per workdir for ciTTL (300s default —
	// DASH-PERF-003; never below 60s, which is shorter than a cold render)
	// and the cold-cache warm pass is concurrency-bounded + timeout-capped.
	ciMu     sync.Mutex
	ciCache  map[string]ciCacheEntry
	ciTTL    time.Duration               // zero → ciCacheDefaultTTL
	ciRunner func(workdir string) string // injectable for tests; nil → runCIConclusion
}

// SetAgentStateDB wires the reader the tick drill-down uses to resolve
// gateway_trace.session_id into the agent's transcript (SCHED-GAP-1593).
// Nil disables agent-text resolution (the page then degrades with an
// explicit notice). The reader itself is read-only and lazy — see the
// agentlog package contract.
func (g *Generator) SetAgentStateDB(r *agentlog.Reader) { g.agentLog = r }

// SetSpawnCounts wires a callback returning (http, exec) spawn counts since
// restart, surfaced on the /health panel (upstream merge compatibility).
func (g *Generator) SetSpawnCounts(fn func() (httpCount, execCount int64)) {
	g.spawnCounts = fn
}

// NewGenerator creates a dashboard generator. Template is parsed at construction
// time so hot-path Generate() never pays the parse cost. urgencyCalc is the
// scheduler engine's urgency calculator (SCHED-GAP-174) — the caller supplies
// the SAME instance/derivation the API server uses, so /queue and
// /api/v1/queue cannot disagree; it is a required argument precisely because
// omitting it would silently reintroduce a second urgency formula. Nil is
// still accepted (and mirrors the API's priority-only fallback) for callers
// with no resolved config, e.g. template-rendering tests. gatewayURL is
// optional; when supplied, the health panel probes its /health endpoint.
func NewGenerator(db *sql.DB, urgencyCalc *scheduler.UrgencyCalculator, gatewayURL ...string) *Generator {
	var gateway string
	if len(gatewayURL) > 0 {
		gateway = strings.TrimRight(gatewayURL[0], "/")
	}
	g := &Generator{
		db:           db,
		urgencyCalc:  urgencyCalc,
		gatewayURL:   gateway,
		healthClient: &http.Client{Timeout: 2 * time.Second},
		started:      clock.Real().Now(),
		ciCache:      make(map[string]ciCacheEntry),
		ciTTL:        ciCacheDefaultTTL,
		ciRunner:     runCIConclusion,
	}
	// The template func map renders live durations, so it must read the SAME
	// clock seam SetClock installs into (SCHED-GAP-169). It is therefore built
	// after g exists, from a pointer to g's own seam.
	g.tmpl = loadTemplates(&g.clk)
	g.fleetTmpl = g.tmpl.Lookup("fleet_table")
	g.projectTmpl = g.tmpl.Lookup("project_detail")
	g.queueTmpl = g.tmpl.Lookup("queue")
	g.tickHistoryTmpl = g.tmpl.Lookup("tick_history")
	g.tickDetailTmpl = g.tmpl.Lookup("tick_detail")
	g.namespaceViewTmpl = g.tmpl.Lookup("namespace_view")
	g.healthTmpl = g.tmpl.Lookup("health")
	g.tapeTmpl = g.tmpl.Lookup("tape")
	for name, parsed := range map[string]*template.Template{
		"fleet_table":    g.fleetTmpl,
		"project_detail": g.projectTmpl,
		"queue":          g.queueTmpl,
		"tick_history":   g.tickHistoryTmpl,
		"namespace_view": g.namespaceViewTmpl,
		"health":         g.healthTmpl,
		"tape":           g.tapeTmpl,
	} {
		if parsed == nil {
			panic("dashboard: " + name + " template not registered")
		}
	}
	return g
}

// SetDuckBrainURL registers the DuckBrain HTTP endpoint so the health
// panel can probe it (mirrors gateway probing). Optional.
func (g *Generator) SetDuckBrainURL(u string) {
	g.duckbrainURL = strings.TrimRight(u, "/")
}

// SetWeightBudget sets the effective scheduling weight budget rendered on
// the fleet page (ADV-R09/G8). Zero or negative keeps the documented
// default of 100. This is the dashboard's ONLY budget input — main.go passes
// the same resolved --budget value the Loop was built with.
func (g *Generator) SetWeightBudget(n int) {
	if n > 0 {
		g.weightBudget = n
	}
}

// HTMXJS returns the bundled htmx library bytes for serving via HTTP.
func (g *Generator) HTMXJS() []byte { return htmxJS }

// Generate writes the dashboard HTML to w, applying the per-table controls
// with their defaults (SCHED-GAP-1598). Template is pre-parsed — zero
// hot-path overhead beyond the render.
func (g *Generator) Generate(w io.Writer) error {
	return g.GenerateParams(w, nil)
}

// GenerateParams writes the dashboard HTML to w, applying the operator's
// per-table table state (search / sort / page / size) to the four stacked
// tables. A nil params value renders the default view. The stat cards
// (TotalProjects / EnabledProjects / BudgetUsed / cost totals) describe the
// WHOLE fleet — they are computed in collect() before the tables are sliced.
func (g *Generator) GenerateParams(w io.Writer, params *FleetTables) error {
	ctx := context.Background()
	data := g.collect(ctx)
	if params != nil {
		data.TableState = *params
	} else {
		data.TableState = defaultFleetTables()
	}
	// Dropdown vocabulary for the projects-table filters (lane names seen in
	// the ticks table — the same list the tick history page offers).
	var projectOptions []string
	if projs, err := database.DistinctTickProjects(ctx, g.db); err == nil {
		projectOptions = projs
	}
	// Validate the lane filter against the fetched vocabulary: the generic
	// parser cannot know the lane list (mirrors tickHistoryFilter — an
	// unknown value renders unfiltered, not empty).
	if data.TableState.Projects.FilterP != "" && !slices.Contains(projectOptions, data.TableState.Projects.FilterP) {
		data.TableState.Projects.FilterP = ""
	}
	data.TableState.ProjectOptions = projectOptions
	data.OutcomeOptions = outcomeVocabulary
	data.PageSizeOptions = pageSizeOptions
	// Narrow by the validated dropdowns, then search/sort/slice every table.
	data.Projects = fleetProjectFilters(data.Projects, data.TableState.Projects)
	applyFleetTables(&data)
	return g.tmpl.ExecuteTemplate(w, "page", data)
}

// defaultFleetTables returns validated default params for all four tables:
// page 1, the tick-history page size, the tables' existing default order.
func defaultFleetTables() FleetTables {
	return FleetTables{
		Projects:   normalizeFleetTableParams(FleetTableParams{Table: "projects", PerPage: defaultFleetPageSize}),
		Ticks:      normalizeFleetTableParams(FleetTableParams{Table: "ticks", PerPage: defaultFleetPageSize}),
		Namespaces: normalizeFleetTableParams(FleetTableParams{Table: "namespaces", PerPage: defaultFleetPageSize}),
		NSHistory:  normalizeFleetTableParams(FleetTableParams{Table: "nsticks", PerPage: defaultFleetPageSize}),
	}
}

// GenerateFleetTable renders the fleet table partial (tbody only) for htmx
// to swap into the dashboard page. Routes get this from /dashboard/partial.
// The query string carries the operator's current table state; the SAME
// params the full page was rendered with ride the autorefresh request, so a
// refresh re-renders the current view (search / sort / page / size) instead
// of resetting it (SCHED-GAP-1598).
func (g *Generator) GenerateFleetTable(w io.Writer) error {
	return g.GenerateFleetTableParams(w, url.Values{})
}

// GenerateFleetTableParams renders the fleet-table partial from raw query
// values. The partial owns the projects table's tbody only, so only that
// table's state applies here.
func (g *Generator) GenerateFleetTableParams(w io.Writer, q url.Values) error {
	ctx := context.Background()
	data := g.collect(ctx)
	var projectOptions []string
	if projs, err := database.DistinctTickProjects(ctx, g.db); err == nil {
		projectOptions = projs
	}
	p := parseFleetTableParams("projects", q, projectOptions)
	// Dropdown narrowing happens before the slice so the "showing N of M"
	// count reflects the rows matching the filters.
	data.Projects = fleetProjectFilters(data.Projects, p)
	ts := defaultFleetTables()
	ts.Projects = p
	data.TableState = ts
	applyFleetTables(&data)
	return g.fleetTmpl.Execute(w, data)
}

// GenerateProjectDetail renders the project detail page. Returns an error
// wrapping ErrProjectNotFound when no project matches the given name.
func (g *Generator) GenerateProjectDetail(w io.Writer, name string) error {
	if name == "" {
		return errors.New("project name is required")
	}
	ctx := context.Background()
	project, err := database.GetProject(ctx, g.db, name)
	if err != nil {
		return fmt.Errorf("load project %q: %w", name, err)
	}

	data := ProjectDetailData{Title: project.Name, Project: project}

	// Board progress + next-tick timing from the project workdir/cooldown.
	if project.Workdir != "" {
		data.BoardDone, data.BoardTotal = readBoardProgress(filepath.Join(project.Workdir, ".coding-hermes", "tasks.md"))
		data.BoardSteps = readBoardSteps(filepath.Join(project.Workdir, ".coding-hermes", "tasks.md"))
	}
	running := false
	var lastCompleted string
	_ = g.db.QueryRowContext(ctx, `SELECT COALESCE(last_tick_completed, '') FROM projects WHERE name = ?`, name).Scan(&lastCompleted)
	if latest, err := latestTickForProject(ctx, g.db, name); err == nil {
		data.LatestTick = latest
		running = latest != nil && latest.Status == database.StatusRunning
	}
	data.NextTickIn = nextTickInAt(g.clock(), running, lastCompleted, project.CooldownS)
	// Observability: avg tick duration, success rate, ETA over recent ticks.
	var rt, rf int
	rt, rf = g.recentTickHealth(ctx, name, 10)
	data.AvgTickSecs, data.AvgCost, data.SuccessRate, data.ETA, data.CompletionAt, data.ProjectedCost = g.observabilityStats(ctx, name, data.BoardDone, data.BoardTotal, rt, rf)
	// Learning ETA: predict remaining time + cost from per-task-type estimates
	// learned from tick history + the fleet-wide prior (project-biased blend).
	if project.Workdir != "" {
		fleet := g.fleetLearned(ctx)
		if learned, learnedAt, breakdown, projCost := g.learnedETA(ctx, name, project.Workdir, data.BoardSteps, fleet); learned > 0 {
			data.ETA = formatETA(learned)
			data.CompletionAt = learnedAt
			data.EtaBreakdown = breakdown
			if projCost > 0 {
				data.ProjectedCost = projCost
			}
		}
	}
	// GitReins LLM-judge verdict summary (pass rate + latest verdicts).
	// cachedReadGitReins wraps the readGitReins walk in a 60s TTL cache
	// (SCHED-GAP-1576), so repeat renders of the same project reuse the walk.
	if project.Workdir != "" {
		data.GitReins = cachedReadGitReins(project.Workdir, 12)
	}
	// Speed/cost-over-time chart data (last 20 completed ticks).
	data.SpeedCost = g.speedCostSeries(ctx, name, 20)

	// Last 20 ticks for the history table.
	if ticks, err := database.ListTicks(ctx, g.db, name, 20); err == nil {
		data.RecentTicks = ticks
	}

	// "What each tick worked on": map tick id → commit subject line(s).
	data.TickWork = map[string]string{}
	for _, t := range data.RecentTicks {
		data.TickWork[t.ID] = tickWork(g.clock(), project.Workdir, t.SpawnedAt, t.CompletedAt, t.Commits+1)
	}

	return g.projectTmpl.Execute(w, data)
}

const tickHistoryPageSize = 50

// GenerateTickHistory renders one page of the global tick history. Pages are
// one-based; values below one are normalized to the first page. Filter is
// the server-side search/filter (SCHED-GAP-1593); a zero Filter disables
// filtering entirely.
func (g *Generator) GenerateTickHistory(w io.Writer, page int, filter database.TickFilter) error {
	data, err := g.tickHistoryData(page, filter)
	if err != nil {
		return err
	}
	return g.tickHistoryTmpl.Execute(w, data)
}

// GenerateTickHistoryPartial renders only the pagination fragment for htmx
// polling (HX-Request). The page's #tick-history div polls /ticks with
// hx-swap=outerHTML, so the response must be the fragment — a full page
// swapped in compounds itself on every 30s refresh.
func (g *Generator) GenerateTickHistoryPartial(w io.Writer, page int, filter database.TickFilter) error {
	data, err := g.tickHistoryData(page, filter)
	if err != nil {
		return err
	}
	return g.tickHistoryTmpl.ExecuteTemplate(w, "tick_history_partial", data)
}

// statusVocabulary is the fixed tick status list (database.TickStatus
// constants) used for the status filter dropdown and query validation.
var statusVocabulary = []string{
	string(database.StatusQueued),
	string(database.StatusRunning),
	string(database.StatusCompleted),
	string(database.StatusFailed),
	string(database.StatusTimeout),
}

// outcomeVocabulary is the fixed tick outcome list for the outcome filter
// dropdown and query validation.
var outcomeVocabulary = []string{
	string(database.OutcomeCommitted),
	string(database.OutcomeDryRun),
	string(database.OutcomeFailed),
	string(database.OutcomeTimeout),
}

// tickHistoryFilter validates a raw filter against the known vocabularies.
// An unknown status/outcome value is dropped (rendering the unfiltered view
// beats rendering a guaranteed-empty result from a stray query param).
func tickHistoryFilter(f database.TickFilter) database.TickFilter {
	f.Project = strings.TrimSpace(f.Project)
	f.Query = strings.TrimSpace(f.Query)
	f.Status = strings.TrimSpace(f.Status)
	f.Outcome = strings.TrimSpace(f.Outcome)
	valid := func(v string, vocab []string) bool {
		for _, s := range vocab {
			if s == v {
				return true
			}
		}
		return false
	}
	if !valid(f.Status, statusVocabulary) {
		f.Status = ""
	}
	if !valid(f.Outcome, outcomeVocabulary) {
		f.Outcome = ""
	}
	return f
}

// filterQueryParams returns the non-empty filter values as parallel
// query-string fragments (already URL-encoded), used to build pagination
// links that preserve the active filter.
func filterQueryParams(f database.TickFilter) []string {
	var parts []string
	if f.Query != "" {
		parts = append(parts, "q="+url.QueryEscape(f.Query))
	}
	if f.Project != "" {
		parts = append(parts, "project="+url.QueryEscape(f.Project))
	}
	if f.Status != "" {
		parts = append(parts, "status="+url.QueryEscape(f.Status))
	}
	if f.Outcome != "" {
		parts = append(parts, "outcome="+url.QueryEscape(f.Outcome))
	}
	return parts
}

// tickHistoryData loads the paginated tick list shared by the full page and
// the htmx partial, honoring the server-side search/filter.
func (g *Generator) tickHistoryData(page int, raw database.TickFilter) (TickHistoryData, error) {
	ctx := context.Background()
	filter := tickHistoryFilter(raw)
	if page < 1 {
		page = 1
	}

	ticks, total, err := database.ListTicksFiltered(ctx, g.db, filter, tickHistoryPageSize, (page-1)*tickHistoryPageSize)
	if err != nil {
		return TickHistoryData{}, fmt.Errorf("load tick history page %d: %w", page, err)
	}
	totalPages := (total + tickHistoryPageSize - 1) / tickHistoryPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	data := TickHistoryData{
		Title:        "Tick History",
		GeneratedAt:  g.clock().Now().UTC().Format(time.RFC3339),
		Ticks:        ticks,
		Page:         page,
		PageSize:     tickHistoryPageSize,
		TotalTicks:   total,
		TotalPages:   totalPages,
		HasPrevious:  page > 1,
		PreviousPage: page - 1,
		HasNext:      page < totalPages,
		NextPage:     page + 1,
		Filtered:     filter.Query != "" || filter.Project != "" || filter.Status != "" || filter.Outcome != "",
		FilterQ:      filter.Query,
		FilterP:      filter.Project,
		FilterS:      filter.Status,
		FilterO:      filter.Outcome,
	}
	parts := filterQueryParams(filter)
	data.BaseQS = strings.Join(parts, "&")
	data.StatusOptions = statusVocabulary
	data.OutcomeOptions = outcomeVocabulary
	if projs, err := database.DistinctTickProjects(ctx, g.db); err == nil {
		data.ProjectOptions = projs
	}
	return data, nil
}

// tickDetailEventLimit bounds the scheduler log-event scan per tick page.
// The events table has no tick-id column, so the page scans the newest tail
// and selects the events inside (or nearest-before) the tick window.
const tickDetailEventLimit = 400

// GenerateTickDetail renders /ticks/{id}: the tick's own row, the scheduler
// log events around its window, and — when the tick carries a gateway trace
// with a resolvable session id — the agent's actual generated text
// (SCHED-GAP-1593). Every degradation is explicit: no trace, an unknown
// session id, or an unreachable agent state database each render a named
// notice, never an empty pane that could read as "the agent said nothing".
func (g *Generator) GenerateTickDetail(w io.Writer, id string) error {
	if id == "" {
		return errors.New("tick id is required")
	}
	ctx := context.Background()
	tick, traceRaw, err := database.GetTickWithTrace(ctx, g.db, id)
	if err != nil {
		return err
	}

	data := TickDetailData{
		Title:       "Tick " + id,
		GeneratedAt: g.clock().Now().UTC().Format(time.RFC3339),
		Tick:        tick,
	}

	// ── Gateway trace ──
	// The trace is the ONLY bridge to the agent's transcript: ticks.session_id
	// is the scheduler's own tick id and resolves nowhere.
	if traceRaw != "" {
		var tr scheduler.GatewayPOSTTrace
		if err := json.Unmarshal([]byte(traceRaw), &tr); err != nil {
			data.AgentStatus = "no-trace"
			data.AgentDetail = fmt.Sprintf(
				"this tick has a gateway trace, but it could not be parsed (%v) — no session id can be extracted, so the agent's transcript cannot be resolved. Raw trace is preserved in ticks.gateway_trace.", err)
		} else {
			data.HasTrace = true
			data.TraceModel = tr.Model
			data.TraceProvider = tr.Provider
			data.TraceSession = tr.SessionID
			data.TraceElapsedS = int(tr.ElapsedMS / 1000)
			data.TraceEvents = tr.Events
			data.TraceAttempts = tr.Attempts
			data.TraceClass = tr.Classification

			switch {
			case tr.SessionID == "":
				data.AgentStatus = "no-trace"
				data.AgentDetail = "the gateway trace on this tick carries NO session id (older gateway, or the response header was missing) — the agent's transcript cannot be resolved."
			case g.agentLog == nil:
				data.AgentStatus = "unavailable"
				data.AgentDetail = "the agent state database is not configured on this dashboard (no reader wired), so the transcript of session " + tr.SessionID + " cannot be fetched. The session id IS recorded in the trace above."
			default:
				res := g.agentLog.FetchSession(ctx, tr.SessionID)
				data.AgentStatus = string(res.Status)
				data.AgentDetail = res.Detail
				if res.Status == agentlog.StatusResolved {
					s := res.Session
					data.AgentSession = &s
					data.AgentTurns = res.Turns
					data.AgentCapped = res.Capped
				}
			}
		}
	} else {
		data.AgentStatus = "no-trace"
		data.AgentDetail = "this tick has NO gateway trace recorded (predates SCHED-GAP-119 tracing, or the POST never completed) — there is no session id to resolve, so the agent's transcript cannot be fetched. That is a data gap, not an empty transcript."
	}

	// ── Scheduler log events around the tick window ──
	// The events table carries no tick-id column, so selection is a
	// time-window scan over the newest tail: everything inside
	// [spawned−60s, completed+60s] (or everything after spawn when the tick
	// never completed), with events that literally name the tick id marked
	// as matched. Event listing must never fail the page.
	events, err := database.ListEventsRecent(ctx, g.db, tickDetailEventLimit)
	if err == nil {
		evs := make([]TickEventRow, 0, len(events))
		spawned := parseWhen(tick.SpawnedAt)
		completed := parseWhen(tick.CompletedAt)
		hasWindow := !spawned.IsZero()
		for _, e := range events {
			at := parseWhen(e.CreatedAt)
			inWindow := false
			if hasWindow && !at.IsZero() {
				if completed.IsZero() {
					inWindow = at.After(spawned.Add(-time.Minute))
				} else {
					inWindow = at.After(spawned.Add(-time.Minute)) && at.Before(completed.Add(time.Minute))
				}
			}
			matched := strings.Contains(e.Message, tick.ID) ||
				strings.Contains(e.Details, tick.ID)
			if inWindow || matched {
				evs = append(evs, TickEventRow{
					ID: e.ID, Severity: string(e.Severity),
					Component: e.Component, Message: e.Message,
					CreatedAt: e.CreatedAt, Matched: matched,
				})
			}
		}
		data.Events = evs
		data.EventsTotal = len(events)
		if data.EventsTotal == tickDetailEventLimit && len(evs) == 0 {
			data.HasMore = true
		}
	}

	return g.tickDetailTmpl.Execute(w, data)
}

// parseWhen parses the RFC3339-ish timestamps the ticks/events tables use.
// Returns the zero time for empty or unparseable values.
func parseWhen(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// GenerateNamespaceView renders namespace configuration, assigned projects,
// and recent utilization history.
func (g *Generator) GenerateNamespaceView(w io.Writer, id string) error {
	if id == "" {
		return errors.New("namespace id is required")
	}
	ctx := context.Background()
	namespace, err := database.GetNamespace(ctx, g.db, id)
	if err != nil {
		return fmt.Errorf("load namespace %q: %w", id, err)
	}
	projects, err := database.ListProjectsByNamespace(ctx, g.db, id)
	if err != nil {
		return fmt.Errorf("load projects for namespace %q: %w", id, err)
	}
	ticks, err := database.ListNamespaceTicks(ctx, g.db, id, 50)
	if err != nil {
		return fmt.Errorf("load utilization for namespace %q: %w", id, err)
	}

	data := NamespaceViewData{
		Title:       "Namespace: " + id,
		Namespace:   namespace,
		Projects:    projects,
		RecentTicks: ticks,
	}
	for _, project := range projects {
		if project.Enabled {
			data.EnabledProjects++
			data.TotalWeight += project.Weight
		}
	}
	if len(ticks) > 0 {
		data.LatestTick = &ticks[0]
		if ticks[0].Allocated > 0 {
			data.Utilization = float64(ticks[0].Used) / float64(ticks[0].Allocated) * 100
		}
	}
	return g.namespaceViewTmpl.Execute(w, data)
}

// GenerateHealth renders daemon, database, and gateway liveness information.
// The page refreshes itself with htmx, so every render performs fresh probes.
func (g *Generator) GenerateHealth(w io.Writer) error {
	data := g.healthData()
	return g.healthTmpl.Execute(w, data)
}

// GenerateHealthPartial renders only the .cards fragment for htmx polling
// (HX-Request). The page's .cards div polls /health with hx-swap=outerHTML,
// so the response must be the fragment — a full page swapped in compounds
// itself on every 10s refresh.
func (g *Generator) GenerateHealthPartial(w io.Writer) error {
	data := g.healthData()
	return g.healthTmpl.ExecuteTemplate(w, "health_cards", data)
}

// healthData probes daemon, database, gateway, and DuckBrain liveness;
// shared by the full page and the htmx cards fragment.
func (g *Generator) healthData() HealthData {
	ctx := context.Background()
	data := HealthData{
		Title:          "System Health",
		GeneratedAt:    g.clock().Now().UTC().Format(time.RFC3339),
		DaemonStatus:   "running",
		DatabaseStatus: "connected",
		GatewayStatus:  "not configured",
		GatewayURL:     g.gatewayURL,
		Uptime:         g.clock().Since(g.started).Round(time.Second).String(),
		Goroutines:     runtime.NumGoroutine(),
	}
	if err := g.db.PingContext(ctx); err != nil {
		data.DatabaseStatus = "error"
	}
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&data.ActiveTicks)
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks`).Scan(&data.TotalTicks)

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	data.MemoryMB = float64(memory.Alloc) / (1024 * 1024)

	// ADV-R13: surface the latest PERSISTED host load/memory sample (the
	// host_samples telemetry written by the scheduler's evaluation cycle) —
	// distinct from MemoryMB above, which is this daemon process's own
	// runtime.MemStats. sql.ErrNoRows is the normal fresh-database state:
	// render it honestly as "unavailable", never as a 0.00 reading.
	if sample, err := database.LatestHostSample(ctx, g.db); err == nil {
		data.HostSampleAvailable = true
		data.HostLoad1 = sample.Load1
		data.HostLoad5 = sample.Load5
		data.HostLoad15 = sample.Load15
		data.HostMemTotalMB = float64(sample.MemTotalBytes) / (1024 * 1024)
		data.HostMemAvailMB = float64(sample.MemAvailableBytes) / (1024 * 1024)
		data.HostSampleAt = sample.SampledAt
		data.HostSampleSource = sample.Source
	}

	if g.gatewayURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.gatewayURL+"/health", nil)
		if err != nil {
			data.GatewayStatus = "error"
		} else {
			resp, err := g.healthClient.Do(req)
			if err != nil {
				data.GatewayStatus = "unreachable"
			} else {
				if resp.StatusCode == http.StatusOK {
					data.GatewayStatus = "connected"
				} else {
					data.GatewayStatus = fmt.Sprintf("unhealthy (HTTP %d)", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
		}
	}

	// DuckBrain probe (fallback visibility): show reachable/unreachable and
	// any spooled writes pending replay. The sync layer spools failed writes,
	// so "unreachable" here is not data loss — it's queued for replay.
	data.DuckBrainStatus = "not configured"
	data.DuckBrainBaseURL = g.duckbrainURL
	if g.duckbrainURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.duckbrainURL+"/health", nil)
		if err != nil {
			data.DuckBrainStatus = "error"
		} else {
			resp, err := g.healthClient.Do(req)
			if err != nil {
				data.DuckBrainStatus = "unreachable"
			} else {
				if resp.StatusCode == http.StatusOK {
					data.DuckBrainStatus = "connected"
				} else {
					data.DuckBrainStatus = fmt.Sprintf("unhealthy (HTTP %d)", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
		}
		_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_spool`).Scan(&data.DuckBrainSpooled)
	}
	return data
}

// GenerateQueue renders the evaluation queue page — all enabled projects
// sorted by urgency (descending) with their weight, priority, and cooldown.
func (g *Generator) GenerateQueue(w io.Writer) error {
	data, err := g.queueEntries(context.Background())
	if err != nil {
		return err
	}
	return g.queueTmpl.Execute(w, data)
}

// queueEntries builds the /queue ordering: every enabled project with its
// urgency score, sorted descending.
//
// SCHED-GAP-174: urgency comes from the SAME scheduler.UrgencyCalculator the
// API's /api/v1/queue ranks with (g.urgencyCalc, supplied by NewGenerator from
// the resolved interval range) — one formula, one source. The private ad-hoc
// score this function used to apply (a fixed multiplier on priority, then a
// linear ramp from the last tick's spawned_at) is gone: it ignored decay_rate,
// measured elapsed time from spawned_at rather than last_tick_completed, and
// disagreed with the API on both value and order for the majority of the fleet.
//
// The projection, filter, row cap and sort are deliberately the same as
// listQueue (internal/api/server_helpers.go) so the two surfaces rank one
// fleet identically; when no calculator is configured the score is
// priority-only, mirroring listQueue's documented fallback.
func (g *Generator) queueEntries(ctx context.Context) (QueueData, error) {
	data := QueueData{Title: "Evaluation Queue"}

	rows, err := g.db.QueryContext(ctx, `SELECT name, COALESCE(weight,0), COALESCE(priority,0), COALESCE(cooldown_s,0), COALESCE(enabled,1), COALESCE(decay_rate,0), COALESCE(created_at,''), COALESCE(last_tick_completed,''), COALESCE(parent,'')`+queueEvidenceSelect+` FROM projects p WHERE enabled = 1 ORDER BY priority DESC LIMIT 200`)
	if err != nil {
		return data, fmt.Errorf("query queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	calc := g.urgencyCalc
	now := g.clock().Now()
	var laneParents map[string]string // name → explicit parent (SCHED-GAP-1590); built from the same rows, zero extra queries
	for rows.Next() {
		var e QueueEntry
		var decayRate float64
		var createdAtStr, lastStr, parentStr string
		var evidence queueLaneEvidence
		if err := rows.Scan(&e.Name, &e.Weight, &e.Priority, &e.CooldownS, &e.Enabled, &decayRate, &createdAtStr, &lastStr, &parentStr,
			&evidence.DefReason, &evidence.DefDetail, &evidence.DefAt, &evidence.RunTickID, &evidence.RunAdmit, &evidence.RunWaitMs, &evidence.RunAt); err != nil {
			return data, fmt.Errorf("scan queue row: %w", err)
		}
		if laneParents == nil {
			laneParents = make(map[string]string)
		}
		laneParents[e.Name] = parentStr
		// SCHED-GAP-1589: carry the scanned evidence onto the entry; the
		// rendered why-waiting cell is built after the sort (bands and
		// percentiles are order-derived).
		e.WhyReason = evidence.DefReason
		e.WhyDetail = evidence.DefDetail
		e.WhyAt = evidence.DefAt
		if evidence.RunTickID != "" {
			e.WhyRunning = true
			e.WhyReason = evidence.RunAdmit
			e.WhyWaitMs = evidence.RunWaitMs
			e.WhyAt = evidence.RunAt
		}
		if calc != nil {
			// Mirror the engine's input handling exactly, as listQueue does
			// (internal/api/server_helpers.go): created_at parses as RFC3339;
			// an empty/unparseable last_tick_completed leaves lastCompleted
			// nil so urgency falls back to created_at.
			createdAt, _ := time.Parse(time.RFC3339, createdAtStr)
			var lastCompleted *time.Time
			if lastStr != "" {
				if t, err := time.Parse(time.RFC3339, lastStr); err == nil {
					lastCompleted = &t
				}
			}
			e.Urgency = calc.ComputeUrgency(float64(e.Priority), decayRate, now, lastCompleted, createdAt)
			// SCHED-GAP-1589: display-only timing context (interval from
			// priority, waited time, active-cooldown flag) from the same
			// parsed inputs the score used — no extra queries, no second
			// formula.
			fillQueueTiming(&e, calc, now, lastCompleted, createdAt)
		} else {
			// No calculator configured: priority-only base, same fallback the
			// API applies (keeps the surfaces in agreement, never a second
			// formula).
			e.Urgency = float64(e.Priority)
		}
		data.Entries = append(data.Entries, e)
		data.TotalWeight += e.Weight
	}
	if err := rows.Err(); err != nil {
		return data, fmt.Errorf("iterate queue rows: %w", err)
	}

	// Sort by urgency descending — stable, matching listQueue, so equal scores
	// keep the priority-ordered query sequence on both surfaces.
	sort.SliceStable(data.Entries, func(i, j int) bool {
		return data.Entries[i].Urgency > data.Entries[j].Urgency
	})

	// SCHED-GAP-1590: annotate every entry with its lane nesting (depth +
	// primary name + parenthood source) resolved over the SAME row set this
	// function just scanned — laneParents was collected in the loop above, so
	// no additional query runs and the SCHED-GAP-174 one-query budget holds.
	// The sort is deliberately untouched: global urgency order with per-row
	// annotation is the documented sort-vs-nesting decision (see the header
	// of generator_lane_nesting.go) — grouping families under primaries would
	// fork this ordering from /api/v1/queue, breaking the parity contract.
	if laneParents != nil {
		lanes := make([]database.Project, len(data.Entries))
		for i, e := range data.Entries {
			lanes[i] = database.Project{Name: e.Name, Parent: laneParents[e.Name]}
		}
		annotateQueueNesting(data.Entries, lanes)
	}

	// SCHED-GAP-1589: the explain pass (why-waiting cells + urgency
	// bands/scale) is PURE post-processing over the single scan above —
	// it must run AFTER the sort it derives its bands from.
	explainQueueEntries(&data, now)

	data.Count = len(data.Entries)
	return data, nil
}

const pageTemplate = `{{template "head" .}}
{{template "sidebar" "overview"}}
<div class="main" id="main">
<div class="page-head">
<h1>Fleet Overview</h1>
<div class="actions"><span class="signal"><span class="dot"></span> live</span></div>
</div>
<div class="meta">Generated {{.GeneratedAt}}</div>

<div class="cards">
<div class="card"><div class="label">Enabled Projects</div><div class="value">{{.EnabledProjects}}/{{.TotalProjects}}</div></div>
<div class="card"><div class="label">Active Ticks</div><div class="value">{{.ActiveTicks}}</div></div>
<div class="card"><div class="label">Fleet Weight</div><div class="value">{{.BudgetUsed}}</div></div>
{{if .CostTodayTotal}}<div class="card"><div class="label">Cost Today</div><div class="value">${{printf "%.2f" .CostTodayTotal}}</div></div>{{end}}
{{if .CostWeekTotal}}<div class="card"><div class="label">Cost 7d</div><div class="value">${{printf "%.2f" .CostWeekTotal}}</div></div>{{end}}
</div>

{{/* SCHED-GAP-1583: these two numbers are NOT a ratio. BudgetUsed is the fleet-wide sum
     of every enabled lane's weight; BudgetTotal is the PER-TICK packing budget. The packer
     spends that budget once per cycle, so a fleet-wide sum can never sit under it — the old
     "Budget Used 1218/100" fill implied a proportion that does not exist and (unclamped)
     overflowed the card at width:1218%. The two facts are therefore shown LABELLED rather
     than as a misleading fill. SCHED-GAP-1582 extends the honesty: when enabled namespaces
     carry more demand than the per-tick budget, the OVERSUBSCRIBED note states it — a
     configured fleet may legitimately oversubscribe, but the state must be named, not
     implied by a bar that always overflows. */}}
<div class="budget-bar">
<div class="budget-label"><span>Fleet weight — sum of enabled lanes</span><span>{{.BudgetUsed}}</span></div>
<div class="budget-label"><span>Per-tick weight budget</span><span>{{.BudgetTotal}}</span></div>
{{if .BudgetOversubscribed}}<div class="budget-label oversubscribed"><span>Namespace demand vs per-tick budget</span><span>OVERSUBSCRIBED — enabled namespaces demand {{.NamespaceDemandTotal}} against a per-tick budget of {{.BudgetTotal}}; lanes over their namespace's share are held (reason: budget)</span></div>{{end}}
</div>

<h2>Projects</h2>
{{/* SCHED-GAP-1598: per-table server-side search / filter / sort / page-size.
     A plain GET form (no htmx) so it works with and without JS, mirroring the
     tick-history filter bar (SCHED-GAP-1593). The hidden p=1 resets to the
     first page whenever the criteria change. */}}
<div class="table-controls">
<form class="tick-filter" method="get" action="/">
<input type="hidden" name="page" value="1">
<input type="search" name="q" value="{{.TableState.Projects.Q}}" placeholder="search lanes…" aria-label="Search projects by lane name">
<select name="project" aria-label="Filter by lane">
<option value="">lane: all</option>
{{range .TableState.ProjectOptions}}<option value="{{.}}" {{if eq . $.TableState.Projects.FilterP}}selected{{end}}>{{.}}</option>{{end}}
</select>
<select name="outcome" aria-label="Filter by last outcome">
<option value="">outcome: all</option>
{{range .OutcomeOptions}}<option value="{{.}}" {{if eq . $.TableState.Projects.FilterS}}selected{{end}}>{{.}}</option>{{end}}
</select>
<select name="size" aria-label="Rows per page">
{{$ps := .TableState.Projects.PerPage}}{{range .PageSizeOptions}}<option value="{{.}}" {{if eq . $ps}}selected{{end}}>{{if eq . 0}}all{{else}}{{.}}{{end}}</option>{{end}}
</select>
<select name="sort" aria-label="Sort projects by">
{{$psort := .TableState.Projects.Sort}}
<option value="" {{if eq $psort ""}}selected{{end}}>sort: default</option>
<option value="name" {{if eq $psort "name"}}selected{{end}}>name</option>
<option value="weight" {{if eq $psort "weight"}}selected{{end}}>weight</option>
<option value="priority" {{if eq $psort "priority"}}selected{{end}}>priority</option>
<option value="last_tick" {{if eq $psort "last_tick"}}selected{{end}}>last tick</option>
<option value="outcome" {{if eq $psort "outcome"}}selected{{end}}>outcome</option>
<option value="progress" {{if eq $psort "progress"}}selected{{end}}>progress</option>
<option value="next" {{if eq $psort "next"}}selected{{end}}>next tick</option>
<option value="cost_today" {{if eq $psort "cost_today"}}selected{{end}}>cost today</option>
</select>
<select name="dir" aria-label="Sort direction">
{{$pdir := .TableState.Projects.Dir}}
<option value="asc" {{if eq $pdir "asc"}}selected{{end}}>ascending</option>
<option value="desc" {{if eq $pdir "desc"}}selected{{end}}>descending</option>
</select>
<button type="submit">Apply</button>
{{if or .TableState.Projects.Q .TableState.Projects.FilterP .TableState.Projects.FilterS .TableState.Projects.Sort}}<a class="page-btn" href="/">Clear</a>{{end}}
</form>
</div>
<div class="showing-count">showing {{len .Projects}} of {{.TableState.Projects.FilteredTotal}} lanes</div>
<div class="table-wrap">
<table>
<thead><tr><th>Project</th><th>W</th><th>P</th><th>Last Tick</th><th>Outcome</th><th>Progress</th><th>Steps Left</th><th>Est. Completion</th><th>Next Tick</th><th>Cost</th><th>GitReins</th><th>Recent</th></tr></thead>
<tbody id="fleet-overview"
hx-get="/dashboard/partial?{{if .TableState.Projects.BaseQS}}{{.TableState.Projects.BaseQS}}&{{end}}page={{.TableState.Projects.Page}}"
hx-trigger="autorefresh from:body"
hx-swap="innerHTML">
{{range .Projects}}
<tr class="{{if not .Enabled}}disabled{{end}}">
<td data-parent-source="{{.Nesting.ParentSource}}">{{if .Nesting.Rail}}<span class="lane-rail">{{.Nesting.Rail}}</span> {{end}}<a href="/projects/{{.Name}}">{{.Name}}</a>{{if .Nesting.Parent}} <span class="lane-parent" title="satellite of {{.Nesting.Parent}} ({{.Nesting.ParentSource}})">↳ {{.Nesting.Parent}}{{if .Nesting.ParentKnown}} (L{{.Nesting.Depth}}){{end}}</span>{{end}}{{if .RecentFailures}} <span class="fail-flag" title="{{.RecentFailures}} of last {{.RecentTicks}} ticks failed/timed out">●</span>{{end}}</td>
<td class="num">{{.Weight}}</td>
<td class="num">{{.Priority}}</td>
<td class="meta">{{shortTime .LastTick}}</td>
<td>{{if eq .LastOutcome "committed"}}<span class="pill ok">committed</span>{{else if eq .LastOutcome "failed"}}<span class="pill fail">failed</span>{{else if eq .LastOutcome "timeout"}}<span class="pill warn">timeout</span>{{else}}<span class="meta">—</span>{{end}}</td>
<td>
{{if .BoardTotal}}
<div class="prog"><div class="prog-fill" style="width:{{percent .BoardDone .BoardTotal}}%"></div></div>
<span class="meta num">{{.BoardDone}}/{{.BoardTotal}} · {{percent .BoardDone .BoardTotal}}%</span>
{{else}}
<span class="meta">—</span>
{{end}}
</td>
<td>{{if .BoardTotal}}<span class="meta num">{{sub .BoardTotal .BoardDone}} left</span>{{else}}<span class="meta">—</span>{{end}}</td>
<td class="num">{{if .ETA}}{{localtime .CompletionAt}}{{if .EtaBreakdown}}<span class="meta" title="{{.EtaBreakdown}}">{{else}}<span class="meta" title="avg {{.AvgTickSecs}}s/tick · {{.SuccessRate}}% success">{{end}} · {{.ETA}}</span>{{else}}<span class="meta">—</span>{{end}}<br>{{if .ProjectedCost}}<span class="meta">~{{money .ProjectedCost}} left</span>{{end}}</td>
<td class="{{if eq .NextTickIn "running"}}status-running{{else if eq .NextTickIn "due now"}}status-ok{{end}}">{{if .NextTickIn}}{{.NextTickIn}}{{else}}—{{end}}</td>
<td class="num">{{if .CostToday}}<span title="today">${{printf "%.3f" .CostToday}}</span>{{else}}<span class="meta">—</span>{{end}}{{if sparkline .CostSeries}}<br>{{sparkline .CostSeries}}{{end}}</td>
<td>{{if lt .GitReinsPass 0}}<span class="meta">—</span>{{else}}{{if and (eq .GitReinsPass 100) (eq .CIConclusion "failure")}}<span class="pill fail" title="GitReins says 100% but CI failed — judge may be passing a red suite (cached/LLM-asserted). Trust CI.">{{.GitReinsPass}}% ⚠CI</span>{{else if eq .GitReinsPass 100}}<span class="pill ok">{{.GitReinsPass}}%</span>{{else if ge .GitReinsPass 70}}<span class="pill warn">{{.GitReinsPass}}%</span>{{else}}<span class="pill fail">{{.GitReinsPass}}%</span>{{end}}{{if eq .CIConclusion "failure"}} <span class="meta" title="CI failing">ci✗</span>{{else if eq .CIConclusion "success"}} <span class="meta" title="CI green">ci✓</span>{{end}}{{end}}</td>
<td class="num">{{if .RecentFailures}}<span class="status-fail">{{.RecentFailures}}/{{.RecentTicks}}</span>{{else if .RecentTicks}}<span class="status-ok">{{.RecentTicks}} ok</span>{{else}}<span class="meta">—</span>{{end}}</td>
</tr>{{end}}
{{if or .TableState.Projects.Q .TableState.Projects.FilterP .TableState.Projects.FilterS}}<tr><td colspan="12" class="meta">No lanes match the current search/filter{{if or .TableState.Projects.Q .TableState.Projects.FilterP .TableState.Projects.FilterS}} — clear it to see all lanes{{end}}.</td></tr>{{end}}
</tbody>
</table>
</div>
{{if gt .TableState.Projects.TotalPages 1}}
<div class="pagination">
{{if .TableState.Projects.HasPrevious}}<a class="page-btn" href="/?{{if .TableState.Projects.BaseQS}}{{.TableState.Projects.BaseQS}}&{{end}}page={{.TableState.Projects.PreviousPage}}">← Previous</a>{{else}}<span class="page-btn disabled">← Previous</span>{{end}}
<span class="page-current">Page {{.TableState.Projects.Page}} / {{.TableState.Projects.TotalPages}}</span>
{{if .TableState.Projects.HasNext}}<a class="page-btn" href="/?{{if .TableState.Projects.BaseQS}}{{.TableState.Projects.BaseQS}}&{{end}}page={{.TableState.Projects.NextPage}}">Next →</a>{{else}}<span class="page-btn disabled">Next →</span>{{end}}
</div>
{{end}}

<h2>Recent Ticks</h2>
{{/* SCHED-GAP-1598: server-side search + sort + page-size on the recent-ticks
     table. Sorting by spawned reverses the default newest-first order. */}}
<div class="table-controls">
<form class="tick-filter" method="get" action="/">
<input type="hidden" name="tpage" value="1">
<input type="search" name="tq" value="{{.TableState.Ticks.Q}}" placeholder="search recent ticks…" aria-label="Search recent ticks by project">
<select name="tsort" aria-label="Sort recent ticks by">
{{$tsort := .TableState.Ticks.Sort}}
<option value="" {{if eq $tsort ""}}selected{{end}}>sort: newest first</option>
<option value="project" {{if eq $tsort "project"}}selected{{end}}>project</option>
<option value="spawned" {{if eq $tsort "spawned"}}selected{{end}}>spawned (oldest first)</option>
</select>
<select name="tsize" aria-label="Recent ticks per page">
{{$tsz := .TableState.Ticks.PerPage}}{{range .PageSizeOptions}}<option value="{{.}}" {{if eq . $tsz}}selected{{end}}>{{if eq . 0}}all{{else}}{{.}}{{end}}</option>{{end}}
</select>
<button type="submit">Apply</button>
{{if or .TableState.Ticks.Q .TableState.Ticks.Sort}}<a class="page-btn" href="/">Clear</a>{{end}}
</form>
</div>
<div class="showing-count">showing {{len .RecentTicks}} of {{.TableState.Ticks.FilteredTotal}} ticks</div>
<div class="table-wrap">
<table>
<thead><tr><th>Project</th><th>Status</th><th>Outcome</th><th>Duration</th><th>Spawned</th><th>Commits</th><th>Files</th></tr></thead>
<tbody>
{{range .RecentTicks}}
<tr>
<td>{{.Project}}</td>
<td>{{if eq .Status "completed"}}<span class="pill ok">completed</span>{{else if eq .Status "failed"}}<span class="pill fail">failed</span>{{else if eq .Status "timeout"}}<span class="pill warn">timeout</span>{{else if eq .Status "running"}}<span class="pill run"><span class="running-dot"></span>running</span>{{else}}<span class="meta">—</span>{{end}}</td>
<td>{{if .Outcome}}{{.Outcome}}{{else}}—{{end}}</td>
<td class="num">{{if eq .Status "running"}}{{liveDur .SpawnedAt}}{{else if .Duration}}{{.Duration}}{{else}}<span class="meta">—</span>{{end}}</td>
<td class="meta">{{shortTime .SpawnedAt}}</td>
<td class="num">{{.Commits}}</td>
<td class="num">{{.FilesChanged}}</td>
</tr>{{end}}
{{if .TableState.Ticks.Q}}<tr><td colspan="7" class="meta">No ticks match the search.</td></tr>{{end}}
</tbody>
</table>
</div>
{{if gt .TableState.Ticks.TotalPages 1}}
<div class="pagination">
{{if .TableState.Ticks.HasPrevious}}<a class="page-btn" href="/?{{if .TableState.Ticks.BaseQS}}{{.TableState.Ticks.BaseQS}}&{{end}}tpage={{.TableState.Ticks.PreviousPage}}">← Previous</a>{{else}}<span class="page-btn disabled">← Previous</span>{{end}}
<span class="page-current">Page {{.TableState.Ticks.Page}} / {{.TableState.Ticks.TotalPages}}</span>
{{if .TableState.Ticks.HasNext}}<a class="page-btn" href="/?{{if .TableState.Ticks.BaseQS}}{{.TableState.Ticks.BaseQS}}&{{end}}tpage={{.TableState.Ticks.NextPage}}">Next →</a>{{else}}<span class="page-btn disabled">Next →</span>{{end}}
</div>
{{end}}

<h2>Namespaces</h2>
{{/* SCHED-GAP-1598: controls, table and count render for ALL tables
   regardless of row count; the paragraph below distinguishes "not
   configured" from "search matched nothing" from "empty page". */}}
<div class="table-controls">
<form class="tick-filter" method="get" action="/">
<input type="hidden" name="npage" value="1">
<input type="search" name="nq" value="{{.TableState.Namespaces.Q}}" placeholder="search namespaces…" aria-label="Search namespaces">
<select name="nsort" aria-label="Sort namespaces by">
{{$nsort := .TableState.Namespaces.Sort}}{{$nsq := .TableState.Namespaces.Q}}
<option value="" {{if eq $nsort ""}}selected{{end}}>sort: default</option>
<option value="id" {{if eq $nsort "id"}}selected{{end}}>name</option>
<option value="weight" {{if eq $nsort "weight"}}selected{{end}}>weight</option>
<option value="allocated" {{if eq $nsort "allocated"}}selected{{end}}>allocated</option>
<option value="used" {{if eq $nsort "used"}}selected{{end}}>used</option>
<option value="utilization" {{if eq $nsort "utilization"}}selected{{end}}>utilization</option>
<option value="projects" {{if eq $nsort "projects"}}selected{{end}}>projects</option>
</select>
<select name="nsize" aria-label="Namespaces per page">
{{$nsz := .TableState.Namespaces.PerPage}}{{range .PageSizeOptions}}<option value="{{.}}" {{if eq . $nsz}}selected{{end}}>{{if eq . 0}}all{{else}}{{.}}{{end}}</option>{{end}}
</select>
<button type="submit">Apply</button>
{{if or .TableState.Namespaces.Q .TableState.Namespaces.Sort}}<a class="page-btn" href="/">Clear</a>{{end}}
</form>
</div>
<div class="showing-count">showing {{len .Namespaces}} of {{.TableState.Namespaces.FilteredTotal}} namespaces</div>
<div class="table-wrap">
<table>
<thead><tr><th>Namespace</th><th>Weight</th><th>Reserved</th><th>Hard Cap</th><th>Allocated</th><th>Used</th><th>Demand</th><th>Status</th><th>Utilization</th><th>Borrowed</th><th>Lent</th><th>Projects</th></tr></thead>
<tbody>
{{range .Namespaces}}
<tr class="{{utilClass .Reserved .HardCap .Used}}">
  <td class="mono">{{.ID}}</td>
  <td>{{.Weight}}</td>
  <td>{{.Reserved}}</td>
  <td>{{if .HardCap}}{{.HardCap}}{{else}}∞{{end}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td>{{.Demand}}</td>
  <td>{{if .Overcommitted}}<span class="pill fail" title="enabled weight {{.Demand}} exceeds the allocated {{.Allocated}} by {{.Overcommitted}} — the surplus is held each cycle (reason: budget)">over by {{.Overcommitted}}</span>{{else}}<span class="pill ok">within budget</span>{{end}}</td>
  <td><div class="urgency-bar" style="width:{{printf "%.0f" .Utilization}}%;background:{{utilColor .Utilization}}"></div>{{printf "%.0f" .Utilization}}%</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td>{{.ProjectCount}}</td>
</tr>{{end}}
{{/* SCHED-GAP-1598: the empty-state row tests the VALIDATED search term ($nsq),
     not .Namespaces — a search that matches nothing empties the sliced slice,
     so a guard on the slice would never fire. */}}
{{if eq $nsq ""}}{{else}}<tr><td colspan="12" class="meta">No namespaces match the search.</td></tr>{{end}}
</tbody>
</table>
</div>
{{if gt .TableState.Namespaces.TotalPages 1}}
<div class="pagination">
{{if .TableState.Namespaces.HasPrevious}}<a class="page-btn" href="/?{{if .TableState.Namespaces.BaseQS}}{{.TableState.Namespaces.BaseQS}}&{{end}}npage={{.TableState.Namespaces.PreviousPage}}">← Previous</a>{{else}}<span class="page-btn disabled">← Previous</span>{{end}}
<span class="page-current">Page {{.TableState.Namespaces.Page}} / {{.TableState.Namespaces.TotalPages}}</span>
{{if .TableState.Namespaces.HasNext}}<a class="page-btn" href="/?{{if .TableState.Namespaces.BaseQS}}{{.TableState.Namespaces.BaseQS}}&{{end}}npage={{.TableState.Namespaces.NextPage}}">Next →</a>{{else}}<span class="page-btn disabled">Next →</span>{{end}}
</div>
{{end}}
<p class="meta">{{if .TableState.Namespaces.Q}}No namespaces match the search{{else if not .TableState.Namespaces.FilteredTotal}}No namespaces configured{{else}}No namespaces on this page{{end}}</p>

<h2>Namespace Utilization History</h2>
{{/* SCHED-GAP-1598: same rule as the namespaces table — controls, table and
   count always render; the paragraph distinguishes "no data" from "no
   match". */}}
<div class="table-controls">
<form class="tick-filter" method="get" action="/">
<input type="hidden" name="hpage" value="1">
<input type="search" name="hq" value="{{.TableState.NSHistory.Q}}" placeholder="search namespaces…" aria-label="Search utilization history by namespace">
<select name="hsort" aria-label="Sort utilization history by">
{{$hsort := .TableState.NSHistory.Sort}}
<option value="" {{if eq $hsort ""}}selected{{end}}>sort: newest first</option>
<option value="namespace" {{if eq $hsort "namespace"}}selected{{end}}>namespace</option>
<option value="created" {{if eq $hsort "created"}}selected{{end}}>oldest first</option>
</select>
<select name="hsize" aria-label="History rows per page">
{{$hsz := .TableState.NSHistory.PerPage}}{{range .PageSizeOptions}}<option value="{{.}}" {{if eq . $hsz}}selected{{end}}>{{if eq . 0}}all{{else}}{{.}}{{end}}</option>{{end}}
</select>
<button type="submit">Apply</button>
{{if or .TableState.NSHistory.Q .TableState.NSHistory.Sort}}<a class="page-btn" href="/">Clear</a>{{end}}
</form>
</div>
<div class="showing-count">showing {{len .NamespaceTicks}} of {{.TableState.NSHistory.FilteredTotal}} history rows</div>
<div class="table-wrap">
<table>
<thead><tr><th>Namespace</th><th>Tick Group</th><th>Allocated</th><th>Used</th><th>Borrowed</th><th>Lent</th><th>Time</th></tr></thead>
<tbody>
{{range .NamespaceTicks}}
<tr>
  <td class="mono">{{.NamespaceID}}</td>
  <td>{{.TickGroup}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td class="meta">{{shortTime .CreatedAt}}</td>
</tr>{{end}}
{{if .TableState.NSHistory.Q}}<tr><td colspan="7" class="meta">No history rows match the search.</td></tr>{{end}}
</tbody>
</table>
</div>
{{if gt .TableState.NSHistory.TotalPages 1}}
<div class="pagination">
{{if .TableState.NSHistory.HasPrevious}}<a class="page-btn" href="/?{{if .TableState.NSHistory.BaseQS}}{{.TableState.NSHistory.BaseQS}}&{{end}}hpage={{.TableState.NSHistory.PreviousPage}}">← Previous</a>{{else}}<span class="page-btn disabled">← Previous</span>{{end}}
<span class="page-current">Page {{.TableState.NSHistory.Page}} / {{.TableState.NSHistory.TotalPages}}</span>
{{if .TableState.NSHistory.HasNext}}<a class="page-btn" href="/?{{if .TableState.NSHistory.BaseQS}}{{.TableState.NSHistory.BaseQS}}&{{end}}hpage={{.TableState.NSHistory.NextPage}}">Next →</a>{{else}}<span class="page-btn disabled">Next →</span>{{end}}
</div>
{{end}}
{{if not .TableState.NSHistory.FilteredTotal}}<p class="meta">No namespace tick data available{{if .TableState.NSHistory.Q}} matching the search{{end}}</p>{{end}}
</div>
{{template "ready_js"}}
</body>
</html>

<style>
/* SCHED-GAP-1598: overview table controls. The form reuses the tick-filter
   bar styles (layout-level classes); the pagination styles live only on the
   tick-history template's own <style> block, so they are restated here. */
.showing-count{color:var(--muted);font-size:0.74rem;font-family:var(--mono);margin:-6px 0 8px}
.pagination{display:flex;gap:8px;align-items:center;justify-content:center;margin:14px 0}
.page-btn{padding:6px 14px;border:1px solid var(--border);border-radius:7px;color:var(--text);font-size:0.82rem;font-weight:510;transition:background 160ms var(--ease-out)}
.page-btn:hover{background:var(--surface2);text-decoration:none}
.page-btn.disabled{opacity:0.4;cursor:default}
.page-current{color:var(--muted);font-size:0.82rem;padding:6px 12px}
</style>`

// SetClock installs the clock this generator renders time through
// (SCHED-GAP-169). nil keeps the wall clock. Installing a clock re-anchors the
// uptime origin at that clock's instant, so a simulated run reports simulated
// uptime instead of mixing a wall-clock start with a simulated now.
func (g *Generator) SetClock(c clock.Clock) {
	if c == nil {
		return
	}
	g.clk.Set(c)
	g.started = c.Now()
}

// clock returns the generator's clock, never nil.
func (g *Generator) clock() clock.Clock { return g.clk.Get() }
