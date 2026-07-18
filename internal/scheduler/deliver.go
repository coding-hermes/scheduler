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
// The foreman produces clean markdown. This layer passes it through to Hermes,
// which handles per-platform formatting. No stripping, no capping, no conversion.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output", project, tickID)
		return
	}

	target := deliver
	if target == "" {
		target = "telegram:-1003310984808:83996"
	}

	// Strip tool noise before the final --- separator, keep foreman's markdown
	body := trimToSummary(strings.TrimSpace(output.String()))
	body = fmt.Sprintf("%s\n\n_%s_", body, tickID)

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
// If no separator, keeps everything (foreman formatted it already).
func trimToSummary(raw string) string {
	if idx := strings.LastIndex(raw, "\n---\n"); idx >= 0 {
		s := strings.TrimSpace(raw[idx+5:])
		if len(s) > 50 {
			return s
		}
	}
	return raw
}
