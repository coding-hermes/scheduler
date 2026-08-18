package dashboard

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

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
		"sub": func(a, b int) int { return a - b },
		"duration": func(spawned, completed string) string {
			return tickDuration(spawned, completed)
		},
		// liveDur renders the live elapsed time for a running tick (now minus
		// spawned), as "12m 30s". Returns "—" when spawned is empty/unparseable.
		"liveDur": func(spawned string) string {
			if spawned == "" {
				return "—"
			}
			s, err := time.Parse(time.RFC3339, spawned)
			if err != nil {
				return "—"
			}
			d := time.Since(s)
			if d < 0 {
				d = 0
			}
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
		},
		// liveCost estimates the running cost of a live tick by scaling the
		// project's average cost-per-tick by the fraction of the average tick
		// duration already elapsed. Returns 0 when there's no signal.
		"liveCost": func(spawned string, avgSecs int, avgCost float64) float64 {
			if spawned == "" || avgSecs <= 0 || avgCost <= 0 {
				return 0
			}
			s, err := time.Parse(time.RFC3339, spawned)
			if err != nil {
				return 0
			}
			elapsed := time.Since(s).Seconds()
			if elapsed <= 0 {
				return 0
			}
			frac := elapsed / float64(avgSecs)
			if frac > 1 {
				frac = 1 // cap at one full tick's average cost
			}
			return avgCost * frac
		},
		// localtime renders a UTC RFC3339 timestamp as a <time> element with a
		// data-utc attribute; the page's JS converts it to the viewer's local
		// timezone (the server can't know where each person connects from).
		"localtime": func(utc string) template.HTML {
			if utc == "" {
				return "—"
			}
			return template.HTML(fmt.Sprintf(`<time class="local" data-utc="%s">…</time>`, template.HTMLEscapeString(utc)))
		},
		"money": func(v float64) string {
			return fmt.Sprintf("$%.2f", v)
		},
		// linechart renders a hand-rolled SVG line chart from []SpeedCostPoint.
		// mode "speed" plots tick duration (seconds), "cost" plots cost_usd.
		"linechart": func(pts []SpeedCostPoint, mode string) template.HTML {
			return renderLineChart(pts, mode)
		},
		// sparkline renders a small inline SVG line chart from a []float64 cost
		// series (w×h viewBox). Empty/zero-series → "—". No external chart lib:
		// this dashboard is no-CDN/no-build (stdlib Go templates).
		"sparkline": func(series []float64) template.HTML {
			const w, h = 64, 20
			// A 1-element series would divide by len-1 == 0 (NaN x-coords) —
			// render the placeholder instead.
			if len(series) < 2 {
				return "—"
			}
			maxv := series[0]
			for _, v := range series {
				if v > maxv {
					maxv = v
				}
			}
			// Build polyline points.
			pts := make([]string, 0, len(series))
			for i, v := range series {
				x := float64(i) * w / float64(len(series)-1)
				y := h - 2.0
				if maxv > 0 {
					y = h - 2.0 - (v/maxv)*(h-4.0)
				}
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
			}
			return template.HTML(fmt.Sprintf(
				`<svg class="spark" width="%d" height="%d" viewBox="0 0 %d %d" aria-hidden="true"><polyline fill="none" stroke="var(--accent)" stroke-width="1.5" points="%s"/></svg>`,
				w, h, w, h, strings.Join(pts, " ")))
		},
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
		// utilColor returns a literal hex color: html/template's CSS sanitizer
		// (cssValueFilter) replaces unparseable values like `var(--x)` inside
		// inline style attributes with "ZgotmplZ" (GAP-055). Values match the
		// layout.html palette: --err, --warn/--signal, --ok.
		"utilColor": func(utilization float64) string {
			if utilization > 80 {
				return "#ff6b6b"
			}
			if utilization >= 50 {
				return "#e8a33d"
			}
			return "#37d399"
		},
		"add1": func(i int) int { return i + 1 },
		"urgencyPct": func(u float64) float64 {
			// Scale urgency 0..maxUrgency to 0..100 width.
			// Typical max urgency in practice ~500; cap at 100 for bar width.
			pct := u / 5.0
			if pct > 100 {
				return 100
			}
			return pct
		},
		// urgencyColor returns a literal hex color for the same sanitizer
		// reason as utilColor (GAP-055). Values match the layout.html palette:
		// --ok, --warn/--signal, --err.
		"urgencyColor": func(u float64) string {
			if u < 50 {
				return "#37d399"
			}
			if u < 200 {
				return "#e8a33d"
			}
			return "#ff6b6b"
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
