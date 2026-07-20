# S08 — Dashboard

**Status:** Draft  
**Depends on:** S01, S02, S06  
**Pages target:** 2-3

---

## 1. Overview

The scheduler ships with a built-in HTML dashboard at `/` on the daemon port (default `:9090`). It uses [htmx](https://htmx.org/) for partial page updates without a JavaScript framework. The dashboard is server-rendered — all HTML is generated from Go templates and served as full pages or htmx partials.

## 2. Architecture

```
Browser
  │ GET / → full page (generator.go)
  │ htmx GET /dashboard/partial → partial HTML (htmx_test.go)
  ▼
internal/dashboard/
  ├── generator.go           — Full page renderer, wires all partials
  ├── generator_data.go      — Data collection: fleet status, project list, tick history
  ├── generator_templates.go — HTML template strings (embedded, no external files)
  └── htmx_test.go           — Tests for htmx partial endpoints
```

## 3. Pages & Endpoints

| Route | Type | Description |
|---|---|---|
| `/` | Full page | Fleet overview dashboard |
| `/dashboard/partial` | htmx partial | Project table refresh (polled) |
| `/projects/{name}` | Full page | Per-project detail with tick history |
| `/queue` | Full page | Global queue view |
| `/ticks?page=N` | Full page | Paginated tick history |
| `/namespaces/{id}` | Full page | Namespace drill-down |
| `/health` | Full page | Health panel (uptime, DB status, spawn counts) |

## 4. Data Flow

1. **Request arrives** → `ServeHTTP` routes to generator
2. **Generator collects data** via `collect()` — queries DB for projects, namespaces, ticks
3. **Template renders** HTML from embedded template strings
4. **htmx partials** return only the section that changed (e.g., `/dashboard/partial` returns just the project table `<tbody>`)

## 5. Data Collection (`generator_data.go`)

- `collect()` — one-shot data gather for full page render
- `collectProjectDetail()` — per-project drill-down
- Queries use the database package directly (no API calls)
- Known issue: AUDIT-014 — N+1 query pattern in `collect()` for namespace lookups

## 6. Template System

- Templates are Go string constants (`const templateFoo = "..."`) embedded in `generator_templates.go`
- No external `.html` template files — everything compiles into the binary
- Dark theme (`#0d1117` background, GitHub-style color palette)
- Mobile-responsive via CSS grid (`grid-template-columns: repeat(auto-fit, minmax(...))`)

## 7. htmx Integration

- `htmx.min.js` served from `/static/` (embedded in binary)
- Partial updates use `hx-get` and `hx-trigger="every 30s"` for auto-refresh
- No WebSocket — polling model is simple and sufficient for fleet dashboards
