package scheduler

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// deliverOutput sends tick output to the configured delivery target via Hermes' gateway.
// Strips terminal tool output (diffs, review panels, worker prompts) and delivers
// the foreman's actual summary. No length cap — full detail preserved.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output", project, tickID)
		return
	}

	if deliver == "" {
		log.Printf("DELIVER: %s tick=%s — no delivery target configured", project, tickID)
		return
	}

	body := trimToolNoise(strings.TrimSpace(output.String()))
	body = fmt.Sprintf("%s\n\n_%s_", body, tickID)

	f, err := os.CreateTemp("", fmt.Sprintf("chtick-%s-*.txt", tickID))
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — temp file: %v", project, tickID, err)
		return
	}
	defer f.Close()
	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.WriteString(body); err != nil {
		log.Printf("DELIVER: %s tick=%s — write temp file: %v", project, tickID, err)
		return
	}
	f.Close()

	subject := fmt.Sprintf("🤖 %s [%s]", project, tickID)
	cmd := exec.Command("hermes", "send",
		"--to", deliver,
		"--subject", subject,
		"--file", f.Name(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — hermes send failed: %v (%s)", project, tickID, err, bytes.TrimSpace(out))
		return
	}
	log.Printf("DELIVER: %s tick=%s → %s", project, tickID, deliver)
}

// deliverAlert sends a short alert message for timeouts/errors so they
// are visible in the chat rather than silently swallowed.
func deliverAlert(deliver, project, tickID, reason string) {
	if deliver == "" {
		log.Printf("ALERT: %s tick=%s — %s (no delivery target configured)", project, tickID, reason)
		return
	}
	msg := fmt.Sprintf("⚠️ %s timed out — %s\nTick: %s", project, reason, tickID)
	f, err := os.CreateTemp("", fmt.Sprintf("chtick-alert-%s-*.txt", tickID))
	if err != nil {
		log.Printf("ALERT: temp file: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(msg)
	f.Close()
	cmd := exec.Command("hermes", "send", "-f", f.Name(), deliver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("ALERT: send failed: %v (%s)", err, bytes.TrimSpace(out))
		return
	}
	log.Printf("ALERT: %s tick=%s → %s", project, tickID, deliver)
}

// trimToolNoise strips terminal/tool output from the foreman's raw stdout,
// keeping only the human-written summary. Handles multiple noise sources:
//
// 1. Final "---" separator — everything before it is tool noise
// 2. "┊" prefixed lines — terminal review panels (review diff, review file)
// 3. Git diff blocks (+/-/@@ lines)
// 4. Worker prompt dumps (long unbroken instruction blocks)
func trimToolNoise(raw string) string {
	// Strategy 1: Final "---" separator is the strongest signal
	if idx := strings.LastIndex(raw, "\n---\n"); idx >= 0 {
		s := strings.TrimSpace(raw[idx+5:])
		if len(s) > 50 {
			return s
		}
	}

	// Strategy 2: Strip tool output lines and compact
	lines := strings.Split(raw, "\n")
	var result []string
	inDiff := false
	inCodeBlock := false
	skippingWorker := false

	// Patterns that indicate non-summary lines
	deltaRe := regexp.MustCompile(`^@@\s+-\d+`)
	pipeRe := regexp.MustCompile(`^\s*┊`)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip tool review panels (┊ review diff, ┊ review file, etc.)
		if pipeRe.MatchString(line) {
			continue
		}

		// Skip git diff blocks
		if deltaRe.MatchString(trimmed) {
			inDiff = true
			continue
		}
		if inDiff {
			if strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") ||
				strings.HasPrefix(trimmed, "a/") || strings.HasPrefix(trimmed, "b/") ||
				strings.HasPrefix(trimmed, "index ") || strings.HasPrefix(trimmed, "---") {
				continue
			}
			inDiff = false
		}

		// Skip code block fences (not useful in delivery)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// Skip worker prompt instructions (long, dense, no blank lines)
		// These are recognizable: start with "You are a coding agent" or
		// "## TASK:" after the foreman's actual report
		if strings.HasPrefix(trimmed, "You are a coding agent") ||
			strings.HasPrefix(trimmed, "## TASK:") ||
			strings.HasPrefix(trimmed, "## INSERTION POINT") ||
			strings.HasPrefix(trimmed, "## PATTERN") ||
			strings.HasPrefix(trimmed, "## STORE API") ||
			strings.HasPrefix(trimmed, "## ALL") {
			skippingWorker = true
			result = append(result, "…") // indicate skipped content
			continue
		}
		if skippingWorker {
			// Stop skipping on blank lines or markdown headings (end of worker prompt)
			if trimmed == "" || strings.HasPrefix(trimmed, "**") {
				skippingWorker = false
				if trimmed != "" {
					result = append(result, line)
				}
			}
			continue
		}

		result = append(result, line)
	}

	// Compact blank lines: max 2 consecutive
	compacted := make([]string, 0, len(result))
	blankCount := 0
	for _, line := range result {
		if strings.TrimSpace(line) == "" {
			blankCount++
			if blankCount <= 2 {
				compacted = append(compacted, line)
			}
		} else {
			blankCount = 0
			compacted = append(compacted, line)
		}
	}

	cleaned := strings.TrimSpace(strings.Join(compacted, "\n"))

	// If the result is suspiciously short, return the raw (don't over-trim)
	if len(cleaned) < 50 && len(raw) > 200 {
		return raw
	}

	return cleaned
}
