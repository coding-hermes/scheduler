package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

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

// Generate writes the dashboard HTML to w. Template is pre-parsed — zero hot-path overhead.
func (g *Generator) Generate(w io.Writer) error {
	ctx := context.Background()
	data := g.collect(ctx)
	return g.tmpl.ExecuteTemplate(w, "page", data)
}

// GenerateFleetTable renders the fleet table partial (tbody only) for htmx
// to swap into the dashboard page. Routes get this from /dashboard/partial.
func (g *Generator) GenerateFleetTable(w io.Writer) error {
	ctx := context.Background()
	data := g.collect(ctx)
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
// one-based; values below one are normalized to the first page.
func (g *Generator) GenerateTickHistory(w io.Writer, page int) error {
	data, err := g.tickHistoryData(page)
	if err != nil {
		return err
	}
	return g.tickHistoryTmpl.Execute(w, data)
}

// GenerateTickHistoryPartial renders only the pagination fragment for htmx
// polling (HX-Request). The page's #tick-history div polls /ticks with
// hx-swap=outerHTML, so the response must be the fragment — a full page
// swapped in compounds itself on every 30s refresh.
func (g *Generator) GenerateTickHistoryPartial(w io.Writer, page int) error {
	data, err := g.tickHistoryData(page)
	if err != nil {
		return err
	}
	return g.tickHistoryTmpl.ExecuteTemplate(w, "tick_history_partial", data)
}

// tickHistoryData loads the paginated tick list shared by the full page and
// the htmx partial.
func (g *Generator) tickHistoryData(page int) (TickHistoryData, error) {
	ctx := context.Background()
	if page < 1 {
		page = 1
	}

	var total int
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks`).Scan(&total); err != nil {
		return TickHistoryData{}, fmt.Errorf("count ticks: %w", err)
	}
	totalPages := (total + tickHistoryPageSize - 1) / tickHistoryPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	ticks, err := database.ListAllTicks(ctx, g.db, tickHistoryPageSize, (page-1)*tickHistoryPageSize)
	if err != nil {
		return TickHistoryData{}, fmt.Errorf("load tick history page %d: %w", page, err)
	}
	return TickHistoryData{
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
	}, nil
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

	rows, err := g.db.QueryContext(ctx, `SELECT name, COALESCE(weight,0), COALESCE(priority,0), COALESCE(cooldown_s,0), COALESCE(enabled,1), COALESCE(decay_rate,0), COALESCE(created_at,''), COALESCE(last_tick_completed,'') FROM projects WHERE enabled = 1 ORDER BY priority DESC LIMIT 200`)
	if err != nil {
		return data, fmt.Errorf("query queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	calc := g.urgencyCalc
	now := g.clock().Now()
	for rows.Next() {
		var e QueueEntry
		var decayRate float64
		var createdAtStr, lastStr string
		if err := rows.Scan(&e.Name, &e.Weight, &e.Priority, &e.CooldownS, &e.Enabled, &decayRate, &createdAtStr, &lastStr); err != nil {
			return data, fmt.Errorf("scan queue row: %w", err)
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
<div class="table-wrap">
<table>
<thead><tr><th>Project</th><th>W</th><th>P</th><th>Last Tick</th><th>Outcome</th><th>Progress</th><th>Steps Left</th><th>Est. Completion</th><th>Next Tick</th><th>Cost</th><th>GitReins</th><th>Recent</th></tr></thead>
<tbody id="fleet-overview"
hx-get="/dashboard/partial"
hx-trigger="autorefresh from:body"
hx-swap="innerHTML">
{{range .Projects}}
<tr class="{{if not .Enabled}}disabled{{end}}">
<td><a href="/projects/{{.Name}}">{{.Name}}</a>{{if .RecentFailures}} <span class="fail-flag" title="{{.RecentFailures}} of last {{.RecentTicks}} ticks failed/timed out">●</span>{{end}}</td>
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
</tbody>
</table>
</div>

<h2>Recent Ticks</h2>
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
</tbody>
</table>
</div>

<h2>Namespaces</h2>
{{if .Namespaces}}
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
</tbody>
</table>
</div>
{{else}}
<p class="meta">No namespaces configured</p>
{{end}}

<h2>Namespace Utilization History</h2>
{{if .NamespaceTicks}}
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
</tbody>
</table>
</div>
{{else}}
<p class="meta">No namespace tick data available</p>
{{end}}
</div>
{{template "ready_js"}}
</body>
</html>`

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
