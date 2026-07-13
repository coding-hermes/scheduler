package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"time"
)

// Generator produces the fleet dashboard as a single-file HTML page.
type Generator struct {
	db *sql.DB
}

// NewGenerator creates a dashboard generator.
func NewGenerator(db *sql.DB) *Generator {
	return &Generator{db: db}
}

// FleetRow is one project in the fleet overview table.
type FleetRow struct {
	Name      string
	Weight    int
	Priority  int
	Enabled   bool
	LastTick  string
	LastOutcome string
	Urgency   float64
	RunningNow bool
}

// TickRow is one tick in the history table.
type TickRow struct {
	ID, Project, Status, Outcome, SpawnedAt, CompletedAt string
	Commits, FilesChanged int
}

// FleetData holds all data for the dashboard.
type FleetData struct {
	GeneratedAt    string
	BudgetTotal    int
	BudgetUsed     int
	ActiveTicks    int
	TotalProjects  int
	EnabledProjects int
	Projects       []FleetRow
	RecentTicks    []TickRow
}

// Generate writes the dashboard HTML to w.
func (g *Generator) Generate(w io.Writer) error {
	ctx := context.Background()
	data := g.collect(ctx)
	funcMap := template.FuncMap{
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
	}
	tmpl, err := template.New("dashboard").Funcs(funcMap).Parse(pageTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	return tmpl.Execute(w, data)
}

func (g *Generator) collect(ctx context.Context) FleetData {
	data := FleetData{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		BudgetTotal:  100,
	}

	// Project stats.
	rows, err := g.db.QueryContext(ctx, `SELECT name, weight, priority, enabled FROM projects ORDER BY name`)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r FleetRow
			if err := rows.Scan(&r.Name, &r.Weight, &r.Priority, &r.Enabled); err != nil {
				continue
			}
			data.TotalProjects++
			if r.Enabled {
				data.EnabledProjects++
				data.BudgetUsed += r.Weight
			}
			// Running check.
			_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE project_name=? AND status='running'`, r.Name).Scan(&r.RunningNow)
			// Last tick.
			_ = g.db.QueryRowContext(ctx, `SELECT spawned_at, outcome FROM ticks WHERE project_name=? ORDER BY spawned_at DESC LIMIT 1`, r.Name).Scan(&r.LastTick, &r.LastOutcome)
			// Urgency — simplified: priority * (1 + idle_hours).
			var lastTime sql.NullString
			_ = g.db.QueryRowContext(ctx, `SELECT MAX(spawned_at) FROM ticks WHERE project_name=?`, r.Name).Scan(&lastTime)
			if lastTime.Valid && lastTime.String != "" {
				if t, err := time.Parse(time.RFC3339, lastTime.String); err == nil {
					hours := time.Since(t).Hours()
					r.Urgency = float64(r.Priority) * (1 + hours)
				}
			}
			data.Projects = append(data.Projects, r)
		}
	}

	// Active ticks.
	g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status='running'`).Scan(&data.ActiveTicks)

	// Recent ticks.
	tickRows, _ := g.db.QueryContext(ctx, `SELECT id, project_name, status, COALESCE(outcome,''), spawned_at, COALESCE(completed_at,''), commits, files_changed FROM ticks ORDER BY spawned_at DESC LIMIT 20`)
	if tickRows != nil {
		defer tickRows.Close()
		for tickRows.Next() {
			var t TickRow
			tickRows.Scan(&t.ID, &t.Project, &t.Status, &t.Outcome, &t.SpawnedAt, &t.CompletedAt, &t.Commits, &t.FilesChanged)
			data.RecentTicks = append(data.RecentTicks, t)
		}
	}

	return data
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Coding Hermes Fleet</title>
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
@media(max-width:600px){table{font-size:0.75rem}th,td{padding:6px 8px}}
</style>
</head>
<body>
<h1>🚀 Coding Hermes Fleet</h1>
<div class="meta">Generated {{.GeneratedAt}} · Auto-refresh 60s</div>

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
<tbody>
{{range .Projects}}
<tr class="{{if not .Enabled}}disabled{{end}}">
<td>{{.Name}}</td>
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
</body>
</html>`
