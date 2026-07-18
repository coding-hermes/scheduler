package scheduler

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
)

// deliverOutput sends tick output to the configured delivery target via Hermes' gateway.
// Uses `hermes send` which routes through the gateway to any configured platform
// (Telegram, Discord, Signal, Slack, etc.) — no platform-specific code needed.
//
// deliver format: "platform:chat_id:thread_id" (from project deliver column).
// Falls back to "telegram:-1003310984808:83996" if no target is configured.
func deliverOutput(project, tickID, deliver string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		log.Printf("DELIVER: %s tick=%s — no output to deliver", project, tickID)
		return
	}

	target := deliver
	if target == "" {
		target = "telegram:-1003310984808:83996" // default: scheduler foreman thread
	}

	subject := fmt.Sprintf("🤖 Scheduler Tick: %s [%s]", project, tickID)

	// Write output to a temp file for hermes send --file.
	f, err := os.CreateTemp("", fmt.Sprintf("chtick-%s-*.txt", tickID))
	if err != nil {
		log.Printf("DELIVER: %s tick=%s — temp file: %v", project, tickID, err)
		return
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil {
			log.Printf("DELIVER: %s tick=%s — cleanup temp file: %v", project, tickID, err)
		}
	}()
	defer f.Close()

	if _, err := f.Write(output.Bytes()); err != nil {
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
