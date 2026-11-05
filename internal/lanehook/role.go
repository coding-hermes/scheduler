package lanehook

import "strings"

// roleSuffixes is the canonical role vocabulary (mirrors lane_cadence.py). The
// order matters only for documentation; matching is longest-suffix first.
var roleSuffixes = []string{
	"foreman", "qa", "ops", "pm", "sync", "releng", "dogfood",
	"docs", "readme", "review", "perf",
}

// RoleFor returns the lane's role and its project name. A lane that is the
// project itself (`code-factory`) has role "primary". Composite names keep the
// trailing role: `code-factory-duckbrain-qa` → project `code-factory-duckbrain`,
// role `qa`.
func RoleFor(lane string) (project, role string) {
	best := ""
	bestLen := 0
	for _, r := range roleSuffixes {
		suf := "-" + r
		if strings.HasSuffix(lane, suf) && len(suf) > bestLen && len(lane) > len(suf) {
			best, bestLen = r, len(suf)
		}
	}
	if best == "" {
		return lane, "primary"
	}
	return lane[:len(lane)-bestLen], best
}
