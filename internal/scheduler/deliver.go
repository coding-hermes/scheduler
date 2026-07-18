package scheduler

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// deliverOutput sends tick output to the configured delivery target via Hermes' gateway.
// Uses `hermes send` which routes through the gateway to any configured platform
// (Telegram, Discord, Signal, Slack, etc.) — no platform-specific code needed.
//
// Before delivery, the raw stdout is trimmed to the foreman's final summary —
// tool output, diffs, and build logs are stripped so Telegram shows a clean report.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output to deliver", project, tickID)
		return
	}

	target := deliver
	if target == "" {
		target = "telegram:-1003310984808:83996" // default: scheduler foreman thread
	}

	// Trim to summary: extract the foreman's final report, skip tool noise.
	body := trimSummary(output.String())

	subject := fmt.Sprintf("🤖 %s [%s]", project, tickID)

	// Write trimmed output to a temp file for hermes send --file.
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

// trimSummary extracts the foreman's final report from the raw stdout,
// stripping tool output (diffs, build logs, terminal output) and keeping
// only the human-readable summary section at the end.
func trimSummary(raw string) string {
	// Find the last "---" separator — foreman reports delimit sections with it.
	lastDash := strings.LastIndex(raw, "\n---\n")
	if lastDash >= 0 {
		summary := strings.TrimSpace(raw[lastDash:])
		if len(summary) > 0 {
			return truncate(summary, 4000)
		}
	}

	// Fallback: find the last "Foreman Tick" or "Result:" or "Verdict:" marker.
	markers := []string{"\n---", "**Verdict:", "**Result:", "## Summary", "# Summary"}
	for _, m := range markers {
		idx := strings.LastIndex(raw, m)
		if idx >= 0 {
			summary := strings.TrimSpace(raw[idx:])
			return truncate(summary, 4000)
		}
	}

	// Last resort: take the final 40% (summary is always at the end).
	trimLen := len(raw) * 40 / 100
	if trimLen > 0 {
		return truncate(raw[len(raw)-trimLen:], 4000)
	}
	return truncate(raw, 4000)
}

// truncate cuts text to maxLen chars, adding "…" if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Find a clean break — try last newline within maxLen.
	cut := strings.LastIndex(s[:maxLen], "\n")
	if cut < maxLen/2 {
		cut = maxLen - 1
	}
	return s[:cut] + "\n…"
}
