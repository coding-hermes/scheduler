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
// Formats the raw foreman output into a clean, consistent Telegram-friendly message.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output", project, tickID)
		return
	}

	target := deliver
	if target == "" {
		target = "telegram:-1003310984808:83996"
	}

	body := formatOutput(project, tickID, output.String())

	f, err := os.CreateTemp("", fmt.Sprintf("chtick-%s-*.txt", tickID))
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — temp file: %v", project, tickID, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if _, err := f.WriteString(body); err != nil {
		log.Printf("DELIVER: %s tick=%s — write temp file: %v", project, tickID, err)
		return
	}
	f.Close()

	subject := fmt.Sprintf("🤖 %s [%s]", project, tickID)
	cmd := exec.Command("hermes", "send",
		"--to", target,
		"--subject", subject,
		"--file", f.Name(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — hermes send failed: %v (%s)", project, tickID, err, bytes.TrimSpace(out))
		return
	}
	log.Printf("DELIVER: %s tick=%s → %s", project, tickID, target)
}

// formatOutput normalizes raw foreman output into a clean Telegram-friendly format.
// Extracts the verdict, status table, and key metrics into a consistent structure.
func formatOutput(project, tickID, raw string) string {
	trimmed := trimToSummary(raw)
	verdict := extractVerdict(trimmed)
	metrics := extractMetrics(trimmed)

	var b strings.Builder
	b.WriteString(verdict)
	if len(metrics) > 0 {
		b.WriteString("\n\n")
		b.WriteString(formatMetrics(metrics))
	}
	b.WriteString(fmt.Sprintf("\n\n_%s_", tickID))

	result := b.String()
	if len(result) > 3000 {
		result = result[:3000]
		if idx := strings.LastIndex(result, "\n"); idx > 2500 {
			result = result[:idx]
		}
		result += "\n…"
	}
	return strings.TrimSpace(result)
}

// trimToSummary extracts the final report section, skipping tool noise.
func trimToSummary(raw string) string {
	// Last "---" separator → summary after it
	if idx := strings.LastIndex(raw, "\n---\n"); idx >= 0 {
		s := strings.TrimSpace(raw[idx+5:])
		if len(s) > 50 {
			return s
		}
	}
	// Fallback: find verdict/result markers
	for _, m := range []string{"**Verdict:", "**Result:", "## Summary", "**Status:"} {
		if idx := strings.LastIndex(raw, m); idx >= 0 {
			return strings.TrimSpace(raw[idx:])
		}
	}
	// Last resort: final 40%
	t := len(raw) * 40 / 100
	return strings.TrimSpace(raw[len(raw)-t:])
}

// extractVerdict pulls the single most important line from the output.
func extractVerdict(text string) string {
	// Try explicit verdict markers — capture everything after the marker
	markerRe := regexp.MustCompile(`\*\*Verdict:\*\*\s*(.+?)(?:\n|$)`)
	if m := markerRe.FindStringSubmatch(text); len(m) > 1 {
		return "Verdict: " + strings.TrimSpace(strings.TrimRight(m[1], "*"))
	}
	resultRe := regexp.MustCompile(`\*\*Result:\*\*\s*(.+?)(?:\n|$)`)
	if m := resultRe.FindStringSubmatch(text); len(m) > 1 {
		return "Result: " + strings.TrimSpace(strings.TrimRight(m[1], "*"))
	}
	summaryRe := regexp.MustCompile(`##\s*Summary\s*\n(.+?)(?:\n|$)`)
	if m := summaryRe.FindStringSubmatch(text); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	// Fallback: first significant line under 120 chars, not a table row
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.ReplaceAll(l, "**", "")
		l = strings.TrimPrefix(l, "# ")
		if len(l) > 10 && len(l) < 120 && !strings.HasPrefix(l, "|") {
			return l
		}
	}
	return "Tick complete"
}

// extractMetrics finds key=value pairs and table rows in the output.
func extractMetrics(text string) map[string]string {
	m := make(map[string]string)

	// Table rows: | Key | Value |
	tableRe := regexp.MustCompile(`\|\s*\*?(.+?)\*?\s*\|\s*(.+?)\s*\|`)
	for _, match := range tableRe.FindAllStringSubmatch(text, -1) {
		key := strings.TrimSpace(strings.Trim(match[1], "*-✓✅⚠️❌"))
		val := strings.TrimSpace(match[2])
		if key != "" && key != "Check" && key != "Gate" && key != "Step" &&
			!strings.Contains(key, "---") {
			m[key] = val
		}
	}

	// Bold key-value: **Key:** value or **Key**: value
	kvRe := regexp.MustCompile(`\*\*(.+?)\*\*[:\s]+(.+?)(?:\n|$)`)
	for _, match := range kvRe.FindAllStringSubmatch(text, -1) {
		key := strings.TrimSpace(match[1])
		val := strings.TrimSpace(match[2])
		if len(key) < 30 && len(val) < 60 {
			m[key] = val
		}
	}

	return m
}

// formatMetrics renders extracted metrics as a clean Telegram-friendly list.
func formatMetrics(metrics map[string]string) string {
	// Priority order for meaningful metrics
	order := []string{
		"Build", "Tests", "Guard", "Audit", "Vulns",
		"Board", "CI", "Remote", "Deps", "Live",
		"Cost", "Commit", "Session",
	}
	seen := make(map[string]bool)
	var lines []string
	for _, k := range order {
		if v, ok := metrics[k]; ok {
			lines = append(lines, fmt.Sprintf("• %s: %s", k, v))
			seen[k] = true
		}
	}
	for k, v := range metrics {
		if !seen[k] && len(lines) < 12 {
			lines = append(lines, fmt.Sprintf("• %s: %s", k, v))
		}
	}
	return strings.Join(lines, "\n")
}
