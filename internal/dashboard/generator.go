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
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
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
	db                *sql.DB
	tmpl              *template.Template // parsed once, reused
	fleetTmpl         *template.Template // partial: project table body only
	projectTmpl       *template.Template // full page: /projects/{name}
	queueTmpl         *template.Template // full page: /queue
	tickHistoryTmpl   *template.Template // full page: /ticks
	namespaceViewTmpl *template.Template // full page: /namespaces/{id}
	healthTmpl        *template.Template // full page: /health
	gatewayURL        string
	healthClient      *http.Client
	started           time.Time
}

// NewGenerator creates a dashboard generator. Template is parsed at construction
// time so hot-path Generate() never pays the parse cost. gatewayURL is optional;
// when supplied, the health panel probes its /health endpoint.
func NewGenerator(db *sql.DB, gatewayURL ...string) *Generator {
	tmpl := loadTemplates()
	var gateway string
	if len(gatewayURL) > 0 {
		gateway = strings.TrimRight(gatewayURL[0], "/")
	}
	g := &Generator{
		db:                db,
		tmpl:              tmpl,
		fleetTmpl:         tmpl.Lookup("fleet_table"),
		projectTmpl:       tmpl.Lookup("project_detail"),
		queueTmpl:         tmpl.Lookup("queue"),
		tickHistoryTmpl:   tmpl.Lookup("tick_history"),
		namespaceViewTmpl: tmpl.Lookup("namespace_view"),
		healthTmpl:        tmpl.Lookup("health"),
		gatewayURL:        gateway,
		healthClient:      &http.Client{Timeout: 2 * time.Second},
		started:           time.Now(),
	}
	for name, parsed := range map[string]*template.Template{
		"fleet_table":    g.fleetTmpl,
		"project_detail": g.projectTmpl,
		"queue":          g.queueTmpl,
		"tick_history":   g.tickHistoryTmpl,
		"namespace_view": g.namespaceViewTmpl,
		"health":         g.healthTmpl,
	} {
		if parsed == nil {
			panic("dashboard: " + name + " template not registered")
		}
	}
	return g
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

	data := ProjectDetailData{Project: project}

	// Latest tick for this project (single-row fetch).
	if latest, err := latestTickForProject(ctx, g.db, name); err == nil {
		data.LatestTick = latest
	}

	// Last 20 ticks for the history table.
	if ticks, err := database.ListTicks(ctx, g.db, name, 20); err == nil {
		data.RecentTicks = ticks
	}

	return g.projectTmpl.Execute(w, data)
}

const tickHistoryPageSize = 50

// GenerateTickHistory renders one page of the global tick history. Pages are
// one-based; values below one are normalized to the first page.
func (g *Generator) GenerateTickHistory(w io.Writer, page int) error {
	ctx := context.Background()
	if page < 1 {
		page = 1
	}

	var total int
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks`).Scan(&total); err != nil {
		return fmt.Errorf("count ticks: %w", err)
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
		return fmt.Errorf("load tick history page %d: %w", page, err)
	}
	data := TickHistoryData{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Ticks:        ticks,
		Page:         page,
		PageSize:     tickHistoryPageSize,
		TotalTicks:   total,
		TotalPages:   totalPages,
		HasPrevious:  page > 1,
		PreviousPage: page - 1,
		HasNext:      page < totalPages,
		NextPage:     page + 1,
	}
	return g.tickHistoryTmpl.Execute(w, data)
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
	ctx := context.Background()
	data := HealthData{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		DaemonStatus:   "running",
		DatabaseStatus: "connected",
		GatewayStatus:  "not configured",
		GatewayURL:     g.gatewayURL,
		Uptime:         time.Since(g.started).Round(time.Second).String(),
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
	return g.healthTmpl.Execute(w, data)
}

// GenerateQueue renders the evaluation queue page — all enabled projects
// sorted by urgency (descending) with their weight, priority, and cooldown.
func (g *Generator) GenerateQueue(w io.Writer) error {
	ctx := context.Background()
	data := QueueData{}

	rows, err := g.db.QueryContext(ctx, `
		SELECT p.name, p.weight, p.priority, p.cooldown_s, p.enabled
		FROM projects p
		WHERE p.enabled = 1
		ORDER BY p.name
	`)
	if err != nil {
		return fmt.Errorf("query queue: %w", err)
	}

	// Collect all projects first (close rows before nested queries to avoid
	// SQLite lock contention with modernc.org/sqlite).
	type raw struct {
		name      string
		weight    int
		priority  int
		cooldownS int
		enabled   bool
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.name, &r.weight, &r.priority, &r.cooldownS, &r.enabled); err != nil {
			continue
		}
		raws = append(raws, r)
	}
	_ = rows.Close()

	latestTickRows, err := g.db.QueryContext(ctx, `
		SELECT project_name, COALESCE(MAX(spawned_at), '')
		FROM ticks
		WHERE project_name IN (SELECT name FROM projects WHERE enabled = 1)
		GROUP BY project_name
	`)
	if err != nil {
		return fmt.Errorf("query latest queue ticks: %w", err)
	}
	lastTicks := make(map[string]string, len(raws))
	for latestTickRows.Next() {
		var projectName, spawnedAt string
		if err := latestTickRows.Scan(&projectName, &spawnedAt); err != nil {
			_ = latestTickRows.Close()
			return fmt.Errorf("scan latest queue tick: %w", err)
		}
		lastTicks[projectName] = spawnedAt
	}
	if err := latestTickRows.Err(); err != nil {
		_ = latestTickRows.Close()
		return fmt.Errorf("iterate latest queue ticks: %w", err)
	}
	_ = latestTickRows.Close()

	for _, r := range raws {
		e := QueueEntry{
			Name:      r.name,
			Weight:    r.weight,
			Priority:  r.priority,
			CooldownS: r.cooldownS,
			Enabled:   r.enabled,
			Urgency:   float64(r.priority) * 10.0, // base urgency from priority alone
		}
		if lastTick := lastTicks[r.name]; lastTick != "" {
			if t, err := time.Parse(time.RFC3339, lastTick); err == nil {
				e.Urgency = float64(r.priority) * (1 + time.Since(t).Hours())
			}
		}
		data.Entries = append(data.Entries, e)
		data.TotalWeight += r.weight
	}

	// Sort by urgency descending.
	sort.Slice(data.Entries, func(i, j int) bool {
		return data.Entries[i].Urgency > data.Entries[j].Urgency
	})

	data.Count = len(data.Entries)
	return g.queueTmpl.Execute(w, data)
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Coding Hermes Fleet</title>
<script src="/static/htmx.min.js"></script>
<style>
:root{--bg:#0d1117;--fg:#c9d1d9;--accent:#58a6ff;--green:#3fb950;--red:#f85149;--yellow:#d2991d;--muted:#8b949e;--border:#21262d;--card:#161b22}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:var(--bg);color:var(--fg);padding:16px;max-width:1200px;margin:0 auto}
h1{font-size:1.5rem;margin-bottom:4px}h2{font-size:1.1rem;margin:24px 0 8px}
.meta{color:var(--muted);font-size:0.8rem;margin-bottom:16px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px;margin-bottom:24px}
.card{background:var(--card);border:1px solid var(--border);border-radius:8px;padding:12px}
.card .label{color:var(--muted);font-size:0.75rem;text-transform:uppercase}
.card .value{font-size:1.5rem;font-weight:600;margin-top:4px}
.budget-bar{background:var(--card);border:1px solid var(--border);border-radius:8px;padding:12px;margin-bottom:16px}
.budget-fill{height:8px;background:linear-gradient(90deg,var(--green),var(--yellow),var(--red));border-radius:4px;margin-top:4px;transition:width .3s}
.budget-label{display:flex;justify-content:space-between;font-size:0.8rem;margin-top:4px;color:var(--muted)}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:8px;overflow:hidden;font-size:0.85rem}
th,td{padding:8px 12px;text-align:left;border-bottom:1px solid var(--border)}
th{background:var(--card);color:var(--muted);font-weight:600;text-transform:uppercase;font-size:0.7rem;position:sticky;top:0}
tr:last-child td{border-bottom:none}
.status-ok{color:var(--green)}.status-fail{color:var(--red)}.status-running{color:var(--accent);animation:pulse 1.5s infinite}
.running-dot{display:inline-block;width:6px;height:6px;background:var(--accent);border-radius:50%;margin-right:4px;animation:pulse 1.5s infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.3}}
.disabled{opacity:0.5}
.util-green{color:var(--green)}.util-yellow{color:var(--yellow)}.util-red{color:var(--red)}
.utilization-bar{display:inline-block;height:6px;background:var(--accent);border-radius:3px;margin-right:4px;vertical-align:middle;max-width:60px}
.htmx-indicator{color:var(--muted);font-size:0.7rem;margin-left:8px;display:none}
.htmx-request .htmx-indicator{display:inline}
.nav{display:flex;gap:12px;margin-bottom:20px}
.nav a{color:var(--accent);text-decoration:none;font-size:0.85rem;padding:4px 8px;border-radius:4px}
.nav a:hover{text-decoration:underline}.nav a.active{background:var(--accent);color:var(--bg)}
@media(max-width:600px){table{font-size:0.75rem}th,td{padding:6px 8px}}
</style>
</head>
<body>
<div class="nav">
<a href="/" class="active">Fleet Overview</a>
<a href="/queue">Queue</a>
<a href="/ticks">Tick History</a>
<a href="/health">Health</a>
</div>
<h1>🚀 Coding Hermes Fleet</h1>
<div class="meta">Generated {{.GeneratedAt}} · Auto-refresh 60s · Live updates via htmx every 10s</div>

<div class="cards">
<div class="card"><div class="label">Enabled Projects</div><div class="value">{{.EnabledProjects}}/{{.TotalProjects}}</div></div>
<div class="card"><div class="label">Active Ticks</div><div class="value">{{.ActiveTicks}}</div></div>
<div class="card"><div class="label">Budget</div><div class="value">{{.BudgetUsed}}/{{.BudgetTotal}}</div></div>
</div>

<div class="budget-bar">
<div class="budget-label"><span>Weight Budget</span><span>{{.BudgetUsed}}/{{.BudgetTotal}}</span></div>
<div class="budget-fill" style="width:{{percent .BudgetUsed .BudgetTotal}}%"></div>
</div>

<h2>Projects</h2>
<table>
<thead><tr><th>Project</th><th>W</th><th>P</th><th>Last Tick</th><th>Outcome</th><th>Running</th></tr></thead>
<tbody id="fleet-overview"
hx-get="/dashboard/partial"
hx-trigger="every 10s"
hx-swap="innerHTML">
{{range .Projects}}
<tr class="{{if not .Enabled}}disabled{{end}}">
<td><a href="/projects/{{.Name}}" style="color:var(--accent);text-decoration:none">{{.Name}}</a></td>
<td>{{.Weight}}</td>
<td>{{.Priority}}</td>
<td class="meta">{{shortTime .LastTick}}</td>
<td class="{{if eq .LastOutcome "committed"}}status-ok{{else if eq .LastOutcome "failed"}}status-fail{{end}}">{{.LastOutcome}}</td>
<td>{{if .RunningNow}}<span class="running-dot"></span>running{{end}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>Recent Ticks</h2>
<table>
<thead><tr><th>Project</th><th>Status</th><th>Outcome</th><th>Spawned</th><th>Commits</th><th>Files</th></tr></thead>
<tbody>
{{range .RecentTicks}}
<tr>
<td>{{.Project}}</td>
<td class="{{if eq .Status "completed"}}status-ok{{else if eq .Status "failed"}}status-fail{{else if eq .Status "running"}}status-running{{end}}">{{.Status}}</td>
<td>{{.Outcome}}</td>
<td class="meta">{{shortTime .SpawnedAt}}</td>
<td>{{.Commits}}</td>
<td>{{.FilesChanged}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>Namespaces</h2>
{{if .Namespaces}}
<table>
<thead><tr><th>Namespace</th><th>Weight</th><th>Reserved</th><th>Hard Cap</th><th>Allocated</th><th>Used</th><th>Utilization</th><th>Borrowed</th><th>Lent</th><th>Projects</th></tr></thead>
<tbody>
{{range .Namespaces}}
<tr class="{{utilClass .Reserved .HardCap .Used}}">
  <td>{{.ID}}</td>
  <td>{{.Weight}}</td>
  <td>{{.Reserved}}</td>
  <td>{{if .HardCap}}{{.HardCap}}{{else}}∞{{end}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td><div class="utilization-bar" style="width:{{printf "%.0f" .Utilization}}%;background:{{utilColor .Utilization}}"></div>{{printf "%.0f" .Utilization}}%</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td>{{.ProjectCount}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}
<p class="meta">No namespaces configured</p>
{{end}}

<h2>Namespace Utilization History</h2>
{{if .NamespaceTicks}}
<table>
<thead><tr><th>Namespace</th><th>Tick Group</th><th>Allocated</th><th>Used</th><th>Borrowed</th><th>Lent</th><th>Time</th></tr></thead>
<tbody>
{{range .NamespaceTicks}}
<tr>
  <td>{{.NamespaceID}}</td>
  <td>{{.TickGroup}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td class="meta">{{shortTime .CreatedAt}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}
<p class="meta">No namespace tick data available</p>
{{end}}
</body>
</html>`
