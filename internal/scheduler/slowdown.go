package scheduler

import (
	"bytes"
	"database/sql"
	"log"
	"strconv"
	"strings"
)

// autoSlowdown detects IDLE signals in tick output and doubles the project's cooldown.
// Cooldown caps at 1 hour (3600s). On first non-idle tick, cooldown resets to 600s.
func autoSlowdown(db *sql.DB, project string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		return
	}

	text := output.String()

	// Detect idle signal: "IDLE TICK" or "SLOWDOWN" in the foreman output.
	isIdle := strings.Contains(text, "IDLE TICK") || strings.Contains(text, "SLOWDOWN REQUESTED")

	// Extract idle tick number for logging.
	idleNum := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "IDLE TICK") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "TICK" && i+1 < len(parts) {
					// e.g. "IDLE TICK 1/7"
					nums := strings.Split(strings.TrimRight(parts[i+1], "/7"), "/")
					if len(nums) > 0 {
						idleNum, _ = strconv.Atoi(nums[0])
					}
				}
			}
		}
	}

	if isIdle {
		// Double the cooldown, cap at 1 hour.
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		if currentCD == 0 {
			currentCD = 600
		}
		newCD := currentCD * 2
		if newCD > 3600 {
			newCD = 3600
		}
		if newCD != currentCD {
			db.Exec("UPDATE projects SET cooldown_s = ? WHERE name = ?", newCD, project)
			log.Printf("SLOWDOWN: %s idle #%d → cooldown %ds → %ds (%dm)", project, idleNum, currentCD, newCD, newCD/60)
		}
	} else if idleNum == 0 {
		// Non-idle tick: reset cooldown to 600s if currently elevated.
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		if currentCD > 1200 {
			db.Exec("UPDATE projects SET cooldown_s = 600 WHERE name = ?", project)
			log.Printf("SLOWDOWN: %s active again → cooldown reset to 600s", project)
		}
	}
}

// timeoutBackoff doubles a project's cooldown after a timeout to prevent
// the spawn→timeout→spawn loop. Cap at 1 hour. When the project later
// completes successfully, the normal cooldown flow takes over.
func TimeoutBackoff(db *sql.DB, project string) {
	var currentCD int
	if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
		return
	}
	if currentCD == 0 {
		currentCD = 600
	}
	newCD := currentCD * 2
	if newCD > 3600 {
		newCD = 3600
	}
	if newCD != currentCD {
		db.Exec("UPDATE projects SET cooldown_s = ? WHERE name = ?", newCD, project)
		log.Printf("TIMEOUT-BACKOFF: %s timed out → cooldown %ds → %ds (%dm)", project, currentCD, newCD, newCD/60)
	}
}
