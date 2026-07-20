package dashboard

import (
	"fmt"
	"html/template"
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
		"urgencyColor": func(u float64) string {
			if u < 50 {
				return "var(--green)"
			}
			if u < 200 {
				return "var(--yellow)"
			}
			return "var(--red)"
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
