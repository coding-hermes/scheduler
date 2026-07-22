package scheduler

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
)

// autoSlowdown detects IDLE signals in tick output and adjusts the project's cooldown.
// Uses the structured VERDICT: line from the foreman (e.g. "VERDICT: productively — IDLE").
// Cooldown caps at 1 hour. On any non-idle productive tick, resets to base 600s.
func autoSlowdown(db *sql.DB, project string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		return
	}

	text := output.String()

	// Detect idle: "VERDICT: ... — IDLE" or explicit "IDLE TICK" marker.
	isIdle := strings.Contains(text, "IDLE TICK") ||
		strings.Contains(text, "SLOWDOWN REQUESTED") ||
		(strings.Contains(text, "VERDICT:") && strings.Contains(text, "IDLE"))

	// Detect productive non-idle: "VERDICT: ... — PRODUCTIVE" or "FIXED"/"FIXED" keywords.
	isProductive := !isIdle && (strings.Contains(text, "VERDICT:") &&
		(strings.Contains(text, "PRODUCTIVE") || strings.Contains(text, "productively")))

	if isIdle {
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		if currentCD == 0 {
			currentCD = 600
		}
		// Multiply by 1.5x instead of 2x — gentler escalation.
		newCD := currentCD + currentCD/2
		if newCD > 86400 {
			newCD = 86400
		}
		if newCD != currentCD {
			db.Exec("UPDATE projects SET cooldown_s = ? WHERE name = ?", newCD, project)
			log.Printf("SLOWDOWN: %s idle → cooldown %ds → %ds (%dm)", project, currentCD, newCD, newCD/60)
		}
	} else if isProductive {
		// Productive non-idle tick: reset cooldown to base if elevated.
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		if currentCD > 600 {
			db.Exec("UPDATE projects SET cooldown_s = 600 WHERE name = ?", project)
			log.Printf("SLOWDOWN: %s productive \u2192 cooldown reset %ds \u2192 600s", project, currentCD)
		}
	}
}
