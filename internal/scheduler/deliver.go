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
// Converts foreman markdown to platform-appropriate format before delivery.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output", project, tickID)
		return
	}

	target := deliver
	if target == "" {
		target = "telegram:-1003310984808:83996"
	}

	// Strip tool noise before --- separator, keep foreman's markdown summary
	body := trimToSummary(strings.TrimSpace(output.String()))

	// Convert markdown to platform-appropriate format
	body = formatForPlatform(body, target)

	// Append tick ID footer
	body = fmt.Sprintf("%s\n\n_%s_", body, tickID)

	// Cap length
	if len(body) > 3800 {
		body = body[:3800]
		if idx := strings.LastIndex(body, "\n"); idx > 3000 {
			body = body[:idx]
		}
	}

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

// trimToSummary strips tool output before the final "---" separator.
func trimToSummary(raw string) string {
	if idx := strings.LastIndex(raw, "\n---\n"); idx >= 0 {
		s := strings.TrimSpace(raw[idx+5:])
		if len(s) > 50 {
			return s
		}
	}
	return raw
}

// formatForPlatform converts markdown to the appropriate format for a delivery target.
// The foreman outputs clean markdown. This layer adapts it for each platform.
// New platforms: add a case here — no platform-specific code in the foreman prompt.
func formatForPlatform(markdown, target string) string {
	platform := strings.SplitN(target, ":", 2)[0]

	switch platform {
	case "telegram":
		return markdownToTelegram(markdown)
	case "discord":
		return markdownToDiscord(markdown)
	case "slack":
		return markdownToSlack(markdown)
	default:
		// email, signal, whatsapp, sms: markdown is fine
		return markdown
	}
}

// markdownToTelegram converts markdown to Telegram-friendly text.
// Telegram doesn't render code fences or HTML. Tables become key:value lines.
func markdownToTelegram(text string) string {
	text = stripCodeBlocks(text) // ``` → nothing
	text = convertTables(text)   // | Key | Val | → Key: Val
	text = strings.ReplaceAll(text, "`", "")                  // inline code
	return text
}

// markdownToDiscord handles Discord's partial markdown support.
func markdownToDiscord(text string) string {
	return text // Discord renders most markdown natively
}

// markdownToSlack handles Slack's mrkdwn format.
func markdownToSlack(text string) string {
	return text // Slack renders markdown natively
}

// stripCodeBlocks removes markdown code fences (triple backticks).
func stripCodeBlocks(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	inCode := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}
		if !inCode {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// convertTables converts markdown tables to "Key: Value" lines.
func convertTables(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	var tableBuf []string

	flushTable := func() {
		if len(tableBuf) < 2 {
			for _, l := range tableBuf {
				result = append(result, l)
			}
			tableBuf = nil
			return
		}
		sepIdx := -1
		for i, row := range tableBuf {
			if strings.Contains(row, "---") {
				sepIdx = i
				break
			}
		}
		if sepIdx < 0 {
			for _, l := range tableBuf {
				result = append(result, l)
			}
			tableBuf = nil
			return
		}
		for _, row := range tableBuf[sepIdx+1:] {
			parts := strings.Split(row, "|")
			if len(parts) >= 4 {
				key := strings.TrimSpace(strings.ReplaceAll(parts[1], "**", ""))
				val := strings.TrimSpace(strings.ReplaceAll(strings.Join(parts[2:len(parts)-1], " | "), "**", ""))
				if key != "" && val != "" {
					result = append(result, key+": "+val)
				}
			}
		}
		tableBuf = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			tableBuf = append(tableBuf, trimmed)
		} else {
			flushTable()
			result = append(result, line)
		}
	}
	flushTable()
	return strings.Join(result, "\n")
}
