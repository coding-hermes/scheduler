package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
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

// mustReadStatic panics if the embedded asset cannot be read at init time —
// that always indicates a build problem (missing file), not a runtime fault.
func mustReadStatic(path string) []byte {
	data, err := staticFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("dashboard: embedded asset %s missing: %v", path, err))
	}
	return data
}

// loadTemplates parses every embedded template, applies the shared func map,
// and registers each {{define "..."}} block by name. Returns the parsed set.
func loadTemplates() *template.Template {
	funcs := template.FuncMap{
		"percent": func(used, total int) int {
			if total == 0 {
				return 0
			}
			return used * 100 / total
		},
		"shortTime": func(s string) string {
			if s == "" {
				return "—"
			}
			if len(s) >= 16 {
				return s[11:16]
			}
			return s
		},
		"add": func(a, b, c int) int { return a + b + c },
		"statusClass": func(s string) string {
			switch s {
			case "completed":
				return "status-ok"
			case "failed":
				return "status-fail"
			case "timeout":
				return "status-timeout"
			case "running":
				return "status-running"
			default:
				return ""
			}
		},
		"utilClass": func(reserved, hardCap, used int) string {
			if used < reserved {
				return "util-green"
			}
			if hardCap > 0 && used >= hardCap {
				return "util-red"
			}
			return "util-yellow"
		},
		"utilColor": func(utilization float64) string {
			if utilization > 80 {
				return "var(--red)"
			}
			if utilization >= 50 {
				return "var(--yellow)"
			}
			return "var(--green)"
		},
	}
	t := template.New("").Funcs(funcs)
	// Add the existing pageTemplate under the name "page" so it composes with
	// the partials and project-detail template in the same set.
	parsed, err := t.New("page").Parse(pageTemplate)
	if err != nil {
		panic(fmt.Sprintf("dashboard: parse pageTemplate: %v", err))
	}
	matches, err := templatesFS.ReadDir("templates")
	if err != nil {
		panic(fmt.Sprintf("dashboard: read embedded templates/: %v", err))
	}
	for _, entry := range matches {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			panic(fmt.Sprintf("dashboard: read template %s: %v", name, err))
		}
		if _, err := parsed.New(name).Parse(string(data)); err != nil {
			panic(fmt.Sprintf("dashboard: parse template %s: %v", name, err))
		}
	}
	return parsed
}

// Generator produces the fleet dashboard as a single-file HTML page.
type Generator struct {
	db          *sql.DB
	tmpl        *template.Template // parsed once, reused
	fleetTmpl   *template.Template // partial: project table body only
	projectTmpl *template.Template // full page: /projects/{name}
}

// NewGenerator creates a dashboard generator. Template is parsed at construction
// time so hot-path Generate() never pays the parse cost.
func NewGenerator(db *sql.DB) *Generator {
	tmpl := loadTemplates()
	g := &Generator{
		db:          db,
		tmpl:        tmpl,
		fleetTmpl:   tmpl.Lookup("fleet_table"),
		projectTmpl: tmpl.Lookup("project_detail"),
	}
	if g.fleetTmpl == nil {
		panic("dashboard: fleet_table template not registered")
	}
	if g.projectTmpl == nil {
		panic("dashboard: project_detail template not registered")
	}
	return g
}

// HTMXJS returns the bundled htmx library bytes for serving via HTTP.
func (g *Generator) HTMXJS() []byte { return htmxJS }

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
}

// TickRow is one tick in the history table.
type TickRow struct {
	ID, Project, Status, Outcome, SessionID, SpawnedAt, CompletedAt string
	Commits, FilesChanged                                           int
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

// FleetData holds all data for the dashboard.
type FleetData struct {
	GeneratedAt     string
	BudgetTotal     int
	BudgetUsed      int
	ActiveTicks     int
	TotalProjects   int
	EnabledProjects int
	Projects        []FleetRow
	RecentTicks     []TickRow
	Namespaces      []NamespaceRow
	NamespaceTicks  []NamespaceTickRow
	CostTodayTotal  float64
	CostWeekTotal   float64
}

// ProjectDetailData holds all data for the /projects/{name} page.
type ProjectDetailData struct {
	Project     *database.Project
	LatestTick  *database.Tick
	RecentTicks []database.Tick
}

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

func (g *Generator) collect(ctx context.Context) FleetData {
	data := FleetData{
		GeneratedAt: time.Now().Format(time.RFC3339),
		BudgetTotal: 100,
	}

	// ── Projects: batch query with per-project stats via LEFT JOINs ──
	// Single query replaces 7 per-project queries (N+1 → 1).
	// Note: outcome and session_id are fetched via a SECOND LEFT JOIN to ticks
	// (t2) rather than correlated subqueries — SQLite's modernc driver rejects
	// MAX() references inside correlated subqueries ("misuse of aggregate").
	projectQuery := `
		SELECT
			p.name, p.weight, p.priority, p.enabled,
			COALESCE(t.spawned_at, '')            AS last_tick,
			COALESCE(t2.outcome, '')               AS last_outcome,
			COALESCE(t2.session_id, '')            AS session_id,
			COALESCE(t.running, 0) > 0             AS running_now,
			COALESCE(t.completed, 0)               AS completed,
			COALESCE(t.failed, 0)                  AS failed,
			COALESCE(t.timed_out, 0)              AS timed_out,
			COALESCE(t.cost_today, 0.0)            AS cost_today,
			COALESCE(t.cost_week, 0.0)             AS cost_week
		FROM projects p
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
	dayAgo := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	rows, err := g.db.QueryContext(ctx, projectQuery, dayAgo, weekAgo)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r FleetRow
			if err := rows.Scan(&r.Name, &r.Weight, &r.Priority, &r.Enabled,
				&r.LastTick, &r.LastOutcome, &r.SessionID,
				&r.RunningNow, &r.Completed, &r.Failed, &r.Timeout,
				&r.CostToday, &r.CostWeek); err != nil {
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
					r.Urgency = float64(r.Priority) * (1 + time.Since(t).Hours())
				}
			}
			data.CostTodayTotal += r.CostToday
			data.CostWeekTotal += r.CostWeek
			data.Projects = append(data.Projects, r)
		}
	}

	// Active ticks count.
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&data.ActiveTicks)

	// Recent ticks.
	tickRows, _ := g.db.QueryContext(ctx, `SELECT id, project_name, status, COALESCE(outcome,''), COALESCE(session_id,''), spawned_at, COALESCE(completed_at,''), commits, files_changed FROM ticks ORDER BY spawned_at DESC LIMIT 20`)
	if tickRows != nil {
		defer tickRows.Close()
		for tickRows.Next() {
			var t TickRow
			_ = tickRows.Scan(&t.ID, &t.Project, &t.Status, &t.Outcome, &t.SessionID, &t.SpawnedAt, &t.CompletedAt, &t.Commits, &t.FilesChanged)
			data.RecentTicks = append(data.RecentTicks, t)
		}
	}

	// Namespaces.
	namespaces, err := database.ListNamespaces(ctx, g.db, false)
	if err == nil {
		for _, ns := range namespaces {
			row := NamespaceRow{
				ID:       ns.ID,
				Weight:   ns.Weight,
				Reserved: ns.Reserved,
				HardCap:  ns.HardCap,
			}
			ticks, terr := database.ListNamespaceTicks(ctx, g.db, ns.ID, 1)
			if terr == nil && len(ticks) > 0 {
				row.Allocated = ticks[0].Allocated
				row.Used = ticks[0].Used
				row.Borrowed = ticks[0].Borrowed
				row.Lent = ticks[0].Lent
			}
			if row.Allocated > 0 {
				row.Utilization = float64(row.Used) / float64(row.Allocated) * 100
			}
			_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE namespace_id=? AND enabled=1`, ns.ID).Scan(&row.ProjectCount)
			data.Namespaces = append(data.Namespaces, row)
		}
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
@media(max-width:600px){table{font-size:0.75rem}th,td{padding:6px 8px}}
</style>
</head>
<body>
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
